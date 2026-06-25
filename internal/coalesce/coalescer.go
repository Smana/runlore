// Package coalesce folds correlated Alertmanager incidents into a single
// investigation, suppressing the redundant per-alert investigations a storm
// would otherwise spawn. In-memory, mutex-guarded, clock injected for tests.
package coalesce

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/config"
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
	incidents           []config.Incident
	firstSeen, lastSeen time.Time
}

// Coalescer buffers correlated incidents and flushes one investigation per key.
type Coalescer struct {
	cfg     Config
	now     func() time.Time
	out     func([]config.Incident) // flush sink (build a Request + enqueue)
	Metrics *telemetry.Metrics      // optional; nil-safe OTel counters

	mu      sync.Mutex
	pending map[string]*batch
	recent  map[string]time.Time
}

// New builds a Coalescer. out is called with each flushed batch.
func New(cfg Config, out func([]config.Incident)) *Coalescer {
	return &Coalescer{
		cfg: cfg, now: time.Now, out: out,
		pending: map[string]*batch{}, recent: map[string]time.Time{},
	}
}

// key returns the correlation key for an incident. When CorrelationLabels are
// set, the key is namespace + those label values. Otherwise it's the AM
// groupKey, falling back to namespace/alertname.
func (c *Coalescer) key(inc config.Incident) string {
	if len(c.cfg.CorrelationLabels) > 0 {
		parts := make([]string, 0, len(c.cfg.CorrelationLabels))
		anyPresent := false
		for _, l := range c.cfg.CorrelationLabels {
			v := inc.Labels[l]
			if v != "" {
				anyPresent = true
			}
			parts = append(parts, v)
		}
		// Only correlate on label values when at least one is present. If every
		// configured label is absent the key would degenerate to "ns/" and
		// collapse unrelated incidents, so fall through to the GroupKey path.
		if anyPresent {
			return inc.Namespace + "/" + strings.Join(parts, "/")
		}
	}
	if inc.GroupKey != "" {
		return inc.GroupKey
	}
	return inc.Namespace + "/" + inc.AlertName
}

// Summarize renders a one-line digest of a coalesced batch for the seed prompt.
func Summarize(incs []config.Incident) string {
	counts := map[string]int{}
	for _, in := range incs {
		counts[in.AlertName]++
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
	return fmt.Sprintf("%d correlated alerts: %s", len(incs), strings.Join(parts, ", "))
}

// Add ingests one incident: critical → flush now; within cooldown → suppress;
// else buffer (flushing when MaxBatch is reached).
func (c *Coalescer) Add(inc config.Incident) {
	k := c.key(inc)
	var flush []config.Incident
	c.mu.Lock()
	now := c.now()
	switch {
	case c.withinCooldown(k, now):
		// An investigation for this key already fired within the cooldown — suppress
		// the rest of the storm. This is checked before the critical fast-path so a
		// storm of critical alerts collapses to one investigation (the first) plus
		// suppressions, rather than one investigation per alert.
		c.mu.Unlock()
		if m := c.Metrics; m != nil {
			m.AlertsSuppressed.Add(context.Background(), 1)
		}
		return
	case strings.EqualFold(inc.Severity, "critical"):
		// First critical for this key (not in cooldown): flush immediately with no
		// debounce wait, draining any pending batch. Subsequent same-key alerts fall
		// into the cooldown case above and are suppressed.
		flush = []config.Incident{inc}
		if b, ok := c.pending[k]; ok {
			flush = make([]config.Incident, 0, len(b.incidents)+1)
			flush = append(flush, b.incidents...)
			flush = append(flush, inc)
			delete(c.pending, k)
		}
		c.recent[k] = now
	default:
		b := c.pending[k]
		if b == nil {
			b = &batch{firstSeen: now}
			c.pending[k] = b
		}
		b.incidents = append(b.incidents, inc)
		b.lastSeen = now
		if c.cfg.MaxBatch > 0 && len(b.incidents) >= c.cfg.MaxBatch {
			flush = b.incidents
			delete(c.pending, k)
			c.recent[k] = now
		}
	}
	c.mu.Unlock()
	if flush != nil {
		c.emit(flush)
	}
}

// emit records coalescing metrics for a flushed batch, then hands it to out.
// Keeping this in the Coalescer (rather than the out closure) means the package
// owns its full metric surface, including for tests that supply a custom out.
func (c *Coalescer) emit(batch []config.Incident) {
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

func (c *Coalescer) withinCooldown(k string, now time.Time) bool {
	t, ok := c.recent[k]
	return ok && c.cfg.Cooldown > 0 && now.Sub(t) < c.cfg.Cooldown
}

// sweep flushes every pending batch that has been quiet for >= Debounce or is
// older than MaxWait.
func (c *Coalescer) sweep() {
	var flushes [][]config.Incident
	c.mu.Lock()
	now := c.now()
	for k, b := range c.pending {
		debounced := now.Sub(b.lastSeen) >= c.cfg.Debounce
		maxWaited := c.cfg.MaxWait > 0 && now.Sub(b.firstSeen) >= c.cfg.MaxWait
		if debounced || maxWaited {
			flushes = append(flushes, b.incidents)
			delete(c.pending, k)
			c.recent[k] = now
		}
	}
	c.mu.Unlock()
	for _, f := range flushes {
		c.emit(f)
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
