package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/J-X0-cloud/flagpost/model"
)

// eventProcessor buffers exposures and conversions and posts them to the
// server in batches. Buffers are bounded: when the server is unreachable
// for long enough to fill them, the oldest events are dropped and counted,
// because an SDK must never let analytics take its host process down.
type eventProcessor struct {
	post     func(ctx context.Context, b model.EventBatch) error
	capacity int
	log      *slog.Logger
	now      func() time.Time

	mu          sync.Mutex
	exposures   []model.ExposureEvent
	conversions []model.ConversionEvent
	seen        *lruSet
	dropped     int
	flushing    sync.Mutex // serialises flushes
}

func newEventProcessor(capacity, dedupe int, post func(context.Context, model.EventBatch) error, log *slog.Logger, now func() time.Time) *eventProcessor {
	return &eventProcessor{post: post, capacity: capacity, seen: newLRUSet(dedupe), log: log, now: now}
}

func (p *eventProcessor) exposure(flag, variant, contextKey string) {
	if contextKey == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seen.Add(flag + "\x00" + variant + "\x00" + contextKey) {
		return
	}
	if len(p.exposures)+len(p.conversions) >= p.capacity {
		p.dropOldestLocked()
	}
	p.exposures = append(p.exposures, model.ExposureEvent{Flag: flag, Variant: variant, ContextKey: contextKey, Timestamp: p.now()})
}

func (p *eventProcessor) conversion(event, contextKey string) {
	if contextKey == "" || event == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.exposures)+len(p.conversions) >= p.capacity {
		p.dropOldestLocked()
	}
	p.conversions = append(p.conversions, model.ConversionEvent{Event: event, ContextKey: contextKey, Timestamp: p.now()})
}

// dropOldestLocked discards the oldest buffered event of either kind.
func (p *eventProcessor) dropOldestLocked() {
	p.dropped++
	switch {
	case len(p.exposures) == 0:
		p.conversions = p.conversions[1:]
	case len(p.conversions) == 0:
		p.exposures = p.exposures[1:]
	case p.exposures[0].Timestamp.After(p.conversions[0].Timestamp):
		p.conversions = p.conversions[1:]
	default:
		p.exposures = p.exposures[1:]
	}
}

// pending returns the number of buffered events.
func (p *eventProcessor) pending() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.exposures) + len(p.conversions)
}

// flush posts everything buffered, in chunks no larger than the server's
// batch limit. On failure the unsent events are put back at the front of
// the buffer, subject to capacity, so the next flush retries them.
func (p *eventProcessor) flush(ctx context.Context) error {
	p.flushing.Lock()
	defer p.flushing.Unlock()

	p.mu.Lock()
	xs, cs := p.exposures, p.conversions
	p.exposures, p.conversions = nil, nil
	dropped := p.dropped
	p.dropped = 0
	p.mu.Unlock()
	if dropped > 0 {
		p.log.Warn("flagpost: dropped events because the buffer was full", "count", dropped)
	}

	for len(xs) > 0 || len(cs) > 0 {
		var b model.EventBatch
		n := min(len(xs), model.MaxEventBatch)
		b.Exposures, xs = xs[:n], xs[n:]
		m := min(len(cs), model.MaxEventBatch-n)
		b.Conversions, cs = cs[:m], cs[m:]
		if err := p.post(ctx, b); err != nil {
			p.requeue(slices.Concat(b.Exposures, xs), slices.Concat(b.Conversions, cs))
			return err
		}
	}
	return nil
}

func (p *eventProcessor) requeue(xs []model.ExposureEvent, cs []model.ConversionEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exposures = slices.Concat(xs, p.exposures)
	p.conversions = slices.Concat(cs, p.conversions)
	for len(p.exposures)+len(p.conversions) > p.capacity {
		p.dropOldestLocked()
	}
}

// postEvents is the HTTP implementation of eventProcessor.post.
func (c *Client) postEvents(ctx context.Context, b model.EventBatch) error {
	body, err := json.Marshal(b)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/sdk/v1/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	res, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("post events: %s", res.Status)
	}
	return nil
}
