package httpx

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestSSEDataFramesEvents asserts the SSE framing is parsed correctly: each event's
// data: line(s) are yielded as one payload at the dispatching blank line, multi-line
// data is joined with newlines, and non-data lines (event:, comments, id:) are
// ignored. A trailing event with no blank line is still flushed at EOF.
func TestSSEDataFramesEvents(t *testing.T) {
	const stream = "event: message_start\n" +
		"data: {\"a\":1}\n" +
		"\n" +
		": this is a comment\n" +
		"event: chunk\n" +
		"data: line1\n" +
		"data: line2\n" +
		"\n" +
		"data: {\"last\":true}\n" // no trailing blank line — must still flush at EOF

	var got []string
	for payload, err := range SSEData(strings.NewReader(stream)) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, string(payload))
	}
	want := []string{`{"a":1}`, "line1\nline2", `{"last":true}`}
	if len(got) != len(want) {
		t.Fatalf("got %d events %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSSEDataSurfacesReadError asserts a mid-stream read error is yielded (not
// silently swallowed) so a consumer can fail a dropped stream.
func TestSSEDataSurfacesReadError(t *testing.T) {
	boom := errors.New("boom")
	r := io.MultiReader(strings.NewReader("data: {\"a\":1}\n\n"), &errReader{err: boom})
	var sawErr error
	var events int
	for payload, err := range SSEData(r) {
		if err != nil {
			sawErr = err
			break
		}
		_ = payload
		events++
	}
	if events != 1 {
		t.Fatalf("want 1 good event before the error, got %d", events)
	}
	if !errors.Is(sawErr, boom) {
		t.Fatalf("read error not surfaced: %v", sawErr)
	}
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }
