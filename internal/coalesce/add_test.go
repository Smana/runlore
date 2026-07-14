// SPDX-License-Identifier: Apache-2.0

package coalesce

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// recurrenceStub adapts a func to investigate.RecurrenceStats for wiring test
// ledger views into Coalescer.Outcome.
type recurrenceStub func(triggerKey string) outcome.TriggerRecurrence

func (f recurrenceStub) Recurrence(k string) outcome.TriggerRecurrence { return f(k) }

// contestedFor returns a stub whose Recurrence reports a standing 👎 for
// exactly the given TriggerKey.
func contestedFor(key string) recurrenceStub {
	return func(k string) outcome.TriggerRecurrence {
		if k == key {
			return outcome.TriggerRecurrence{FeedbackDown: 1}
		}
		return outcome.TriggerRecurrence{}
	}
}

type sink struct{ batches [][]investigate.Request }

func (s *sink) out(b []investigate.Request) { s.batches = append(s.batches, b) }

func newAt(cfg Config, s *sink, now *time.Time) *Coalescer {
	c := New(cfg, s.out)
	c.now = func() time.Time { return *now }
	return c
}

func TestAddBuffersUntilMaxBatch(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 2}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK")) // buffered
	if len(s.batches) != 0 {
		t.Fatal("first alert must buffer, not flush")
	}
	c.Add(inc("X", "ns", "warning", "GK")) // hits MaxBatch=2 → flush
	if len(s.batches) != 1 || len(s.batches[0]) != 2 {
		t.Fatalf("MaxBatch should flush 2, got %v", s.batches)
	}
}

func TestAddCooldownSuppresses(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 1, Cooldown: 10 * time.Minute}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK")) // MaxBatch=1 → immediate flush, seeds recent[GK]
	now = now.Add(time.Minute)
	c.Add(inc("X", "ns", "warning", "GK")) // within cooldown → suppressed
	if len(s.batches) != 1 {
		t.Fatalf("second alert should be suppressed, batches=%d", len(s.batches))
	}
}

func TestAddCriticalFastPath(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 100}, s, &now)
	c.Add(inc("X", "ns", "critical", "GK")) // critical bypasses debounce → immediate flush
	if len(s.batches) != 1 {
		t.Fatalf("critical should flush immediately, batches=%d", len(s.batches))
	}
}

// A storm of critical alerts for one key flushes the first immediately (no debounce
// wait) and suppresses the rest within the cooldown — one investigation, not N.
func TestAddCriticalStormSuppressedAfterFirst(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 100, Cooldown: 10 * time.Minute}, s, &now)
	for i := 0; i < 5; i++ {
		c.Add(inc("X", "ns", "critical", "GK"))
	}
	if len(s.batches) != 1 || len(s.batches[0]) != 1 {
		t.Fatalf("critical storm should flush once (the first), got batches=%v", s.batches)
	}
	if len(c.pending) != 0 {
		t.Fatalf("storm alerts after the first should be suppressed, not buffered (pending=%d)", len(c.pending))
	}
}

// During cooldown, a critical with an alertname not yet seen for the key is a
// genuinely new problem and must flush — it must not be swallowed as storm
// noise. Same-key correlation (CorrelationLabels) is what makes two distinct
// alertnames share a key.
func TestAddNewCriticalDuringCooldownFlushes(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 100, Cooldown: 10 * time.Minute,
		CorrelationLabels: []string{"app"}}, s, &now)
	first := investigate.Request{Title: "CrashLoop", Workload: providers.Workload{Namespace: "ns"}, Severity: "critical",
		Labels: map[string]string{"app": "web"}}
	second := investigate.Request{Title: "PodNotReady", Workload: providers.Workload{Namespace: "ns"}, Severity: "critical",
		Labels: map[string]string{"app": "web"}} // same key (app=web), different alertname

	c.Add(first) // flush #1, seeds recent + seen{CrashLoop}
	now = now.Add(time.Minute)
	c.Add(second) // within cooldown but a NEW alertname → flush #2
	if len(s.batches) != 2 {
		t.Fatalf("a new critical alertname during cooldown must flush, batches=%d", len(s.batches))
	}
	// A repeat of the same alertname during cooldown is still suppressed.
	now = now.Add(time.Minute)
	c.Add(second) // PodNotReady again → suppressed
	if len(s.batches) != 2 {
		t.Fatalf("repeat critical alertname during cooldown must be suppressed, batches=%d", len(s.batches))
	}
}

// A standing 👎 on the incoming trigger must bypass the cooldown suppression:
// the human said "that diagnosis is wrong", so the very next occurrence must
// reach the recurrence gate (which re-arms on the same signal) instead of being
// silently absorbed by a layer that knows nothing about feedback (#288).
func TestAddContestedTriggerBypassesCooldown(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 1, Cooldown: 10 * time.Minute}, s, &now)
	c.Outcome = contestedFor("tk-1")

	first := inc("X", "ns", "warning", "GK")
	first.TriggerKey = "tk-1"
	c.Add(first) // MaxBatch=1 → flush, seeds cooldown
	now = now.Add(time.Minute)
	c.Add(first) // within cooldown BUT contested → must not be suppressed
	if len(s.batches) != 2 {
		t.Fatalf("a contested trigger during cooldown must bypass suppression, batches=%d", len(s.batches))
	}
}

// An uncontested trigger stays suppressed during cooldown even with the
// ledger view wired — the escape hatch is per-trigger, not global.
func TestAddUncontestedTriggerStaysSuppressed(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 1, Cooldown: 10 * time.Minute}, s, &now)
	var asked []string
	c.Outcome = recurrenceStub(func(k string) outcome.TriggerRecurrence {
		asked = append(asked, k)
		return outcome.TriggerRecurrence{} // no standing 👎
	})

	first := inc("X", "ns", "warning", "GK")
	first.TriggerKey = "tk-1"
	c.Add(first) // flush, seeds cooldown
	now = now.Add(time.Minute)
	c.Add(first) // within cooldown, callback says no standing 👎 → suppressed
	if len(s.batches) != 1 {
		t.Fatalf("an uncontested trigger during cooldown must stay suppressed, batches=%d", len(s.batches))
	}
	if len(asked) != 1 || asked[0] != "tk-1" {
		t.Fatalf("the ledger must be consulted with the incoming TriggerKey, asked=%v", asked)
	}
}

// A request without a TriggerKey never consults the ledger — there is nothing
// to look up a standing 👎 by, so the cooldown suppresses as before.
func TestAddEmptyTriggerKeySkipsContestedLookup(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 1, Cooldown: 10 * time.Minute}, s, &now)
	c.Outcome = recurrenceStub(func(string) outcome.TriggerRecurrence {
		t.Fatal("the ledger must not be consulted for an empty TriggerKey")
		return outcome.TriggerRecurrence{}
	})

	c.Add(inc("X", "ns", "warning", "GK")) // flush, seeds cooldown (no TriggerKey)
	now = now.Add(time.Minute)
	c.Add(inc("X", "ns", "warning", "GK")) // within cooldown → suppressed, no lookup
	if len(s.batches) != 1 {
		t.Fatalf("empty-TriggerKey repeat must stay suppressed, batches=%d", len(s.batches))
	}
}

// A contested CRITICAL repeat must take the buffer path, not the critical
// fast-path: the "never delay the first look at a critical page" invariant is
// about the FIRST look, and a contested repeat is by definition not that. If it
// flushed immediately, a 40-alert contested storm would fan out to 40
// investigations — the bypass would defeat the very storm collapse the
// coalescer exists for. Buffered, the storm collapses to ONE re-investigation
// per debounce window.
func TestAddContestedCriticalStormCollapses(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 100, Cooldown: 10 * time.Minute}, s, &now)
	c.Outcome = contestedFor("tk-1")

	first := inc("X", "ns", "critical", "GK")
	first.TriggerKey = "tk-1"
	c.Add(first) // first look: critical fast-path → flush #1, arms cooldown
	if len(s.batches) != 1 {
		t.Fatalf("first critical must flush immediately, batches=%d", len(s.batches))
	}
	for i := 0; i < 5; i++ { // contested critical storm within the cooldown
		now = now.Add(time.Second)
		c.Add(first)
	}
	if len(s.batches) != 1 {
		t.Fatalf("contested critical repeats must buffer, not flush per alert, batches=%d", len(s.batches))
	}
	if len(c.pending) != 1 {
		t.Fatalf("contested repeats must be buffered under their key, pending=%d", len(c.pending))
	}
	now = now.Add(2 * time.Minute) // past Debounce → sweep flushes the batch
	c.sweep()
	if len(s.batches) != 2 || len(s.batches[1]) != 5 {
		t.Fatalf("storm must collapse to ONE re-investigation batch of 5, batches=%v", len(s.batches))
	}
}

// Cooldown suppression must be visible in logs: an `investigate=true` incident
// line followed by silence is undiagnosable from logs alone (#288). The line is
// symmetrical to the recurrence gate's "recurrence cooldown: suppressing".
func TestAddCooldownSuppressionLogs(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 1, Cooldown: 10 * time.Minute}, s, &now)
	var buf bytes.Buffer
	c.Log = slog.New(slog.NewTextHandler(&buf, nil))

	c.Add(inc("X", "ns", "warning", "GK")) // flush, seeds cooldown
	now = now.Add(time.Minute)
	c.Add(inc("X", "ns", "warning", "GK")) // within cooldown → suppressed + logged
	if got := buf.String(); !strings.Contains(got, "coalesce cooldown: suppressing alert") {
		t.Fatalf("cooldown suppression must emit a log line, got: %q", got)
	}
}

// The contested bypass is equally log-visible, so a re-investigation that
// "should have been suppressed" is explainable from logs.
func TestAddContestedBypassLogs(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 1, Cooldown: 10 * time.Minute}, s, &now)
	c.Outcome = contestedFor("tk-1")
	var buf bytes.Buffer
	c.Log = slog.New(slog.NewTextHandler(&buf, nil))

	first := inc("X", "ns", "warning", "GK")
	first.TriggerKey = "tk-1"
	c.Add(first)
	now = now.Add(time.Minute)
	c.Add(first) // contested → bypass + logged
	if got := buf.String(); !strings.Contains(got, "standing 👎 bypasses coalesce cooldown") {
		t.Fatalf("contested bypass must emit a log line, got: %q", got)
	}
}

// A warning repeat during cooldown stays suppressed — only genuinely new
// criticals get the cooldown bypass.
func TestAddWarningDuringCooldownSuppressed(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 1, Cooldown: 10 * time.Minute,
		CorrelationLabels: []string{"app"}}, s, &now)
	first := investigate.Request{Title: "A", Workload: providers.Workload{Namespace: "ns"}, Severity: "warning",
		Labels: map[string]string{"app": "web"}}
	newWarn := investigate.Request{Title: "B", Workload: providers.Workload{Namespace: "ns"}, Severity: "warning",
		Labels: map[string]string{"app": "web"}} // same key, new alertname, but warning
	c.Add(first) // MaxBatch=1 → flush, seeds cooldown
	now = now.Add(time.Minute)
	c.Add(newWarn) // new alertname but only warning → still suppressed
	if len(s.batches) != 1 {
		t.Fatalf("a new warning during cooldown must stay suppressed, batches=%d", len(s.batches))
	}
}
