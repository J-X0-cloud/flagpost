package stream

import (
	"fmt"
	"sync"
	"testing"
)

func drain(t *testing.T, sub *Subscription, n int) []Event {
	t.Helper()
	var out []Event
	for i := 0; i < n; i++ {
		ev, ok := <-sub.C
		if !ok {
			t.Fatalf("channel closed after %d of %d events", i, n)
		}
		out = append(out, ev)
	}
	return out
}

func TestPublishSubscribe(t *testing.T) {
	b := NewBroker(Options{})
	sub, replay, resumed, err := b.Subscribe("prod", "")
	if err != nil || replay != nil || resumed {
		t.Fatalf("fresh subscribe: %v %v %v", replay, resumed, err)
	}
	other, _, _, _ := b.Subscribe("dev", "")
	b.Publish("prod", "patch", []byte("1"))
	b.Publish("prod", "patch", []byte("2"))
	got := drain(t, sub, 2)
	if string(got[0].Data) != "1" || string(got[1].Data) != "2" || got[0].ID == got[1].ID {
		t.Errorf("unexpected events %+v", got)
	}
	select {
	case ev := <-other.C:
		t.Errorf("dev subscriber received prod event %+v", ev)
	default:
	}
	if b.Subscribers("prod") != 1 || b.Subscribers("nope") != 0 {
		t.Error("subscriber count wrong")
	}
	sub.Close()
	sub.Close() // idempotent
	if _, ok := <-sub.C; ok {
		t.Error("channel not closed after Close")
	}
	if b.Subscribers("prod") != 0 {
		t.Error("closed subscription still counted")
	}
}

func TestResume(t *testing.T) {
	b := NewBroker(Options{Buffer: 4})
	var ids []string
	for i := 1; i <= 6; i++ {
		ids = append(ids, b.Publish("prod", "patch", []byte(fmt.Sprint(i))).ID)
	}
	if b.LastEventID("prod") != ids[5] || b.LastEventID("dev") != "" {
		t.Fatal("LastEventID wrong")
	}
	tests := []struct {
		name    string
		last    string
		resumed bool
		replay  string
	}{
		{"up to date", ids[5], true, ""},
		{"one behind", ids[4], true, "6"},
		{"oldest replayable", ids[1], true, "3456"},
		{"expired", ids[0], false, ""},
		{"from another server instance", "abc-3", false, ""},
		{"from the future", b.formatID(99), false, ""},
		{"garbage", "not-an-id", false, ""},
		{"no separator", "17", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, replay, resumed, err := b.Subscribe("prod", tt.last)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			got := ""
			for _, ev := range replay {
				got += string(ev.Data)
			}
			if resumed != tt.resumed || got != tt.replay {
				t.Errorf("resumed=%v replay=%q, want %v %q", resumed, got, tt.resumed, tt.replay)
			}
		})
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	b := NewBroker(Options{SubscriberBuffer: 2})
	slow, _, _, _ := b.Subscribe("prod", "")
	fast, _, _, _ := b.Subscribe("prod", "")
	received := 0
	for i := 0; i < 5; i++ {
		b.Publish("prod", "patch", nil)
		received += len(drain(t, fast, 1)) // the fast reader keeps up
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != 2 || !slow.Lagged() {
		t.Errorf("slow subscriber got %d events, lagged=%v", n, slow.Lagged())
	}
	if received != 5 || b.Subscribers("prod") != 1 {
		t.Errorf("fast subscriber got %d events; %d subscribers remain", received, b.Subscribers("prod"))
	}
	fast.Close()
	if fast.Lagged() {
		t.Error("closing normally should not mark a subscription as lagged")
	}
}

func TestShutdown(t *testing.T) {
	b := NewBroker(Options{})
	sub, _, _, _ := b.Subscribe("prod", "")
	b.Shutdown()
	if _, ok := <-sub.C; ok {
		t.Error("subscription survived shutdown")
	}
	if sub.Lagged() {
		t.Error("shutdown should not mark subscriptions as lagged")
	}
	if _, _, _, err := b.Subscribe("prod", ""); err != ErrClosed {
		t.Errorf("subscribe after shutdown: %v", err)
	}
	sub.Close() // must not panic on a closed channel
}

func TestConcurrentPublishers(t *testing.T) {
	b := NewBroker(Options{SubscriberBuffer: 1000})
	sub, _, _, _ := b.Subscribe("prod", "")
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				b.Publish("prod", "patch", nil)
			}
		}()
	}
	wg.Wait()
	evs := drain(t, sub, 500)
	for i := 1; i < len(evs); i++ {
		if evs[i].seq != evs[i-1].seq+1 {
			t.Fatalf("events out of order at %d: %d after %d", i, evs[i].seq, evs[i-1].seq)
		}
	}
}
