// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"testing"
	"time"
)

func TestDeduper(t *testing.T) {
	d := NewDeduper(30 * time.Minute)
	base := time.Date(2026, 6, 20, 3, 0, 0, 0, time.UTC)
	cur := base
	d.now = func() time.Time { return cur }

	if d.Seen("abc") {
		t.Fatal("first sighting should not be deduped")
	}
	cur = base.Add(10 * time.Minute)
	if !d.Seen("abc") {
		t.Fatal("within window should be deduped")
	}
	cur = base.Add(31 * time.Minute)
	if d.Seen("abc") {
		t.Fatal("after window should not be deduped")
	}
}

func TestDeduperDisabled(t *testing.T) {
	d := NewDeduper(0)
	first := d.Seen("abc")
	second := d.Seen("abc")
	if first || second {
		t.Fatal("zero window disables dedup")
	}
}
