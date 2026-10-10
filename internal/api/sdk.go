package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/internal/stream"
	"github.com/J-X0-cloud/flagpost/model"
)

type sdkHandler func(w http.ResponseWriter, r *http.Request, env model.Environment)

// requireSDK authenticates an SDK key from the Authorization header or, for
// browser EventSource clients that cannot set headers, the sdkKey query
// parameter.
func (s *Server) requireSDK(h sdkHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := bearer(r)
		if key == "" {
			key = r.URL.Query().Get("sdkKey")
		}
		env, err := s.svc.EnvironmentForSDKKey(r.Context(), key)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, errUnauthorized)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h(w, r, env)
	})
}

// sdkSnapshot serves the full evaluation state of the SDK key's
// environment. The environment revision is the ETag, so pollers that send
// If-None-Match get a cheap 304 when nothing changed.
func (s *Server) sdkSnapshot(w http.ResponseWriter, r *http.Request, env model.Environment) {
	tag := etag(env.Revision)
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "no-cache")
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	snap, err := s.svc.Snapshot(r.Context(), env.Key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(snap.Revision))
	writeJSON(w, http.StatusOK, snap)
}

func etagMatches(header, tag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "*" || strings.TrimPrefix(part, "W/") == tag {
			return true
		}
	}
	return false
}

// sseWriter formats server-sent events.
type sseWriter struct {
	w  *bufio.Writer
	rc *http.ResponseController
}

func (s *sseWriter) event(id, typ string, data []byte) error {
	if id != "" {
		s.w.WriteString("id: " + id + "\n")
	}
	s.w.WriteString("event: " + typ + "\n")
	// JSON produced by encoding/json never contains raw newlines, but
	// split defensively so arbitrary payloads stay well-formed.
	for _, line := range strings.Split(string(data), "\n") {
		s.w.WriteString("data: " + line + "\n")
	}
	s.w.WriteString("\n")
	return s.flush()
}

func (s *sseWriter) comment(text string) error {
	s.w.WriteString(": " + text + "\n\n")
	return s.flush()
}

func (s *sseWriter) flush() error {
	if err := s.w.Flush(); err != nil {
		return err
	}
	return s.rc.Flush()
}

// sdkStream serves a server-sent event stream of changes to the SDK key's
// environment. A new connection receives a "put" with the full snapshot; a
// reconnecting client that sends Last-Event-ID receives just the events it
// missed when the server still has them, and a fresh "put" otherwise.
func (s *Server) sdkStream(w http.ResponseWriter, r *http.Request, env model.Environment) {
	broker := s.svc.Broker()
	// Capture the newest event ID before subscribing: anything published
	// after this point is delivered on the subscription, and anything
	// before it is contained in the snapshot loaded below.
	putID := broker.LastEventID(env.Key)
	sub, replay, resumed, err := broker.Subscribe(env.Key, r.Header.Get("Last-Event-ID"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": "shutting_down", "message": err.Error()}})
		return
	}
	defer sub.Close()

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // streams outlive the server's write timeout
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: bufio.NewWriter(w), rc: rc}
	sw.w.WriteString("retry: 3000\n\n")

	if !resumed {
		snap, err := s.svc.Snapshot(r.Context(), env.Key)
		if err != nil {
			s.log.Error("stream snapshot", "env", env.Key, "err", err)
			return
		}
		data, err := json.Marshal(snap)
		if err != nil {
			s.log.Error("encode snapshot", "env", env.Key, "err", err)
			return
		}
		if err := sw.event(putID, model.EventPut, data); err != nil {
			return
		}
	}
	for _, ev := range replay {
		if err := sw.event(ev.ID, ev.Type, ev.Data); err != nil {
			return
		}
	}
	if err := sw.flush(); err != nil {
		return
	}
	s.pump(r.Context(), sw, sub.C)
}

// pump forwards events until the client goes away or the subscription
// ends (shutdown, or the client lagged and must reconnect).
func (s *Server) pump(ctx context.Context, sw *sseWriter, events <-chan stream.Event) {
	ticker := time.NewTicker(s.cfg.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := sw.event(ev.ID, ev.Type, ev.Data); err != nil {
				return
			}
		case <-ticker.C:
			if err := sw.comment("ping"); err != nil {
				return
			}
		}
	}
}

func (s *Server) sdkEvents(w http.ResponseWriter, r *http.Request, env model.Environment) {
	var batch model.EventBatch
	if err := decode(w, r, &batch); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.svc.RecordEvents(r.Context(), env.Key, batch, s.cfg.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// sdkEvaluate evaluates flags on the server for clients that cannot run
// the engine locally. Explanations are reserved for the admin API.
func (s *Server) sdkEvaluate(w http.ResponseWriter, r *http.Request, env model.Environment) {
	s.evaluate(w, r, env.Key, false)
}
