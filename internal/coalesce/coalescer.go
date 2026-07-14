// SPDX-License-Identifier: Apache-2.0

// Package coalesce folds correlated Alertmanager incidents into a single
// investigation, suppressing the redundant per-alert investigations a storm
// would otherwise spawn. In-memory, mutex-guarded, clock injected for tests.
package coalesce

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/telemetry"
)

// Config mirrors config.Coalesce with std durations. The enable gate lives in
// main.go — a constructed Coalescer is always active.
type Config struct {
	Debounce          time.Duration
	MaxWait           time.Duration
	MaxBatch          int
	Cooldown          time.Duration
	CorrelationLabels []string
}

type batch struct {
	incidents           []investigate.Request
	firstSeen, lastSeen time.Time
}

// cooldown records, per correlation key, when an investigation last fired and
// which alertnames it has already covered. The alertname set lets a genuinely
// new critical problem (an unseen alertname sharing the key) bypass the
// cooldown, while same-alertname repeats — the storm noise the cooldown exists
// to quash — stay suppressed. Evicted by sweep once aged past Cooldown.
type cooldown struct {
	last time.Time
	seen map[string]struct{} // alertnames already flushed under this key
}

// Coalescer buffers correlated incidents and flushes one investigation per key.
type Coalescer struct {
	cfg     Config
	now     func() time.Time
	out     func([]investigate.Request) // flush sink (build a Request + enqueue)
	Metrics *telemetry.Metrics          // optional; nil-safe OTel counters
	Log     *slog.Logger                // optional; nil-safe

	// Contested reports whether a TriggerKey has standing 👎 feedback (wired to
	// the outcome ledger). A contested trigger bypasses the cooldown suppression:
	// the recurrence gate re-arms on a standing 👎, and this outer layer must not
	// silently defer that re-arm for up to Cooldown (#288). Consulted only when
	// suppression would otherwise happen, so the ledger stays off the common path.
	// Optional; nil means no bypass.
	Contested func(triggerKey string) bool

	mu      sync.Mutex
	pending map[string]*batch
	recent  map[string]*cooldown
}

// New builds a Coalescer. out is called with each flushed batch.
func New(cfg Config, out func([]investigate.Request)) *Coalescer {
	return &Coalescer{
		cfg: cfg, now: time.Now, out: out,
		pending: map[string]*batch{}, recent: map[string]*cooldown{},
	}
}

// key returns the correlation key for an incident. When CorrelationLabels are
// set, the key is namespace + those label values. Otherwise it's the AM
// groupKey, falling back to namespace/alertname.
func (c *Coalescer) key(r investigate.Request) string {
	if len(c.cfg.CorrelationLabels) > 0 {
		parts := make([]string, 0, len(c.cfg.CorrelationLabels))
		anyPresent := false
		for _, l := range c.cfg.CorrelationLabels {
			v := r.Labels[l]
			if v != "" {
				anyPresent = true
			}
			parts = append(parts, v)
		}
		// Only correlate on label values when at least one is present. If every
		// configured label is absent the key would degenerate to "ns/" and
		// collapse unrelated incidents, so fall through to the GroupKey path.
		if anyPresent {
			return r.Workload.Namespace + "/" + strings.Join(parts, "/")
		}
	}
	if r.GroupKey != "" {
		return r.GroupKey
	}
	return r.Workload.Namespace + "/" + r.Title
}

// Summarize renders a one-line digest of a coalesced batch for the seed prompt.
func Summarize(reqs []investigate.Request) string {
	counts := map[string]int{}
	for _, r := range reqs {
		counts[r.Title]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", n, counts[n]))
	}
	return fmt.Sprintf("%d correlated alerts: %s", len(reqs), strings.Join(parts, ", "))
}

// maxConstituents bounds the distinct constituent list surfaced in the seed so a
// pathological storm (hundreds of distinct workloads under one key) can't blow up
// the prompt. Beyond the cap the excess is dropped — the summary count still
// reflects the true batch size, so nothing about the storm's magnitude is hidden.
const maxConstituents = 20

// Constituents returns the DISTINCT constituent identities of a coalesced batch
// OTHER than the representative's (batch[0]) own, for surfacing in the seed as the
// storm's blast radius. Each entry prefers the workload ref ("ns/name" or "ns"),
// falling back to the alert title when the alert carries no workload. Order is
// stable (first-seen) and the list is capped at maxConstituents. Returns nil for a
// singleton batch (nothing beyond the representative to surface).
func Constituents(batch []investigate.Request) []string {
	if len(batch) < 2 {
		return nil
	}
	seen := map[string]struct{}{}
	if rep := constituentID(batch[0]); rep != "" {
		seen[rep] = struct{}{} // never echo the representative back to itself
	}
	out := make([]string, 0, len(batch)-1)
	for _, r := range batch[1:] {
		id := constituentID(r)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= maxConstituents {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// constituentID renders one incident's identity for the constituent list: its
// workload ref when it names a workload, else its alert title.
func constituentID(r investigate.Request) string {
	if ref := r.Workload.Ref(); ref != "" {
		return ref
	}
	return r.Title
}

// Add ingests one incident: critical → flush now; within cooldown → suppress;
// else buffer (flushing when MaxBatch is reached).
func (c *Coalescer) Add(r investigate.Request) {
	k := c.key(r)
	var flush []investigate.Request
	c.mu.Lock()
	now := c.now()
	// An investigation for this key already fired within the cooldown — suppress
	// the rest of the storm. This is checked before the critical fast-path so a
	// storm of critical alerts collapses to one investigation (the first) plus
	// suppressions, rather than one investigation per alert. Two exceptions:
	// newCriticalDuringCooldown lets a critical with an alertname not yet seen for
	// this key through (a genuinely new problem, not storm noise), and a standing
	// 👎 on the trigger bypasses suppression so the human's re-arm reaches the
	// recurrence gate at the very next occurrence instead of being deferred here.
	suppress := c.withinCooldown(k, now) && !c.newCriticalDuringCooldown(k, r)
	// Contested (a ledger lookup with its own lock, no call cycles back here) is
	// consulted under mu but only when suppression would otherwise happen.
	bypass := suppress && c.Contested != nil && r.TriggerKey != "" && c.Contested(r.TriggerKey)
	switch {
	case suppress && !bypass:
		c.mu.Unlock()
		if l := c.Log; l != nil {
			l.Info("coalesce cooldown: suppressing alert",
				"key", k, "title", r.Title, "trigger_key", r.TriggerKey, "cooldown", c.cfg.Cooldown)
		}
		if m := c.Metrics; m != nil {
			m.AlertsSuppressed.Add(context.Background(), 1)
		}
		return
	case r.IsCritical():
		// Critical (first for this key, or a new alertname during cooldown): flush
		// immediately with no debounce wait, draining any pending batch. Same-key
		// same-alertname criticals fall into the cooldown case above and are suppressed.
		// The predicate is investigate.Request.IsCritical, shared with the incident
		// debouncer's identical carve-out (source.incidentDebouncer.Hold) so the two
		// waits enforce "never delay the first look at a critical page" identically.
		flush = []investigate.Request{r}
		if b, ok := c.pending[k]; ok {
			flush = make([]investigate.Request, 0, len(b.incidents)+1)
			flush = append(flush, b.incidents...)
			flush = append(flush, r)
			delete(c.pending, k)
		}
		c.markFlushed(k, now, flush)
	default:
		b := c.pending[k]
		if b == nil {
			b = &batch{firstSeen: now}
			c.pending[k] = b
		}
		b.incidents = append(b.incidents, r)
		b.lastSeen = now
		if c.cfg.MaxBatch > 0 && len(b.incidents) >= c.cfg.MaxBatch {
			flush = b.incidents
			delete(c.pending, k)
			c.markFlushed(k, now, flush)
		}
	}
	c.mu.Unlock()
	if bypass {
		if l := c.Log; l != nil {
			l.Info("standing 👎 bypasses coalesce cooldown",
				"key", k, "title", r.Title, "trigger_key", r.TriggerKey)
		}
	}
	if flush != nil {
		c.emit(flush)
	}
}

// Enqueue satisfies investigate.Enqueuer by folding the request through the
// coalescer, so a source pipeline can route admitted Requests through coalescing.
func (c *Coalescer) Enqueue(r investigate.Request) { c.Add(r) }

// emit records coalescing metrics for a flushed batch, then hands it to out.
// Keeping this in the Coalescer (rather than the out closure) means the package
// owns its full metric surface, including for tests that supply a custom out.
func (c *Coalescer) emit(batch []investigate.Request) {
	if m := c.Metrics; m != nil {
		if n := len(batch); n > 1 {
			m.AlertsCoalesced.Add(context.Background(), int64(n-1))
		}
		m.CoalesceBatchSize.Record(context.Background(), int64(len(batch)))
	}
	if c.out != nil {
		c.out(batch)
	}
}

// withinCooldown reports whether key k fired an investigation recently enough
// that same-key alerts should be suppressed. Caller holds mu.
func (c *Coalescer) withinCooldown(k string, now time.Time) bool {
	cd, ok := c.recent[k]
	return ok && c.cfg.Cooldown > 0 && now.Sub(cd.last) < c.cfg.Cooldown
}

// newCriticalDuringCooldown reports whether inc is a critical alert whose
// alertname has not yet been covered by the current cooldown for key k — i.e. a
// genuinely new problem that should bypass suppression. Caller holds mu.
func (c *Coalescer) newCriticalDuringCooldown(k string, r investigate.Request) bool {
	if !r.IsCritical() {
		return false
	}
	cd, ok := c.recent[k]
	if !ok {
		return true
	}
	_, seen := cd.seen[r.Title]
	return !seen
}

// markFlushed records, for key k, that an investigation fired at now covering
// the alertnames in the flushed batch. Caller holds mu.
func (c *Coalescer) markFlushed(k string, now time.Time, batch []investigate.Request) {
	cd := c.recent[k]
	if cd == nil {
		cd = &cooldown{seen: map[string]struct{}{}}
		c.recent[k] = cd
	}
	cd.last = now
	for _, r := range batch {
		cd.seen[r.Title] = struct{}{}
	}
}

// sweep flushes every pending batch that has been quiet for >= Debounce or is
// older than MaxWait.
func (c *Coalescer) sweep() {
	var flushes [][]investigate.Request
	c.mu.Lock()
	now := c.now()
	for k, b := range c.pending {
		debounced := now.Sub(b.lastSeen) >= c.cfg.Debounce
		maxWaited := c.cfg.MaxWait > 0 && now.Sub(b.firstSeen) >= c.cfg.MaxWait
		if debounced || maxWaited {
			flushes = append(flushes, b.incidents)
			delete(c.pending, k)
			c.markFlushed(k, now, b.incidents)
		}
	}
	c.evictExpiredCooldowns(now)
	c.mu.Unlock()
	for _, f := range flushes {
		c.emit(f)
	}
}

// evictExpiredCooldowns drops recent records that can no longer suppress
// anything, keeping the map bounded over a long serve with churning keys. An
// entry is dead once aged past Cooldown; when Cooldown <= 0, withinCooldown
// never consults recent, so every entry is dead weight. Caller holds mu.
func (c *Coalescer) evictExpiredCooldowns(now time.Time) {
	for k, cd := range c.recent {
		if c.cfg.Cooldown <= 0 || now.Sub(cd.last) >= c.cfg.Cooldown {
			delete(c.recent, k)
		}
	}
}

// Run sweeps on a ticker until ctx is cancelled. tick should be ~Debounce/2.
// A zero or negative tick is clamped to 1s to avoid a NewTicker panic.
func (c *Coalescer) Run(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweep()
		}
	}
}
