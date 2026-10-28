// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type countingSink struct{ n int }

func (c *countingSink) Register(string, string, string, providers.Investigation) { c.n++ }

func TestSinksFansOutAndSkipsNil(t *testing.T) {
	a, b := &countingSink{}, &countingSink{}
	Sinks{a, nil, b}.Register("slack", "root", "C1", providers.Investigation{TriggerKey: "k"})
	if a.n != 1 || b.n != 1 {
		t.Fatalf("each sink once: %d, %d", a.n, b.n)
	}
	Sinks(nil).Register("slack", "root", "C1", providers.Investigation{}) // an empty fan-out is a no-op
}

func TestMultiAdd(t *testing.T) {
	m := NewMulti(nil)
	m.Add(nopNotifier{}, nopNotifier{})
	if m.Len() != 2 {
		t.Fatalf("got %d notifiers", m.Len())
	}
}
