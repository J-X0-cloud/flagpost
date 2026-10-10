package sdk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/J-X0-cloud/flagpost/model"
)

func TestLRUSet(t *testing.T) {
	s := newLRUSet(3)
	for _, k := range []string{"a", "b", "c"} {
		if !s.Add(k) {
			t.Fatalf("%s reported as present", k)
		}
	}
	if s.Add("a") {
		t.Error("a should be present")
	}
	s.Add("d") // evicts b, the least recently used
	if s.Len() != 3 {
		t.Errorf("len = %d", s.Len())
	}
	if !s.Add("b") {
		t.Error("b should have been evicted")
	}
	if s.Add("a") {
		t.Error("a was refreshed and should have survived")
	}
	if newLRUSet(0).cap != 1 {
		t.Error("capacity should be clamped to 1")
	}
}

type recorder struct {
	batches []model.EventBatch
	fail    int
}

func (r *recorder) post(_ context.Context, b model.EventBatch) error {
	if r.fail > 0 {
		r.fail--
		return errors.New("server down")
	}
	r.batches = append(r.batches, b)
	return nil
}

func newTestProcessor(capacity int, r *recorder) *eventProcessor {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := func() time.Time {
		clock = clock.Add(time.Millisecond)
		return clock
	}
	return newEventProcessor(capacity, 100, r.post, slog.New(slog.DiscardHandler), now)
}

func TestEventProcessorDedupesExposures(t *testing.T) {
	r := &recorder{}
	p := newTestProcessor(100, r)
	p.exposure("f", "a", "u1")
	p.exposure("f", "a", "u1")
	p.exposure("f", "b", "u1")
	p.exposure("f", "a", "")
	p.conversion("buy", "u1")
	p.conversion("buy", "u1")
	p.conversion("", "u1")
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.batches) != 1 || len(r.batches[0].Exposures) != 2 || len(r.batches[0].Conversions) != 2 {
		t.Fatalf("batches = %+v", r.batches)
	}
	if err := p.flush(context.Background()); err != nil || len(r.batches) != 1 {
		t.Error("empty flush should not post")
	}
}

func TestEventProcessorRequeuesOnFailure(t *testing.T) {
	r := &recorder{fail: 1}
	p := newTestProcessor(100, r)
	for i := 0; i < 5; i++ {
		p.exposure("f", "a", fmt.Sprint(i))
	}
	if err := p.flush(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if p.pending() != 5 {
		t.Fatalf("pending after failure = %d", p.pending())
	}
	p.exposure("f", "a", "late")
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := r.batches[0].Exposures
	if len(got) != 6 || got[0].ContextKey != "0" || got[5].ContextKey != "late" {
		t.Errorf("order not preserved: %+v", got)
	}
}

func TestEventProcessorDropsOldestWhenFull(t *testing.T) {
	r := &recorder{}
	p := newTestProcessor(3, r)
	p.exposure("f", "a", "u1")
	p.conversion("buy", "u1")
	p.exposure("f", "a", "u2")
	p.conversion("buy", "u2") // drops the u1 exposure, the oldest event
	if p.pending() != 3 {
		t.Fatalf("pending = %d", p.pending())
	}
	_ = p.flush(context.Background())
	b := r.batches[0]
	if len(b.Exposures) != 1 || b.Exposures[0].ContextKey != "u2" || len(b.Conversions) != 2 {
		t.Errorf("unexpected batch %+v", b)
	}
}

func TestEventProcessorChunksLargeBatches(t *testing.T) {
	r := &recorder{}
	p := newEventProcessor(20000, 20000, r.post, slog.New(slog.DiscardHandler), time.Now)
	for i := 0; i < model.MaxEventBatch+10; i++ {
		p.exposure("f", "a", fmt.Sprint(i))
	}
	for i := 0; i < 5; i++ {
		p.conversion("buy", fmt.Sprint(i))
	}
	if err := p.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.batches) != 2 {
		t.Fatalf("batches = %d", len(r.batches))
	}
	first := len(r.batches[0].Exposures) + len(r.batches[0].Conversions)
	second := len(r.batches[1].Exposures) + len(r.batches[1].Conversions)
	if first != model.MaxEventBatch || second != 15 {
		t.Errorf("chunk sizes %d and %d", first, second)
	}
}

func TestBackoff(t *testing.T) {
	b := backoff{base: 100 * time.Millisecond, max: time.Second}
	for i := 0; i < 50; i++ {
		d := b.next()
		limit := min(100*time.Millisecond<<min(i, 20), time.Second)
		if d <= 0 || d > limit {
			t.Fatalf("attempt %d: delay %v exceeds %v", i, d, limit)
		}
	}
	b.reset()
	if b.attempt != 0 {
		t.Error("reset did not clear attempts")
	}
}
