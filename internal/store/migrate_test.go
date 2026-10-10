package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	ran, err := Migrate(ctx, s.DB(), ms)
	if err != nil || len(ran) != 0 {
		t.Fatalf("second migrate ran %v, err %v", ran, err)
	}
	applied, err := AppliedMigrations(ctx, s.DB())
	if err != nil || len(applied) != len(ms) {
		t.Fatalf("applied = %v, err %v", applied, err)
	}
	for i, a := range applied {
		if a.Version != ms[i].Version || a.Checksum != ms[i].Checksum || a.AppliedAt.IsZero() {
			t.Errorf("applied[%d] = %+v", i, a)
		}
	}
}

func TestMigrateRejectsEditedHistory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "m.db"), Options{SkipMigrations: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v1 := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")}}
	ms, err := LoadMigrations(v1)
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := Migrate(ctx, s.DB(), ms); err != nil || len(ran) != 1 {
		t.Fatalf("ran %v err %v", ran, err)
	}

	edited := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (x TEXT);")}}
	ms2, _ := LoadMigrations(edited)
	if _, err := Migrate(ctx, s.DB(), ms2); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("expected checksum error, got %v", err)
	}
	if _, err := Migrate(ctx, s.DB(), nil); err == nil || !strings.Contains(err.Error(), "does not know") {
		t.Fatalf("expected unknown-migration error, got %v", err)
	}

	// A failing migration rolls back and is not recorded.
	bad := fstest.MapFS{
		"0001_a.sql": v1["0001_a.sql"],
		"0002_b.sql": {Data: []byte("CREATE TABLE b (x INTEGER); THIS IS NOT SQL;")},
	}
	ms3, _ := LoadMigrations(bad)
	if _, err := Migrate(ctx, s.DB(), ms3); err == nil {
		t.Fatal("expected failure")
	}
	var n int
	_ = s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'b'`).Scan(&n)
	if n != 0 {
		t.Error("failed migration was not rolled back")
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	tests := []struct {
		name string
		fs   fstest.MapFS
		ok   bool
	}{
		{"ok and sorted", fstest.MapFS{"0002_b.sql": {}, "0001_a.sql": {}, "README.md": {}}, true},
		{"no description", fstest.MapFS{"0001.sql": {}}, false},
		{"non-numeric", fstest.MapFS{"abc_x.sql": {}}, false},
		{"zero", fstest.MapFS{"0000_x.sql": {}}, false},
		{"duplicate", fstest.MapFS{"0001_a.sql": {}, "1_b.sql": {}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms, err := LoadMigrations(tt.fs)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
			if tt.ok && (len(ms) != 2 || ms[0].Version != 1 || ms[1].Name != "b") {
				t.Errorf("unexpected migrations %+v", ms)
			}
		})
	}
}
