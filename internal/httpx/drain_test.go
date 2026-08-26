// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"io"
	"strings"
	"testing"
)

// There are deliberately NO tests here for connection reuse, and that is a
// finding rather than an omission.
//
// Three were written and all three were unreliable. A table of body size to dial
// count flips between runs; even the weaker "draining is never worse than a bare
// Close" failed at 64 KB on a rerun. Whether the transport had already buffered a
// response by the time Close runs depends on header size, the read-buffer
// boundary and timing, so dial counts are not a deterministic function of body
// size — an assertion on them is a flaky test wearing a proof.
//
// A flaky test pinning a claim is worse than no test: it gets muted, and then the
// claim is unguarded AND believed. So the reuse rationale lives in Drain's doc
// comment, stated as the size-dependent and modest thing it is, and what IS
// deterministic — the bound, and nil-tolerance — is what gets asserted below.
// If the reuse effect is ever worth pinning, it belongs in a benchmark.

// TestDrainIsBounded: past the cap the body must NOT be consumed whole, so a
// runaway upstream cannot hold the caller's timeout open inside cleanup.
func TestDrainIsBounded(t *testing.T) {
	body := io.NopCloser(io.LimitReader(filler{}, int64(maxDrainBytes)*4))
	Drain(body)
	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read rest: %v", err)
	}
	if len(rest) == 0 {
		t.Error("Drain consumed the whole body — the cap did not apply")
	}
	if want := maxDrainBytes * 3; len(rest) != want {
		t.Errorf("drained past the cap: %d bytes left, want %d", len(rest), want)
	}
}

func TestDrainToleratesNil(_ *testing.T) {
	Drain(nil)
	Drain(strings.NewReader(""))
}

type filler struct{}

func (filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
