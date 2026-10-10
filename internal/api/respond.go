package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/J-X0-cloud/flagpost/internal/service"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/model"
)

// apiError is a client-facing error with an HTTP status and stable code.
type apiError struct {
	Status   int             `json:"-"`
	Code     string          `json:"code"`
	Message  string          `json:"message"`
	Problems []model.Problem `json:"problems,omitempty"`
}

func (e *apiError) Error() string { return e.Message }

var (
	errUnauthorized = &apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "missing or invalid credentials"}
	errInternal     = &apiError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal server error"}
)

func badRequest(format string, args ...any) error {
	return &apiError{Status: http.StatusBadRequest, Code: "bad_request", Message: fmt.Sprintf(format, args...)}
}

// toAPIError maps domain errors onto HTTP semantics.
func toAPIError(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	var ve *model.ValidationError
	switch {
	case errors.As(err, &ve):
		return &apiError{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: "validation failed", Problems: ve.Problems}
	case errors.Is(err, store.ErrNotFound):
		return &apiError{Status: http.StatusNotFound, Code: "not_found", Message: err.Error()}
	case errors.Is(err, store.ErrAlreadyExists):
		return &apiError{Status: http.StatusConflict, Code: "already_exists", Message: err.Error()}
	case errors.Is(err, store.ErrConflict):
		return &apiError{Status: http.StatusPreconditionFailed, Code: "version_conflict",
			Message: err.Error() + "; reload the resource and reapply your change"}
	case errors.Is(err, service.ErrInUse):
		return &apiError{Status: http.StatusConflict, Code: "in_use", Message: err.Error()}
	case errors.Is(err, service.ErrInvalid):
		return &apiError{Status: http.StatusBadRequest, Code: "bad_request", Message: err.Error()}
	}
	return errInternal
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, _ *http.Request, err error) {
	ae := toAPIError(err)
	writeJSON(w, ae.Status, map[string]any{"error": ae})
}

// fail writes err as a JSON error response. Unexpected errors are logged
// with their cause and reported to the client only as "internal".
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if toAPIError(err) == errInternal {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeError(w, r, err)
}

// maxBody bounds request bodies.
const maxBody = 8 << 20

// decode reads a JSON body into v, rejecting unknown fields and trailing
// data so that typos in field names are reported instead of ignored.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &apiError{Status: http.StatusRequestEntityTooLarge, Code: "too_large", Message: "request body too large"}
		}
		if errors.Is(err, io.EOF) {
			return badRequest("request body is empty")
		}
		return badRequest("invalid JSON: %v", err)
	}
	if dec.More() {
		return badRequest("unexpected data after JSON body")
	}
	return nil
}

// expectedVersion resolves the version a write is based on: an If-Match
// header wins over the body's version field. Absent both, the write is
// unconditional.
func expectedVersion(r *http.Request, bodyVersion *int64) (int64, error) {
	if im := strings.TrimSpace(r.Header.Get("If-Match")); im != "" {
		if im == "*" {
			return service.AnyVersion, nil
		}
		v, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(im, "W/"), `"`), 10, 64)
		if err != nil || v < 0 {
			return 0, badRequest("If-Match must be a quoted version number")
		}
		return v, nil
	}
	if bodyVersion != nil {
		return *bodyVersion, nil
	}
	return service.AnyVersion, nil
}

func etag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }
