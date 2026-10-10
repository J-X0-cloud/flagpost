package sdk

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
)

// sseEvent is one dispatched server-sent event.
type sseEvent struct {
	ID    string
	Event string
	Data  string
}

// sseReader parses a text/event-stream body following the WHATWG HTML
// specification's event stream interpretation rules: lines may end in LF,
// CR or CRLF; lines starting with a colon are comments; a field without a
// colon has an empty value; a single leading space after the colon is
// dropped; multiple data lines are joined with LF; and an event is
// dispatched on a blank line only if it carries data.
type sseReader struct {
	r *bufio.Reader
	// lastID persists across events, as the spec requires.
	lastID string
	// retry is the most recent reconnection delay sent by the server, in
	// milliseconds, or -1 if none was sent.
	retry int
	// onActivity is called whenever bytes arrive, including comments,
	// so idle-connection watchdogs can be fed by heartbeats.
	onActivity func()
}

func newSSEReader(r io.Reader, lastID string) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10), lastID: lastID, retry: -1}
}

// readLine returns the next line without its terminator, treating CR,
// LF and CRLF as line endings.
func (s *sseReader) readLine() (string, error) {
	var buf bytes.Buffer
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			if err == io.EOF && buf.Len() > 0 {
				// A final unterminated line is discarded per spec, since
				// the event it belongs to can never be completed.
				return "", io.ErrUnexpectedEOF
			}
			return "", err
		}
		if s.onActivity != nil {
			s.onActivity()
		}
		switch b {
		case '\n':
			return buf.String(), nil
		case '\r':
			if next, err := s.r.Peek(1); err == nil && next[0] == '\n' {
				_, _ = s.r.ReadByte()
			}
			return buf.String(), nil
		default:
			buf.WriteByte(b)
		}
	}
}

// Next blocks until the next event is dispatched.
func (s *sseReader) Next() (sseEvent, error) {
	var data strings.Builder
	hasData := false
	eventType := ""
	for {
		line, err := s.readLine()
		if err != nil {
			return sseEvent{}, err
		}
		if line == "" {
			if !hasData {
				eventType = ""
				continue
			}
			ev := sseEvent{ID: s.lastID, Event: eventType, Data: data.String()}
			if ev.Event == "" {
				ev.Event = "message"
			}
			return ev, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventType = value
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			hasData = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				s.lastID = value
			}
		case "retry":
			if n, err := strconv.Atoi(value); err == nil && n >= 0 {
				s.retry = n
			}
		}
	}
}
