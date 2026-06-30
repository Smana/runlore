package httpx

import (
	"bufio"
	"bytes"
	"io"
	"iter"
	"strings"
)

// maxSSELine caps a single SSE line so a hostile/buggy upstream cannot exhaust memory
// with one unbounded line. Generous enough for large tool schemas / JSON arg deltas.
const maxSSELine = 1 << 20 // 1 MiB

// SSEData parses a Server-Sent Events stream and yields each event's concatenated
// `data:` payload (multiple data: lines in one event are joined with newlines, per
// the SSE spec). A blank line dispatches the accumulated event; a final event with no
// trailing blank line is flushed at EOF. Non-data lines (event:, id:, retry:, and
// `:`-comment lines) are ignored — callers that need the event type read it from the
// JSON payload. A read error is yielded as the second value (so a dropped stream is
// observable); io.EOF is treated as a clean end, not an error.
//
// Iteration stops when the consumer breaks or the stream ends. It is the provider's
// SSE framing primitive; each provider unmarshals the payload into its own event type.
func SSEData(r io.Reader) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
		var data []string // data: lines for the current event
		flush := func() bool {
			if len(data) == 0 {
				return true
			}
			payload := strings.Join(data, "\n")
			data = data[:0]
			return yield([]byte(payload), nil)
		}
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 { // blank line: dispatch the buffered event
				if !flush() {
					return
				}
				continue
			}
			if line[0] == ':' { // comment line
				continue
			}
			field, value, _ := bytes.Cut(line, []byte(":"))
			if string(field) != "data" {
				continue // event:, id:, retry:, … — not part of the payload
			}
			// A single leading space after the colon is stripped per the SSE spec.
			value = bytes.TrimPrefix(value, []byte(" "))
			data = append(data, string(value))
		}
		if err := sc.Err(); err != nil {
			yield(nil, err)
			return
		}
		// Flush a trailing event that had no blank-line terminator before EOF.
		flush()
	}
}
