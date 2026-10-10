package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseServeConfig(t *testing.T) {
	good := "0123456789abcdef"
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, c serveConfig)
	}{
		{
			name: "defaults",
			env:  map[string]string{"FLAGPOST_ADMIN_TOKEN": good},
			check: func(t *testing.T, c serveConfig) {
				if c.Addr != ":8080" || c.DB != "flagpost.db" || c.LogFormat != "text" {
					t.Errorf("defaults: %+v", c)
				}
			},
		},
		{
			name: "PORT honoured",
			env:  map[string]string{"FLAGPOST_ADMIN_TOKEN": good, "PORT": "3000"},
			check: func(t *testing.T, c serveConfig) {
				if c.Addr != ":3000" {
					t.Errorf("addr = %s", c.Addr)
				}
			},
		},
		{
			name: "flags beat environment",
			args: []string{"-addr", "127.0.0.1:9000", "-db", "/tmp/x.db", "-log-format", "json", "-log-level", "debug"},
			env:  map[string]string{"FLAGPOST_ADMIN_TOKEN": good, "PORT": "3000", "FLAGPOST_DB": "env.db"},
			check: func(t *testing.T, c serveConfig) {
				if c.Addr != "127.0.0.1:9000" || c.DB != "/tmp/x.db" || c.LogFormat != "json" || c.LogLevel.String() != "DEBUG" {
					t.Errorf("config: %+v", c)
				}
			},
		},
		{
			name: "bootstrap environments",
			env:  map[string]string{"FLAGPOST_ADMIN_TOKEN": good, "FLAGPOST_BOOTSTRAP_ENVS": "production, staging,"},
			check: func(t *testing.T, c serveConfig) {
				if strings.Join(c.BootstrapEnvs, "|") != "production|staging" {
					t.Errorf("envs = %v", c.BootstrapEnvs)
				}
			},
		},
		{name: "missing token", wantErr: "admin token"},
		{name: "short token", env: map[string]string{"FLAGPOST_ADMIN_TOKEN": "short"}, wantErr: "at least 16"},
		{name: "bad log format", args: []string{"-log-format", "xml"}, env: map[string]string{"FLAGPOST_ADMIN_TOKEN": good}, wantErr: "log format"},
		{name: "bad log level", args: []string{"-log-level", "loud"}, env: map[string]string{"FLAGPOST_ADMIN_TOKEN": good}, wantErr: "log level"},
		{name: "bad env key", env: map[string]string{"FLAGPOST_ADMIN_TOKEN": good, "FLAGPOST_BOOTSTRAP_ENVS": "no spaces"}, wantErr: "bootstrap"},
		{name: "stray argument", args: []string{"extra"}, env: map[string]string{"FLAGPOST_ADMIN_TOKEN": good}, wantErr: "unexpected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseServeConfig(tt.args, io.Discard, env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, c)
		})
	}
}

func TestBucketCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"bucket", "new-checkout", "user-1", "user-2"}, &out, io.Discard, env(nil)); err != nil {
		t.Fatal(err)
	}
	want := "user-1\t90116\t90.116%\nuser-2\t95626\t95.626%\n"
	if out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
	if err := run(context.Background(), []string{"bucket", "only-seed"}, io.Discard, io.Discard, env(nil)); err == nil {
		t.Error("missing value accepted")
	}
}

func TestVersionHelpAndUnknown(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &out, io.Discard, env(nil)); err != nil || !strings.HasPrefix(out.String(), "flagpost ") {
		t.Errorf("version: %q %v", out.String(), err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"help"}, &out, io.Discard, env(nil)); err != nil || !strings.Contains(out.String(), "commands:") {
		t.Errorf("help: %v", err)
	}
	if err := run(context.Background(), []string{"frobnicate"}, io.Discard, io.Discard, env(nil)); err == nil {
		t.Error("unknown command accepted")
	}
}

func TestMigrateCommand(t *testing.T) {
	var out bytes.Buffer
	db := filepath.Join(t.TempDir(), "m.db")
	if err := run(context.Background(), []string{"migrate", "-db", db}, &out, io.Discard, env(nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0001  init") || !strings.Contains(out.String(), "0002  experiments") {
		t.Errorf("migrate output: %s", out.String())
	}
}

func TestServeEndToEnd(t *testing.T) {
	cfg, err := parseServeConfig([]string{"-addr", "127.0.0.1:0", "-db", filepath.Join(t.TempDir(), "e2e.db")}, io.Discard,
		env(map[string]string{"FLAGPOST_ADMIN_TOKEN": "0123456789abcdef", "FLAGPOST_BOOTSTRAP_ENVS": "production,staging"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, io.Discard, func(a string) { addrc <- a }) }()
	var base string
	select {
	case a := <-addrc:
		base = "http://" + a
	case err := <-done:
		t.Fatalf("server exited: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}

	req, _ := http.NewRequest("GET", base+"/api/v1/environments", nil)
	req.Header.Set("Authorization", "Bearer 0123456789abcdef")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var envs struct {
		Items []struct {
			Key    string `json:"key"`
			SDKKey string `json:"sdkKey"`
		} `json:"items"`
	}
	_ = json.NewDecoder(res.Body).Decode(&envs)
	res.Body.Close()
	if len(envs.Items) != 2 || envs.Items[0].Key != "production" {
		t.Fatalf("bootstrapped environments: %+v", envs)
	}

	// An open SDK stream must not block graceful shutdown.
	sreq, _ := http.NewRequest("GET", base+"/sdk/v1/stream", nil)
	sreq.Header.Set("Authorization", envs.Items[0].SDKKey)
	stream, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	root, err := noRedirect.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	root.Body.Close()
	if root.StatusCode != http.StatusFound || root.Header.Get("Location") != "/ui/" {
		t.Errorf("root redirect: %d %s", root.StatusCode, root.Header.Get("Location"))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("graceful shutdown hung")
	}
}
