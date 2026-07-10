// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"sync"
	"time"
)

// Deduper suppresses repeated investigations of the same still-firing alert
// within a time window. Safe for concurrent use. The clock is injectable for tests.
type Deduper struct {
	window time.Duration
	now    func() time.Time
	mu     sync.Mutex
	seen   map[string]time.Time
}

// NewDeduper returns a Deduper with the given window. A zero window disables dedup.
func NewDeduper(window time.Duration) *Deduper {
	return &Deduper{window: window, now: time.Now, seen: map[string]time.Time{}}
}

// Seen records the key and reports whether it was already seen within the window.
// The key must be non-empty. Expired entries are evicted on each call so the
// map stays bounded by the dedup window.
func (d *Deduper) Seen(key string) bool {
	if d.window <= 0 {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	for k, t := range d.seen {
		if now.Sub(t) >= d.window {
			delete(d.seen, k)
		}
	}
	if last, ok := d.seen[key]; ok && now.Sub(last) < d.window {
		return true
	}
	d.seen[key] = now
	return false
}
