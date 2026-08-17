// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"errors"
	"fmt"
	"testing"
)

func TestPermanent(t *testing.T) {
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must be nil")
	}
	base := errors.New("boom")
	p := Permanent(base)
	if !IsPermanent(p) {
		t.Fatal("IsPermanent(Permanent(err)) = false")
	}
	if !errors.Is(p, base) {
		t.Fatal("Permanent must unwrap to the original error")
	}
	// Must survive %w wrapping — loop.go returns fmt.Errorf("model: %w", err).
	if !IsPermanent(fmt.Errorf("model: %w", p)) {
		t.Fatal("IsPermanent must see through %w wrapping")
	}
	if IsPermanent(base) {
		t.Fatal("a plain error must not be permanent")
	}
}

// TestWithAttempts pins the marker a cost-charging caller reads: how many
// upstream requests a failed completion actually cost. A client that retries a
// transient failure sends — and a provider bills — one request per attempt,
// while the caller sees a single Complete return; without this the whole retry
// schedule is invisible to any budget.
func TestWithAttempts(t *testing.T) {
	if WithAttempts(nil, 3) != nil {
		t.Fatal("WithAttempts(nil, n) must be nil")
	}
	base := errors.New("boom")
	// One attempt is what an unmarked error already means, so it must not
	// allocate a wrapper that says nothing.
	for _, n := range []int{-1, 0, 1} {
		var wrapper *AttemptsError
		if errors.As(WithAttempts(base, n), &wrapper) {
			t.Fatalf("WithAttempts(err, %d) wrapped the error, want it unchanged", n)
		}
	}
	marked := WithAttempts(base, 3)
	if got := AttemptsOf(marked); got != 3 {
		t.Fatalf("AttemptsOf = %d, want 3", got)
	}
	if !errors.Is(marked, base) {
		t.Fatal("WithAttempts must unwrap to the original error")
	}
	if marked.Error() != base.Error() {
		t.Fatalf("Error() = %q, want the wrapped error's own message %q", marked.Error(), base.Error())
	}
	// Must survive %w wrapping and compose with Permanent, which clientcore
	// applies to the same error on the 4xx path.
	if got := AttemptsOf(fmt.Errorf("model: %w", marked)); got != 3 {
		t.Fatalf("AttemptsOf through %%w = %d, want 3", got)
	}
	both := WithAttempts(Permanent(base), 2)
	if !IsPermanent(both) {
		t.Fatal("an attempts-marked permanent error must still report IsPermanent")
	}
	if got := AttemptsOf(both); got != 2 {
		t.Fatalf("AttemptsOf(permanent) = %d, want 2", got)
	}
	// 0 means "unknown", and a caller billing per attempt reads it as one.
	if got := AttemptsOf(base); got != 0 {
		t.Fatalf("AttemptsOf(unmarked) = %d, want 0 — unmarked means unknown", got)
	}
	if got := AttemptsOf(nil); got != 0 {
		t.Fatalf("AttemptsOf(nil) = %d, want 0", got)
	}
}
