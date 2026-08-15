// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
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
