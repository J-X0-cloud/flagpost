package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/internal/audit"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/model"
)

// listResponse wraps collections so fields can be added without breaking
// clients that expect an object.
type listResponse[T any] struct {
	Items []T `json:"items"`
}

func list[T any](items []T) listResponse[T] {
	if items == nil {
		items = []T{}
	}
	return listResponse[T]{Items: items}
}

// --- environments ----------------------------------------------------------

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) {
	envs, err := s.svc.ListEnvironments(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(envs))
}

type environmentInput struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var in environmentInput
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	env, err := s.svc.CreateEnvironment(r.Context(), model.Environment{Key: in.Key, Name: in.Name, Description: in.Description})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/environments/"+env.Key)
	writeJSON(w, http.StatusCreated, env)
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	env, err := s.svc.GetEnvironment(r.Context(), r.PathValue("env"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("env")
	cur, err := s.svc.GetEnvironment(r.Context(), key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Name != nil {
		cur.Name = *in.Name
	}
	if in.Description != nil {
		cur.Description = *in.Description
	}
	env, err := s.svc.UpdateEnvironment(r.Context(), key, cur.Name, cur.Description)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteEnvironment(r.Context(), r.PathValue("env")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rotateSDKKey(w http.ResponseWriter, r *http.Request) {
	env, err := s.svc.RotateSDKKey(r.Context(), r.PathValue("env"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) adminSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.svc.Snapshot(r.Context(), r.PathValue("env"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// evaluateRequest is shared by the admin and SDK evaluate endpoints.
type evaluateRequest struct {
	Context eval.Context `json:"context"`
	// Flag evaluates a single flag; when empty every flag is evaluated.
	Flag string `json:"flag,omitempty"`
	// Explain adds a decision trace to single-flag results.
	Explain bool `json:"explain,omitempty"`
}

func (s *Server) evaluate(w http.ResponseWriter, r *http.Request, env string, allowExplain bool) {
	var in evaluateRequest
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Flag != "" {
		res, err := s.svc.Evaluate(r.Context(), env, in.Flag, in.Context, in.Explain && allowExplain)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	all, err := s.svc.EvaluateAll(r.Context(), env, in.Context)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(all))
}

func (s *Server) adminEvaluate(w http.ResponseWriter, r *http.Request) {
	s.evaluate(w, r, r.PathValue("env"), true)
}

// --- flags -----------------------------------------------------------------

func (s *Server) listFlags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	archived, _ := strconv.ParseBool(q.Get("archived"))
	flags, err := s.svc.ListFlags(r.Context(), store.FlagFilter{Query: q.Get("q"), Tag: q.Get("tag"), IncludeArchived: archived})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(flags))
}

// flagInput is the writable subset of a flag. Version, when present, is
// the version the update is based on.
type flagInput struct {
	Key         string          `json:"key"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Kind        model.FlagKind  `json:"kind"`
	Variants    []model.Variant `json:"variants"`
	Tags        []string        `json:"tags"`
	Archived    bool            `json:"archived"`
	Version     *int64          `json:"version"`
}

func (in flagInput) flag() model.Flag {
	return model.Flag{Key: in.Key, Name: in.Name, Description: in.Description, Kind: in.Kind,
		Variants: in.Variants, Tags: in.Tags, Archived: in.Archived}
}

func (s *Server) createFlag(w http.ResponseWriter, r *http.Request) {
	var in flagInput
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	f, err := s.svc.CreateFlag(r.Context(), in.flag())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/flags/"+f.Key)
	w.Header().Set("ETag", etag(f.Version))
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) getFlag(w http.ResponseWriter, r *http.Request) {
	f, err := s.svc.GetFlag(r.Context(), r.PathValue("flag"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(f.Version))
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) updateFlag(w http.ResponseWriter, r *http.Request) {
	var in flagInput
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	key := r.PathValue("flag")
	if in.Key != "" && in.Key != key {
		s.fail(w, r, badRequest("key in body (%q) does not match the URL (%q)", in.Key, key))
		return
	}
	in.Key = key
	version, err := expectedVersion(r, in.Version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if version < 0 {
		cur, err := s.svc.GetFlag(r.Context(), key)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		version = cur.Version
	}
	f, err := s.svc.UpdateFlag(r.Context(), in.flag(), version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(f.Version))
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) deleteFlag(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteFlag(r.Context(), r.PathValue("flag")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- flag configuration ------------------------------------------------------

func (s *Server) listFlagStates(w http.ResponseWriter, r *http.Request) {
	snap, err := s.svc.Snapshot(r.Context(), r.PathValue("env"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	states := make([]*model.FlagState, 0, len(snap.Flags))
	for _, fs := range snap.Flags {
		states = append(states, fs)
	}
	sortStates(states)
	writeJSON(w, http.StatusOK, list(states))
}

func sortStates(states []*model.FlagState) {
	slices.SortFunc(states, func(a, b *model.FlagState) int { return strings.Compare(a.Flag.Key, b.Flag.Key) })
}

func (s *Server) getFlagConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.svc.GetFlagConfig(r.Context(), r.PathValue("env"), r.PathValue("flag"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(cfg.Version))
	writeJSON(w, http.StatusOK, cfg)
}

// configInput is the writable part of a flag configuration.
type configInput struct {
	Enabled       bool                 `json:"enabled"`
	OffVariant    string               `json:"offVariant"`
	Fallthrough   model.Serve          `json:"fallthrough"`
	Targets       []model.Target       `json:"targets"`
	Rules         []model.Rule         `json:"rules"`
	Prerequisites []model.Prerequisite `json:"prerequisites"`
	Salt          string               `json:"salt"`
	Version       *int64               `json:"version"`
	Comment       string               `json:"comment"`
	// Read-only fields echoed back by clients that PUT what they GET.
	FlagKey     string `json:"flagKey"`
	Environment string `json:"environment"`
	UpdatedAt   any    `json:"updatedAt"`
}

func (s *Server) putFlagConfig(w http.ResponseWriter, r *http.Request) {
	var in configInput
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	env, flag := r.PathValue("env"), r.PathValue("flag")
	if (in.FlagKey != "" && in.FlagKey != flag) || (in.Environment != "" && in.Environment != env) {
		s.fail(w, r, badRequest("flagKey/environment in body do not match the URL"))
		return
	}
	version, err := expectedVersion(r, in.Version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cfg := model.FlagConfig{
		Enabled: in.Enabled, OffVariant: in.OffVariant, Fallthrough: in.Fallthrough,
		Targets: in.Targets, Rules: in.Rules, Prerequisites: in.Prerequisites, Salt: in.Salt,
	}
	saved, err := s.svc.UpdateFlagConfig(r.Context(), env, flag, cfg, version, in.Comment)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(saved.Version))
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) toggleFlag(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool   `json:"enabled"`
		Comment string `json:"comment"`
	}
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	saved, err := s.svc.ToggleFlag(r.Context(), r.PathValue("env"), r.PathValue("flag"), in.Enabled, in.Comment)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(saved.Version))
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) experimentResults(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	confidence := 0.95
	if c := q.Get("confidence"); c != "" {
		v, err := strconv.ParseFloat(c, 64)
		if err != nil {
			s.fail(w, r, badRequest("confidence must be a number"))
			return
		}
		confidence = v
	}
	metric := q.Get("metric")
	if metric == "" {
		s.fail(w, r, badRequest("the metric query parameter is required"))
		return
	}
	res, err := s.svc.ExperimentResults(r.Context(), r.PathValue("env"), r.PathValue("flag"), metric, q.Get("control"), confidence)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- segments ----------------------------------------------------------------

func (s *Server) listSegments(w http.ResponseWriter, r *http.Request) {
	segs, err := s.svc.ListSegments(r.Context(), r.PathValue("env"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(segs))
}

type segmentInput struct {
	Key         string              `json:"key"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Included    []string            `json:"included"`
	Excluded    []string            `json:"excluded"`
	Rules       []model.SegmentRule `json:"rules"`
	Version     *int64              `json:"version"`
	Environment string              `json:"environment"`
	CreatedAt   any                 `json:"createdAt"`
	UpdatedAt   any                 `json:"updatedAt"`
}

func (in segmentInput) segment(env string) model.Segment {
	return model.Segment{Environment: env, Key: in.Key, Name: in.Name, Description: in.Description,
		Included: in.Included, Excluded: in.Excluded, Rules: in.Rules}
}

func (s *Server) createSegment(w http.ResponseWriter, r *http.Request) {
	var in segmentInput
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	env := r.PathValue("env")
	seg, err := s.svc.CreateSegment(r.Context(), in.segment(env))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/environments/"+env+"/segments/"+seg.Key)
	w.Header().Set("ETag", etag(seg.Version))
	writeJSON(w, http.StatusCreated, seg)
}

func (s *Server) getSegment(w http.ResponseWriter, r *http.Request) {
	seg, err := s.svc.GetSegment(r.Context(), r.PathValue("env"), r.PathValue("segment"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(seg.Version))
	writeJSON(w, http.StatusOK, seg)
}

func (s *Server) updateSegment(w http.ResponseWriter, r *http.Request) {
	var in segmentInput
	if err := decode(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	env, key := r.PathValue("env"), r.PathValue("segment")
	if in.Key != "" && in.Key != key {
		s.fail(w, r, badRequest("key in body (%q) does not match the URL (%q)", in.Key, key))
		return
	}
	in.Key = key
	version, err := expectedVersion(r, in.Version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	seg, err := s.svc.UpdateSegment(r.Context(), in.segment(env), version)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(seg.Version))
	writeJSON(w, http.StatusOK, seg)
}

func (s *Server) deleteSegment(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteSegment(r.Context(), r.PathValue("env"), r.PathValue("segment")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- audit -------------------------------------------------------------------

type auditPage struct {
	Items []audit.Entry `json:"items"`
	// Next is the "before" cursor for the following page, or 0.
	Next int64 `json:"next,omitempty"`
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := audit.Filter{
		Environment:  q.Get("env"),
		ResourceType: q.Get("type"),
		ResourceKey:  q.Get("key"),
		Actor:        q.Get("actor"),
		Limit:        50,
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			s.fail(w, r, badRequest("limit must be a positive integer"))
			return
		}
		f.Limit = min(n, store.MaxAuditPage)
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			s.fail(w, r, badRequest("before must be a positive integer"))
			return
		}
		f.BeforeID = n
	}
	entries, err := s.svc.ListAudit(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page := auditPage{Items: entries}
	if page.Items == nil {
		page.Items = []audit.Entry{}
	}
	if len(entries) == f.Limit {
		page.Next = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, page)
}
