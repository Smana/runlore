// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGenerationOutcome(t *testing.T) {
	for name, c := range map[string]struct {
		g    Generation
		want string
	}{
		"resolved and healthy":        {Generation{Verdict: "resolved", Monitor: "healthy"}, "held"},
		"resolved and inconclusive":   {Generation{Verdict: "resolved", Monitor: "inconclusive"}, ""},
		"resolved, no monitor yet":    {Generation{Verdict: "resolved"}, ""},
		"persisting":                  {Generation{Verdict: "persisting"}, "failed"},
		"resolved and regression":     {Generation{Verdict: "resolved", Monitor: "regression"}, "failed"},
		"resolved, healthy, reverted": {Generation{Verdict: "resolved", Monitor: "healthy", Reverted: true}, "failed"},
		"a revert before any verdict": {Generation{Reverted: true}, "failed"},
		"a healthy monitor, no soak":  {Generation{Monitor: "healthy"}, ""},
		"nothing":                     {Generation{}, ""},
	} {
		if got := c.g.Outcome(); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestRecordGeneration(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	r := Record{Room: "abcdefgh", FixRoom: "fix00001", Repo: "o/r", PR: 7, MergedAt: at, MonitorAt: at, Verdict: "resolved",
		Monitor: "healthy", Evidence: "e", Observed: "held", Resolved: "held", Reverted: true}
	want := Generation{Room: "abcdefgh", FixRoom: "fix00001", Repo: "o/r", PR: 7, MergedAt: at, MonitorAt: at, Verdict: "resolved",
		Monitor: "healthy", Evidence: "e", Observed: "held", Resolved: "held", Reverted: true}
	if got := r.Generation(); got != want {
		t.Fatalf("%+v", got)
	}
}

func TestEvidenceLine(t *testing.T) {
	if got := EvidenceLine(true, false, "line1\n\nline2 <!-- x -->"); got != "firing at merge: true, firing now: false. line1 line2 x" {
		t.Fatalf("%q", got)
	}
	if got := EvidenceLine(false, true, ""); got != "firing at merge: false, firing now: true." {
		t.Fatalf("%q", got)
	}
}

type monitorRig struct {
	*webhookRig
	n int
}

func (m *monitorRig) send(t *testing.T, body string) {
	t.Helper()
	m.n++
	id := "mon" + string(rune('a'+m.n))
	if got := m.post(sign(m.key, id, m.g(), []byte(body)), body); got != http.StatusNoContent {
		t.Fatalf("%s: %d", body, got)
	}
}

func (m *monitorRig) g() time.Time { return m.now }

func newMonitorRig(t *testing.T) *monitorRig {
	g := newRig(t)
	m := &monitorRig{webhookRig: g}
	m.send(t, `{"type":"room.pr_opened","room":"fix00001","pr":7,"origin":"abcdefgh"}`)
	g.rep.got = nil
	return m
}

func (m *monitorRig) rec() Record { r, _ := m.l.Get("k1"); return r }

func TestReceiverMonitorVerdict(t *testing.T) {
	t.Run("healthy on the fix room", func(t *testing.T) {
		m := newMonitorRig(t)
		m.send(t, `{"type":"room.monitor_verdict","room":"fix00001","pr":7,"origin":"abcdefgh","verdict":"healthy"}`)
		r := m.rec()
		if r.Monitor != "healthy" || !r.MonitorAt.Equal(m.now) {
			t.Fatalf("%+v", r)
		}
		if len(m.rep.got) != 1 || !strings.HasSuffix(m.rep.got[0], "The factory's monitor read the fix #7: `healthy`.") {
			t.Fatalf("%v", m.rep.got)
		}
	})
	t.Run("an unknown verdict is not recorded", func(t *testing.T) {
		m := newMonitorRig(t)
		m.send(t, `{"type":"room.monitor_verdict","room":"fix00001","pr":7,"origin":"abcdefgh","verdict":"excellent"}`)
		if r := m.rec(); r.Monitor != "" || !r.MonitorAt.IsZero() || len(m.rep.got) != 0 {
			t.Fatalf("%+v %v", r, m.rep.got)
		}
	})
	t.Run("on the task room", func(t *testing.T) {
		m := newMonitorRig(t)
		m.send(t, `{"type":"room.monitor_verdict","room":"abcdefgh","verdict":"healthy"}`)
		if r := m.rec(); r.Monitor != "" || len(m.rep.got) != 0 {
			t.Fatalf("%+v %v", r, m.rep.got)
		}
	})
	t.Run("before the merge", func(t *testing.T) {
		m := newMonitorRig(t)
		m.send(t, `{"type":"room.monitor_verdict","room":"fix00001","pr":7,"origin":"abcdefgh","verdict":"healthy"}`)
		r := m.rec()
		if r.Monitor != "healthy" || !r.MergedAt.IsZero() || r.Generation().Outcome() != "" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a revert on the fix room", func(t *testing.T) {
		m := newMonitorRig(t)
		m.send(t, `{"type":"room.reverted","room":"fix00001","pr":7,"origin":"abcdefgh"}`)
		if r := m.rec(); !r.Reverted {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a revert on the task room is no revert of the fix", func(t *testing.T) {
		m := newMonitorRig(t)
		m.send(t, `{"type":"room.reverted","room":"abcdefgh","pr":7}`)
		if r := m.rec(); r.Reverted {
			t.Fatalf("%+v", r)
		}
	})
}

func TestReceiverRepo(t *testing.T) {
	m := newMonitorRig(t)
	m.send(t, `{"type":"room.task_received","room":"abcdefgh","repo":"Smana/cloud-native-ref"}`)
	if r := m.rec(); r.Repo != "Smana/cloud-native-ref" {
		t.Fatalf("task_received: %q", r.Repo)
	}
	m.send(t, `{"type":"room.merged","room":"fix00001","pr":7,"origin":"abcdefgh"}`)
	if r := m.rec(); r.Repo != "Smana/cloud-native-ref" || r.MergedAt.IsZero() {
		t.Fatalf("a merge without repo leaves it: %+v", r)
	}
	m.send(t, `{"type":"room.monitor_verdict","room":"fix00001","repo":"Smana/other","verdict":"healthy"}`)
	if r := m.rec(); r.Repo != "Smana/other" {
		t.Fatalf("monitor_verdict: %q", r.Repo)
	}
	for _, bad := range []string{"nope", "a/b/c", "../x", "a/..", "a b/c", "/r", "o/", "o/r\\n<!--", strings.Repeat("a", 200) + "/r"} {
		m.send(t, `{"type":"room.task_received","room":"abcdefgh","repo":"`+bad+`"}`)
		if r := m.rec(); r.Repo != "Smana/other" {
			t.Fatalf("%q was recorded: %q", bad, r.Repo)
		}
	}
}

// archivedRig: generation 1 (fix00001) archived, generation 2 current (task room, fix room fix00002 adopted).
func archivedRig(t *testing.T) *monitorRig {
	m := newMonitorRig(t)
	must(t, m.l.Upsert("k1", func(r *Record) {
		r.Curation = &Hold{Generations: []Generation{{Room: "abcdefgh", FixRoom: "fix00001", PR: 7, Verdict: "resolved"}}}
		r.Room, r.FixRoom, r.PR, r.Verdict = "task0002", "", 0, ""
	}))
	m.send(t, `{"type":"room.pr_opened","room":"fix00002","pr":9,"origin":"task0002"}`)
	m.rep.got = nil
	m.logs.Reset()
	return m
}

func TestReceiverLateEventOnAnArchivedGeneration(t *testing.T) {
	t.Run("revert", func(t *testing.T) {
		m := archivedRig(t)
		before := m.rec()
		m.send(t, `{"type":"room.reverted","room":"fix00001","pr":7,"origin":"abcdefgh"}`)
		r := m.rec()
		if !r.Curation.Generations[0].Reverted || r.Reverted || r.FixRoom != "fix00002" || r.PR != before.PR || !r.MergedAt.IsZero() ||
			!r.SoakUntil.IsZero() || len(m.rep.got) != 0 {
			t.Fatalf("%+v %v", r, m.rep.got)
		}
		if n := strings.Count(m.logs.String(), "level=INFO"); n != 1 {
			t.Fatalf("%d Info lines: %s", n, m.logs)
		}
	})
	t.Run("regression", func(t *testing.T) {
		m := archivedRig(t)
		m.send(t, `{"type":"room.monitor_verdict","room":"fix00001","pr":7,"repo":"o/r","verdict":"regression"}`)
		r := m.rec()
		g := r.Curation.Generations[0]
		if g.Monitor != "regression" || !g.MonitorAt.Equal(m.now) || g.Repo != "o/r" || r.Monitor != "" || len(m.rep.got) != 0 || !r.SoakUntil.IsZero() {
			t.Fatalf("%+v %v", r, m.rep.got)
		}
		if g.Outcome() != "failed" {
			t.Fatal(g)
		}
	})
	t.Run("an unknown verdict is ignored", func(t *testing.T) {
		m := archivedRig(t)
		m.send(t, `{"type":"room.monitor_verdict","room":"fix00001","verdict":"excellent"}`)
		if g := m.rec().Curation.Generations[0]; g.Monitor != "" {
			t.Fatalf("%+v", g)
		}
	})
	t.Run("a merge is ignored and logged", func(t *testing.T) {
		m := archivedRig(t)
		m.rc.OnMerged = func(context.Context, string) { t.Error("an archived merge starts no soak") }
		m.send(t, `{"type":"room.merged","room":"fix00001","pr":7,"origin":"abcdefgh"}`)
		r := m.rec()
		if !r.MergedAt.IsZero() || !r.Curation.Generations[0].MergedAt.IsZero() || len(m.rep.got) != 0 || !strings.Contains(m.logs.String(), "archived") {
			t.Fatalf("%+v %s", r, m.logs)
		}
	})
	t.Run("a second fix room is still dropped", func(t *testing.T) {
		m := archivedRig(t)
		m.send(t, `{"type":"room.pr_opened","room":"fix00003","pr":11,"origin":"task0002"}`)
		if r := m.rec(); r.FixRoom != "fix00002" || r.PR != 9 {
			t.Fatalf("%+v", r)
		}
	})
}

func TestLedgerCopiesAreIsolated(t *testing.T) {
	m := newMonitorRig(t)
	must(t, m.l.Upsert("k1", func(r *Record) {
		r.Curation = &Hold{Generations: []Generation{{FixRoom: "fix00001"}}, Investigation: []byte(`{"a":1}`)}
	}))
	got, _ := m.l.Get("k1")
	got.Curation.Generations[0].Reverted = true
	got.Curation.Investigation[0] = 'X'
	if r, _ := m.l.Get("k1"); r.Curation.Generations[0].Reverted || string(r.Curation.Investigation) != `{"a":1}` {
		t.Fatal("Get returns a clone")
	}
	by, _ := m.l.ByRoom("fix00001")
	by.Curation.Generations[0].Reverted = true
	if r, _ := m.l.Get("k1"); r.Curation.Generations[0].Reverted {
		t.Fatal("ByRoom returns a clone")
	}
	for _, h := range m.l.Held() {
		h.Curation.Generations[0].Reverted = true
	}
	if r, _ := m.l.Get("k1"); r.Curation.Generations[0].Reverted {
		t.Fatal("Held returns clones")
	}
	// fn failing to persist leaves the stored hold untouched.
	m.l.path = "/nonexistent-dir/x.jsonl"
	err := m.l.Upsert("k1", func(r *Record) {
		r.Curation.Generations = append(r.Curation.Generations, Generation{FixRoom: "x"})
		r.Curation.Generations[0].Reverted = true
	})
	if err == nil {
		t.Fatal("the append must fail")
	}
	if r, _ := m.l.Get("k1"); len(r.Curation.Generations) != 1 || r.Curation.Generations[0].Reverted {
		t.Fatalf("a failed append changed memory: %+v", r.Curation)
	}
}

func TestLedgerHeldRace(t *testing.T) {
	m := newMonitorRig(t)
	must(t, m.l.Upsert("k1", func(r *Record) { r.Curation = &Hold{Generations: []Generation{{FixRoom: "fix00001"}}} }))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			for _, h := range m.l.Held() {
				_ = h.Curation.Generations[0].Reverted
				h.Curation.Generations[0].Monitor = "x"
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			_ = m.l.Upsert("k1", func(r *Record) { r.Curation.Generations[0].Reverted = !r.Curation.Generations[0].Reverted })
		}
	}()
	wg.Wait()
}

func TestReceiverRefusesWithoutChangingTheRecord(t *testing.T) {
	for name, body := range map[string]string{
		"an out-of-set verdict from an unknown room":      `{"type":"room.monitor_verdict","room":"evil0001","pr":99,"repo":"o/r","origin":"task0002","verdict":"excellent"}`,
		"an out-of-set verdict on the known fix room":     `{"type":"room.monitor_verdict","room":"fix00002","pr":99,"repo":"o/r","origin":"task0002","verdict":"excellent"}`,
		"a new room whose origin is an archived fix room": `{"type":"room.pr_opened","room":"other001","pr":99,"repo":"o/r","origin":"fix00001"}`,
	} {
		t.Run(name, func(t *testing.T) {
			m := archivedRig(t)
			if strings.Contains(name, "unknown room") || strings.Contains(name, "new room") {
				must(t, m.l.Upsert("k1", func(r *Record) { r.FixRoom, r.PR = "", 0 })) // the current generation has no fix yet
			}
			before, _ := json.Marshal(m.rec())
			m.send(t, body)
			if after, _ := json.Marshal(m.rec()); !bytes.Equal(before, after) || len(m.rep.got) != 0 {
				t.Fatalf("before %s\nafter  %s", before, after)
			}
		})
	}
	t.Run("a direct event on the archived fix room still reaches it", func(t *testing.T) {
		m := archivedRig(t)
		m.send(t, `{"type":"room.reverted","room":"fix00001","pr":7}`)
		if !m.rec().Curation.Generations[0].Reverted {
			t.Fatal("not reverted")
		}
	})
}
