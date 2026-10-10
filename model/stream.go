package model

// Server-sent event types used on the SDK stream.
const (
	// EventPut carries a complete Snapshot. It is always the first event
	// on a connection, and is re-sent whenever the server cannot replay
	// the events a reconnecting client missed.
	EventPut = "put"
	// EventPatch carries a StreamPatch that upserts one flag or segment.
	EventPatch = "patch"
	// EventDelete carries a StreamDelete that removes one flag or segment.
	EventDelete = "delete"
)

// Kinds of object a patch or delete applies to.
const (
	ObjectFlag    = "flag"
	ObjectSegment = "segment"
)

// StreamPatch upserts a single object in a client's snapshot. Revision is
// the environment revision after the change; clients ignore patches whose
// revision is not newer than the snapshot they hold.
type StreamPatch struct {
	Revision int64      `json:"revision"`
	Flag     *FlagState `json:"flag,omitempty"`
	Segment  *Segment   `json:"segment,omitempty"`
}

// StreamDelete removes a single object from a client's snapshot.
type StreamDelete struct {
	Revision int64  `json:"revision"`
	Kind     string `json:"kind"`
	Key      string `json:"key"`
}

// Apply returns a copy of s with the patch applied, or s itself when the
// patch is stale. The copy shares unchanged flag and segment values with s,
// which is safe because snapshots are treated as immutable.
func (s *Snapshot) Apply(p StreamPatch) *Snapshot {
	if p.Revision <= s.Revision {
		return s
	}
	out := s.shallowCopy(p.Revision)
	if p.Flag != nil {
		out.Flags[p.Flag.Flag.Key] = p.Flag
	}
	if p.Segment != nil {
		out.Segments[p.Segment.Key] = p.Segment
	}
	return out
}

// Delete returns a copy of s without the named object, or s itself when
// the delete is stale.
func (s *Snapshot) Delete(d StreamDelete) *Snapshot {
	if d.Revision <= s.Revision {
		return s
	}
	out := s.shallowCopy(d.Revision)
	switch d.Kind {
	case ObjectFlag:
		delete(out.Flags, d.Key)
	case ObjectSegment:
		delete(out.Segments, d.Key)
	}
	return out
}

func (s *Snapshot) shallowCopy(revision int64) *Snapshot {
	out := &Snapshot{
		Environment: s.Environment,
		Revision:    revision,
		Flags:       make(map[string]*FlagState, len(s.Flags)+1),
		Segments:    make(map[string]*Segment, len(s.Segments)+1),
	}
	for k, v := range s.Flags {
		out.Flags[k] = v
	}
	for k, v := range s.Segments {
		out.Segments[k] = v
	}
	return out
}
