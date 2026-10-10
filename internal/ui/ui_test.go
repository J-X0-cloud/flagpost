package ui

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/J-X0-cloud/flagpost/internal/service"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/internal/stream"
)

const token = "ui-secret-token"

type browser struct {
	t      *testing.T
	srv    *httptest.Server
	client *http.Client
	svc    *service.Service
	csrf   string
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ui.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(st, stream.NewBroker(stream.Options{}), nil)
	u, err := New(svc, Config{CheckToken: func(s string) bool { return s == token }, Secret: token})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(u)
	t.Cleanup(func() { srv.Close(); st.Close() })
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, srv: srv, svc: svc, client: &http.Client{Jar: jar}}
}

func (b *browser) get(path string) (int, string) {
	b.t.Helper()
	res, err := b.client.Get(b.srv.URL + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	b.captureCSRF(string(body))
	return res.StatusCode, string(body)
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (b *browser) captureCSRF(body string) {
	if m := csrfPattern.FindStringSubmatch(body); m != nil {
		b.csrf = m[1]
	}
}

func (b *browser) post(path string, form url.Values) (int, string) {
	b.t.Helper()
	if form.Get("csrf") == "" && b.csrf != "" {
		form.Set("csrf", b.csrf)
	}
	res, err := b.client.PostForm(b.srv.URL+path, form)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	b.captureCSRF(string(body))
	return res.StatusCode, string(body)
}

func (b *browser) login() {
	b.t.Helper()
	status, body := b.post("/ui/login", url.Values{"token": {token}})
	if status != 200 {
		b.t.Fatalf("login: %d %s", status, body)
	}
}

func mustContain(t *testing.T, body string, fragments ...string) {
	t.Helper()
	for _, f := range fragments {
		if !strings.Contains(body, f) {
			t.Errorf("page does not contain %q", f)
		}
	}
}

func TestLoginFlow(t *testing.T) {
	b := newBrowser(t)
	status, body := b.get("/ui/environments")
	if status != 200 || !strings.Contains(body, "Admin token") {
		t.Fatalf("unauthenticated request should land on login: %d", status)
	}
	if status, _ := b.post("/ui/login", url.Values{"token": {"wrong"}}); status != http.StatusUnauthorized {
		t.Errorf("bad token: %d", status)
	}
	status, body = b.post("/ui/login", url.Values{"token": {token}, "next": {"/ui/environments"}})
	if status != 200 {
		t.Fatalf("login: %d", status)
	}
	mustContain(t, body, "New environment", "Sign out")

	if status, _ := b.post("/ui/logout", url.Values{}); status != 200 {
		t.Errorf("logout: %d", status)
	}
	if _, body := b.get("/ui/environments"); !strings.Contains(body, "Admin token") {
		t.Error("still signed in after logout")
	}
}

func TestCSRFRequired(t *testing.T) {
	b := newBrowser(t)
	b.login()
	b.get("/ui/environments")
	if status, _ := b.post("/ui/environments", url.Values{"csrf": {"forged"}, "key": {"x"}, "name": {"x"}}); status != http.StatusForbidden {
		t.Errorf("forged csrf: %d", status)
	}
	res, err := http.PostForm(b.srv.URL+"/ui/environments", url.Values{"key": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("post without session: %d", res.StatusCode)
	}
}

func TestFlagWorkflow(t *testing.T) {
	b := newBrowser(t)
	b.login()
	b.get("/ui/environments")
	status, body := b.post("/ui/environments", url.Values{"key": {"prod"}, "name": {"Production"}})
	if status != 200 {
		t.Fatalf("create env: %d", status)
	}
	mustContain(t, body, "Created environment prod", "fp-sdk-")

	status, body = b.post("/ui/environments", url.Values{"key": {"bad key"}, "name": {""}})
	if status != http.StatusUnprocessableEntity {
		t.Errorf("invalid env: %d", status)
	}
	mustContain(t, body, "Please fix the problems below", "<code>key</code>")

	if _, body := b.get("/ui/"); !strings.Contains(body, "Flags <span class=\"muted\">in prod") {
		t.Error("home should redirect to the first environment's flags")
	}
	status, body = b.post("/ui/flags", url.Values{"env": {"prod"}, "key": {"checkout"}, "name": {"Checkout"}, "kind": {"boolean"}, "tags": {"web, payments"}})
	if status != 200 {
		t.Fatalf("create flag: %d %s", status, body)
	}
	mustContain(t, body, "Created flag checkout", "Targeting in prod", "off in prod")

	_, body = b.post("/ui/env/prod/flags/checkout/toggle", url.Values{"enabled": {"true"}, "from": {"detail"}})
	mustContain(t, body, "Turned checkout on", "on in prod")

	cfg := `{"enabled": true, "offVariant": "off", "fallthrough": {"variant": "off"}, "targets": [], "prerequisites": [], "salt": "checkout",
	  "rules": [{"id": "nz", "clauses": [{"attribute": "country", "operator": "eq", "values": ["NZ"]}], "serve": {"variant": "on"}}]}`
	status, body = b.post("/ui/env/prod/flags/checkout/config", url.Values{"version": {"1"}, "config": {cfg}, "comment": {"NZ launch"}})
	if status != 200 {
		t.Fatalf("save config: %d", status)
	}
	mustContain(t, body, "Saved configuration", "NZ launch", "fallthrough.variant")

	status, body = b.post("/ui/env/prod/flags/checkout/config", url.Values{"version": {"1"}, "config": {cfg}})
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "Someone else changed this") {
		t.Errorf("stale save: %d", status)
	}
	bad := strings.Replace(cfg, `"offVariant": "off"`, `"offVariant": "maybe"`, 1)
	_, body = b.post("/ui/env/prod/flags/checkout/config", url.Values{"version": {"2"}, "config": {bad}})
	mustContain(t, body, "offVariant", "unknown variant")
	_, body = b.post("/ui/env/prod/flags/checkout/config", url.Values{"version": {"2"}, "config": {"{"}})
	mustContain(t, body, "not valid JSON")

	_, body = b.post("/ui/env/prod/flags/checkout/evaluate", url.Values{"context": {`{"key":"u1","attributes":{"country":"NZ"}}`}})
	mustContain(t, body, "Serves <code>on</code>", "RULE_MATCH", "rule 0 (nz) matched")

	_, body = b.get("/ui/env/prod/flags?q=payments")
	mustContain(t, body, "checkout", "<span class=\"pill on\">on</span>")
	_, body = b.get("/ui/env/prod/flags?q=nothing-matches")
	mustContain(t, body, "No flags match")

	_, body = b.get("/ui/env/prod/flags/checkout?metric=purchase")
	mustContain(t, body, "Experiment results")

	_, body = b.get("/ui/audit?env=prod")
	mustContain(t, body, "toggle", "NZ launch")

	if status, _ := b.get("/ui/env/prod/flags/ghost"); status != http.StatusNotFound {
		t.Errorf("missing flag: %d", status)
	}
}

func TestSegmentWorkflow(t *testing.T) {
	b := newBrowser(t)
	b.login()
	b.get("/ui/environments")
	b.post("/ui/environments", url.Values{"key": {"prod"}, "name": {"Production"}})
	b.get("/ui/env/prod/segments")
	seg := `{"name": "Beta", "included": ["u1"], "rules": []}`
	status, body := b.post("/ui/env/prod/segments", url.Values{"key": {"beta"}, "segment": {seg}})
	if status != 200 {
		t.Fatalf("create segment: %d %s", status, body)
	}
	mustContain(t, body, "Saved segment", "Version 1")
	upd := `{"name": "Beta testers", "included": ["u1", "u2"], "excluded": ["u1"]}`
	_, body = b.post("/ui/env/prod/segments", url.Values{"key": {"beta"}, "version": {"1"}, "segment": {upd}})
	mustContain(t, body, "both included and excluded")
	_, body = b.post("/ui/env/prod/segments", url.Values{"key": {"beta"}, "version": {"1"}, "segment": {`{"name": "Beta testers", "included": ["u1", "u2"]}`}})
	mustContain(t, body, "Saved segment", "Version 2")
	_, body = b.get("/ui/env/prod/segments")
	mustContain(t, body, "beta")
}

func TestSessions(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &sessions{secret: []byte("k"), now: func() time.Time { return now }}
	v, _ := s.issue()
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"fresh", v, true},
		{"tampered expiry", "9" + v, false},
		{"tampered signature", v[:len(v)-1] + "0", false},
		{"no separator", "abc", false},
		{"garbage expiry", "x.y", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		if got := s.valid(tt.value); got != tt.want {
			t.Errorf("%s: valid = %v", tt.name, got)
		}
	}
	now = now.Add(sessionTTL + time.Second)
	if s.valid(v) {
		t.Error("expired session accepted")
	}
	other := &sessions{secret: []byte("other"), now: s.now}
	if other.checkCSRF(v, s.csrfToken(v)) {
		t.Error("CSRF token valid under a different secret")
	}
}

func TestSafeNext(t *testing.T) {
	tests := map[string]string{
		"/ui/env/prod/flags": "/ui/env/prod/flags",
		"https://evil.test/": "/ui/",
		"//evil.test/ui/":    "/ui/",
		"/ui/\\evil":         "/ui/",
		"/api/v1/flags":      "/ui/",
		"":                   "/ui/",
		"/ui/audit?env=prod": "/ui/audit?env=prod",
	}
	for in, want := range tests {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewRequiresConfig(t *testing.T) {
	if _, err := New(nil, Config{}); err == nil {
		t.Error("missing config accepted")
	}
}

func TestStaticAssets(t *testing.T) {
	b := newBrowser(t)
	status, body := b.get("/ui/static/style.css")
	if status != 200 || !strings.Contains(body, "--accent") {
		t.Errorf("stylesheet: %d", status)
	}
}
