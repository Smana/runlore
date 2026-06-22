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

// Config mirrors config.Coalesce with std durations.
type Config struct {
	Enabled           bool
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

	mu         sync.Mutex
	pending    map[string]*batch
	recent     map[string]time.Time
	suppressed map[string]int
}

// New builds a Coalescer. out is called with each flushed batch.
func New(cfg Config, out func([]config.Incident)) *Coalescer {
	return &Coalescer{
		cfg: cfg, now: time.Now, out: out,
		pending: map[string]*batch{}, recent: map[string]time.Time{}, suppressed: map[string]int{},
	}
}

// key returns the correlation key for an incident. When CorrelationLabels are
// set, the key is namespace + those label values. Otherwise it's the AM
// groupKey, falling back to namespace/alertname.
func (c *Coalescer) key(inc config.Incident) string {
	if len(c.cfg.CorrelationLabels) > 0 {
		parts := make([]string, 0, len(c.cfg.CorrelationLabels))
		for _, l := range c.cfg.CorrelationLabels {
			parts = append(parts, inc.Labels[l])
		}
		return inc.Namespace + "/" + strings.Join(parts, "/")
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
	case strings.EqualFold(inc.Severity, "critical"):
		// Critical alerts are never throttled/suppressed — flush immediately regardless of cooldown.
		flush = []config.Incident{inc}
		if b, ok := c.pending[k]; ok {
			flush = make([]config.Incident, 0, len(b.incidents)+1)
			flush = append(flush, b.incidents...)
			flush = append(flush, inc)
			delete(c.pending, k)
		}
		c.recent[k] = now
	case c.withinCooldown(k, now):
		c.suppressed[k]++
		c.mu.Unlock()
		if m := c.Metrics; m != nil {
			m.AlertsSuppressed.Add(context.Background(), 1)
		}
		return
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
	if flush != nil && c.out != nil {
		c.out(flush)
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
		if c.out != nil {
			c.out(f)
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
