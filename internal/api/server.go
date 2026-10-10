// Package api exposes flagpost over HTTP: the admin REST API under
// /api/v1, the SDK endpoints under /sdk/v1 (snapshot, server-sent event
// stream, event ingestion and remote evaluation) and a health check.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/J-X0-cloud/flagpost/internal/service"
)

// Config configures the API server.
type Config struct {
	// AdminToken authenticates admin API requests. It must be non-empty.
	AdminToken string
	// Logger receives request logs. Nil discards them.
	Logger *slog.Logger
	// Heartbeat is the interval between keep-alive comments on SDK
	// streams. Defaults to 25 seconds, comfortably below the idle
	// timeouts of common proxies.
	Heartbeat time.Duration
	// Now overrides the clock, for tests.
	Now func() time.Time
}

// Server holds the HTTP handlers.
type Server struct {
	svc       *service.Service
	cfg       Config
	log       *slog.Logger
	mux       *http.ServeMux
	adminHash []byte
}

// New builds a Server. Additional handlers, such as the admin UI, can be
// mounted with Handle before calling Handler.
func New(svc *service.Service, cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 25 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{svc: svc, cfg: cfg, log: cfg.Logger, mux: http.NewServeMux(), adminHash: []byte(cfg.AdminToken)}
	s.routes()
	return s
}

// Handle mounts an extra handler on the server's mux.
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Handler returns the root handler with middleware applied.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.requestLog(s.mux))
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.health)

	admin := func(pattern string, h http.HandlerFunc) { m.Handle(pattern, s.requireAdmin(h)) }
	admin("GET /api/v1/environments", s.listEnvironments)
	admin("POST /api/v1/environments", s.createEnvironment)
	admin("GET /api/v1/environments/{env}", s.getEnvironment)
	admin("PATCH /api/v1/environments/{env}", s.updateEnvironment)
	admin("DELETE /api/v1/environments/{env}", s.deleteEnvironment)
	admin("POST /api/v1/environments/{env}/sdk-key", s.rotateSDKKey)
	admin("GET /api/v1/environments/{env}/snapshot", s.adminSnapshot)
	admin("POST /api/v1/environments/{env}/evaluate", s.adminEvaluate)

	admin("GET /api/v1/flags", s.listFlags)
	admin("POST /api/v1/flags", s.createFlag)
	admin("GET /api/v1/flags/{flag}", s.getFlag)
	admin("PUT /api/v1/flags/{flag}", s.updateFlag)
	admin("DELETE /api/v1/flags/{flag}", s.deleteFlag)

	admin("GET /api/v1/environments/{env}/flags", s.listFlagStates)
	admin("GET /api/v1/environments/{env}/flags/{flag}", s.getFlagConfig)
	admin("PUT /api/v1/environments/{env}/flags/{flag}", s.putFlagConfig)
	admin("POST /api/v1/environments/{env}/flags/{flag}/toggle", s.toggleFlag)
	admin("GET /api/v1/environments/{env}/flags/{flag}/results", s.experimentResults)

	admin("GET /api/v1/environments/{env}/segments", s.listSegments)
	admin("POST /api/v1/environments/{env}/segments", s.createSegment)
	admin("GET /api/v1/environments/{env}/segments/{segment}", s.getSegment)
	admin("PUT /api/v1/environments/{env}/segments/{segment}", s.updateSegment)
	admin("DELETE /api/v1/environments/{env}/segments/{segment}", s.deleteSegment)

	admin("GET /api/v1/audit", s.listAudit)

	sdk := func(pattern string, h sdkHandler) { m.Handle(pattern, s.cors(s.requireSDK(h))) }
	sdk("GET /sdk/v1/snapshot", s.sdkSnapshot)
	sdk("GET /sdk/v1/stream", s.sdkStream)
	sdk("POST /sdk/v1/events", s.sdkEvents)
	sdk("POST /sdk/v1/evaluate", s.sdkEvaluate)
	m.Handle("OPTIONS /sdk/v1/", s.cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Store().Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// bearer extracts the credential from an Authorization header, accepting
// both "Bearer <token>" and a bare token.
func bearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}

// AdminTokenValid reports whether token matches the configured admin token
// in constant time. The UI uses it to validate its login form.
func (s *Server) AdminTokenValid(token string) bool {
	return len(s.adminHash) > 0 && subtle.ConstantTimeCompare([]byte(token), s.adminHash) == 1
}

func (s *Server) requireAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.AdminTokenValid(bearer(r)) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="flagpost"`)
			writeError(w, r, errUnauthorized)
			return
		}
		actor := strings.TrimSpace(r.Header.Get("X-Flagpost-Actor"))
		if actor == "" || len(actor) > 128 {
			actor = "api"
		}
		next := r.WithContext(service.WithActor(r.Context(), actor))
		h.ServeHTTP(w, next)
	})
}

func (s *Server) cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Access-Control-Allow-Origin", "*")
		hdr.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Last-Event-ID, If-None-Match")
		hdr.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		hdr.Set("Access-Control-Expose-Headers", "ETag")
		hdr.Set("Access-Control-Max-Age", "86400")
		h.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach Flush and deadlines on the
// underlying writer, which the SSE handler needs.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Server) requestLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		s.log.Info("request",
			"id", id, "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "bytes", sw.bytes, "duration", time.Since(start).Round(time.Microsecond))
	})
}

func (s *Server) recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic", "path", r.URL.Path, "value", v, "stack", string(debug.Stack()))
				writeError(w, r, errInternal)
			}
		}()
		h.ServeHTTP(w, r)
	})
}
