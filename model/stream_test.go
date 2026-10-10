package model

import "testing"

func TestSnapshotApplyAndDelete(t *testing.T) {
	base := NewSnapshot("prod", 5)
	base.Flags["a"] = &FlagState{Flag: Flag{Key: "a"}}
	base.Segments["s"] = &Segment{Key: "s"}

	tests := []struct {
		name      string
		apply     func(*Snapshot) *Snapshot
		wantRev   int64
		wantFlags int
		wantSegs  int
		same      bool
	}{
		{"stale patch ignored", func(s *Snapshot) *Snapshot {
			return s.Apply(StreamPatch{Revision: 5, Flag: &FlagState{Flag: Flag{Key: "b"}}})
		}, 5, 1, 1, true},
		{"flag upsert", func(s *Snapshot) *Snapshot {
			return s.Apply(StreamPatch{Revision: 6, Flag: &FlagState{Flag: Flag{Key: "b"}}})
		}, 6, 2, 1, false},
		{"segment upsert", func(s *Snapshot) *Snapshot {
			return s.Apply(StreamPatch{Revision: 7, Segment: &Segment{Key: "s", Name: "new"}})
		}, 7, 1, 1, false},
		{"flag delete", func(s *Snapshot) *Snapshot {
			return s.Delete(StreamDelete{Revision: 6, Kind: ObjectFlag, Key: "a"})
		}, 6, 0, 1, false},
		{"segment delete", func(s *Snapshot) *Snapshot {
			return s.Delete(StreamDelete{Revision: 6, Kind: ObjectSegment, Key: "s"})
		}, 6, 1, 0, false},
		{"stale delete ignored", func(s *Snapshot) *Snapshot {
			return s.Delete(StreamDelete{Revision: 4, Kind: ObjectFlag, Key: "a"})
		}, 5, 1, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.apply(base)
			if got.Revision != tt.wantRev || len(got.Flags) != tt.wantFlags || len(got.Segments) != tt.wantSegs {
				t.Errorf("got rev %d flags %d segs %d", got.Revision, len(got.Flags), len(got.Segments))
			}
			if (got == base) != tt.same {
				t.Errorf("identity: got same=%v, want %v", got == base, tt.same)
			}
			if len(base.Flags) != 1 || len(base.Segments) != 1 || base.Revision != 5 {
				t.Fatal("base snapshot was mutated")
			}
		})
	}
}
