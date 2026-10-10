// Package ui is flagpost's server-rendered admin interface. It is built
// with html/template and plain HTML forms, needs no JavaScript to work,
// and talks to the service layer directly, so every change it makes goes
// through the same validation, audit logging and streaming as the REST API.
package ui

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/internal/audit"
	"github.com/J-X0-cloud/flagpost/internal/service"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/model"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Config configures the UI.
type Config struct {
	// CheckToken validates the admin token entered on the login page.
	CheckToken func(string) bool
	// Secret signs session cookies. Use the admin token.
	Secret string
	Logger *slog.Logger
	Now    func() time.Time
}

// UI serves the admin interface under /ui/.
type UI struct {
	svc      *service.Service
	cfg      Config
	sessions *sessions
	pages    map[string]*template.Template
	mux      *http.ServeMux
}

// New parses the templates and builds the handler.
func New(svc *service.Service, cfg Config) (*UI, error) {
	if cfg.CheckToken == nil || cfg.Secret == "" {
		return nil, errors.New("ui: CheckToken and Secret are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	u := &UI{svc: svc, cfg: cfg, sessions: &sessions{secret: []byte(cfg.Secret), now: cfg.Now}, mux: http.NewServeMux()}
	if err := u.parse(); err != nil {
		return nil, err
	}
	u.routes()
	return u, nil
}

func toJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}

func weightPct(w int) string {
	return strconv.FormatFloat(float64(w)/float64(model.TotalWeight)*100, 'f', -1, 64) + "%"
}

var funcs = template.FuncMap{
	"json": toJSON,
	"raw":  func(b json.RawMessage) string { return string(b) },
	"time": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	},
	"join":   strings.Join,
	"pct":    func(f float64) string { return strconv.FormatFloat(f*100, 'f', 2, 64) + "%" },
	"weight": weightPct,
	"deref": func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	},
	"pathEscape": url.PathEscape,
}

func (u *UI) parse() error {
	u.pages = map[string]*template.Template{}
	layout, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		return err
	}
	entries, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "layout.html" {
			continue
		}
		body, err := fs.ReadFile(templateFS, "templates/"+name)
		if err != nil {
			return err
		}
		t, err := template.New("layout").Funcs(funcs).Parse(string(layout))
		if err != nil {
			return fmt.Errorf("parse layout: %w", err)
		}
		if _, err := t.Parse(string(body)); err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		u.pages[strings.TrimSuffix(name, ".html")] = t
	}
	return nil
}

// ServeHTTP implements http.Handler.
func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) { u.mux.ServeHTTP(w, r) }

func (u *UI) routes() {
	m := u.mux
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /ui/login", u.loginPage)
	m.HandleFunc("POST /ui/login", u.login)

	auth := func(pattern string, h pageHandler) { m.Handle(pattern, u.authed(h)) }
	auth("POST /ui/logout", u.logout)
	auth("GET /ui/{$}", u.home)
	auth("GET /ui/environments", u.environments)
	auth("POST /ui/environments", u.createEnvironment)
	auth("GET /ui/env/{env}/flags", u.flags)
	auth("POST /ui/flags", u.createFlag)
	auth("GET /ui/env/{env}/flags/{flag}", u.flag)
	auth("POST /ui/env/{env}/flags/{flag}/toggle", u.toggle)
	auth("POST /ui/env/{env}/flags/{flag}/config", u.saveConfig)
	auth("POST /ui/env/{env}/flags/{flag}/evaluate", u.evaluate)
	auth("GET /ui/env/{env}/segments", u.segments)
	auth("POST /ui/env/{env}/segments", u.saveSegment)
	auth("GET /ui/env/{env}/segments/{segment}", u.segment)
	auth("GET /ui/audit", u.auditLog)
}

// request carries per-request UI state into handlers and templates.
type request struct {
	w       http.ResponseWriter
	r       *http.Request
	session string
}

type pageHandler func(req *request) error

// page is the data passed to every template.
type page struct {
	Title        string
	CSRF         string
	Env          string
	Environments []model.Environment
	Flash        string
	Error        string
	Problems     []model.Problem
	Data         any
}

func (u *UI) authed(h pageHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := u.sessions.cookie(r)
		if !u.sessions.valid(session) {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/ui/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "session expired; log in again", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil || !u.sessions.checkCSRF(session, r.PostForm.Get("csrf")) {
				http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
				return
			}
		}
		ctx := service.WithActor(r.Context(), "ui")
		req := &request{w: w, r: r.WithContext(ctx), session: session}
		if err := h(req); err != nil {
			u.cfg.Logger.Error("ui handler", "path", r.URL.Path, "err", err)
			status := http.StatusInternalServerError
			msg := "internal error"
			if errors.Is(err, store.ErrNotFound) {
				status, msg = http.StatusNotFound, "not found"
			}
			http.Error(w, msg, status)
		}
	})
}

func (u *UI) render(req *request, name string, p page) error {
	t := u.pages[name]
	if t == nil {
		return fmt.Errorf("unknown page %q", name)
	}
	if req != nil {
		p.CSRF = u.sessions.csrfToken(req.session)
		envs, err := u.svc.ListEnvironments(req.r.Context())
		if err != nil {
			return err
		}
		p.Environments = envs
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		return err
	}
	w := req.w
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, err := buf.WriteTo(w)
	return err
}

// redirect sends the browser to path with an optional flash message.
func redirect(req *request, path, flash string) error {
	if flash != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "flash=" + url.QueryEscape(flash)
	}
	http.Redirect(req.w, req.r, path, http.StatusSeeOther)
	return nil
}

// userError separates problems a user can fix from server faults. It
// returns the message to show and true for the former.
func userError(err error) (string, []model.Problem, bool) {
	var ve *model.ValidationError
	switch {
	case errors.As(err, &ve):
		return "Please fix the problems below.", ve.Problems, true
	case errors.Is(err, store.ErrConflict):
		return "Someone else changed this while you were editing. Reload and reapply your change.", nil, true
	case errors.Is(err, store.ErrAlreadyExists), errors.Is(err, service.ErrInUse), errors.Is(err, service.ErrInvalid):
		return err.Error(), nil, true
	}
	return "", nil, false
}

// --- login --------------------------------------------------------------------

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	_ = u.renderLogin(w, r, "")
}

func (u *UI) renderLogin(w http.ResponseWriter, r *http.Request, msg string) error {
	var buf bytes.Buffer
	data := page{Title: "Sign in", Error: msg, Data: safeNext(r.URL.Query().Get("next"))}
	if err := u.pages["login"].ExecuteTemplate(&buf, "layout", data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if msg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_, err := buf.WriteTo(w)
	return err
}

// safeNext only allows redirects back into the UI, preventing open
// redirects through the login form.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/ui/") && !strings.HasPrefix(next, "//") && !strings.Contains(next, `\`) {
		return next
	}
	return "/ui/"
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !u.cfg.CheckToken(r.PostForm.Get("token")) {
		_ = u.renderLogin(w, r, "That token is not valid.")
		return
	}
	u.sessions.set(w, r)
	http.Redirect(w, r, safeNext(r.PostForm.Get("next")), http.StatusSeeOther)
}

func (u *UI) logout(req *request) error {
	u.sessions.clear(req.w)
	http.Redirect(req.w, req.r, "/ui/login", http.StatusSeeOther)
	return nil
}

// --- environments -----------------------------------------------------------------

func (u *UI) home(req *request) error {
	envs, err := u.svc.ListEnvironments(req.r.Context())
	if err != nil {
		return err
	}
	if len(envs) == 0 {
		return redirect(req, "/ui/environments", "")
	}
	return redirect(req, "/ui/env/"+url.PathEscape(envs[0].Key)+"/flags", "")
}

func (u *UI) environments(req *request) error {
	return u.render(req, "environments", page{Title: "Environments", Flash: req.r.URL.Query().Get("flash")})
}

func (u *UI) createEnvironment(req *request) error {
	f := req.r.PostForm
	env, err := u.svc.CreateEnvironment(req.r.Context(), model.Environment{
		Key: strings.TrimSpace(f.Get("key")), Name: strings.TrimSpace(f.Get("name")), Description: f.Get("description"),
	})
	if err != nil {
		msg, problems, ok := userError(err)
		if !ok {
			return err
		}
		req.w.WriteHeader(http.StatusUnprocessableEntity)
		return u.render(req, "environments", page{Title: "Environments", Error: msg, Problems: problems})
	}
	return redirect(req, "/ui/environments", "Created environment "+env.Key)
}

// --- flags ---------------------------------------------------------------------------

type flagRow struct {
	State *model.FlagState
	Serve string
}

func describeServe(s model.Serve) string {
	if s.Rollout == nil {
		return s.Variant
	}
	parts := make([]string, 0, len(s.Rollout.Variants))
	for _, wv := range s.Rollout.Variants {
		parts = append(parts, fmt.Sprintf("%s %s", wv.Variant, weightPct(wv.Weight)))
	}
	label := "rollout"
	if s.Rollout.Experiment {
		label = "experiment"
	}
	return label + ": " + strings.Join(parts, ", ")
}

func (u *UI) flags(req *request) error {
	env := req.r.PathValue("env")
	snap, err := u.svc.Snapshot(req.r.Context(), env)
	if err != nil {
		return err
	}
	q := strings.ToLower(req.r.URL.Query().Get("q"))
	var rows []flagRow
	for _, fs := range snap.Flags {
		if q != "" && !strings.Contains(strings.ToLower(fs.Flag.Key+" "+fs.Flag.Name+" "+strings.Join(fs.Flag.Tags, " ")), q) {
			continue
		}
		rows = append(rows, flagRow{State: fs, Serve: describeServe(fs.Config.Fallthrough)})
	}
	sortRows(rows)
	return u.render(req, "flags", page{Title: "Flags", Env: env, Flash: req.r.URL.Query().Get("flash"),
		Data: map[string]any{"Rows": rows, "Query": req.r.URL.Query().Get("q"), "Revision": snap.Revision}})
}

func sortRows(rows []flagRow) {
	slices.SortFunc(rows, func(a, b flagRow) int { return strings.Compare(a.State.Flag.Key, b.State.Flag.Key) })
}

func (u *UI) createFlag(req *request) error {
	f := req.r.PostForm
	env := f.Get("env")
	flag := model.Flag{
		Key:         strings.TrimSpace(f.Get("key")),
		Name:        strings.TrimSpace(f.Get("name")),
		Description: f.Get("description"),
		Kind:        model.FlagKind(f.Get("kind")),
	}
	for _, t := range strings.Split(f.Get("tags"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			flag.Tags = append(flag.Tags, t)
		}
	}
	if flag.Kind == model.KindBoolean && strings.TrimSpace(f.Get("variants")) == "" {
		flag.Variants = []model.Variant{
			{Key: "on", Value: json.RawMessage(`true`)},
			{Key: "off", Value: json.RawMessage(`false`)},
		}
	} else if err := json.Unmarshal([]byte(f.Get("variants")), &flag.Variants); err != nil {
		return redirect(req, "/ui/env/"+url.PathEscape(env)+"/flags", "Variants must be a JSON array: "+err.Error())
	}
	if _, err := u.svc.CreateFlag(req.r.Context(), flag); err != nil {
		msg, problems, ok := userError(err)
		if !ok {
			return err
		}
		return redirect(req, "/ui/env/"+url.PathEscape(env)+"/flags", msg+" "+formatProblems(problems))
	}
	return redirect(req, "/ui/env/"+url.PathEscape(env)+"/flags/"+url.PathEscape(flag.Key), "Created flag "+flag.Key)
}

func formatProblems(ps []model.Problem) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.Field + ": " + p.Message
	}
	return strings.Join(parts, "; ")
}

type flagPage struct {
	State      *model.FlagState
	ConfigJSON string
	Audit      []audit.Entry
	Context    string
	Result     *eval.Result
	Metric     string
	Results    *service.ExperimentResults
	ResultsErr string
	Operators  []model.Operator
}

func (u *UI) loadFlagPage(req *request, env, key string) (*flagPage, error) {
	ctx := req.r.Context()
	f, err := u.svc.GetFlag(ctx, key)
	if err != nil {
		return nil, err
	}
	cfg, err := u.svc.GetFlagConfig(ctx, env, key)
	if err != nil {
		return nil, err
	}
	entries, err := u.svc.ListAudit(ctx, audit.Filter{ResourceKey: key, Limit: 20})
	if err != nil {
		return nil, err
	}
	editable := editableConfig(cfg)
	p := &flagPage{
		State:      &model.FlagState{Flag: f, Config: cfg},
		ConfigJSON: toJSON(editable),
		Audit:      entries,
		Context:    `{"key": "user-1", "attributes": {"country": "NZ"}}`,
		Operators:  model.Operators(),
	}
	if metric := req.r.URL.Query().Get("metric"); metric != "" {
		p.Metric = metric
		res, err := u.svc.ExperimentResults(ctx, env, key, metric, "", 0.95)
		if err != nil {
			msg, _, ok := userError(err)
			if !ok {
				return nil, err
			}
			p.ResultsErr = msg
		} else {
			p.Results = &res
		}
	}
	return p, nil
}

// editableConfig is the subset of a configuration shown in the editor.
type editable struct {
	Enabled       bool                 `json:"enabled"`
	OffVariant    string               `json:"offVariant"`
	Fallthrough   model.Serve          `json:"fallthrough"`
	Targets       []model.Target       `json:"targets"`
	Rules         []model.Rule         `json:"rules"`
	Prerequisites []model.Prerequisite `json:"prerequisites"`
	Salt          string               `json:"salt"`
}

func editableConfig(c model.FlagConfig) editable {
	e := editable{Enabled: c.Enabled, OffVariant: c.OffVariant, Fallthrough: c.Fallthrough,
		Targets: c.Targets, Rules: c.Rules, Prerequisites: c.Prerequisites, Salt: c.Salt}
	if e.Targets == nil {
		e.Targets = []model.Target{}
	}
	if e.Rules == nil {
		e.Rules = []model.Rule{}
	}
	if e.Prerequisites == nil {
		e.Prerequisites = []model.Prerequisite{}
	}
	return e
}

func (u *UI) flag(req *request) error {
	env, key := req.r.PathValue("env"), req.r.PathValue("flag")
	p, err := u.loadFlagPage(req, env, key)
	if err != nil {
		return err
	}
	return u.render(req, "flag", page{Title: key, Env: env, Flash: req.r.URL.Query().Get("flash"), Data: p})
}

func (u *UI) toggle(req *request) error {
	env, key := req.r.PathValue("env"), req.r.PathValue("flag")
	enabled := req.r.PostForm.Get("enabled") == "true"
	if _, err := u.svc.ToggleFlag(req.r.Context(), env, key, enabled, req.r.PostForm.Get("comment")); err != nil {
		msg, _, ok := userError(err)
		if !ok {
			return err
		}
		return redirect(req, "/ui/env/"+url.PathEscape(env)+"/flags", msg)
	}
	state := "off"
	if enabled {
		state = "on"
	}
	back := "/ui/env/" + url.PathEscape(env) + "/flags"
	if req.r.PostForm.Get("from") == "detail" {
		back += "/" + url.PathEscape(key)
	}
	return redirect(req, back, fmt.Sprintf("Turned %s %s", key, state))
}

func (u *UI) saveConfig(req *request) error {
	env, key := req.r.PathValue("env"), req.r.PathValue("flag")
	form := req.r.PostForm
	version, err := strconv.ParseInt(form.Get("version"), 10, 64)
	if err != nil {
		return redirect(req, req.r.URL.Path[:strings.LastIndex(req.r.URL.Path, "/")], "Missing version")
	}
	var e editable
	dec := json.NewDecoder(strings.NewReader(form.Get("config")))
	dec.DisallowUnknownFields()
	decodeErr := dec.Decode(&e)
	var saveErr error
	if decodeErr == nil {
		cfg := model.FlagConfig{Enabled: e.Enabled, OffVariant: e.OffVariant, Fallthrough: e.Fallthrough,
			Targets: e.Targets, Rules: e.Rules, Prerequisites: e.Prerequisites, Salt: e.Salt}
		_, saveErr = u.svc.UpdateFlagConfig(req.r.Context(), env, key, cfg, version, form.Get("comment"))
		if saveErr == nil {
			return redirect(req, "/ui/env/"+url.PathEscape(env)+"/flags/"+url.PathEscape(key), "Saved configuration")
		}
	}
	p, err := u.loadFlagPage(req, env, key)
	if err != nil {
		return err
	}
	p.ConfigJSON = form.Get("config")
	pg := page{Title: key, Env: env, Data: p}
	if decodeErr != nil {
		pg.Error = "The configuration is not valid JSON: " + decodeErr.Error()
	} else {
		msg, problems, ok := userError(saveErr)
		if !ok {
			return saveErr
		}
		pg.Error, pg.Problems = msg, problems
	}
	req.w.WriteHeader(http.StatusUnprocessableEntity)
	return u.render(req, "flag", pg)
}

func (u *UI) evaluate(req *request) error {
	env, key := req.r.PathValue("env"), req.r.PathValue("flag")
	p, err := u.loadFlagPage(req, env, key)
	if err != nil {
		return err
	}
	p.Context = req.r.PostForm.Get("context")
	pg := page{Title: key, Env: env, Data: p}
	var ec eval.Context
	if err := json.Unmarshal([]byte(p.Context), &ec); err != nil {
		pg.Error = "The context is not valid JSON: " + err.Error()
		return u.render(req, "flag", pg)
	}
	res, err := u.svc.Evaluate(req.r.Context(), env, key, ec, true)
	if err != nil {
		return err
	}
	p.Result = &res
	return u.render(req, "flag", pg)
}

// --- segments ---------------------------------------------------------------------

func (u *UI) segments(req *request) error {
	env := req.r.PathValue("env")
	segs, err := u.svc.ListSegments(req.r.Context(), env)
	if err != nil {
		return err
	}
	return u.render(req, "segments", page{Title: "Segments", Env: env, Flash: req.r.URL.Query().Get("flash"), Data: segs})
}

type segmentPage struct {
	Segment model.Segment
	JSON    string
	New     bool
}

func segmentJSON(s model.Segment) string {
	doc := struct {
		Name        string              `json:"name"`
		Description string              `json:"description"`
		Included    []string            `json:"included"`
		Excluded    []string            `json:"excluded"`
		Rules       []model.SegmentRule `json:"rules"`
	}{s.Name, s.Description, s.Included, s.Excluded, s.Rules}
	if doc.Included == nil {
		doc.Included = []string{}
	}
	if doc.Excluded == nil {
		doc.Excluded = []string{}
	}
	if doc.Rules == nil {
		doc.Rules = []model.SegmentRule{}
	}
	return toJSON(doc)
}

func (u *UI) segment(req *request) error {
	env, key := req.r.PathValue("env"), req.r.PathValue("segment")
	seg, err := u.svc.GetSegment(req.r.Context(), env, key)
	if err != nil {
		return err
	}
	return u.render(req, "segment", page{Title: key, Env: env, Flash: req.r.URL.Query().Get("flash"),
		Data: segmentPage{Segment: seg, JSON: segmentJSON(seg)}})
}

func (u *UI) saveSegment(req *request) error {
	env := req.r.PathValue("env")
	form := req.r.PostForm
	key := strings.TrimSpace(form.Get("key"))
	var doc struct {
		Name        string              `json:"name"`
		Description string              `json:"description"`
		Included    []string            `json:"included"`
		Excluded    []string            `json:"excluded"`
		Rules       []model.SegmentRule `json:"rules"`
	}
	seg := model.Segment{Environment: env, Key: key}
	decodeErr := json.Unmarshal([]byte(form.Get("segment")), &doc)
	var err error
	if decodeErr == nil {
		seg.Name, seg.Description, seg.Included, seg.Excluded, seg.Rules = doc.Name, doc.Description, doc.Included, doc.Excluded, doc.Rules
		if form.Get("version") == "" {
			_, err = u.svc.CreateSegment(req.r.Context(), seg)
		} else {
			var v int64
			if v, err = strconv.ParseInt(form.Get("version"), 10, 64); err == nil {
				_, err = u.svc.UpdateSegment(req.r.Context(), seg, v)
			}
		}
		if err == nil {
			return redirect(req, "/ui/env/"+url.PathEscape(env)+"/segments/"+url.PathEscape(key), "Saved segment")
		}
	}
	pg := page{Title: "Segment", Env: env, Data: segmentPage{Segment: seg, JSON: form.Get("segment"), New: form.Get("version") == ""}}
	if decodeErr != nil {
		pg.Error = "The segment is not valid JSON: " + decodeErr.Error()
	} else {
		msg, problems, ok := userError(err)
		if !ok {
			return err
		}
		pg.Error, pg.Problems = msg, problems
	}
	if !pg.Data.(segmentPage).New {
		if cur, gerr := u.svc.GetSegment(req.r.Context(), env, key); gerr == nil {
			sp := pg.Data.(segmentPage)
			sp.Segment.Version = cur.Version
			pg.Data = sp
		}
	}
	req.w.WriteHeader(http.StatusUnprocessableEntity)
	return u.render(req, "segment", pg)
}

// --- audit ---------------------------------------------------------------------------

func (u *UI) auditLog(req *request) error {
	q := req.r.URL.Query()
	f := audit.Filter{Environment: q.Get("env"), ResourceKey: q.Get("key"), Limit: 50}
	if b, err := strconv.ParseInt(q.Get("before"), 10, 64); err == nil && b > 0 {
		f.BeforeID = b
	}
	entries, err := u.svc.ListAudit(req.r.Context(), f)
	if err != nil {
		return err
	}
	var next int64
	if len(entries) == f.Limit {
		next = entries[len(entries)-1].ID
	}
	return u.render(req, "audit", page{Title: "Audit log", Env: f.Environment,
		Data: map[string]any{"Entries": entries, "Next": next, "Key": f.ResourceKey}})
}
