// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type verifierRig struct {
	t      *testing.T
	now    time.Time
	path   string
	ledger *Ledger
	vs     *verifyServer
	rep    *replies
	v      *Verifier
	firing bool
	amErr  error
	reErr  error
	reText string
	reRuns int
}

const soak = 30 * time.Minute

func newVerifierRig(t *testing.T, script ...int) *verifierRig {
	r := &verifierRig{t: t, now: time.Unix(1_800_000_000, 0), vs: &verifyServer{script: script}, rep: &replies{}, reText: "pods are stable; the rollout fixed the crash loop"}
	r.path = filepath.Join(t.TempDir(), "factory.jsonl")
	r.ledger = r.open()
	r.v = r.verifier(r.ledger)
	_ = r.ledger.Upsert("k1", func(rec *Record) {
		rec.Alert, rec.Namespace, rec.Workload = "KubePodCrashLooping", "apps", "Deployment/web"
		rec.Room, rec.FixRoom, rec.PR = "abcdefgh", "fixt0001", 77
		rec.SlackRoot, rec.SlackChannel = "1729.0001", "C01"
		rec.MergedAt = r.now
	})
	return r
}

func (r *verifierRig) open() *Ledger {
	l, err := OpenLedger(r.path, func() time.Time { return r.now })
	if err != nil {
		r.t.Fatal(err)
	}
	return l
}

func (r *verifierRig) verifier(l *Ledger) *Verifier {
	return &Verifier{
		Ledger: l, Client: r.vs.client(r.t, &r.now), Replier: r.rep, Soak: soak,
		Now: func() time.Time { return r.now },
		Firing: func(context.Context, string, string) (bool, error) {
			return r.firing, r.amErr
		},
		Reinvestigate: func(context.Context, Record) (string, error) {
			r.reRuns++
			return r.reText, r.reErr
		},
	}
}

func (r *verifierRig) lines() []string {
	var out []string
	for _, g := range r.rep.got {
		out = append(out, g[strings.LastIndex(g, "|")+1:])
	}
	return out
}

func (r *verifierRig) posted() []Verification {
	var out []Verification
	for _, b := range r.vs.bodies {
		var v Verification
		if err := json.Unmarshal([]byte(b), &v); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestVerifier(t *testing.T) {
	ctx := context.Background()

	t.Run("OnMerged records the firing state and the deadline", func(t *testing.T) {
		r := newVerifierRig(t)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		rec, _ := r.ledger.Get("k1")
		if !rec.FiringAtMerge || !rec.SoakUntil.Equal(r.now.Add(soak)) {
			t.Fatalf("%+v", rec)
		}
		if got := r.lines(); len(got) != 1 || got[0] != "Soaking for 30m before checking `KubePodCrashLooping`." {
			t.Fatalf("%q", got)
		}
	})

	t.Run("before the soak ends nothing happens", func(t *testing.T) {
		r := newVerifierRig(t)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		r.now = r.now.Add(soak - time.Second)
		r.v.Once(ctx)
		if len(r.vs.bodies) != 0 || r.reRuns != 0 || len(r.lines()) != 1 {
			t.Fatalf("posted %d, ran %d, lines %q", len(r.vs.bodies), r.reRuns, r.lines())
		}
	})

	t.Run("the alert is gone: resolved", func(t *testing.T) {
		r := newVerifierRig(t, 201)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		r.firing = false
		r.now = r.now.Add(soak)
		r.v.Once(ctx)
		got := r.posted()
		if len(got) != 1 || got[0].Verdict != "resolved" || !got[0].Alert.FiringAtMerge || got[0].Alert.FiringNow ||
			got[0].Alert.Name != "KubePodCrashLooping" || got[0].Evidence != r.reText || !got[0].CheckedAt.Equal(r.now.UTC()) {
			t.Fatalf("%+v", got)
		}
		if r.vs.paths[0] != "/v1/tasks/fixt0001/verification" {
			t.Fatal(r.vs.paths)
		}
		rec, _ := r.ledger.Get("k1")
		if rec.Verdict != "resolved" {
			t.Fatalf("%+v", rec)
		}
		if l := r.lines(); l[len(l)-1] != "The alert is gone 30m after the fix: resolved." {
			t.Fatalf("%q", l)
		}
		r.v.Once(ctx) // never due again
		if len(r.vs.bodies) != 1 {
			t.Fatal("a verdict is posted once")
		}
	})

	t.Run("the alert still fires: persisting", func(t *testing.T) {
		r := newVerifierRig(t, 201)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		r.now = r.now.Add(soak)
		r.v.Once(ctx)
		got := r.posted()
		if len(got) != 1 || got[0].Verdict != "persisting" || !got[0].Alert.FiringNow {
			t.Fatalf("%+v", got)
		}
		if l := r.lines(); l[len(l)-1] != "The alert still fires 30m after the fix: persisting. The factory opens a follow-up." {
			t.Fatalf("%q", l)
		}
	})

	t.Run("Alertmanager unreachable posts nothing and stays due", func(t *testing.T) {
		r := newVerifierRig(t, 201)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		r.now = r.now.Add(soak)
		r.amErr = errors.New("connection refused")
		r.v.Once(ctx)
		if len(r.vs.bodies) != 0 || r.reRuns != 0 || len(r.lines()) != 1 || len(r.ledger.Due(r.now)) != 1 {
			t.Fatalf("posted %d, lines %q, due %d", len(r.vs.bodies), r.lines(), len(r.ledger.Due(r.now)))
		}
		r.amErr = nil
		r.now = r.now.Add(time.Minute)
		r.v.Once(ctx) // retried on the next tick
		if len(r.vs.bodies) != 1 {
			t.Fatal("the next tick posts")
		}
	})

	t.Run("window_closed is terminal", func(t *testing.T) {
		r := newVerifierRig(t, 409)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		r.now = r.now.Add(soak)
		r.v.Once(ctx)
		rec, _ := r.ledger.Get("k1")
		if rec.Outcome != "window_closed" || rec.Verdict != "" || len(r.ledger.Due(r.now.Add(time.Hour))) != 0 {
			t.Fatalf("%+v", rec)
		}
		if l := r.lines(); l[len(l)-1] != "The factory's window for a verdict closed." {
			t.Fatalf("%q", l)
		}
	})

	t.Run("a factory outage keeps it due", func(t *testing.T) {
		r := newVerifierRig(t, 503, 503, 503, 201)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		r.now = r.now.Add(soak)
		r.v.Once(ctx)
		if rec, _ := r.ledger.Get("k1"); rec.Verdict != "" || rec.Outcome != "" || len(r.lines()) != 1 {
			t.Fatalf("%+v %q", rec, r.lines())
		}
		r.v.Once(ctx)
		if rec, _ := r.ledger.Get("k1"); rec.Verdict != "persisting" {
			t.Fatalf("%+v", rec)
		}
	})

	t.Run("a new leader resumes from the file and posts once", func(t *testing.T) {
		r := newVerifierRig(t, 201, 201)
		r.firing = true
		r.v.OnMerged(ctx, "k1") // the old leader records the deadline, then dies
		r.now = r.now.Add(soak + time.Minute)
		l2 := r.open()
		if err := l2.Reload(); err != nil {
			t.Fatal(err)
		}
		v2 := r.verifier(l2)
		v2.Once(ctx)
		v2.Once(ctx)
		if len(r.vs.bodies) != 1 {
			t.Fatalf("posted %d times", len(r.vs.bodies))
		}
		if rec, _ := l2.Get("k1"); rec.Verdict != "persisting" {
			t.Fatalf("%+v", rec)
		}
	})

	t.Run("a failed re-investigation still posts the verdict", func(t *testing.T) {
		r := newVerifierRig(t, 201)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		r.now = r.now.Add(soak)
		r.reErr, r.reText = context.DeadlineExceeded, ""
		r.v.Once(ctx)
		got := r.posted()
		if len(got) != 1 || got[0].Evidence != "re-investigation unavailable: timeout" {
			t.Fatalf("%+v", got)
		}
	})

	t.Run("evidence is redacted and capped", func(t *testing.T) {
		r := newVerifierRig(t, 201)
		r.firing = true
		r.v.OnMerged(ctx, "k1")
		r.now = r.now.Add(soak)
		r.reText = "token ghp_" + strings.Repeat("a", 36) + "\n" + strings.Repeat("é", 20<<10)
		r.v.Once(ctx)
		ev := r.posted()[0].Evidence
		if strings.Contains(ev, "ghp_aaaa") || len(ev) > 16<<10 || !strings.HasPrefix(ev, "token ") {
			t.Fatalf("len %d, %q", len(ev), ev[:60])
		}
	})
}

func TestSoakText(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Minute: "30m", time.Hour: "1h", 90 * time.Minute: "1h30m", 5 * time.Minute: "5m"} {
		if got := soakText(d); got != want {
			t.Fatalf("%v: %q, want %q", d, got, want)
		}
	}
}
