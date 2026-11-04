// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLedgerSurvivesAReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "factory.jsonl")
	now := time.Unix(1_800_000_000, 0)
	l, err := OpenLedger(p, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	must(t, l.Upsert("k1", func(r *Record) { r.Task, r.Room, r.Issue = "abcdefgh", "abcdefgh", 42 }))
	l.Register("slack", "1729.0001", "C01", providers.Investigation{TriggerKey: "k1"})
	must(t, l.Upsert("k1", func(r *Record) {
		r.FixRoom, r.PR, r.MergedAt, r.SoakUntil = "fix00001", 7, now, now.Add(30*time.Minute)
	}))
	again, err := OpenLedger(p, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	r, ok := again.ByRoom("fix00001")
	if !ok || r.TriggerKey != "k1" || r.Issue != 42 || r.SlackRoot != "1729.0001" || r.PR != 7 {
		t.Fatalf("%+v", r)
	}
	if got := again.Due(now.Add(29 * time.Minute)); len(got) != 0 {
		t.Fatal("not due before the soak ends")
	}
	if got := again.Due(now.Add(30 * time.Minute)); len(got) != 1 {
		t.Fatal("due once it ends")
	}
}

func TestLedgerSeenWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), func() time.Time { return now })
	if seen, _ := l.Seen("evt_a", now); seen {
		t.Fatal("first sight")
	}
	if seen, _ := l.Seen("evt_a", now.Add(time.Minute)); !seen {
		t.Fatal("a replay within the window")
	}
	if seen, _ := l.Seen("evt_a", now.Add(11*time.Minute)); seen {
		t.Fatal("forgotten after 10 minutes; the timestamp check refuses it anyway")
	}
}

func TestLedgerDisabledWithoutAPath(t *testing.T) {
	l, err := OpenLedger("", time.Now)
	if err != nil || l.Upsert("k", func(*Record) {}) != nil {
		t.Fatal("an empty path is a no-op ledger")
	}
}

func TestLedgerSeenSurvivesAReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.jsonl")
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	l, _ := OpenLedger(p, clock)
	_, _ = l.Seen("evt_a", now)
	_, _ = l.Seen("evt_old", now.Add(-time.Hour))
	again, _ := OpenLedger(p, clock)
	if seen, _ := again.Seen("evt_a", now); !seen {
		t.Fatal("a recent id survives a restart, so a replay after a failover is dropped")
	}
	if seen, _ := again.Seen("evt_old", now); seen {
		t.Fatal("an id past the window is not replayed")
	}
}

func TestLedgerTornLastLineIsSkipped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.jsonl")
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	l, _ := OpenLedger(p, clock)
	must(t, l.Upsert("k1", func(r *Record) { r.Task = "abcdefgh" }))
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	must(t, err)
	_, _ = f.WriteString(`{"record":{"trigger_key":"k2","ta`) // a crash mid-write
	must(t, f.Close())
	again, err := OpenLedger(p, clock)
	if err != nil {
		t.Fatalf("a torn line is a warning, not an error: %v", err)
	}
	if r, ok := again.Get("k1"); !ok || r.Task != "abcdefgh" {
		t.Fatalf("the intact lines survive: %+v", r)
	}
	must(t, again.Upsert("k3", func(r *Record) { r.Task = "zzzzzzzz" })) // the next append must not fuse with the torn bytes
	third, _ := OpenLedger(p, clock)
	if _, ok := third.Get("k3"); !ok {
		t.Fatal("an append after a torn line is readable")
	}
}

// poisoned writes seven holds and one line of size over the limit, second in the file.
func poisoned(t *testing.T, p string, size int) []string {
	t.Helper()
	var lines, keys []string
	for i := range 7 {
		key := fmt.Sprintf("k%d", i)
		keys = append(keys, key)
		b, err := json.Marshal(line{Record: &Record{TriggerKey: key, Curation: &Hold{Fingerprint: "fp" + key}}})
		must(t, err)
		lines = append(lines, string(b))
	}
	poison := `{"record":{"trigger_key":"poison","task":"` + strings.Repeat("x", size) + `"}}`
	lines = slices.Insert(lines, 1, poison)
	must(t, os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return keys
}

func assertHolds(t *testing.T, l *Ledger, keys []string) {
	t.Helper()
	held := l.Held()
	if len(held) != len(keys) {
		t.Fatalf("%d holds, want %d", len(held), len(keys))
	}
	for i, r := range held {
		if r.TriggerKey != keys[i] || r.Curation.Fingerprint != "fp"+keys[i] {
			t.Fatalf("%+v", r)
		}
	}
}

func TestLedgerAnOversizedLineIsTorn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.jsonl")
	now := func() time.Time { return time.Unix(1_800_000_000, 0) }
	l, err := OpenLedger(p, now)
	must(t, err)
	keys := poisoned(t, p, 5<<20)

	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	must(t, l.Reload())
	assertHolds(t, l, keys)
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("the skipped line is logged: %q", logs.String())
	}
	if _, ok := l.Get("poison"); ok {
		t.Fatal("the oversized line is skipped")
	}

	// A start reads it too, and the compaction has made the skip durable.
	again, err := OpenLedger(p, now)
	must(t, err)
	assertHolds(t, again, keys)
	for range 70 {
		must(t, again.Upsert("k0", func(r *Record) { r.Task = "abcdefgh" }))
	}
	third, err := OpenLedger(p, now)
	must(t, err)
	assertHolds(t, third, keys)
}

func TestLedgerAnOversizedLineStopsNoStart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.jsonl")
	keys := poisoned(t, p, 5<<20)
	l, err := OpenLedger(p, func() time.Time { return time.Unix(1_800_000_000, 0) })
	if err != nil {
		t.Fatalf("one line does not stop serve: %v", err)
	}
	assertHolds(t, l, keys)
}

func TestLedgerALineAtTheLimitIsRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.jsonl")
	keys := poisoned(t, p, maxLineBytes-1024)
	l, err := OpenLedger(p, func() time.Time { return time.Unix(1_800_000_000, 0) })
	must(t, err)
	if _, ok := l.Get("poison"); !ok || len(l.Held()) != len(keys) {
		t.Fatalf("a big line under the limit is an ordinary line: %d holds", len(l.Held()))
	}
}

func TestLedgerAFailedReloadKeepsThePriorState(t *testing.T) {
	for name, breakIt := range map[string]func(t *testing.T, p string){
		"a read error": func(t *testing.T, p string) { // reading a directory fails after the open
			t.Helper()
			must(t, os.Remove(p))
			must(t, os.Mkdir(p, 0o700))
		},
		"an open error": func(t *testing.T, p string) {
			t.Helper()
			if os.Geteuid() == 0 {
				t.Skip("root opens any file")
			}
			must(t, os.Chmod(p, 0))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "f.jsonl")
			at := time.Unix(1_800_000_000, 0)
			l, err := OpenLedger(p, func() time.Time { return at })
			must(t, err)
			keys := []string{"k0", "k1"}
			for _, k := range keys {
				must(t, l.Upsert(k, func(r *Record) { r.Curation = &Hold{Fingerprint: "fp" + k} }))
			}
			must(t, l.MarkSeen("evt1", at))
			breakIt(t, p)
			if err := l.Reload(); err == nil {
				t.Fatal("the reload failed")
			}
			assertHolds(t, l, keys)
			if !l.HasSeen("evt1", at) {
				t.Fatal("a failed reload keeps the seen ids")
			}
		})
	}
}

func TestLedgerCompactsAndKeepsTheLastRecord(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.jsonl")
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	l, _ := OpenLedger(p, clock)
	for i := range 200 {
		must(t, l.Upsert("k1", func(r *Record) { r.PR = i }))
	}
	b, _ := os.ReadFile(p)
	if n := strings.Count(string(b), "\n"); n >= 200 {
		t.Fatalf("expected compaction, the file holds %d lines", n)
	}
	again, _ := OpenLedger(p, clock)
	if r, _ := again.Get("k1"); r.PR != 199 {
		t.Fatalf("the last record wins: %+v", r)
	}
}

func TestLedgerEventsKeepTheLast20(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), time.Now)
	for i := range 30 {
		must(t, l.Upsert("k1", func(r *Record) { r.Events = append(r.Events, string(rune('a'+i%26))) }))
	}
	if r, _ := l.Get("k1"); len(r.Events) != 20 {
		t.Fatalf("%d events", len(r.Events))
	}
}

func TestRegisterIgnoresOtherTransportsAndEmptyKeys(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), time.Now)
	l.Register("matrix", "r", "c", providers.Investigation{TriggerKey: "k1"})
	l.Register("slack", "r", "c", providers.Investigation{})
	if _, ok := l.Get("k1"); ok {
		t.Fatal("matrix is not recorded")
	}
	if _, ok := l.Get(""); ok {
		t.Fatal("an empty key is not recorded")
	}
}

func TestLedgerCompactionFailureDoesNotLoseTheWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not bind root")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "f.jsonl")
	now := time.Unix(1_800_000_000, 0)
	l, _ := OpenLedger(p, func() time.Time { return now })
	must(t, l.Upsert("k1", func(r *Record) { r.Room = "abcdefgh" }))
	must(t, os.Chmod(dir, 0o500)) // appending to the file works; creating the compaction temp file does not
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	var merged error
	for i := range 100 {
		merged = l.Upsert("k1", func(r *Record) { r.PR = i; r.MergedAt = now })
		if merged != nil {
			break
		}
	}
	if merged != nil {
		t.Fatalf("the line is on disk, so a failed compaction is not a failed write: %v", merged)
	}
	again, _ := OpenLedger(p, func() time.Time { return now })
	if r, _ := again.Get("k1"); r.PR != 99 || r.MergedAt.IsZero() {
		t.Fatalf("memory and disk agree: %+v", r)
	}
}
