// Package stream fans out environment change events to SDK connections.
//
// Each environment has a topic with a monotonically increasing sequence and
// a bounded ring buffer of recent events. A reconnecting client presents
// the ID of the last event it saw; if that event is still in the buffer the
// broker replays everything after it, otherwise the caller must fall back
// to sending a full snapshot. Event IDs embed the broker's start time, so
// IDs from before a server restart are recognised as unreplayable instead
// of being confused with new sequence numbers.
//
// Publishing never blocks. A subscriber that falls more than its channel
// buffer behind is disconnected; its client reconnects and catches up
// through replay or a fresh snapshot, which is cheaper and more predictable
// than letting one slow consumer stall every other.
package stream

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one message on an environment's stream.
type Event struct {
	ID   string
	Type string
	Data []byte
	seq  uint64
}

// Broker routes events from publishers to subscribers.
type Broker struct {
	mu         sync.Mutex
	epoch      string
	bufferSize int
	chanSize   int
	topics     map[string]*topic
	closed     bool
}

type topic struct {
	seq  uint64
	ring []Event // ring[seq % len] holds event seq once written
	subs map[*Subscription]struct{}
}

// Options configures a Broker.
type Options struct {
	// Buffer is the number of past events kept per environment for
	// replay. Defaults to 256.
	Buffer int
	// SubscriberBuffer is the channel capacity per subscriber. Defaults
	// to 64.
	SubscriberBuffer int
}

// NewBroker returns a ready broker.
func NewBroker(opts Options) *Broker {
	if opts.Buffer <= 0 {
		opts.Buffer = 256
	}
	if opts.SubscriberBuffer <= 0 {
		opts.SubscriberBuffer = 64
	}
	return &Broker{
		epoch:      strconv.FormatInt(time.Now().UnixNano(), 36),
		bufferSize: opts.Buffer,
		chanSize:   opts.SubscriberBuffer,
		topics:     map[string]*topic{},
	}
}

func (b *Broker) topic(env string) *topic {
	t := b.topics[env]
	if t == nil {
		t = &topic{ring: make([]Event, b.bufferSize), subs: map[*Subscription]struct{}{}}
		b.topics[env] = t
	}
	return t
}

func (b *Broker) formatID(seq uint64) string { return b.epoch + "-" + strconv.FormatUint(seq, 10) }

// parseID returns the sequence number encoded in id if it was issued by
// this broker instance.
func (b *Broker) parseID(id string) (uint64, bool) {
	epoch, seq, ok := strings.Cut(id, "-")
	if !ok || epoch != b.epoch {
		return 0, false
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	return n, err == nil
}

// Publish appends an event to env's stream and delivers it to current
// subscribers. It returns the stored event.
func (b *Broker) Publish(env, typ string, data []byte) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.topic(env)
	t.seq++
	ev := Event{ID: b.formatID(t.seq), Type: typ, Data: data, seq: t.seq}
	t.ring[t.seq%uint64(len(t.ring))] = ev
	for sub := range t.subs {
		select {
		case sub.ch <- ev:
		default:
			sub.lagged = true
			b.dropLocked(t, sub)
		}
	}
	return ev
}

// Subscription is a live feed of one environment's events. Read from C
// until it is closed; then check Lagged to see whether the broker dropped
// the subscription because it fell behind.
type Subscription struct {
	C      <-chan Event
	ch     chan Event
	env    string
	broker *Broker
	lagged bool
	done   bool
}

// Lagged reports whether the subscription was closed for falling behind.
// It must only be called after C has been closed.
func (s *Subscription) Lagged() bool {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	return s.lagged
}

// Close unsubscribes. It is safe to call more than once.
func (s *Subscription) Close() {
	s.broker.mu.Lock()
	defer s.broker.mu.Unlock()
	if t := s.broker.topics[s.env]; t != nil {
		s.broker.dropLocked(t, s)
	}
}

func (b *Broker) dropLocked(t *topic, s *Subscription) {
	if s.done {
		return
	}
	s.done = true
	delete(t.subs, s)
	close(s.ch)
}

// ErrClosed is returned by Subscribe after Shutdown.
var ErrClosed = fmt.Errorf("stream: broker is shut down")

// Subscribe starts a subscription to env. If lastEventID is non-empty the
// broker tries to resume after it: replay holds the missed events and
// resumed is true. When resumption is impossible (unknown or expired ID),
// resumed is false and the caller should send a full snapshot first. The
// subscription is registered atomically with computing the replay, so no
// event can fall between them.
func (b *Broker) Subscribe(env, lastEventID string) (sub *Subscription, replay []Event, resumed bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, nil, false, ErrClosed
	}
	t := b.topic(env)
	ch := make(chan Event, b.chanSize)
	sub = &Subscription{C: ch, ch: ch, env: env, broker: b}
	t.subs[sub] = struct{}{}

	if lastEventID == "" {
		return sub, nil, false, nil
	}
	last, ok := b.parseID(lastEventID)
	if !ok || last > t.seq {
		return sub, nil, false, nil
	}
	oldest := uint64(1)
	if t.seq > uint64(len(t.ring)) {
		oldest = t.seq - uint64(len(t.ring)) + 1
	}
	if last+1 < oldest {
		return sub, nil, false, nil
	}
	for seq := last + 1; seq <= t.seq; seq++ {
		replay = append(replay, t.ring[seq%uint64(len(t.ring))])
	}
	return sub, replay, true, nil
}

// LastEventID returns the ID of the newest event published to env, or ""
// if none has been. Handlers stamp full snapshots with it so a client that
// reconnects right after receiving one can resume from that point.
func (b *Broker) LastEventID(env string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.topics[env]
	if t == nil || t.seq == 0 {
		return ""
	}
	return b.formatID(t.seq)
}

// Subscribers returns the number of live subscriptions to env.
func (b *Broker) Subscribers(env string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t := b.topics[env]; t != nil {
		return len(t.subs)
	}
	return 0
}

// Shutdown closes every subscription and rejects new ones.
func (b *Broker) Shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, t := range b.topics {
		for s := range t.subs {
			b.dropLocked(t, s)
		}
	}
}
