package sdk

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEReader(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   []sseEvent
		lastID string
		retry  int
	}{
		{
			name:  "simple",
			input: "event: put\ndata: {}\n\n",
			want:  []sseEvent{{Event: "put", Data: "{}"}},
			retry: -1,
		},
		{
			name:  "default event type and multi-line data",
			input: "data: a\ndata: b\n\n",
			want:  []sseEvent{{Event: "message", Data: "a\nb"}},
			retry: -1,
		},
		{
			name:   "ids persist and comments are skipped",
			input:  ": hello\nid: 7\nevent: patch\ndata: x\n\nevent: patch\ndata: y\n\n",
			want:   []sseEvent{{ID: "7", Event: "patch", Data: "x"}, {ID: "7", Event: "patch", Data: "y"}},
			lastID: "7",
			retry:  -1,
		},
		{
			name:  "CRLF and CR line endings",
			input: "event: a\r\ndata: 1\r\n\r\nevent: b\rdata: 2\r\r",
			want:  []sseEvent{{Event: "a", Data: "1"}, {Event: "b", Data: "2"}},
			retry: -1,
		},
		{
			name:  "no space after colon and field without colon",
			input: "data:tight\ndata\n\n",
			want:  []sseEvent{{Event: "message", Data: "tight\n"}},
			retry: -1,
		},
		{
			name:  "event without data is not dispatched",
			input: "event: lonely\n\ndata: real\n\n",
			want:  []sseEvent{{Event: "message", Data: "real"}},
			retry: -1,
		},
		{
			name:  "retry field",
			input: "retry: 3000\n\nretry: nope\ndata: x\n\n",
			want:  []sseEvent{{Event: "message", Data: "x"}},
			retry: 3000,
		},
		{
			name:  "unknown fields ignored",
			input: "foo: bar\ndata: x\n\n",
			want:  []sseEvent{{Event: "message", Data: "x"}},
			retry: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newSSEReader(strings.NewReader(tt.input), "")
			for i, want := range tt.want {
				got, err := r.Next()
				if err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
				if got != want {
					t.Errorf("event %d = %+v, want %+v", i, got, want)
				}
			}
			if _, err := r.Next(); err != io.EOF {
				t.Errorf("expected EOF after events, got %v", err)
			}
			if r.lastID != tt.lastID || r.retry != tt.retry {
				t.Errorf("lastID=%q retry=%d, want %q %d", r.lastID, r.retry, tt.lastID, tt.retry)
			}
		})
	}
}

func TestSSEReaderTruncatedEvent(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: partial"), "")
	if _, err := r.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want ErrUnexpectedEOF", err)
	}
}

func TestSSEReaderActivityCallback(t *testing.T) {
	r := newSSEReader(strings.NewReader(": ping\n\n"), "")
	n := 0
	r.onActivity = func() { n++ }
	_, _ = r.Next()
	if n == 0 {
		t.Error("activity callback not invoked for comments")
	}
}
