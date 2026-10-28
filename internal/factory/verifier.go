// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

const (
	// reinvestigateTimeout bounds one short re-investigation.
	reinvestigateTimeout = 5 * time.Minute
	// maxEvidence is the factory's evidence ceiling.
	maxEvidence = 16 << 10
	// verifyTimeout bounds the factory call of one verdict, retries included.
	verifyTimeout = 30 * time.Second
	// Verdicts, as the factory names them.
	verdictResolved   = "resolved"
	verdictPersisting = "persisting"
)

// Verifier is the leader-only soak check: once a fix has merged and soaked, it asks Alertmanager
// whether the alert still fires, re-investigates briefly for the evidence and tells the factory.
// Alertmanager's answer is the verdict; the re-investigation's text is only the evidence.
type Verifier struct {
	Ledger *Ledger
	Client *Client
	// Firing reports whether the alert is firing now; an error means unknown (nothing is posted).
	Firing func(ctx context.Context, alert, ns string) (bool, error)
	// Reinvestigate runs the short check on the record's TriggerKey and returns its text.
	Reinvestigate func(ctx context.Context, r Record) (string, error)
	// Replier posts into the incident's Slack thread; nil posts nothing.
	Replier providers.ThreadNotifier
	Soak    time.Duration
	Now     func() time.Time
	Log     *slog.Logger

	once sync.Mutex // one pass at a time: a slow pass must not overlap the next tick
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Verifier) log() *slog.Logger {
	if v.Log != nil {
		return v.Log
	}
	return slog.Default()
}

// OnMerged records, for the incident key, whether its alert fires at the merge and when the soak
// ends, and says so in the thread. When Alertmanager cannot be asked the alert is taken to have
// been firing: the incident was filed because it was.
func (v *Verifier) OnMerged(ctx context.Context, key string) {
	rec, ok := v.Ledger.Get(key)
	if !ok || rec.Alert == "" {
		return
	}
	firing, err := v.Firing(ctx, rec.Alert, rec.Namespace)
	if err != nil {
		v.log().Warn("factory verifier: cannot tell whether the alert fires at the merge; assuming it does", "alert", rec.Alert, "err", err)
		firing = true
	}
	start := rec.MergedAt
	if start.IsZero() {
		start = v.now()
	}
	if err := v.Ledger.Upsert(key, func(r *Record) {
		r.FiringAtMerge = firing
		r.SoakUntil = start.Add(v.Soak)
	}); err != nil {
		v.log().Error("factory verifier: recording the soak failed", "err", err)
		return
	}
	v.reply(ctx, rec, fmt.Sprintf("Soaking for %s before checking `%s`.", soakText(v.Soak), code(rec.Alert)))
}

// Run calls Once every `every` until ctx ends.
func (v *Verifier) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v.Once(ctx)
		}
	}
}

// Once handles every record whose soak has ended and that has no verdict yet.
func (v *Verifier) Once(ctx context.Context) {
	v.once.Lock()
	defer v.once.Unlock()
	for _, rec := range v.Ledger.Due(v.now()) {
		if ctx.Err() != nil {
			return
		}
		v.check(ctx, rec)
	}
}

func (v *Verifier) check(ctx context.Context, rec Record) {
	firing, err := v.Firing(ctx, rec.Alert, rec.Namespace)
	if err != nil {
		v.log().Warn("factory verifier: Alertmanager unreachable; retrying next tick", "alert", rec.Alert, "err", err)
		return
	}
	verdict := verdictResolved
	if firing {
		verdict = verdictPersisting
	}
	ev := v.evidence(ctx, rec)
	vctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	status, reason, err := v.Client.PostVerification(vctx, rec.FixRoom, Verification{
		Verdict:   verdict,
		Alert:     VerifiedAlert{Name: rec.Alert, FiringAtMerge: rec.FiringAtMerge, FiringNow: firing},
		CheckedAt: v.now().UTC(),
		Evidence:  ev,
	})
	if err != nil {
		v.log().Warn("factory verifier: the factory did not take the verdict; retrying next tick", "alert", rec.Alert, "err", err)
		return
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		if err := v.Ledger.Upsert(rec.TriggerKey, func(r *Record) { r.Verdict = verdict }); err != nil {
			v.log().Error("factory verifier: recording the verdict failed", "err", err)
			return
		}
		if firing {
			v.reply(ctx, rec, fmt.Sprintf("The alert still fires %s after the fix: persisting. The factory opens a follow-up.", soakText(v.Soak)))
		} else {
			v.reply(ctx, rec, fmt.Sprintf("The alert is gone %s after the fix: resolved.", soakText(v.Soak)))
		}
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
		// The factory refused for good: the window closed, the fix is not ours, the verdict
		// stands already. Asking again cannot change the answer.
		outcome := code(reason)
		if reason == "" {
			outcome = fmt.Sprintf("http_%d", status)
		}
		if err := v.Ledger.Upsert(rec.TriggerKey, func(r *Record) { r.Outcome = outcome }); err != nil {
			v.log().Error("factory verifier: recording the outcome failed", "err", err)
			return
		}
		v.log().Warn("factory verifier: the factory refused the verdict", "status", status, "reason", reason, "alert", rec.Alert)
		if outcome == "window_closed" {
			v.reply(ctx, rec, "The factory's window for a verdict closed.")
		}
	default:
		v.log().Warn("factory verifier: unexpected answer; retrying next tick", "status", status, "reason", reason)
	}
}

// evidence is the re-investigation's text, redacted and cut to the factory's ceiling; when it
// cannot run, a fixed line with the error's class (never its text).
func (v *Verifier) evidence(ctx context.Context, rec Record) string {
	if v.Reinvestigate == nil {
		return "re-investigation unavailable: not configured"
	}
	rctx, cancel := context.WithTimeout(ctx, reinvestigateTimeout)
	defer cancel()
	text, err := v.Reinvestigate(rctx, rec)
	if err != nil {
		class := "error"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			class = "timeout"
		case errors.Is(err, context.Canceled):
			class = "canceled"
		}
		v.log().Warn("factory verifier: re-investigation failed", "alert", rec.Alert, "err", err)
		return "re-investigation unavailable: " + class
	}
	return truncate(redact.Secrets(text), maxEvidence)
}

func (v *Verifier) reply(ctx context.Context, rec Record, line string) {
	if v.Replier == nil || rec.SlackRoot == "" {
		return
	}
	if err := v.Replier.ReplyInThread(ctx, rec.SlackRoot, rec.SlackChannel, line); err != nil {
		v.log().Warn("factory verifier: the thread post failed", "err", err)
	}
}

// truncate cuts s to at most n bytes without splitting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// soakText renders a soak as "30m", "1h" or "1h30m".
func soakText(d time.Duration) string {
	s := d.Round(time.Minute).String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
