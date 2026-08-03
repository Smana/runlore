// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadBodyBoundary: a body of exactly MaxResponseBytes is legitimate and
// must come back whole; only the byte after it proves the upstream had more.
func TestReadBodyBoundary(t *testing.T) {
	exact := strings.Repeat("a", MaxResponseBytes)
	got, err := ReadBody(strings.NewReader(exact))
	if err != nil {
		t.Fatalf("a body at the cap must be accepted: %v", err)
	}
	if len(got) != MaxResponseBytes {
		t.Fatalf("short read: got %d want %d", len(got), MaxResponseBytes)
	}

	_, err = ReadBody(strings.NewReader(exact + "!"))
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("one byte over the cap must fail: %v", err)
	}
	if !strings.Contains(err.Error(), "narrow the query") {
		t.Errorf("the model reads this text — it must be actionable: %v", err)
	}
}

// TestCappedReaderFailsRatherThanEndingQuietly is the whole reason CappedReader
// exists instead of io.LimitReader: a streaming parser reads a limit as a clean
// end-of-stream and reports its partial result as the complete answer. A partial
// answer the model has no reason to doubt is worse than no answer.
func TestCappedReaderFailsRatherThanEndingQuietly(t *testing.T) {
	r := CappedReader(strings.NewReader(strings.Repeat("b", MaxResponseBytes+64)))
	if _, err := io.ReadAll(r); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}

	// io.LimitReader is the trap being avoided: it reports io.EOF, indistinguishable
	// from a body that genuinely ended.
	lim := io.LimitReader(strings.NewReader(strings.Repeat("b", MaxResponseBytes+64)), MaxResponseBytes)
	if _, err := io.ReadAll(lim); err != nil {
		t.Fatalf("premise broken — LimitReader is supposed to end quietly: %v", err)
	}
}

// TestReadErrorBody bounds the diagnostic prefix of a failure response and
// strips the credential the request carried.
func TestReadErrorBody(t *testing.T) {
	const secret = "glpat-SUPERSECRETVALUE1234"
	got := ReadErrorBody(strings.NewReader(`{"error":"401 for `+secret+`"}`), secret)
	if strings.Contains(got, secret) {
		t.Errorf("credential survived: %q", got)
	}
	if got := ReadErrorBody(strings.NewReader(strings.Repeat("c", 1<<20))); len(got) != maxErrorBody {
		t.Errorf("error body not bounded: len=%d want %d", len(got), maxErrorBody)
	}
}
