// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
	"github.com/Smana/runlore/internal/telemetry"
)

// meteredInstruments installs a REAL SDK meter provider backed by a manual
// reader and returns the instrument set bound to it, plus a reader that sums
// an int64 counter by its exported series name (0, false when the series was
// never recorded). The provider is global, so a test using this must not run
// in parallel with another that does; the cleanup restores the no-op
// provider. Mirrors internal/server/events_test.go's helper of the same name.
func meteredInstruments(t *testing.T) (*telemetry.Metrics, func(series string) (int64, bool)) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return telemetry.NewMetrics(), func(series string) (int64, bool) {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != series {
					continue
				}
				sum, ok := md.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("series %q is not an int64 sum (%T)", series, md.Data)
				}
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				return total, true
			}
		}
		return 0, false
	}
}

type fakeForge struct {
	// mu guards every field below. Every test using fakeForge so far has been
	// single-goroutine, so this is a no-op for them; concurrency tests (see
	// TestMentionConcurrentFirstMessagesRehydrateRegistryOnceAndCountEveryWrite)
	// drive HandleMention from real goroutines, and without this lock two
	// goroutines appending to the same slice concurrently is itself a data
	// race the -race detector would catch, independent of anything under test.
	mu       sync.Mutex
	comments []struct {
		number int
		body   string
	}
	opened  []providers.KBEntry
	openURL string
	openErr error
	commErr error
	// prOpen reports the open state IsPROpen returns for a given PR number.
	// A number absent from the map defaults to true (open) so every existing
	// test — none of which sets prOpen — keeps exercising the "comment on the
	// open PR" path unmodified.
	prOpen map[int]bool
	// prOpenErr, when set, makes IsPROpen fail for every number — used to pin
	// the open-check error-path behaviour.
	prOpenErr error
	// prOpenCalls records every number IsPROpen was asked about, in order, so
	// a test can pin that the check runs before a comment is ever posted.
	prOpenCalls []int
	// entered/proceed are an optional rendezvous used only by the
	// different-roots concurrency test: when entered is non-nil, OpenPR
	// signals its arrival on entered and then blocks on proceed, so the test
	// can prove two OpenPR calls for two different roots are inside the
	// forge call AT THE SAME TIME — something a per-root guard must allow,
	// unlike two calls for the SAME root.
	entered chan struct{}
	proceed chan struct{}
}

func (f *fakeForge) IsPROpen(_ context.Context, number int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prOpenCalls = append(f.prOpenCalls, number)
	if f.prOpenErr != nil {
		return false, f.prOpenErr
	}
	if open, ok := f.prOpen[number]; ok {
		return open, nil
	}
	return true, nil
}

func (f *fakeForge) CommentOnPR(_ context.Context, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commErr != nil {
		return f.commErr
	}
	f.comments = append(f.comments, struct {
		number int
		body   string
	}{number, body})
	return nil
}

func (f *fakeForge) OpenPR(_ context.Context, e providers.KBEntry) (providers.Ref, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.proceed
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return providers.Ref{}, f.openErr
	}
	f.opened = append(f.opened, e)
	url := f.openURL
	if url == "" {
		url = "https://github.com/o/r/pull/99"
	}
	return providers.Ref{URL: url}, nil
}

// counts returns the number of comments and PRs opened so far, taken under
// the lock so a concurrency test can read them safely.
func (f *fakeForge) counts() (comments, opened int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments), len(f.opened)
}

func newTestResponder(t *testing.T, f *fakeForge) *Responder {
	t.Helper()
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return &Responder{
		Forge:             f,
		Registry:          reg,
		MaxNotesPerThread: 3,
		ForgeWrites:       ratelimit.New(10, time.Hour),
		Now:               func() time.Time { return noteAt },
		Log:               silentLog(),
	}
}

func TestPRNumber(t *testing.T) {
	tests := []struct {
		url  string
		want int
		ok   bool
	}{
		{"https://github.com/o/r/pull/42", 42, true},
		{"https://gitlab.com/o/r/-/merge_requests/7", 7, true},
		{"https://gitlab.example.com/grp/sub/proj/-/merge_requests/1234", 1234, true},
		{"https://github.com/o/r/pull/42#issuecomment-1", 42, true},
		{"https://github.com/o/r/issues/42", 0, false},
		{"", 0, false},
		{"not a url", 0, false},
		{"https://github.com/o/r/pull/notanumber", 0, false},
	}
	for _, tt := range tests {
		got, ok := PRNumber(tt.url)
		if got != tt.want || ok != tt.ok {
			t.Errorf("PRNumber(%q) = (%d, %v), want (%d, %v)", tt.url, got, ok, tt.want, tt.ok)
		}
	}
}

// TestResponderRefusesAPRURLOutsideTheConfiguredRepo closes the routing
// finding. PRNumber matches "/pull/<n>" ANYWHERE in a URL, and the number it
// yields is applied to the CONFIGURED repo — the only one the forge client
// knows how to address. So the number has to come from a URL naming that
// repository, and nothing less will do:
//
//   - another host entirely was the first half of this, and the only half an
//     earlier version of the anchor enforced;
//   - the SAME host, a different repository, is the half that mattered more.
//     On github.com anybody owns a repository, so https://github.com/attacker/
//     x/pull/9999 passed the host check and posted the human's note onto an
//     unrelated pull request inside the operator's own knowledge-base repo;
//   - a "/pull/<n>" somewhere further down the path of the right repository is
//     not the pull request either — the segment has to follow the repo path.
//
// Not reachable today (the only untrusted source of a CuratedURL is the Matrix
// stamp, and contextFor discards a stamp whose sender is not the bot itself),
// which is exactly why it is worth anchoring: the defence should not depend on
// a control one layer up staying correct forever.
func TestResponderRefusesAPRURLOutsideTheConfiguredRepo(t *testing.T) {
	for _, tt := range []struct{ name, url string }{
		{"another host", "https://github.example.evil/acme/kb/pull/1337"},
		{"another repo on the same host", "https://github.com/attacker/x/pull/9999"},
		{"the configured owner, another repo", "https://github.com/acme/not-kb/pull/9999"},
		{"the configured repo as a path suffix", "https://github.com/attacker/acme/kb/pull/9999"},
		{"the configured repo as a host prefix", "https://github.com.evil/acme/kb/pull/9999"},
		{"a sibling repo sharing the configured repo's name prefix", "https://github.com/acme/kb-staging/pull/9999"},
		{"a pull segment deeper in the right repo", "https://github.com/acme/kb/blob/main/pull/9999"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForge{}
			r := newTestResponder(t, f)
			r.ForgeRepo = "github.com/acme/kb"
			tc := Context{Root: "111.222", CuratedURL: tt.url}
			if err := r.Registry.Put(tc); err != nil {
				t.Fatalf("Put: %v", err)
			}

			if _, err := r.Handle(context.Background(), tc, "alice", "note: the real cause was a spot reclaim"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(f.comments) != 0 {
				t.Errorf("commented on PR #%d taken from %s — the number must not select a pull request in the configured repo", f.comments[0].number, tt.url)
			}
			if len(f.opened) != 1 {
				t.Fatalf("opened = %d, want 1 — a refused URL must fall through to the standalone route, never drop the note", len(f.opened))
			}
		})
	}
}

// TestResponderRoutesOnTheConfiguredRepo is the positive control: the anchor
// must not break the ordinary case it guards, including the ports, the
// letter-casing and the nested group paths real instances actually use.
func TestResponderRoutesOnTheConfiguredRepo(t *testing.T) {
	for _, tt := range []struct{ repo, url string }{
		{"github.com/o/r", "https://github.com/o/r/pull/42"},
		{"github.com/o/r", "https://GitHub.com/O/R/pull/42"},
		{"ghe.example.com/o/r", "https://ghe.example.com/o/r/pull/42"},
		{"github.com/o/r", "https://github.com/o/r/pull/42#issuecomment-1"},
		{"github.com/o/r", "https://github.com/o/r/pull/42/files"},
		{"gitlab.example.com/grp/proj", "https://gitlab.example.com:8443/grp/proj/-/merge_requests/42"},
		{"gitlab.example.com/grp/sub/proj", "https://gitlab.example.com/grp/sub/proj/-/merge_requests/42"},
		{"gitlab.example.com/grp/proj", "https://gitlab.example.com/grp/proj/merge_requests/42"},
	} {
		t.Run(tt.url, func(t *testing.T) {
			f := &fakeForge{}
			r := newTestResponder(t, f)
			r.ForgeRepo = tt.repo
			tc := Context{Root: "111.222", CuratedURL: tt.url}
			if err := r.Registry.Put(tc); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(f.comments) != 1 || f.comments[0].number != 42 {
				t.Fatalf("comments = %+v, want one on #42 — the anchor must not refuse the configured repo", f.comments)
			}
		})
	}
}

// TestResponderUnsetForgeRepoRoutesExactlyAsBefore pins that the anchor is
// opt-in: a Responder built without ForgeRepo — every existing caller, and
// every test in internal/notify and internal/server — behaves byte-for-byte
// as it did before the field existed.
func TestResponderUnsetForgeRepoRoutesExactlyAsBefore(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.example.evil/attacker/repo/pull/1337"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 || f.comments[0].number != 1337 {
		t.Fatalf("comments = %+v, want one on #1337 — an unset ForgeRepo must not change routing", f.comments)
	}
}

// TestResponderRefusesEveryURLWhenForgeRepoIsMalformed pins the fail-closed
// direction for a ForgeRepo that names no repository path ("github.com", or a
// stray "/"). Set-but-unusable must refuse everything rather than silently
// degrade to the host-only check this replaced — a misconfiguration should
// cost a fallback to the standalone route, never a comment on a pull request
// chosen by a URL.
func TestResponderRefusesEveryURLWhenForgeRepoIsMalformed(t *testing.T) {
	for _, tt := range []struct{ repo, url string }{
		{"github.com", "https://github.com/acme/kb/pull/42"},
		{"/", "https://github.com/acme/kb/pull/42"},
		{"github.com/", "https://github.com/acme/kb/pull/42"},
		// An empty repository path would otherwise reduce the prefix to "/",
		// which a doubled slash then satisfies — the one input on which a
		// half-formed ForgeRepo would have yielded a number rather than a refusal.
		{"github.com", "https://github.com//pull/42"},
	} {
		t.Run(tt.repo+" "+tt.url, func(t *testing.T) {
			f := &fakeForge{}
			r := newTestResponder(t, f)
			r.ForgeRepo = tt.repo
			tc := Context{Root: "111.222", CuratedURL: tt.url}
			if err := r.Registry.Put(tc); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(f.comments) != 0 {
				t.Errorf("commented on PR #%d with a ForgeRepo that names no repository", f.comments[0].number)
			}
			if len(f.opened) != 1 {
				t.Fatalf("opened = %d, want 1 — the note must still be recorded", len(f.opened))
			}
		})
	}
}

func TestHandleNoteCommentsOnTheOpenPR(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: spot reclaim, not OOM")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if f.comments[0].number != 42 {
		t.Errorf("commented on PR %d, want 42", f.comments[0].number)
	}
	if !strings.Contains(f.comments[0].body, "spot reclaim, not OOM") {
		t.Errorf("comment lost the note text: %s", f.comments[0].body)
	}
	if len(f.opened) != 0 {
		t.Errorf("must not open a PR when one is already linked; opened %d", len(f.opened))
	}
	if !strings.Contains(reply, "42") {
		t.Errorf("reply must point at the PR it wrote to: %q", reply)
	}
}

func TestHandleNoteOpensStandalonePRWhenNoneLinked(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", RecalledEntry: "incidents/foo.md"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: stale since Karpenter")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want 1", len(f.opened))
	}
	if f.opened[0].Type != "Concept" {
		t.Errorf("Type = %q, want Concept", f.opened[0].Type)
	}
	if !strings.Contains(reply, "99") {
		t.Errorf("reply must name the PR it opened: %q", reply)
	}
	stored, ok := r.Registry.Get("111.222")
	if !ok {
		t.Fatal("registry lost the thread")
	}
	if stored.NoteURL == "" {
		t.Error("NoteURL must be written back so the next note comments instead of opening again")
	}
}

func TestHandleSecondNoteCommentsOnTheFirstNotesPR(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: first"); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	refreshed, _ := r.Registry.Get("111.222")
	if _, err := r.Handle(context.Background(), refreshed, "bob", "note: second"); err != nil {
		t.Fatalf("second Handle: %v", err)
	}

	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want exactly 1 — a thread opens at most one standalone PR", len(f.opened))
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1 (the second note)", len(f.comments))
	}
	if f.comments[0].number != 99 {
		t.Errorf("second note went to PR %d, want 99 (the first note's PR)", f.comments[0].number)
	}
}

func TestHandlePerThreadCap(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var lastReply string
	for i := 0; i < 5; i++ {
		cur, _ := r.Registry.Get("111.222")
		var err error
		lastReply, err = r.Handle(context.Background(), cur, "alice", "note: spam")
		if err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if len(f.comments) != 3 {
		t.Fatalf("comments = %d, want 3 (MaxNotesPerThread)", len(f.comments))
	}
	if !strings.Contains(strings.ToLower(lastReply), "limit") {
		t.Errorf("the capped reply must say so: %q", lastReply)
	}
}

func TestHandleOpenPRRateLimit(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.ForgeWrites = ratelimit.New(1, time.Hour)

	for _, root := range []string{"a", "b"} {
		tc := Context{Root: root}
		if err := r.Registry.Put(tc); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want 1 — the global write budget caps the second", len(f.opened))
	}
	// Thread "b" hit the global rate limit: nothing landed in the knowledge base
	// for it, so its per-thread budget must be untouched.
	throttled, ok := r.Registry.Get("b")
	if !ok {
		t.Fatal("registry lost thread b")
	}
	if throttled.Notes != 0 {
		t.Errorf("Notes = %d for the throttled thread, want 0 — a throttled write must not burn the thread's budget", throttled.Notes)
	}
}

// TestHandleGlobalRateLimitGatesCommentsToo pins the fix for the defect where
// the global hourly window only ever gated OpenPR: write() returned from the
// CommentOnPR branch before the window check was ever reached, so commenting
// on an already-linked PR was bounded only by the per-thread cap (20) times
// however many threads the registry happened to be holding (up to 2000) —
// not by the global window the design spec says exists so "a chatty channel
// cannot become a forge- or token-spend incident".
func TestHandleGlobalRateLimitGatesCommentsToo(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.ForgeWrites = ratelimit.New(1, time.Hour)

	tc1 := Context{Root: "a", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), tc1, "alice", "note: first"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	tc2 := Context{Root: "b", CuratedURL: "https://github.com/o/r/pull/77"}
	if err := r.Registry.Put(tc2); err != nil {
		t.Fatalf("Put: %v", err)
	}
	reply, err := r.Handle(context.Background(), tc2, "bob", "note: second")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1 — the global window must gate CommentOnPR too, not just OpenPR", len(f.comments))
	}
	if !strings.Contains(strings.ToLower(reply), "paused") {
		t.Errorf("the throttled reply must say so: %q", reply)
	}
	// Thread "b" was throttled, not written: its per-thread budget must be
	// untouched, exactly like the OpenPR-route throttle case above.
	throttled, ok := r.Registry.Get("b")
	if !ok {
		t.Fatal("registry lost thread b")
	}
	if throttled.Notes != 0 {
		t.Errorf("Notes = %d for the throttled thread, want 0", throttled.Notes)
	}
}

// TestHandleGlobalRateLimitIsSharedAcrossBothRoutes proves the comment route
// and the OpenPR route draw from ONE budget rather than each silently getting
// its own: a comment landing must be able to exhaust the window a subsequent
// OpenPR then hits, and vice versa.
func TestHandleGlobalRateLimitIsSharedAcrossBothRoutes(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.ForgeWrites = ratelimit.New(1, time.Hour)

	// First write takes the comment route and spends the whole shared budget.
	tc1 := Context{Root: "a", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), tc1, "alice", "note: first"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// Second write would take the OpenPR route (no linked PR at all) — but the
	// shared budget is already spent, by the FIRST write's comment.
	tc2 := Context{Root: "b"}
	if err := r.Registry.Put(tc2); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), tc2, "bob", "note: second"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(f.comments) != 1 || len(f.opened) != 0 {
		t.Fatalf("comments=%d opened=%d, want 1/0 — one shared budget across both write routes", len(f.comments), len(f.opened))
	}
}

// TestHandlePerThreadCapIndependentOfGlobalWindow pins that the per-thread cap
// (a separate, narrower control) still applies on its own even when the
// global window is generous enough never to bind — the two must not collapse
// into one check now that the global window gates both routes.
func TestHandlePerThreadCapIndependentOfGlobalWindow(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.MaxNotesPerThread = 1
	r.ForgeWrites = ratelimit.New(100, time.Hour)
	tc := Context{Root: "a", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: first"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	cur, _ := r.Registry.Get("a")
	reply, err := r.Handle(context.Background(), cur, "alice", "note: second")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1 — the per-thread cap must still bind independently of the global window", len(f.comments))
	}
	if !strings.Contains(strings.ToLower(reply), "limit") {
		t.Errorf("the capped reply must say so: %q", reply)
	}
}

// TestHandleFreeformPerformsZeroForgeCallsAndRepliesWithHowTo pins the fixed
// contract for an addressed message with no recognised prefix. This test used
// to be TestHandleFreeformIsCapturedWhenNoModelIsWired and asserted the
// OPPOSITE: that freeform text was captured into the knowledge base exactly
// like an explicit "note:". A security audit found that the wrong default —
// an on-call typing "anyone checked what runlore said about the CNI?" inside
// an investigation thread opened or commented a KB PR with no explicit intent
// to record anything, and it made the reserved reinvestigate: prefix
// pointless: a message that evaded THAT prefix match (e.g. a filler word
// ahead of it, or the word with no colon) fell through to freeform, which
// wrote anyway.
//
// Freeform must now write NOTHING — zero forge calls — and reply with a
// notice that (a) says plainly nothing was recorded and (b) shows the exact
// way to record it explicitly.
func TestHandleFreeformPerformsZeroForgeCallsAndRepliesWithHowTo(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> anyone checked what runlore said about the CNI?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatalf("freeform must write NOTHING to the knowledge base; comments=%d opened=%d", len(f.comments), len(f.opened))
	}
	if !strings.Contains(strings.ToLower(reply), "nothing") {
		t.Errorf("the reply must say plainly that nothing was recorded: %q", reply)
	}
	if !strings.Contains(reply, "note:") {
		t.Errorf("the reply must show how to record it explicitly with note\": %q", reply)
	}
	if reply != FreeformNotRecordedReply {
		t.Errorf("reply = %q, want the shared FreeformNotRecordedReply constant so every freeform message gets identical wording", reply)
	}
}

// TestHandleFreeformWithEmptyTextAlsoDoesNotWrite covers the boundary case: a
// bare mention with nothing after it ("<@U0BOT>") parses as IntentFreeform
// with empty Text (see grammar_test.go). It must not fall through to the
// "Tell me what to record" note-empty-text prompt, and — like every other
// freeform message — must never write.
func TestHandleFreeformWithEmptyTextAlsoDoesNotWrite(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT>")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatalf("an empty freeform message must write NOTHING; comments=%d opened=%d", len(f.comments), len(f.opened))
	}
	if reply != FreeformNotRecordedReply {
		t.Errorf("reply = %q, want FreeformNotRecordedReply", reply)
	}
}

func TestHandleReinvestigateIsReservedNotImplemented(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> reinvestigate: check the CNI")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatal("reinvestigate must write nothing to the KB")
	}
	if !strings.Contains(strings.ToLower(reply), "not supported") {
		t.Errorf("the reply must say it is unsupported: %q", reply)
	}
}

// TestHandleReservedPrefixAnywhereIsRefused is the regression test for the
// defect this commit fixes: Parse used to match "reinvestigate:" only at
// position 0 of the mention-stripped text, so a single filler word between
// the mention and the command — "<@U0BOT> please reinvestigate: …" — fell
// through to IntentFreeform, which Handle treats identically to an explicit
// "note:": the operator's re-run request was silently written to the
// knowledge base and reported back as "Noted", leaving them believing
// something happened when nothing did. Driven through the real
// Responder.Handle with a fake Forge and asserting ZERO forge calls, not
// just the parsed intent — a correct Intent with a still-broken Handle
// wiring would pass a parse-only test and still write to the forge.
func TestHandleReservedPrefixAnywhereIsRefused(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"leading filler word", "<@U0BOT> please reinvestigate: the network issue"},
		{"different leading filler", "<@U0BOT> can you reinvestigate: this"},
		{"position 0, unchanged from before this fix", "<@U0BOT> reinvestigate: go look"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForge{}
			r := newTestResponder(t, f)
			tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

			reply, err := r.Handle(context.Background(), tc, "alice", tt.text)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(f.comments) != 0 || len(f.opened) != 0 {
				t.Fatalf("forge calls: %d comments, %d opened — want 0/0, a reserved command must never write to the knowledge base no matter what precedes it (text: %q)",
					len(f.comments), len(f.opened), tt.text)
			}
			if reply != ReinvestigateNotSupportedReply {
				t.Errorf("reply = %q, want the reserved-command reply %q", reply, ReinvestigateNotSupportedReply)
			}
		})
	}
}

// TestHandleNoteContainingReservedWordWithoutColonIsCaptured is the
// Handle-level narrowness counterpart to TestHandleReservedPrefixAnywhereIsRefused:
// an explicit note whose TEXT happens to contain the bare word
// "reinvestigate" — no trailing ':' — must still be captured normally, not
// refused as the reserved command. This is what proves the reserved-token
// match is colon-anchored, not a blanket refusal of the word wherever it
// appears.
//
// This test used to drive Handle with NO "note:" prefix at all — under the
// old contract, freeform text was captured exactly like an explicit note, so
// that was sufficient to exercise the reserved-word boundary. Freeform no
// longer writes (see TestHandleFreeformPerformsZeroForgeCallsAndRepliesWithHowTo),
// so an explicit "note:" prefix is now required to reach the write path at
// all; the reserved-word boundary this test pins is otherwise unchanged.
func TestHandleNoteContainingReservedWordWithoutColonIsCaptured(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice",
		"<@U0BOT> note: we had to reinvestigate the DNS path and it was stale")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1 — a note whose text contains the bare word without a colon must still be recorded", len(f.comments))
	}
	if reply == ReinvestigateNotSupportedReply {
		t.Errorf("reply = %q, want the note recorded, not refused", reply)
	}
}

func TestHandleEmptyTextAsksForContent(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note:   ")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 {
		t.Fatal("an empty note must write nothing")
	}
	if reply == "" {
		t.Fatal("an empty note must still get a reply")
	}
}

func TestHandleForgeFailureIsReportedNotSwallowed(t *testing.T) {
	f := &fakeForge{commErr: errors.New("403 forbidden")}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "note: x")
	if err == nil {
		t.Fatal("Handle must return the forge error")
	}
	if !strings.Contains(reply, "403 forbidden") {
		t.Errorf("the human must see why their note was not saved: %q", reply)
	}
	cur, _ := r.Registry.Get("111.222")
	if cur.Notes != 0 {
		t.Errorf("a failed write must not consume the per-thread budget; Notes = %d", cur.Notes)
	}
}

// TestHandleUncountableWriteIsSurfaced pins the fix for the defect where
// Registry.Update returning nil on a miss was indistinguishable from a
// successful counter write-back: Handle would report success even though the
// per-thread cap had just silently failed to record the write. tc is
// deliberately never Put into the registry, so the Notes++ write-back at the
// end of Handle misses and must now be surfaced rather than swallowed.
func TestHandleUncountableWriteIsSurfaced(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

	reply, err := r.Handle(context.Background(), tc, "alice", "note: x")
	if err == nil {
		t.Fatal("an update that cannot be recorded must be surfaced as an error, not swallowed")
	}
	if len(f.comments) != 1 {
		t.Fatalf("the forge write itself must still have landed; comments = %d", len(f.comments))
	}
	if reply == "" {
		t.Fatal("the human must still learn what happened")
	}
}

func TestHandleUnparseableCuratedURLFallsBackToOpeningAPR(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/issues/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("a URL with no parseable PR number must fall back to OpenPR; opened = %d", len(f.opened))
	}
}

func TestMaxNotesDefaultsWhenUnset(t *testing.T) {
	r := &Responder{}
	if got := r.maxNotes(); got != DefaultMaxNotesPerThread {
		t.Errorf("maxNotes() = %d, want DefaultMaxNotesPerThread (%d)", got, DefaultMaxNotesPerThread)
	}
	r.MaxNotesPerThread = -5
	if got := r.maxNotes(); got != DefaultMaxNotesPerThread {
		t.Errorf("maxNotes() with a non-positive override = %d, want the default %d", got, DefaultMaxNotesPerThread)
	}
}

// TestHandleMergedCuratedURLFallsBackToOpeningAPR pins the fix for the bug this
// commit closes: a CuratedURL that has already merged must never be commented
// on — a comment on a merged PR is never indexed by the catalog, so the
// knowledge is silently lost while the human is told it was saved. The
// responder must instead open a standalone Concept PR, exactly as it does for
// a thread with no CuratedURL at all.
func TestHandleMergedCuratedURLFallsBackToOpeningAPR(t *testing.T) {
	f := &fakeForge{prOpen: map[int]bool{42: false}}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "note: spot reclaim, not OOM")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 {
		t.Fatalf("must never comment on a merged PR; comments = %d", len(f.comments))
	}
	if len(f.opened) != 1 {
		t.Fatalf("a merged CuratedURL must fall back to opening a standalone PR; opened = %d", len(f.opened))
	}
	if f.opened[0].Type != "Concept" {
		t.Errorf("Type = %q, want Concept", f.opened[0].Type)
	}
	if !strings.Contains(reply, "99") {
		t.Errorf("reply must name the PR it actually opened: %q", reply)
	}
}

// TestHandleOpenCuratedURLStillComments is the sibling of the merged case: an
// open PR must still receive the comment, and the open-check must run first.
func TestHandleOpenCuratedURLStillComments(t *testing.T) {
	f := &fakeForge{prOpen: map[int]bool{42: true}}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 || f.comments[0].number != 42 {
		t.Fatalf("an open PR must still receive the comment; comments = %+v", f.comments)
	}
	if len(f.prOpenCalls) == 0 || f.prOpenCalls[0] != 42 {
		t.Fatalf("IsPROpen must be checked before commenting; calls = %v", f.prOpenCalls)
	}
}

// TestHandleMergedNoteURLFallsBackToOpeningANewPR is the NoteURL half of the
// same fix — the spec's routing gives NoteURL (the standalone PR a previous
// note in this thread opened) the exact same "must be open" invariant.
func TestHandleMergedNoteURLFallsBackToOpeningANewPR(t *testing.T) {
	f := &fakeForge{prOpen: map[int]bool{77: false}}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", NoteURL: "https://github.com/o/r/pull/77"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 {
		t.Fatalf("must never comment on a merged NoteURL PR; comments = %d", len(f.comments))
	}
	if len(f.opened) != 1 {
		t.Fatalf("a merged NoteURL must fall back to opening a new standalone PR; opened = %d", len(f.opened))
	}
}

// TestHandleFallsBackToNoteURLWhenCuratedURLIsMerged exercises both links being
// set at once (see TestHandlePrefersCuratedURLOverNoteURL for the open/open
// case): when CuratedURL has merged but NoteURL is still open, the note must
// land on NoteURL rather than opening a third PR.
func TestHandleFallsBackToNoteURLWhenCuratedURLIsMerged(t *testing.T) {
	f := &fakeForge{prOpen: map[int]bool{42: false, 77: true}}
	r := newTestResponder(t, f)
	tc := Context{
		Root:       "111.222",
		CuratedURL: "https://github.com/o/r/pull/42",
		NoteURL:    "https://github.com/o/r/pull/77",
	}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.opened) != 0 {
		t.Fatalf("must not open a third PR when NoteURL is still open; opened = %d", len(f.opened))
	}
	if len(f.comments) != 1 || f.comments[0].number != 77 {
		t.Fatalf("must fall back to the still-open NoteURL; comments = %+v", f.comments)
	}
}

// TestHandleIsPROpenErrorDoesNotEscalateToOpeningAPR pins the corrected
// behaviour when the open-check itself fails (network blip, rate limit, forge
// outage). An earlier version of this test pinned the OPPOSITE choice —
// falling through to OpenPR, on the reasoning that dropping the note outright
// would be worse. The reasoning was sound; the direction was wrong: on
// GitHub, escalating turns a ~2-call comment into a ~7-call PR creation
// (branch, file PUTs, PR, labels) exactly when the forge is already
// degraded — hitting a read rate limit makes RunLore write MORE, onto an
// already-struggling forge. This must instead be distinguished from a closed
// PR (TestHandleMergedCuratedURLFallsBackToOpeningAPR): "could not tell"
// reports the failure to the human so they can retry; it never silently drops
// the note, and never claims success either.
func TestHandleIsPROpenErrorDoesNotEscalateToOpeningAPR(t *testing.T) {
	f := &fakeForge{prOpenErr: errors.New("503 rate limited")}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "note: x")
	if err == nil {
		t.Fatal("an open-check failure must be reported as an error, not swallowed")
	}
	if len(f.comments) != 0 {
		t.Fatalf("an open-check failure must never risk a comment on a possibly-merged PR; comments = %d", len(f.comments))
	}
	if len(f.opened) != 0 {
		t.Fatalf("an open-check failure must NOT escalate to opening a new PR; opened = %d", len(f.opened))
	}
	if reply == "" {
		t.Fatal("the human must still be told what happened to their note")
	}
	if strings.Contains(reply, "99") {
		t.Errorf("reply must not claim a PR was opened when none was: %q", reply)
	}
	cur, _ := r.Registry.Get("111.222")
	if cur.Notes != 0 {
		t.Errorf("a write that did not land must not consume the per-thread budget; Notes = %d", cur.Notes)
	}
}

func TestHandlePrefersCuratedURLOverNoteURL(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{
		Root:       "111.222",
		CuratedURL: "https://github.com/o/r/pull/42",
		NoteURL:    "https://github.com/o/r/pull/77",
	}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if f.comments[0].number != 42 {
		t.Errorf("commented on PR %d, want 42 — CuratedURL must win when both CuratedURL and NoteURL are set", f.comments[0].number)
	}
	if len(f.opened) != 0 {
		t.Errorf("must not open a PR when CuratedURL is already linked; opened %d", len(f.opened))
	}
}

// TestHandleThrottlePathLogsAndIncrementsCounter pins the fix for the defect
// where a global-window throttle returned a reply string with no log line and
// no metric: an operator had no way to tell the feature was throttling at
// all. The throttle must now log at a level an operator will see AND
// increment ThreadWritesThrottled.
func TestHandleThrottlePathLogsAndIncrementsCounter(t *testing.T) {
	var logBuf bytes.Buffer
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.ForgeWrites = ratelimit.New(1, time.Hour)
	r.ForgeWrites.Allow() // spend the one slot in the budget so the write below is denied
	r.Log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	m, read := meteredInstruments(t)
	r.Metrics = m

	tc := Context{Root: "a", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if !strings.Contains(logBuf.String(), "thread") {
		t.Errorf("the throttle must log at a level an operator will see; log = %q", logBuf.String())
	}
	got, ok := read("runlore_thread_writes_throttled_total")
	if !ok || got != 1 {
		t.Errorf("runlore_thread_writes_throttled_total = %d (ok=%v), want 1", got, ok)
	}
}

// TestHandleThrottlePathWithNilMetricsDoesNotPanic proves the counter is
// nil-safe: a Responder with no Metrics wired (the common case whenever
// telemetry is not configured) must not panic on the throttle path.
func TestHandleThrottlePathWithNilMetricsDoesNotPanic(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.ForgeWrites = ratelimit.New(1, time.Hour)
	r.ForgeWrites.Allow() // spend the one slot in the budget so the write below is denied
	r.Metrics = nil

	tc := Context{Root: "a", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	reply, err := r.Handle(context.Background(), tc, "alice", "note: x") // must not panic
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "paused") {
		t.Fatalf("test setup did not actually reach the throttle path: reply = %q", reply)
	}
}

// TestHandleSuccessfulWritesIncrementCounterByRoute pins the sibling fix: the
// audit noted notes-written was slog.Info only, with no metric, so an
// operator could see throttling but not volume. Both landing routes must
// increment ThreadNotesWritten, distinguished by the route label.
func TestHandleSuccessfulWritesIncrementCounterByRoute(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	m, read := meteredInstruments(t)
	r.Metrics = m

	commentTC := Context{Root: "a", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(commentTC); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), commentTC, "alice", "note: via comment"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	openTC := Context{Root: "b"}
	if err := r.Registry.Put(openTC); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), openTC, "bob", "note: via open pr"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, ok := read("runlore_thread_notes_written_total")
	if !ok || got != 2 {
		t.Errorf("runlore_thread_notes_written_total = %d (ok=%v), want 2 (one per landed route)", got, ok)
	}
}

// TestResponderConcurrentFirstNotesOnSameRootProduceExactlyOneOpenPR pins the
// close of the residual race Registry.GetOrCreate's doc comment describes:
// atomic rehydration alone stops two callers from creating DIVERGENT
// registry entries, but it does not stop them from both observing the SAME
// entry's NoteURL == "" and both calling OpenPR before either write updates
// it. The per-root write guard closes that: write() re-reads the registry
// under the guard, so every writer after the first sees the NoteURL the
// first one just landed and comments instead of opening again.
//
// Driven with real goroutines, not a simulated ordering — the hazard is a
// genuine race between concurrent write() calls for the same root.
func TestResponderConcurrentFirstNotesOnSameRootProduceExactlyOneOpenPR(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.MaxNotesPerThread = 1000                  // the per-thread cap is not what this test pins
	r.ForgeWrites = ratelimit.New(0, time.Hour) // 0 = unlimited; the global budget is not what this test pins
	root := "unknown-thread"
	if err := r.Registry.Put(Context{Root: root}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tc, ok := r.Registry.Get(root)
			if !ok {
				t.Errorf("Get[%d]: registry lost the root mid-test", i)
				return
			}
			if _, err := r.Handle(context.Background(), tc, fmt.Sprintf("user%d", i), "note: concurrent"); err != nil {
				t.Errorf("Handle[%d]: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	comments, opened := f.counts()
	if opened != 1 {
		t.Fatalf("opened = %d, want exactly 1 — every concurrent first note on the same thread must land on the ONE PR the first writer opens, not open its own", opened)
	}
	if comments != n-1 {
		t.Fatalf("comments = %d, want %d — every writer after the first must comment on that one PR", comments, n-1)
	}
}

// TestResponderConcurrentNotesOnDifferentRootsAreNotSerialized proves the
// per-root guard is exactly that — per ROOT — and not a global lock in
// disguise: two notes on two DIFFERENT threads must be able to be inside the
// forge call at the same time. The fakeForge rendezvous (entered/proceed)
// makes this a deterministic proof rather than a timing guess: the test only
// proceeds past the wait once BOTH goroutines have signalled they are inside
// OpenPR simultaneously; if the guard wrongly serialized them, the second
// signal would never arrive and the test would time out.
func TestResponderConcurrentNotesOnDifferentRootsAreNotSerialized(t *testing.T) {
	f := &fakeForge{entered: make(chan struct{}, 2), proceed: make(chan struct{})}
	r := newTestResponder(t, f)
	for _, root := range []string{"root-a", "root-b"} {
		if err := r.Registry.Put(Context{Root: root}); err != nil {
			t.Fatalf("Put(%s): %v", root, err)
		}
	}

	var wg sync.WaitGroup
	for _, root := range []string{"root-a", "root-b"} {
		wg.Add(1)
		go func(root string) {
			defer wg.Done()
			tc, _ := r.Registry.Get(root)
			if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
				t.Errorf("Handle(%s): %v", root, err)
			}
		}(root)
	}

	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-f.entered:
		case <-timeout:
			t.Fatal("two different-root writes must be able to overlap inside the forge call, but a second one never arrived — the guard is serializing across roots")
		}
	}
	close(f.proceed)
	wg.Wait()
}

// TestResponderWriteErrorReleasesTheGuard pins that a forge failure does not
// leave the per-root guard held: the release must be unconditional (deferred
// right after acquisition), or a single failed write on a root would block
// every later write on that same root forever.
func TestResponderWriteErrorReleasesTheGuard(t *testing.T) {
	f := &fakeForge{openErr: errors.New("503 unavailable")}
	r := newTestResponder(t, f)
	root := "r1"
	if err := r.Registry.Put(Context{Root: root}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), Context{Root: root}, "alice", "note: first"); err == nil {
		t.Fatal("test setup: the first write must fail")
	}

	f.mu.Lock()
	f.openErr = nil
	f.mu.Unlock()

	done := make(chan struct{})
	go func() {
		if _, err := r.Handle(context.Background(), Context{Root: root}, "bob", "note: second"); err != nil {
			t.Errorf("Handle after a failed write: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a failed write left the per-root guard held — the second write on the same root deadlocked")
	}

	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want 1 — the second write must have actually landed, not just returned", len(f.opened))
	}
}

// newChatResponder is newTestResponder with the chat layer wired to model. The
// Chat it builds carries nothing but a model and a logger: Budget, Catalog and
// Metrics are all nil-safe, and leaving them nil is what keeps these tests
// about the ROUTING Handle does with an answer rather than about how the
// answer was produced (chat_test.go covers that).
func newChatResponder(t *testing.T, f *fakeForge, model providers.ModelProvider) *Responder {
	t.Helper()
	r := newTestResponder(t, f)
	r.Chat = &Chat{Model: model, Log: silentLog()}
	return r
}

// TestHandleChatUnconfiguredFreeformIsUnchanged pins that the chat layer is
// strictly opt-in: with no Chat wired, a freeform message behaves exactly as
// it did before this route existed — the how-to reply, and nothing written.
// An operator who never configures model.chat must see PR2's behaviour
// byte-for-byte.
func TestHandleChatUnconfiguredFreeformIsUnchanged(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	if r.Chat != nil {
		t.Fatal("test setup: newTestResponder must leave Chat nil")
	}
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> was it the CNI?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply != FreeformNotRecordedReply {
		t.Errorf("reply = %q, want FreeformNotRecordedReply", reply)
	}
	if c, o := f.counts(); c != 0 || o != 0 {
		t.Errorf("forge calls = %d comments / %d opened, want 0/0", c, o)
	}
}

// TestHandleChatAnswersFreeformWithTheModelsReply is the happy path with
// nothing to record: the human asked a question, the model answered it, and
// kb_note came back empty — "file nothing", not an omission. The reply is the
// model's own, and the knowledge base is untouched.
func TestHandleChatAnswersFreeformWithTheModelsReply(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("The CNI was ruled out; it was a spot reclaim.", "")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> was it the CNI?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if model.calls != 1 {
		t.Errorf("Complete called %d times, want exactly 1", model.calls)
	}
	// Quoted, not verbatim: model prose is always marked as the model's — see
	// TestHandleChatModelProseCannotForgeRunLoresStatusLines. It also carries
	// untrusted-span marks for the transport, which RenderReply resolves; a nil
	// escape strips them and leaves the text a human would actually read.
	if got := RenderReply(reply, nil); got != "> The CNI was ruled out; it was a spot reclaim." {
		t.Errorf("reply = %q, want the model's own reply, quoted", got)
	}
	if c, o := f.counts(); c != 0 || o != 0 {
		t.Errorf("forge calls = %d comments / %d opened, want 0/0 — an empty kb_note writes nothing", c, o)
	}
}

// TestHandleChatProposedNoteIsWrittenThroughTheExistingRouting is the core of
// this route: a non-empty kb_note goes through the SAME write() the explicit
// `note:` path uses, so it lands on the PR the thread context points at. The
// model supplied content only — it never named a target, and could not have.
// The thread's note counter is charged for it exactly as an explicit note is.
func TestHandleChatProposedNoteIsWrittenThroughTheExistingRouting(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("Noted — that changes the root cause.", "The real cause was a spot-node reclaim, not the CNI.")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> it was actually a spot reclaim")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 || len(f.opened) != 0 {
		t.Fatalf("forge calls = %d comments / %d opened, want 1/0 — the note must take the linked-PR route", len(f.comments), len(f.opened))
	}
	if f.comments[0].number != 42 {
		t.Errorf("commented on PR #%d, want #42 — the route comes from the thread context, never from the model", f.comments[0].number)
	}
	if !strings.Contains(f.comments[0].body, "spot-node reclaim, not the CNI") {
		t.Errorf("the comment must carry the model's kb_note, got:\n%s", f.comments[0].body)
	}
	// The kb_note is what gets FILED; the human's message is carried alongside it
	// as the evidence a reviewer weighs the draft against — see
	// TestHandleChatProposedNoteIsFiledAsModelDrafted.
	if !strings.Contains(f.comments[0].body, "> it was actually a spot reclaim") {
		t.Errorf("the comment must quote the human message the draft came from, got:\n%s", f.comments[0].body)
	}
	if !strings.Contains(reply, "Noted — that changes the root cause.") {
		t.Errorf("reply = %q, want it to carry the model's answer", reply)
	}
	if !strings.Contains(reply, "#42") {
		t.Errorf("reply = %q, want it to tell the human where the note landed", reply)
	}
	got, ok := r.Registry.Get("111.222")
	if !ok {
		t.Fatal("Get: thread missing from the registry")
	}
	if got.Notes != 1 {
		t.Errorf("Notes = %d, want 1 — a chat-proposed note spends the same per-thread allowance an explicit note does", got.Notes)
	}
}

// TestWriteRouteIgnoresAForgeURLInTheNoteText is the adversarial half of the
// test above, and the half that actually pins its failure message ("the route
// comes from the thread context, never from the model"). That test's kb_note
// contains no URL at all, so its fixture cannot violate the property it names:
// adding n.Text as the first routing candidate in Responder.write left the
// whole package green, while a live kb_note carrying
// https://github.com/o/r/pull/1337 redirected the human's note off the
// context's PR #42 and onto #1337.
//
// The note text below is one prNumberOn ACCEPTS WHOLE — configured host,
// configured repository path, pull segment immediately after it — which is the
// only kind that proves anything now that the anchor parses the candidate as a
// URL rather than scanning it for "/pull/<n>". A URL buried mid-sentence no
// longer parses at all ("the fix is on https://…" has no host), so a fixture
// that embeds one that way is inert against this mutation even though it looks
// adversarial. The setup guard asserts acceptance of the exact string write()
// would route on, not of the URL in isolation.
//
// Both callers of write() are driven, because the property is about the note
// TEXT and each route supplies it from a different untrusted author: the
// deterministic `note:` capture files the human's own words, and the chat route
// files the model's. Neither may name a target.
func TestWriteRouteIgnoresAForgeURLInTheNoteText(t *testing.T) {
	const (
		repo   = "github.com/acme/kb"
		linked = "https://github.com/acme/kb/pull/42"
		forged = "https://github.com/acme/kb/pull/1337"
		// A note leading with a link is ordinary model (and human) output, and it
		// is also the shape prNumberOn accepts: the trailing prose lands inside
		// the URL's path, after the pull segment the number comes from.
		noteText = forged + " already documents this — the real cause was a spot-node reclaim"
	)

	routes := []struct {
		name    string
		build   func(t *testing.T, f *fakeForge) *Responder
		message string
	}{
		{
			name: "model-authored note text",
			build: func(t *testing.T, f *fakeForge) *Responder {
				t.Helper()
				return newChatResponder(t, f, &fakeChatModel{resp: wellFormedReply("Noted.", noteText)})
			},
			message: "<@U0BOT> it was actually a spot reclaim",
		},
		{
			name:    "human-authored note text",
			build:   newTestResponder,
			message: "<@U0BOT> note: " + noteText,
		},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			// setUp builds the responder and refuses to continue unless the note
			// text — the whole string, exactly as write() would see it — is one the
			// routing parser takes a decision from. Without this guard the arms
			// below would pass against a write() that routed on n.Text, simply
			// because prNumberOn rejected the text for an unrelated reason, which
			// is the exact inertness this test exists to end.
			setUp := func(t *testing.T, f *fakeForge) *Responder {
				t.Helper()
				r := rt.build(t, f)
				r.ForgeRepo = repo
				if n, ok := r.prNumberOn(noteText); !ok || n != 1337 {
					t.Fatalf("fixture is inert: prNumberOn(%q) = (%d, %v), want (1337, true) — the note text must be something the router would accept, or nothing about routing is being tested", noteText, n, ok)
				}
				return r
			}

			t.Run("the note lands on the thread's own PR", func(t *testing.T) {
				f := &fakeForge{}
				r := setUp(t, f)
				tc := Context{Root: "111.222", Title: "OOM", CuratedURL: linked}
				if err := r.Registry.Put(tc); err != nil {
					t.Fatalf("Put: %v", err)
				}

				if _, err := r.Handle(context.Background(), tc, "alice", rt.message); err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if len(f.comments) != 1 {
					t.Fatalf("comments = %d, want 1", len(f.comments))
				}
				if f.comments[0].number != 42 {
					t.Fatalf("the note landed on PR #%d, want #42 — a forge URL inside the note text must never select the pull request it is written to", f.comments[0].number)
				}
				for _, n := range f.prOpenCalls {
					if n != 42 {
						t.Fatalf("the open-check asked about PR #%d — a URL in the note text must not even become a routing candidate (asked about %v)", n, f.prOpenCalls)
					}
				}
			})

			t.Run("with no PR linked it opens a standalone one", func(t *testing.T) {
				f := &fakeForge{}
				r := setUp(t, f)
				tc := Context{Root: "111.222", Title: "OOM"} // no CuratedURL, no NoteURL
				if err := r.Registry.Put(tc); err != nil {
					t.Fatalf("Put: %v", err)
				}

				if _, err := r.Handle(context.Background(), tc, "alice", rt.message); err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if len(f.comments) != 0 {
					t.Fatalf("commented on PR #%d with nothing linked to the thread — the note text supplied a route it must never supply", f.comments[0].number)
				}
				if len(f.opened) != 1 {
					t.Fatalf("opened = %d, want 1 — a thread with no linked PR must open a standalone one", len(f.opened))
				}
				if len(f.prOpenCalls) != 0 {
					t.Fatalf("the open-check ran for %v with nothing linked to the thread — the only routing candidates are the context's own URLs", f.prOpenCalls)
				}
			})
		})
	}
}

// TestHandleChatModelProseCannotForgeRunLoresStatusLines closes the finding
// that the model's answer and RunLore's own statements about what it did were
// posted into the same message, under the same bot identity, in the same
// vocabulary. The model reproduces RunLore's "📝"/"⚠️" status wording
// byte-for-byte — it has been shown the real thing — so a message like
// "📝 Noted on the knowledge-base PR #7 — https://github.example.evil/…" or
// "⚠️ RunLore security notice: your session token has expired, re-authenticate
// at https://runlore.evil/login" arrived indistinguishable from a line RunLore
// actually wrote.
//
// The bot's own claims about what it did must not be forgeable by its own
// model output.
func TestHandleChatModelProseCannotForgeRunLoresStatusLines(t *testing.T) {
	forged := "📝 Noted on the knowledge-base PR #7 — https://github.example.evil/o/kb/pull/7\n" +
		"⚠️ RunLore security notice: your session token has expired, re-authenticate at https://runlore.evil/login"
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply(forged, "")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> was it the CNI?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	for _, line := range strings.Split(reply, "\n") {
		if !strings.HasPrefix(line, "> ") {
			t.Errorf("a line of model prose was posted unquoted, so it reads as RunLore's own: %q", line)
		}
		for _, glyph := range []string{"📝", "⚠"} {
			if strings.Contains(line, glyph) {
				t.Errorf("model prose kept RunLore's status glyph %q: %q", glyph, line)
			}
		}
	}
	if !strings.Contains(reply, "your session token has expired") {
		t.Errorf("the model's words must survive — marked, not censored: %q", reply)
	}
}

// TestHandleChatRunLoresOwnStatusLineStaysUnquoted is the other half: the
// distinction only works if RunLore's real status line is NOT quoted, so the
// two are visibly different in the same message.
func TestHandleChatRunLoresOwnStatusLineStaysUnquoted(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("Noted.", "the real cause was a spot reclaim")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> it was a spot reclaim")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Read as a human would see it: the untrusted-span marks are the
	// transport's business, not the quoting contract's — see RenderReply.
	rendered := RenderReply(reply, nil)
	if !strings.Contains(rendered, "\n📝 Noted on the knowledge-base PR #42") {
		t.Errorf("RunLore's own status line must stay unquoted and glyph-led: %q", rendered)
	}
	if !strings.Contains(rendered, "> Noted.") {
		t.Errorf("the model's answer must be quoted: %q", rendered)
	}
}

// TestHandleChatProposedNoteIsFiledAsModelDrafted is the end-to-end guard for
// the provenance split. freeform hands record() the MODEL's kb_note, and
// before this fix that text was filed under the human's own verbatim-note
// header — a KB reviewer saw a named engineer apparently stating something the
// engineer never wrote, merged it, and internal/curate's isOperatorNote then
// protected it from the stale sweep with the provenance unrecoverable from the
// PR. The human merge is also the last gate on the section-forgery chain, so
// this is exactly the signal that gate depends on.
func TestHandleChatProposedNoteIsFiledAsModelDrafted(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("Answered.", "Confirmed: spot-node reclaim, not the CNI.")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", Transport: "slack", Title: "pod crash-looping", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> did you check the NetworkPolicies?"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	body := f.comments[0].body
	if strings.Contains(body, "From **@alice** via slack") {
		t.Errorf("model-drafted text must not be filed under the human's verbatim-note header:\n%s", body)
	}
	if !strings.Contains(body, "> did you check the NetworkPolicies?") {
		t.Errorf("the human's actual message must be quoted so a reviewer can weigh the draft against it:\n%s", body)
	}
}

// TestHandleNoteRouteStillFilesTheHumansVerbatimWords is the other half of the
// split: an explicit "note:" is the route whose whole contract is verbatim
// human words, and it must keep the header that says so.
func TestHandleNoteRouteStillFilesTheHumansVerbatimWords(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Transport: "slack", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: it was a spot reclaim"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if !strings.Contains(f.comments[0].body, "From **@alice** via slack") {
		t.Errorf("an explicit note: must keep the human provenance header:\n%s", f.comments[0].body)
	}
}

// TestHandleChatProposedNoteOpensAStandalonePRWhenNoneIsLinked covers the
// other arm of the same routing: with no PR on the thread context, the note
// opens a standalone Concept PR — again chosen by write(), not by the model.
func TestHandleChatProposedNoteOpensAStandalonePRWhenNoneIsLinked(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("Got it.", "Spot reclaims on the burst pool look like OOM kills.")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", Title: "OOM"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> spot reclaims look like OOM kills")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.opened) != 1 || len(f.comments) != 0 {
		t.Fatalf("forge calls = %d comments / %d opened, want 0/1", len(f.comments), len(f.opened))
	}
	if !strings.Contains(f.opened[0].Body, "burst pool look like OOM kills") {
		t.Errorf("the opened entry must carry the model's kb_note, got:\n%s", f.opened[0].Body)
	}
	if !strings.Contains(reply, "Got it.") {
		t.Errorf("reply = %q, want it to carry the model's answer", reply)
	}
}

// TestHandleChatDegradesToTheDeterministicReply pins the fallback contract:
// every way Answer can report false — a model error, a model that never got
// configured, a response with no tool call — degrades to the reply freeform
// gave before this route existed. A human's message is never silently
// dropped because a provider had a bad minute.
func TestHandleChatDegradesToTheDeterministicReply(t *testing.T) {
	tests := []struct {
		name string
		chat func() *Chat
	}{
		{"model error", func() *Chat {
			return &Chat{Model: &fakeChatModel{err: errors.New("503 unavailable")}, Log: silentLog()}
		}},
		{"no model configured", func() *Chat {
			return &Chat{Log: silentLog()}
		}},
		{"no tool call", func() *Chat {
			return &Chat{Model: &fakeChatModel{resp: providers.CompletionResponse{Text: "sure thing"}}, Log: silentLog()}
		}},
		{"empty reply", func() *Chat {
			return &Chat{Model: &fakeChatModel{resp: wellFormedReply("   ", "a fact")}, Log: silentLog()}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForge{}
			r := newTestResponder(t, f)
			r.Chat = tt.chat()
			tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
			if err := r.Registry.Put(tc); err != nil {
				t.Fatalf("Put: %v", err)
			}

			reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> was it the CNI?")
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if reply != FreeformNotRecordedReply {
				t.Errorf("reply = %q, want FreeformNotRecordedReply — a failed answer must degrade, never drop", reply)
			}
			if c, o := f.counts(); c != 0 || o != 0 {
				t.Errorf("forge calls = %d comments / %d opened, want 0/0 — a failed answer must not write", c, o)
			}
		})
	}
}

// TestHandleChatNotePrefixMakesZeroModelCalls pins that wiring a model changed
// nothing about the deterministic capture path: an explicit `note:` is still
// written verbatim, and costs no tokens at all. Asserted on the model's own
// call count, not inferred from the reply — a route that called the model and
// then ignored its answer would still be a per-message spend nobody asked for.
func TestHandleChatNotePrefixMakesZeroModelCalls(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("should never be used", "should never be written")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: spot reclaim, not OOM")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if model.calls != 0 {
		t.Errorf("Complete called %d times, want 0 — an explicit note is captured verbatim and costs no tokens", model.calls)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if !strings.Contains(f.comments[0].body, "spot reclaim, not OOM") {
		t.Errorf("the comment must carry the human's own words, got:\n%s", f.comments[0].body)
	}
	if !strings.Contains(reply, "#42") {
		t.Errorf("reply = %q, want the note-recorded reply", reply)
	}
}

// TestHandleChatWidenedNotePrefixMakesZeroModelCalls is the cost half of the
// grammar fix. The docs say the deterministic `note:` path is free; a counting
// model showed "hey <@U0BOT> note: …", "<@U0BOT> please note: …" and
// ":wave: <@U0BOT> note: …" each costing one model call, because "note:" was
// only recognised at position 0. An operator told a path is free, who is then
// billed for "please note:", was misled by the interface, not by the docs.
func TestHandleChatWidenedNotePrefixMakesZeroModelCalls(t *testing.T) {
	for _, raw := range []string{
		"<@U0BOT> note: the cause was a spot reclaim",
		"<@U0BOT> Note: the cause was a spot reclaim",
		"note: the cause was a spot reclaim",
		"hey <@U0BOT> note: the cause was a spot reclaim",
		"<@U0BOT> please note: the cause was a spot reclaim",
		":wave: <@U0BOT> note: the cause was a spot reclaim",
		"hey runlore note: the cause was a spot reclaim",
	} {
		t.Run(raw, func(t *testing.T) {
			f := &fakeForge{}
			model := &fakeChatModel{resp: wellFormedReply("should never be called", "")}
			r := newChatResponder(t, f, model)
			tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
			if err := r.Registry.Put(tc); err != nil {
				t.Fatalf("Put: %v", err)
			}

			if _, err := r.Handle(context.Background(), tc, "alice", raw); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if model.calls != 0 {
				t.Errorf("Complete called %d times, want 0 — the deterministic note: path must cost nothing", model.calls)
			}
			if len(f.comments) != 1 {
				t.Fatalf("comments = %d, want 1 — the note must still be recorded", len(f.comments))
			}
			if !strings.Contains(f.comments[0].body, "the cause was a spot reclaim") {
				t.Errorf("the note text must be the words after the prefix:\n%s", f.comments[0].body)
			}
			if strings.Contains(f.comments[0].body, "please note") || strings.Contains(f.comments[0].body, "<@U0BOT>") {
				t.Errorf("the addressing prefix must not be recorded as part of the note:\n%s", f.comments[0].body)
			}
		})
	}
}

// TestHandleChatBareMentionAndFreeformStillCostAModelCall keeps the widening
// from swallowing the route it sits next to: anything WITHOUT the token is
// still freeform, and a bare mention still costs nothing.
func TestHandleChatFreeformWithoutTheTokenStillReachesTheModel(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("answered", "")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> see footnote: at the bottom"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if model.calls != 1 {
		t.Errorf("Complete called %d times, want 1 — a word merely ending in \"note:\" is not the command", model.calls)
	}
}

// TestHandleChatReinvestigateMakesZeroModelCalls pins the reserved command is
// untouched by this route: still refused, still no write — and now also no
// model call, so a reserved command cannot be turned into a token spend by
// wiring a chat model.
func TestHandleChatReinvestigateMakesZeroModelCalls(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("should never be used", "should never be written")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> please reinvestigate: the CNI")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if model.calls != 0 {
		t.Errorf("Complete called %d times, want 0 — a reserved command is refused before any model call", model.calls)
	}
	if c, o := f.counts(); c != 0 || o != 0 {
		t.Errorf("forge calls = %d comments / %d opened, want 0/0", c, o)
	}
	if reply != ReinvestigateNotSupportedReply {
		t.Errorf("reply = %q, want ReinvestigateNotSupportedReply", reply)
	}
}

// TestHandleChatBareMentionMakesZeroModelCalls: a bare "<@U0BOT>" parses as
// freeform with empty Text (see grammar_test.go). There is no question in it
// to answer, so it must not become a paid model call — the cheapest way to
// make a channel expensive would otherwise be to mention the bot repeatedly
// with nothing after it.
func TestHandleChatBareMentionMakesZeroModelCalls(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("should never be used", "")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT>")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if model.calls != 0 {
		t.Errorf("Complete called %d times, want 0 — an empty message has nothing to answer", model.calls)
	}
	if reply != FreeformNotRecordedReply {
		t.Errorf("reply = %q, want FreeformNotRecordedReply", reply)
	}
	if c, o := f.counts(); c != 0 || o != 0 {
		t.Errorf("forge calls = %d comments / %d opened, want 0/0", c, o)
	}
}

// TestHandleChatProposedNoteIsBoundedByTheForgeWriteWindow proves the two
// ceilings stayed separate. ForgeWrites bounds PRs and comments; Budget bounds
// tokens. An exhausted forge window must not suppress the ANSWER — the human
// still gets one — it must only stop the write, and the human must be told the
// note was not saved rather than left believing it was.
func TestHandleChatProposedNoteIsBoundedByTheForgeWriteWindow(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("Understood.", "The real cause was a spot-node reclaim.")}
	r := newChatResponder(t, f, model)
	r.ForgeWrites = ratelimit.New(1, time.Hour)
	if !r.ForgeWrites.Allow() {
		t.Fatal("test setup: the first slot must be available")
	}
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> it was a spot reclaim")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if model.calls != 1 {
		t.Errorf("Complete called %d times, want 1 — the forge window bounds writes, not answers", model.calls)
	}
	if c, o := f.counts(); c != 0 || o != 0 {
		t.Errorf("forge calls = %d comments / %d opened, want 0/0 — the window was exhausted", c, o)
	}
	if !strings.Contains(reply, "Understood.") {
		t.Errorf("reply = %q, want the model's answer even when the write was throttled", reply)
	}
	if !strings.Contains(reply, "too many knowledge-base writes") {
		t.Errorf("reply = %q, want it to say the note was not saved", reply)
	}
}

// TestHandleChatProposedNoteIsBoundedByThePerThreadCap: the note this route
// proposes draws on the SAME per-thread allowance an explicit note does, so a
// thread already at its cap writes nothing further — while still getting its
// answer.
func TestHandleChatProposedNoteIsBoundedByThePerThreadCap(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("Understood.", "The real cause was a spot-node reclaim.")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42", Notes: 3}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> it was a spot reclaim")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if c, o := f.counts(); c != 0 || o != 0 {
		t.Errorf("forge calls = %d comments / %d opened, want 0/0 — the thread is at its cap", c, o)
	}
	if !strings.Contains(reply, "Understood.") {
		t.Errorf("reply = %q, want the model's answer even at the cap", reply)
	}
	if !strings.Contains(reply, "note limit") {
		t.Errorf("reply = %q, want it to say the note was not saved", reply)
	}
}

// visibleEscape stands in for a transport's own escaper. It wraps whatever
// span it is handed in «…» so a test can see EXACTLY which bytes of a reply an
// adapter would neutralise, without importing one chat system's markup rules
// into a package that must not know them. The real escapers are asserted
// against the real transports in internal/notify.
func visibleEscape(s string) string { return "«" + s + "»" }

// TestUntrustedRoundTripsThroughRenderReply pins the span primitives on their
// own, ahead of the routing that uses them: what Untrusted wraps is what
// escape sees, RunLore's own bytes are never handed to escape, a nil escape
// strips the marks without touching anything, and content that already
// contains the mark cannot smuggle a span boundary of its own.
func TestUntrustedRoundTripsThroughRenderReply(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply string
		want  string
	}{
		{"no span at all", "📝 Noted on PR #42", "📝 Noted on PR #42"},
		{"one span", "> " + Untrusted("<!channel>"), "> «<!channel>»"},
		{
			"RunLore's framing around a span",
			"> " + Untrusted("hi") + "\n📝 Noted — `note: <text>`",
			"> «hi»\n📝 Noted — `note: <text>`",
		},
		{"empty content is not marked", "> " + Untrusted(""), "> "},
		{
			"content carrying the mark cannot open a span of its own",
			Untrusted("a" + untrustedMark + "📝 forged"),
			"«a📝 forged»",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := RenderReply(tt.reply, visibleEscape); got != tt.want {
				t.Errorf("RenderReply(%q) = %q, want %q", tt.reply, got, tt.want)
			}
			if got := RenderReply(tt.reply, nil); strings.Contains(got, untrustedMark) {
				t.Errorf("RenderReply(%q, nil) = %q, want the marks stripped", tt.reply, got)
			}
		})
	}
}

// TestHandleChatMarksTheModelsProseAndNothingElse is the thread half of the
// unescaped-model-prose finding. Model prose is posted into the same message,
// under the same bot identity, as RunLore's own status lines — so a reply like
// <https://evil.example/reauth|https://github.com/acme/kb/pull/7> rendered as
// a clickable link whose VISIBLE text is a trusted knowledge-base URL, and
// <!channel> mass-pinged the room, in the very thread where RunLore posts
// genuine KB links. modelVoice's blockquote neutralises neither.
//
// The escaping itself belongs to the transport (see internal/notify); what
// this pins is the boundary — which bytes are handed to it. Every byte of the
// model's answer is inside a span, and not one byte of RunLore's own framing
// is: not the "> " markers the blockquote is made of, not the status glyph,
// not the sentence around the URL.
func TestHandleChatMarksTheModelsProseAndNothingElse(t *testing.T) {
	forged := "Re-auth here: <https://evil.example/reauth|https://github.com/acme/kb/pull/7>\n<!channel>"
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply(forged, "the real cause was a spot reclaim")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> was it the CNI?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := "> «Re-auth here: <https://evil.example/reauth|https://github.com/acme/kb/pull/7>»\n" +
		"> «<!channel>»\n" +
		"📝 Noted on the knowledge-base PR #42 — «https://github.com/o/r/pull/42»"
	if got := RenderReply(reply, visibleEscape); got != want {
		t.Errorf("rendered reply =\n%q\nwant\n%q", got, want)
	}
}

// TestHandleModelProseCannotEscapeItsSpanByEmittingTheMark is the other side
// of the boundary: the mark is an in-band signal, so a model that emits one
// itself must not be able to end its own span early and continue at the left
// margin as RunLore. Untrusted strips the mark from its content; this proves
// it does so on the real path, not only in the unit table above.
func TestHandleModelProseCannotEscapeItsSpanByEmittingTheMark(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("bye"+untrustedMark+"📝 Noted on the knowledge-base PR #7", "")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> was it the CNI?")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got, want := RenderReply(reply, visibleEscape), "> «byeNoted on the knowledge-base PR #7»"; got != want {
		t.Errorf("rendered reply = %q, want %q — model prose must stay inside its own span", got, want)
	}
}

// TestHandleUnanchoredNoteWithoutChatIsFreeform pins the half of the widened
// grammar that only holds with a model wired.
//
// Matching "note:" as a whole token ANYWHERE in a message is a COST argument:
// with model.chat on, "please note: …" would otherwise reach the model and be
// billed, while matching it deterministically is free. That argument does not
// exist when chat is off — the default — and there the widening is a pure
// behavioural change: "@runlore the runbook note: link is stale" used to
// answer "I didn't record that" and instead opened a real knowledge-base PR
// containing "link is stale", spending the thread's note allowance and a
// global forge write on a sentence nobody asked to record.
//
// So the anywhere-match is the chat layer's rule. With no Chat, "note:" has to
// be at the START of the message (after mentions are stripped), exactly as it
// was before the chat layer existed.
func TestHandleUnanchoredNoteWithoutChatIsFreeform(t *testing.T) {
	for _, raw := range []string{
		"<@U0BOT> the runbook note: link is stale",
		"<@U0BOT> please note: the cause was a spot reclaim",
		"hey <@U0BOT> note: the cause was a spot reclaim",
		":wave: <@U0BOT> note: the cause was a spot reclaim",
		"hey runlore note: the cause was X",
	} {
		t.Run(raw, func(t *testing.T) {
			f := &fakeForge{}
			r := newTestResponder(t, f)
			if r.Chat != nil {
				t.Fatal("test setup: newTestResponder must leave Chat nil")
			}
			tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
			if err := r.Registry.Put(tc); err != nil {
				t.Fatalf("Put: %v", err)
			}

			reply, err := r.Handle(context.Background(), tc, "alice", raw)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if c, o := f.counts(); c != 0 || o != 0 {
				t.Errorf("forge calls = %d comments / %d opened, want 0/0 — a mid-sentence note: must not write with no chat layer configured", c, o)
			}
			if reply != FreeformNotRecordedReply {
				t.Errorf("reply = %q, want FreeformNotRecordedReply", reply)
			}
			got, ok := r.Registry.Get("111.222")
			if !ok {
				t.Fatal("Get: thread missing from the registry")
			}
			if got.Notes != 0 {
				t.Errorf("Notes = %d, want 0 — nothing was written, so nothing may be charged", got.Notes)
			}
		})
	}
}

// TestHandleAnchoredNoteWritesWithOrWithoutChat is the control on the other
// side: an explicit note at the start of the message is the deterministic
// capture path, and it is byte-identical whether or not a model is configured.
func TestHandleAnchoredNoteWritesWithOrWithoutChat(t *testing.T) {
	for _, raw := range []string{
		"note: the cause was a spot reclaim",
		"<@U0BOT> note: the cause was a spot reclaim",
		"<@U0BOT> <@U0HUMAN> Note: the cause was a spot reclaim",
	} {
		for _, withChat := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chat=%v", raw, withChat), func(t *testing.T) {
				f := &fakeForge{}
				model := &fakeChatModel{resp: wellFormedReply("should never be called", "")}
				r := newTestResponder(t, f)
				if withChat {
					r.Chat = &Chat{Model: model, Log: silentLog()}
				}
				tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
				if err := r.Registry.Put(tc); err != nil {
					t.Fatalf("Put: %v", err)
				}
				if _, err := r.Handle(context.Background(), tc, "alice", raw); err != nil {
					t.Fatalf("Handle: %v", err)
				}
				if model.calls != 0 {
					t.Errorf("Complete called %d times, want 0 — an anchored note costs no tokens", model.calls)
				}
				if len(f.comments) != 1 {
					t.Fatalf("comments = %d, want 1 — an anchored note is recorded either way", len(f.comments))
				}
				if !strings.Contains(f.comments[0].body, "the cause was a spot reclaim") {
					t.Errorf("the note text must be the words after the prefix:\n%s", f.comments[0].body)
				}
			})
		}
	}
}

// TestHandleUnanchoredReinvestigateIsStillRefusedWithoutChat keeps the two
// prefixes' rules apart. "reinvestigate:" matches anywhere unconditionally
// because it is a REFUSAL, not a write: a false positive costs a message the
// human can act on, never a forge write or a token spend. Only "note:" — the
// one that writes — is narrowed when there is no chat layer to justify the
// widening.
func TestHandleUnanchoredReinvestigateIsStillRefusedWithoutChat(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> please reinvestigate: the CNI")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply != ReinvestigateNotSupportedReply {
		t.Errorf("reply = %q, want ReinvestigateNotSupportedReply", reply)
	}
	if c, o := f.counts(); c != 0 || o != 0 {
		t.Errorf("forge calls = %d comments / %d opened, want 0/0", c, o)
	}
}
