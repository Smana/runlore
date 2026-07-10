// SPDX-License-Identifier: Apache-2.0

package clientcore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// testEvent is a minimal provider-style SSE event payload. Unknown JSON fields
// are ignored by encoding/json, mirroring how the provider event structs decode
// only what they accumulate.
type testEvent struct {
	Type string `json:"type"`
	N    int    `json:"n"`
}

// collectSSE drains SSEEvents over r and returns the yielded events and errors,
// each in yield order.
func collectSSE(t *testing.T, r io.Reader) (events []testEvent, errs []error) {
	t.Helper()
	for ev, err := range SSEEvents[testEvent](r) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		events = append(events, ev)
	}
	return events, errs
}

// TestSSEEventsFraming asserts the framing+decoding grammar over an untrusted
// provider stream: well-formed events decode, keep-alive comments and non-data
// fields are invisible, a trailing unterminated event is flushed at EOF, and —
// critically — a malformed JSON event yields a decode error WITHOUT killing the
// stream (the provider fold decides whether to bail; one garbled frame must not
// discard an otherwise good completion).
func TestSSEEventsFraming(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantEvents []testEvent
		wantErrs   int
	}{
		{
			name:       "single event",
			input:      "data: {\"type\":\"a\",\"n\":1}\n\n",
			wantEvents: []testEvent{{Type: "a", N: 1}},
		},
		{
			name:       "multiple events",
			input:      "data: {\"type\":\"a\",\"n\":1}\n\ndata: {\"type\":\"b\",\"n\":2}\n\n",
			wantEvents: []testEvent{{Type: "a", N: 1}, {Type: "b", N: 2}},
		},
		{
			name:       "no space after the data colon (the space is optional per the SSE spec)",
			input:      "data:{\"type\":\"a\",\"n\":1}\n\n",
			wantEvents: []testEvent{{Type: "a", N: 1}},
		},
		{
			name:       "multi-line data joins with a newline (valid JSON whitespace between tokens)",
			input:      "data: {\"type\":\"multi\",\ndata: \"n\":3}\n\n",
			wantEvents: []testEvent{{Type: "multi", N: 3}},
		},
		{
			name:       "keep-alive comments and non-data fields are ignored",
			input:      ": keep-alive\n\nevent: message_start\nid: 42\nretry: 1000\ndata: {\"type\":\"a\",\"n\":1}\n\n: ping\n\n",
			wantEvents: []testEvent{{Type: "a", N: 1}},
		},
		{
			name:       "final event with no trailing blank line is flushed at EOF",
			input:      "data: {\"type\":\"tail\",\"n\":9}",
			wantEvents: []testEvent{{Type: "tail", N: 9}},
		},
		{
			name:  "empty stream yields nothing",
			input: "",
		},
		{
			name:  "comments and blank lines only yield nothing",
			input: ": ping\n\n: pong\n\n\n",
		},
		{
			name:       "CRLF line endings",
			input:      "data: {\"type\":\"crlf\",\"n\":4}\r\n\r\n",
			wantEvents: []testEvent{{Type: "crlf", N: 4}},
		},
		{
			name:       "malformed JSON yields a decode error and the stream continues",
			input:      "data: {oops\n\ndata: {\"type\":\"ok\",\"n\":6}\n\n",
			wantEvents: []testEvent{{Type: "ok", N: 6}},
			wantErrs:   1,
		},
		{
			name:       "empty data payload yields a decode error and the stream continues",
			input:      "data:\n\ndata: {\"type\":\"after\",\"n\":5}\n\n",
			wantEvents: []testEvent{{Type: "after", N: 5}},
			wantErrs:   1,
		},
		{
			name:       "good-bad-good interleaving keeps both good events",
			input:      "data: {\"type\":\"g1\",\"n\":1}\n\ndata: nope\n\ndata: {\"type\":\"g2\",\"n\":2}\n\n",
			wantEvents: []testEvent{{Type: "g1", N: 1}, {Type: "g2", N: 2}},
			wantErrs:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events, errs := collectSSE(t, strings.NewReader(tc.input))
			if !slices.Equal(events, tc.wantEvents) {
				t.Errorf("events = %v, want %v", events, tc.wantEvents)
			}
			if len(errs) != tc.wantErrs {
				t.Errorf("errors = %v, want %d of them", errs, tc.wantErrs)
			}
		})
	}
}

// TestSSEEventsDecodeError asserts a decode failure surfaces the underlying
// json error (errors.As) under the "decode sse event" wrap, so a provider log
// pinpoints a malformed upstream frame rather than a generic read failure.
func TestSSEEventsDecodeError(t *testing.T) {
	_, errs := collectSSE(t, strings.NewReader("data: {truncated\n\n"))
	if len(errs) != 1 {
		t.Fatalf("want exactly one decode error, got %v", errs)
	}
	var syn *json.SyntaxError
	if !errors.As(errs[0], &syn) {
		t.Errorf("decode error should wrap the json error, got %v", errs[0])
	}
	if !strings.Contains(errs[0].Error(), "decode sse event") {
		t.Errorf("decode error should name the failure, got %v", errs[0])
	}
}

// errReader yields its data, then a permanent read error — a mid-stream
// connection drop as the line scanner sees it.
type errReader struct {
	data []byte
	err  error
	off  int
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

// TestSSEEventsReadErrorIsTerminal asserts a transport read error is yielded
// unwrapped (so callers can errors.Is against it) after the events that were
// fully framed before the drop — a dropped stream must be observable, never
// silently treated as a clean end.
func TestSSEEventsReadErrorIsTerminal(t *testing.T) {
	drop := errors.New("connection reset by peer")
	r := &errReader{data: []byte("data: {\"type\":\"a\",\"n\":1}\n\n"), err: drop}
	events, errs := collectSSE(t, r)
	if want := []testEvent{{Type: "a", N: 1}}; !slices.Equal(events, want) {
		t.Errorf("events before the drop = %v, want %v", events, want)
	}
	if len(errs) != 1 || !errors.Is(errs[0], drop) {
		t.Errorf("want the read error yielded once, got %v", errs)
	}
}

// TestSSEEventsOversizedLineIsTerminal asserts a single line beyond the 1 MiB
// SSE line cap aborts the stream with bufio.ErrTooLong — the memory-exhaustion
// defense against a hostile upstream — and that nothing after it is delivered
// (a framing error is terminal, unlike a per-event decode error).
func TestSSEEventsOversizedLineIsTerminal(t *testing.T) {
	input := "data: " + strings.Repeat("x", 1<<20) + "\n\ndata: {\"type\":\"never\",\"n\":1}\n\n"
	events, errs := collectSSE(t, strings.NewReader(input))
	if len(events) != 0 {
		t.Errorf("no event should survive an oversized line, got %v", events)
	}
	if len(errs) != 1 || !errors.Is(errs[0], bufio.ErrTooLong) {
		t.Fatalf("want a single bufio.ErrTooLong, got %v", errs)
	}
}

// TestSSEEventsLargeEventUnderLimit asserts a large-but-legal event (a ~512 KiB
// payload, the scale of a big tool-args delta or tool schema) still decodes —
// the line cap must stay generous enough for real provider traffic.
func TestSSEEventsLargeEventUnderLimit(t *testing.T) {
	payload := fmt.Sprintf("{\"type\":\"big\",\"n\":7,\"pad\":%q}", strings.Repeat("x", 512<<10))
	events, errs := collectSSE(t, strings.NewReader("data: "+payload+"\n\n"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if want := []testEvent{{Type: "big", N: 7}}; !slices.Equal(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
}

// TestSSEEventsConsumerBreak asserts breaking out of the iteration — on a good
// event or on a decode error — stops it immediately; the provider folds return
// early on a fatal in-band provider error event, and iteration must not keep
// reading the socket behind their back.
func TestSSEEventsConsumerBreak(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"break on a decoded event", "data: {\"type\":\"a\",\"n\":1}\n\ndata: {\"type\":\"b\",\"n\":2}\n\n"},
		{"break on a decode error", "data: nope\n\ndata: {\"type\":\"b\",\"n\":2}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			SSEEvents[testEvent](strings.NewReader(tc.input))(func(testEvent, error) bool {
				calls++
				return false
			})
			if calls != 1 {
				t.Errorf("iteration continued after break: %d yields, want 1", calls)
			}
		})
	}
}
