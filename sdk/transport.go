package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/J-X0-cloud/flagpost/model"
)

// errUnauthorized is fatal: retrying with the same key cannot succeed.
func isFatal(err error) bool { return errors.Is(err, ErrUnauthorized) }

// streamLoop keeps a stream connected, reconnecting with backoff.
func (c *Client) streamLoop(ctx context.Context) {
	bo := backoff{base: 500 * time.Millisecond, max: 30 * time.Second}
	lastID := ""
	for ctx.Err() == nil {
		start := time.Now()
		id, retry, err := c.streamOnce(ctx, lastID)
		lastID = id
		if ctx.Err() != nil {
			return
		}
		if isFatal(err) {
			c.cfg.Logger.Error("flagpost: stopping updates", "err", err)
			c.setFatal(err)
			return
		}
		if time.Since(start) > time.Minute {
			bo.reset() // the connection was healthy; this is a fresh failure
		}
		delay := bo.next()
		if retry > 0 && delay < retry {
			delay = retry
		}
		c.cfg.Logger.Info("flagpost: stream disconnected; reconnecting", "err", err, "in", delay.Round(time.Millisecond))
		if !sleep(ctx, delay) {
			return
		}
	}
}

// streamOnce runs one stream connection until it fails. It returns the ID
// of the last event received, to resume from, and any retry hint.
func (c *Client) streamOnce(ctx context.Context, lastID string) (string, time.Duration, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/sdk/v1/stream", nil)
	if err != nil {
		return lastID, 0, err
	}
	c.authorize(req)
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return lastID, 0, err
	}
	defer res.Body.Close()
	if err := statusError(res); err != nil {
		return lastID, 0, err
	}

	// The watchdog cancels the request if the server goes silent, which
	// catches half-open TCP connections that would otherwise hang forever.
	idle := time.AfterFunc(c.cfg.StreamIdleTimeout, cancel)
	defer idle.Stop()
	r := newSSEReader(res.Body, lastID)
	r.onActivity = func() { idle.Reset(c.cfg.StreamIdleTimeout) }
	retry := func() time.Duration {
		if r.retry < 0 {
			return 0
		}
		return time.Duration(r.retry) * time.Millisecond
	}
	for {
		ev, err := r.Next()
		if err != nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("stream idle or cancelled: %w", err)
			}
			return r.lastID, retry(), err
		}
		if err := c.apply(ev); err != nil {
			// A malformed event means our view may be wrong; drop the
			// resume ID so the reconnect starts from a full snapshot.
			return "", retry(), err
		}
	}
}

func (c *Client) apply(ev sseEvent) error {
	switch ev.Event {
	case model.EventPut:
		var snap model.Snapshot
		if err := json.Unmarshal([]byte(ev.Data), &snap); err != nil {
			return fmt.Errorf("decode put: %w", err)
		}
		if snap.Flags == nil {
			snap.Flags = map[string]*model.FlagState{}
		}
		if snap.Segments == nil {
			snap.Segments = map[string]*model.Segment{}
		}
		c.setSnapshot(&snap, true)
	case model.EventPatch:
		var p model.StreamPatch
		if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
			return fmt.Errorf("decode patch: %w", err)
		}
		c.update(func(s *model.Snapshot) *model.Snapshot { return s.Apply(p) })
	case model.EventDelete:
		var d model.StreamDelete
		if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
			return fmt.Errorf("decode delete: %w", err)
		}
		c.update(func(s *model.Snapshot) *model.Snapshot { return s.Delete(d) })
	default:
		c.cfg.Logger.Debug("flagpost: ignoring unknown stream event", "event", ev.Event)
	}
	return nil
}

// pollLoop fetches the snapshot on an interval with jitter.
func (c *Client) pollLoop(ctx context.Context) {
	bo := backoff{base: time.Second, max: c.cfg.PollInterval}
	etag := ""
	for {
		var err error
		etag, err = c.pollOnce(ctx, etag)
		if ctx.Err() != nil {
			return
		}
		if isFatal(err) {
			c.cfg.Logger.Error("flagpost: stopping updates", "err", err)
			c.setFatal(err)
			return
		}
		delay := c.cfg.PollInterval
		if err != nil {
			c.cfg.Logger.Warn("flagpost: poll failed", "err", err)
			delay = bo.next()
		} else {
			bo.reset()
			// Up to 10% jitter keeps a fleet of pollers from aligning.
			delay += time.Duration(float64(delay) * 0.1 * randFloat())
		}
		if !sleep(ctx, delay) {
			return
		}
	}
}

func (c *Client) pollOnce(ctx context.Context, etag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/sdk/v1/snapshot", nil)
	if err != nil {
		return etag, err
	}
	c.authorize(req)
	if etag != "" && c.Ready() {
		req.Header.Set("If-None-Match", etag)
	}
	res, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return etag, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified {
		return etag, nil
	}
	if err := statusError(res); err != nil {
		return etag, err
	}
	var snap model.Snapshot
	if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
		return etag, fmt.Errorf("decode snapshot: %w", err)
	}
	if snap.Flags == nil {
		snap.Flags = map[string]*model.FlagState{}
	}
	if snap.Segments == nil {
		snap.Segments = map[string]*model.Segment{}
	}
	c.setSnapshot(&snap, true)
	if tag := res.Header.Get("ETag"); tag != "" {
		return tag, nil
	}
	return `"` + strconv.FormatInt(snap.Revision, 10) + `"`, nil
}

func statusError(res *http.Response) error {
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case res.StatusCode/100 != 2:
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("unexpected status %s: %s", res.Status, body)
	}
	return nil
}
