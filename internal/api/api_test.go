package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/internal/service"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/internal/stream"
	"github.com/J-X0-cloud/flagpost/model"
)

const adminToken = "test-admin-token"

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	api    *Server
	sdkKey string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	broker := stream.NewBroker(stream.Options{Buffer: 16})
	svc := service.New(st, broker, nil)
	api := New(svc, Config{AdminToken: adminToken, Heartbeat: 50 * time.Millisecond})
	api.Handle("GET /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("kaboom") }))
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		broker.Shutdown()
		srv.Close()
		st.Close()
	})
	h := &harness{t: t, srv: srv, api: api}
	var env model.Environment
	h.must(h.admin("POST", "/api/v1/environments", `{"key":"prod","name":"Production"}`), http.StatusCreated, &env)
	h.sdkKey = env.SDKKey
	return h
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (h *harness) do(method, path, body string, hdr map[string]string) resp {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func (h *harness) admin(method, path, body string, extra ...string) resp {
	h.t.Helper()
	hdr := map[string]string{"Authorization": "Bearer " + adminToken, "X-Flagpost-Actor": "alice"}
	for i := 0; i+1 < len(extra); i += 2 {
		hdr[extra[i]] = extra[i+1]
	}
	return h.do(method, path, body, hdr)
}

func (h *harness) sdk(method, path, body string, extra ...string) resp {
	h.t.Helper()
	hdr := map[string]string{"Authorization": h.sdkKey}
	for i := 0; i+1 < len(extra); i += 2 {
		hdr[extra[i]] = extra[i+1]
	}
	return h.do(method, path, body, hdr)
}

func (h *harness) must(r resp, status int, out any) {
	h.t.Helper()
	if r.status != status {
		h.t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	if out != nil {
		if err := json.Unmarshal(r.body, out); err != nil {
			h.t.Fatalf("decode %s: %v", r.body, err)
		}
	}
}

func errorCode(t *testing.T, r resp) string {
	t.Helper()
	var e struct {
		Error struct {
			Code     string          `json:"code"`
			Problems []model.Problem `json:"problems"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("not an error body: %s", r.body)
	}
	return e.Error.Code
}

const checkoutFlag = `{"key":"checkout","name":"Checkout","kind":"boolean","variants":[{"key":"on","value":true},{"key":"off","value":false}]}`

func TestHealthAndMiddleware(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/healthz", "", nil)
	if r.status != 200 || !strings.Contains(string(r.body), "ok") || r.header.Get("X-Request-ID") == "" {
		t.Errorf("health: %d %s", r.status, r.body)
	}
	r = h.do("GET", "/healthz", "", map[string]string{"X-Request-ID": "abc"})
	if r.header.Get("X-Request-ID") != "abc" {
		t.Error("request id not propagated")
	}
	r = h.do("GET", "/boom", "", nil)
	if r.status != 500 || errorCode(t, r) != "internal" || strings.Contains(string(r.body), "kaboom") {
		t.Errorf("panic: %d %s", r.status, r.body)
	}
}

func TestAuthentication(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name string
		path string
		hdr  map[string]string
	}{
		{"admin without token", "/api/v1/flags", nil},
		{"admin with wrong token", "/api/v1/flags", map[string]string{"Authorization": "Bearer nope"}},
		{"admin with sdk key", "/api/v1/flags", map[string]string{"Authorization": h.sdkKey}},
		{"sdk without key", "/sdk/v1/snapshot", nil},
		{"sdk with admin token", "/sdk/v1/snapshot", map[string]string{"Authorization": adminToken}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := h.do("GET", tt.path, "", tt.hdr)
			if r.status != http.StatusUnauthorized || errorCode(t, r) != "unauthorized" {
				t.Errorf("status %d: %s", r.status, r.body)
			}
		})
	}
	if r := h.do("GET", "/sdk/v1/snapshot?sdkKey="+h.sdkKey, "", nil); r.status != 200 {
		t.Errorf("query-string sdk key: %d", r.status)
	}
	if r := h.do("GET", "/api/v1/flags", "", map[string]string{"Authorization": adminToken}); r.status != 200 {
		t.Errorf("bare admin token: %d", r.status)
	}
}

func TestEnvironmentsAPI(t *testing.T) {
	h := newHarness(t)
	h.must(h.admin("POST", "/api/v1/environments", `{"key":"staging","name":"Staging"}`), 201, nil)
	if r := h.admin("POST", "/api/v1/environments", `{"key":"staging","name":"Again"}`); r.status != 409 {
		t.Errorf("duplicate: %d", r.status)
	}
	if r := h.admin("POST", "/api/v1/environments", `{"key":"bad key","name":""}`); r.status != 422 || errorCode(t, r) != "validation_failed" {
		t.Errorf("invalid: %d %s", r.status, r.body)
	}
	if r := h.admin("POST", "/api/v1/environments", `{"key":"x","name":"x","color":"red"}`); r.status != 400 {
		t.Errorf("unknown field: %d", r.status)
	}
	if r := h.admin("POST", "/api/v1/environments", ``); r.status != 400 {
		t.Errorf("empty body: %d", r.status)
	}
	if r := h.admin("POST", "/api/v1/environments", `{"key":"a","name":"a"} {}`); r.status != 400 {
		t.Errorf("trailing data: %d", r.status)
	}
	var env model.Environment
	h.must(h.admin("PATCH", "/api/v1/environments/staging", `{"description":"pre-prod"}`), 200, &env)
	if env.Name != "Staging" || env.Description != "pre-prod" {
		t.Errorf("patch: %+v", env)
	}
	var rotated model.Environment
	h.must(h.admin("POST", "/api/v1/environments/staging/sdk-key", ``), 200, &rotated)
	if rotated.SDKKey == "" || rotated.SDKKey == env.SDKKey {
		t.Error("key not rotated")
	}
	var envs listResponse[model.Environment]
	h.must(h.admin("GET", "/api/v1/environments", ""), 200, &envs)
	if len(envs.Items) != 2 {
		t.Errorf("list: %+v", envs)
	}
	h.must(h.admin("DELETE", "/api/v1/environments/staging", ""), 204, nil)
	if r := h.admin("GET", "/api/v1/environments/staging", ""); r.status != 404 || errorCode(t, r) != "not_found" {
		t.Errorf("deleted env: %d", r.status)
	}
}

func TestFlagsAPI(t *testing.T) {
	h := newHarness(t)
	r := h.admin("POST", "/api/v1/flags", checkoutFlag)
	h.must(r, 201, nil)
	if r.header.Get("ETag") != `"1"` || r.header.Get("Location") != "/api/v1/flags/checkout" {
		t.Errorf("headers: %v", r.header)
	}
	bad := h.admin("POST", "/api/v1/flags", `{"key":"x","name":"x","kind":"boolean","variants":[{"key":"a","value":"no"}]}`)
	if bad.status != 422 || !strings.Contains(string(bad.body), "variants[0].value") {
		t.Errorf("validation: %d %s", bad.status, bad.body)
	}

	upd := strings.Replace(checkoutFlag, `"name":"Checkout"`, `"name":"New checkout","tags":["web"]`, 1)
	var f model.Flag
	h.must(h.admin("PUT", "/api/v1/flags/checkout", upd, "If-Match", `"1"`), 200, &f)
	if f.Version != 2 || f.Name != "New checkout" {
		t.Errorf("update: %+v", f)
	}
	stale := h.admin("PUT", "/api/v1/flags/checkout", upd, "If-Match", `"1"`)
	if stale.status != 412 || errorCode(t, stale) != "version_conflict" {
		t.Errorf("stale: %d %s", stale.status, stale.body)
	}
	if r := h.admin("PUT", "/api/v1/flags/checkout", upd, "If-Match", `abc`); r.status != 400 {
		t.Errorf("bad If-Match: %d", r.status)
	}
	if r := h.admin("PUT", "/api/v1/flags/other", upd); r.status != 400 {
		t.Errorf("key mismatch: %d", r.status)
	}
	h.must(h.admin("PUT", "/api/v1/flags/checkout", upd), 200, &f) // unconditional
	if f.Version != 2 {
		t.Errorf("no-op unconditional update changed version to %d", f.Version)
	}

	var flags listResponse[model.Flag]
	h.must(h.admin("GET", "/api/v1/flags?tag=web", ""), 200, &flags)
	if len(flags.Items) != 1 {
		t.Errorf("tag filter: %+v", flags)
	}
	h.must(h.admin("GET", "/api/v1/flags?q=zzz", ""), 200, &flags)
	if len(flags.Items) != 0 || flags.Items == nil {
		t.Errorf("empty list should be [] not null: %+v", flags)
	}
	h.must(h.admin("DELETE", "/api/v1/flags/checkout", ""), 204, nil)
	if r := h.admin("DELETE", "/api/v1/flags/checkout", ""); r.status != 404 {
		t.Errorf("double delete: %d", r.status)
	}
}

func TestFlagConfigAPI(t *testing.T) {
	h := newHarness(t)
	h.must(h.admin("POST", "/api/v1/flags", checkoutFlag), 201, nil)
	var cfg model.FlagConfig
	r := h.admin("GET", "/api/v1/environments/prod/flags/checkout", "")
	h.must(r, 200, &cfg)
	if cfg.Version != 0 || r.header.Get("ETag") != `"0"` {
		t.Errorf("default config: %+v", cfg)
	}
	body := `{"enabled":true,"offVariant":"off","fallthrough":{"variant":"off"},"version":0,"comment":"NZ first",
	  "rules":[{"clauses":[{"attribute":"country","operator":"eq","values":["NZ"]}],"serve":{"variant":"on"}}]}`
	h.must(h.admin("PUT", "/api/v1/environments/prod/flags/checkout", body), 200, &cfg)
	if cfg.Version != 1 || len(cfg.Rules) != 1 || cfg.Rules[0].ID == "" {
		t.Errorf("put: %+v", cfg)
	}
	if r := h.admin("PUT", "/api/v1/environments/prod/flags/checkout", body); r.status != 412 {
		t.Errorf("stale put: %d", r.status)
	}
	bad := `{"offVariant":"nope","fallthrough":{"variant":"off"}}`
	if r := h.admin("PUT", "/api/v1/environments/prod/flags/checkout", bad); r.status != 422 {
		t.Errorf("invalid config: %d %s", r.status, r.body)
	}
	if r := h.admin("PUT", "/api/v1/environments/prod/flags/checkout", `{"flagKey":"other","offVariant":"off","fallthrough":{"variant":"on"}}`); r.status != 400 {
		t.Errorf("mismatched body: %d", r.status)
	}

	// A client may PUT back exactly what it fetched.
	got := h.admin("GET", "/api/v1/environments/prod/flags/checkout", "")
	echo := bytes.Replace(got.body, []byte(`"enabled":true`), []byte(`"enabled":false`), 1)
	h.must(h.admin("PUT", "/api/v1/environments/prod/flags/checkout", string(echo)), 200, &cfg)
	if cfg.Enabled || cfg.Version != 2 {
		t.Errorf("round-trip put: %+v", cfg)
	}

	h.must(h.admin("POST", "/api/v1/environments/prod/flags/checkout/toggle", `{"enabled":true,"comment":"go"}`), 200, &cfg)
	if !cfg.Enabled || cfg.Version != 3 {
		t.Errorf("toggle: %+v", cfg)
	}

	var res eval.Result
	h.must(h.admin("POST", "/api/v1/environments/prod/evaluate", `{"flag":"checkout","explain":true,"context":{"key":"u1","attributes":{"country":"NZ"}}}`), 200, &res)
	if res.Variant != "on" || res.Reason != eval.ReasonRuleMatch || len(res.Trace) == 0 {
		t.Errorf("evaluate: %+v", res)
	}
	var states listResponse[model.FlagState]
	h.must(h.admin("GET", "/api/v1/environments/prod/flags", ""), 200, &states)
	if len(states.Items) != 1 || !states.Items[0].Config.Enabled {
		t.Errorf("states: %+v", states)
	}

	var page auditPage
	h.must(h.admin("GET", "/api/v1/audit?type=flag_config&limit=2", ""), 200, &page)
	if len(page.Items) != 2 || page.Next == 0 || page.Items[0].Actor != "alice" || page.Items[0].Action != "toggle" {
		t.Fatalf("audit page 1: %+v", page)
	}
	var page2 auditPage
	h.must(h.admin("GET", fmt.Sprintf("/api/v1/audit?type=flag_config&limit=2&before=%d", page.Next), ""), 200, &page2)
	if len(page2.Items) != 1 || page2.Next != 0 || page2.Items[0].Comment != "NZ first" {
		t.Errorf("audit page 2: %+v", page2)
	}
	for _, q := range []string{"limit=0", "limit=x", "before=-1"} {
		if r := h.admin("GET", "/api/v1/audit?"+q, ""); r.status != 400 {
			t.Errorf("audit %s: %d", q, r.status)
		}
	}
}

func TestSegmentsAPI(t *testing.T) {
	h := newHarness(t)
	h.must(h.admin("POST", "/api/v1/flags", checkoutFlag), 201, nil)
	var seg model.Segment
	h.must(h.admin("POST", "/api/v1/environments/prod/segments", `{"key":"beta","name":"Beta","included":["u1"]}`), 201, &seg)
	if seg.Version != 1 || seg.Environment != "prod" {
		t.Errorf("create: %+v", seg)
	}
	h.must(h.admin("PUT", "/api/v1/environments/prod/segments/beta", `{"name":"Beta testers","included":["u1","u2"],"version":1}`), 200, &seg)
	if seg.Version != 2 || len(seg.Included) != 2 {
		t.Errorf("update: %+v", seg)
	}
	h.must(h.admin("PUT", "/api/v1/environments/prod/flags/checkout",
		`{"enabled":true,"offVariant":"off","fallthrough":{"variant":"off"},"rules":[{"id":"b","clauses":[{"operator":"in_segment","values":["beta"]}],"serve":{"variant":"on"}}]}`), 200, nil)
	if r := h.admin("DELETE", "/api/v1/environments/prod/segments/beta", ""); r.status != 409 || errorCode(t, r) != "in_use" {
		t.Errorf("delete in use: %d %s", r.status, r.body)
	}
	var segs listResponse[model.Segment]
	h.must(h.admin("GET", "/api/v1/environments/prod/segments", ""), 200, &segs)
	if len(segs.Items) != 1 {
		t.Errorf("list: %+v", segs)
	}
	if r := h.admin("GET", "/api/v1/environments/prod/segments/ghost", ""); r.status != 404 {
		t.Errorf("missing: %d", r.status)
	}
}

func TestSDKSnapshotAndEvaluate(t *testing.T) {
	h := newHarness(t)
	h.must(h.admin("POST", "/api/v1/flags", checkoutFlag), 201, nil)
	r := h.sdk("GET", "/sdk/v1/snapshot", "")
	var snap model.Snapshot
	h.must(r, 200, &snap)
	tag := r.header.Get("ETag")
	if tag != etag(snap.Revision) || len(snap.Flags) != 1 {
		t.Fatalf("snapshot: %s %+v", tag, snap)
	}
	if r := h.sdk("GET", "/sdk/v1/snapshot", "", "If-None-Match", tag); r.status != 304 {
		t.Errorf("conditional get: %d", r.status)
	}
	h.must(h.admin("POST", "/api/v1/environments/prod/flags/checkout/toggle", `{"enabled":true}`), 200, nil)
	if r := h.sdk("GET", "/sdk/v1/snapshot", "", "If-None-Match", tag); r.status != 200 {
		t.Errorf("changed snapshot: %d", r.status)
	}
	var res eval.Result
	h.must(h.sdk("POST", "/sdk/v1/evaluate", `{"flag":"checkout","explain":true,"context":{"key":"u"}}`), 200, &res)
	if res.Variant != "on" || res.Trace != nil {
		t.Errorf("sdk evaluate: %+v", res)
	}
	var all listResponse[eval.Result]
	h.must(h.sdk("POST", "/sdk/v1/evaluate", `{"context":{"key":"u"}}`), 200, &all)
	if len(all.Items) != 1 {
		t.Errorf("sdk evaluate all: %+v", all)
	}
	pre := h.do("OPTIONS", "/sdk/v1/snapshot", "", nil)
	if pre.status != 204 || pre.header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight: %d %v", pre.status, pre.header)
	}
}

func TestEtagMatches(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{`"3"`, true},
		{`W/"3"`, true},
		{`"1", "3"`, true},
		{`*`, true},
		{`"4"`, false},
		{`3`, false},
	}
	for _, tt := range tests {
		if got := etagMatches(tt.header, `"3"`); got != tt.want {
			t.Errorf("etagMatches(%q) = %v", tt.header, got)
		}
	}
}

type sseEvent struct {
	id, typ, data string
}

// readSSE parses events from a stream, skipping comments.
func readSSE(t *testing.T, br *bufio.Reader, n int) []sseEvent {
	t.Helper()
	var out []sseEvent
	var cur sseEvent
	for len(out) < n {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v (after %d events)", err, len(out))
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if cur.typ != "" {
				out = append(out, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			cur.typ = line[7:]
		case strings.HasPrefix(line, "data: "):
			cur.data += line[6:]
		}
	}
	return out
}

func (h *harness) openStream(lastID string) (*bufio.Reader, func()) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", h.srv.URL+"/sdk/v1/stream", nil)
	req.Header.Set("Authorization", h.sdkKey)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
		h.t.Fatalf("stream status %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	return bufio.NewReader(res.Body), func() { cancel(); res.Body.Close() }
}

func TestSDKStream(t *testing.T) {
	h := newHarness(t)
	h.must(h.admin("POST", "/api/v1/flags", checkoutFlag), 201, nil)

	br, closeStream := h.openStream("")
	put := readSSE(t, br, 1)[0]
	if put.typ != model.EventPut {
		t.Fatalf("first event %q", put.typ)
	}
	var snap model.Snapshot
	if err := json.Unmarshal([]byte(put.data), &snap); err != nil || len(snap.Flags) != 1 {
		t.Fatalf("put payload: %v %s", err, put.data)
	}

	h.must(h.admin("POST", "/api/v1/environments/prod/flags/checkout/toggle", `{"enabled":true}`), 200, nil)
	h.must(h.admin("POST", "/api/v1/environments/prod/segments", `{"key":"beta","name":"Beta"}`), 201, nil)
	evs := readSSE(t, br, 2)
	var p1, p2 model.StreamPatch
	_ = json.Unmarshal([]byte(evs[0].data), &p1)
	_ = json.Unmarshal([]byte(evs[1].data), &p2)
	if evs[0].typ != model.EventPatch || p1.Flag == nil || !p1.Flag.Config.Enabled || p1.Revision <= snap.Revision {
		t.Errorf("flag patch: %+v", evs[0])
	}
	if p2.Segment == nil || p2.Segment.Key != "beta" || p2.Revision <= p1.Revision {
		t.Errorf("segment patch: %+v", evs[1])
	}
	closeStream()

	// While disconnected, one more change happens.
	h.must(h.admin("DELETE", "/api/v1/environments/prod/segments/beta", ""), 204, nil)

	br2, close2 := h.openStream(evs[1].id)
	defer close2()
	replayed := readSSE(t, br2, 1)[0]
	if replayed.typ != model.EventDelete || !strings.Contains(replayed.data, `"beta"`) {
		t.Errorf("resume should replay only the missed delete, got %+v", replayed)
	}

	// An unknown ID forces a full snapshot.
	br3, close3 := h.openStream("stale-1")
	defer close3()
	if ev := readSSE(t, br3, 1)[0]; ev.typ != model.EventPut {
		t.Errorf("unresumable stream started with %q", ev.typ)
	}
}

func TestSDKStreamHeartbeat(t *testing.T) {
	h := newHarness(t)
	br, closeStream := h.openStream("")
	defer closeStream()
	readSSE(t, br, 1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, ": ping") {
			return
		}
	}
	t.Error("no heartbeat received")
}

func TestEventsAndResults(t *testing.T) {
	h := newHarness(t)
	h.must(h.admin("POST", "/api/v1/flags", checkoutFlag), 201, nil)
	h.must(h.admin("PUT", "/api/v1/environments/prod/flags/checkout",
		`{"enabled":true,"offVariant":"off","fallthrough":{"rollout":{"experiment":true,"variants":[{"variant":"off","weight":50000},{"variant":"on","weight":50000}]}}}`), 200, nil)

	batch := model.EventBatch{}
	for i := 0; i < 200; i++ {
		v := "off"
		if i%2 == 0 {
			v = "on"
		}
		key := fmt.Sprintf("u%d", i)
		batch.Exposures = append(batch.Exposures, model.ExposureEvent{Flag: "checkout", Variant: v, ContextKey: key})
		if i%4 == 0 {
			batch.Conversions = append(batch.Conversions, model.ConversionEvent{Event: "purchase", ContextKey: key})
		}
	}
	body, _ := json.Marshal(batch)
	var er service.EventResult
	h.must(h.sdk("POST", "/sdk/v1/events", string(body)), 202, &er)
	if er.Accepted != 250 || er.Rejected != 0 {
		t.Errorf("events: %+v", er)
	}
	var res service.ExperimentResults
	h.must(h.admin("GET", "/api/v1/environments/prod/flags/checkout/results?metric=purchase&confidence=0.9", ""), 200, &res)
	if res.Control != "off" || len(res.Arms) != 2 || res.Arms[1].Converted != 50 || res.Confidence != 0.9 {
		t.Errorf("results: %+v", res)
	}
	for _, q := range []string{"", "?metric=purchase&confidence=abc", "?metric=purchase&confidence=2"} {
		if r := h.admin("GET", "/api/v1/environments/prod/flags/checkout/results"+q, ""); r.status != 400 {
			t.Errorf("results%s: %d", q, r.status)
		}
	}
}
