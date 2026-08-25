// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/okf"
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
	// appends records every AppendToEntryOnPR call — the route a note takes onto
	// the standalone PR thread capture itself opened, where the note has to reach
	// the ENTRY FILE rather than the PR conversation the catalog never indexes.
	//
	// Recorded BEFORE appendErr is consulted, deliberately. A fake that returned
	// the error without recording the call could not represent the failure that
	// matters most here — the write LANDED and its response was lost — so a test
	// named for the fallback could never observe the double-write that fallback
	// then causes. The forge clients close that case by re-reading the entry
	// (appendLanded); a fake unable to express it would pass either way.
	appends []struct {
		number int
		body   string
		key    string
	}
	opened  []providers.KBEntry
	openURL string
	openErr error
	commErr error
	// appendErr, when set, fails every AppendToEntryOnPR — used to pin the
	// degrade-to-a-comment fallback, so a forge that cannot rewrite the entry
	// still keeps the human's words somewhere. The call is still recorded, so a
	// test can tell "it never ran" from "it ran and reported failure".
	appendErr error
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

func (f *fakeForge) AppendToEntryOnPR(_ context.Context, number int, body, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends = append(f.appends, struct {
		number int
		body   string
		key    string
	}{number, body, key})
	return f.appendErr
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

// counts returns the number of writes onto an EXISTING pull request (comments
// plus entry appends) and the number of PRs opened so far, taken under the lock
// so a concurrency test can read them safely.
//
// The two existing-PR routes are summed rather than reported separately because
// every caller of this asks the same question — "did the second writer reuse
// the first writer's PR instead of opening another one?" — and the answer must
// not depend on which of the two ways it recorded the note.
func (f *fakeForge) counts() (comments, opened int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments) + len(f.appends), len(f.opened)
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

// TestHandleEveryNoteAfterTheFirstReachesTheEntry closes issue #493, and it is
// the whole point of the append route.
//
// The behaviour it replaced: the first note opened a standalone Concept PR and
// every later note was a COMMENT on it. Only the first note was ever in the
// entry file, so merging the PR gave the catalog one entry holding one note
// while the rest stayed behind as pull-request conversation — never indexed,
// never returned by kb_search, never recalled. Four notes went in and one came
// out, and the field report that produced this issue was exactly that: three
// notes lost on one thread.
//
// So the assertion is not "the second note reused the first note's PR" (the old
// test asserted that much and passed throughout the bug). It is that the note
// reached the ENTRY, which is the only part of that pull request the catalog
// gains on merge — hence the body check, not merely a call count.
func TestHandleEveryNoteAfterTheFirstReachesTheEntry(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.MaxNotesPerThread = 4 // the four notes of the field report this issue came from
	tc := Context{Root: "111.222", Title: "OOM"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: first"); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	notes := []string{"second", "third", "fourth"}
	for _, text := range notes {
		refreshed, _ := r.Registry.Get("111.222")
		reply, err := r.Handle(context.Background(), refreshed, "bob", "note: "+text)
		if err != nil {
			t.Fatalf("Handle %q: %v", text, err)
		}
		// The acknowledgement has to say which of the two it did: "noted ON the
		// PR" and "added to the ENTRY" are different promises about whether the
		// knowledge survives the merge, and the human reading it is the only one
		// who can tell RunLore it picked wrong.
		if !strings.Contains(reply, "Added to the knowledge-base entry on PR #99") {
			t.Errorf("the reply for %q must say the note went into the entry: %q", text, reply)
		}
	}

	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want exactly 1 — a thread opens at most one standalone PR", len(f.opened))
	}
	if len(f.comments) != 0 {
		t.Fatalf("comments = %d, want 0 — a note on RunLore's OWN note PR belongs in the entry, "+
			"not in a conversation the catalog never indexes: %+v", len(f.comments), f.comments)
	}
	if len(f.appends) != len(notes) {
		t.Fatalf("appends = %d, want %d — every note after the first must reach the entry", len(f.appends), len(notes))
	}
	for i, text := range notes {
		if f.appends[i].number != 99 {
			t.Errorf("note %q appended to PR %d, want 99 (the first note's PR)", text, f.appends[i].number)
		}
		if !strings.Contains(f.appends[i].body, text) {
			t.Errorf("the entry append for note %q lost its text: %s", text, f.appends[i].body)
		}
	}
}

// TestHandleAppendFailureFallsBackToCommenting pins the degradation. An append
// is several forge calls against a branch and can fail for reasons that have
// nothing to do with the human — a push race, a transient 5xx, a reviewer who
// renamed the entry file. Losing their words to any of those would be worse
// than the bug #493 fixed, so the note still lands, as a comment, and the reply
// still tells them where.
func TestHandleAppendFailureFallsBackToCommenting(t *testing.T) {
	f := &fakeForge{appendErr: errors.New("409 conflict")}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", NoteURL: "https://github.com/o/r/pull/77"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "note: spot reclaim")
	if err != nil {
		t.Fatalf("a failed append must degrade to a comment, not to an error: %v", err)
	}
	// The append must have been ATTEMPTED. Without this the test passes just as
	// well against a responder that never tries the entry at all — which is the
	// bug #493 fixed, reintroduced and reported as a fallback.
	if len(f.appends) != 1 {
		t.Fatalf("appends = %d, want 1 — the comment is a FALLBACK, reached only after trying the entry", len(f.appends))
	}
	if len(f.comments) != 1 || f.comments[0].number != 77 {
		t.Fatalf("the note must still land as a comment; comments = %+v", f.comments)
	}
	if len(f.opened) != 0 {
		t.Fatalf("a failed append must not escalate to opening another PR; opened = %d", len(f.opened))
	}
	if !strings.Contains(reply, "77") {
		t.Errorf("the reply must still name where the note landed: %q", reply)
	}
}

// TestHandleAppendOntoAClosedPRDoesNotFallBackToCommenting is the ONE append
// failure that must not degrade to a comment.
//
// The open-check and the write are two round trips, and a reviewer merging a
// note PR while an on-call is typing the next note falls between them. Past that
// merge both remaining options are silent losses — a commit onto a merged
// branch never reaches base, and a comment on a merged PR is never indexed by
// the catalog — so the forge reports providers.ErrPRNotOpen and this must be
// handled exactly like the open-check's own closed-PR case: open a standalone
// entry. A note arriving a second too late must not be lost by a route the same
// note a second earlier survives.
func TestHandleAppendOntoAClosedPRDoesNotFallBackToCommenting(t *testing.T) {
	f := &fakeForge{appendErr: fmt.Errorf("github PR 77 is %q: %w", "merged", providers.ErrPRNotOpen)}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", NoteURL: "https://github.com/o/r/pull/77"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "note: spot reclaim")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 {
		t.Fatalf("must never comment onto a PR that closed under the write; comments = %+v", f.comments)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want 1 — a finished PR means a standalone entry, the same answer the open-check gives", len(f.opened))
	}
	if !strings.Contains(reply, "99") {
		t.Errorf("the reply must name the standalone PR it opened: %q", reply)
	}
}

// TestNoteKeyIsStableAcrossARetryAndUniquePerNote pins the idempotency key the
// entry append is made safe with.
//
// Stable, because the deliveries above this layer replay and a replayed APPEND
// is permanent duplicate catalog content (a replayed comment merely looked
// silly). The obvious key — a hash of the rendered body — does NOT have this
// property, which is the whole reason the key is computed here: the body carries
// the provenance timestamp, so the replay of one note renders different bytes
// and would sail straight past its own marker.
//
// Unique, because a key that collided would suppress a genuinely different note
// while telling its author it was saved.
func TestNoteKeyIsStableAcrossARetryAndUniquePerNote(t *testing.T) {
	tc := Context{Root: "111.222"}
	base := HumanNote("alice", "it was a spot reclaim")

	if got, want := noteKey(tc, base), noteKey(tc, base); got != want {
		t.Fatalf("noteKey is not deterministic: %q vs %q", got, want)
	}
	// The retry: same thread, same author, same words, a later attempt. The KEY
	// must not move even though the rendered body does.
	early := NoteBody(tc, base, noteAt, DefaultMaxNoteBytes)
	late := NoteBody(tc, base, noteAt.Add(9*time.Minute), DefaultMaxNoteBytes)
	if early == late {
		t.Fatal("test is not exercising the case — the rendered body must differ between attempts")
	}
	// The key is computed from tc and n alone, neither of which the timestamp
	// touches, so the determinism check above IS the survival property: a key
	// derived from `early` and one derived from `late` could not both equal it.
	if strings.Contains(early, noteKey(tc, base)) || strings.Contains(late, noteKey(tc, base)) {
		t.Error("the key must not be a function of the rendered body — that is what breaks across a retry")
	}

	seen := map[string]string{}
	for name, n := range map[string]struct {
		tc Context
		n  Note
	}{
		"the note":         {tc, base},
		"different text":   {tc, HumanNote("alice", "it was the CNI")},
		"different author": {tc, HumanNote("bob", "it was a spot reclaim")},
		"different thread": {Context{Root: "333.444"}, base},
	} {
		k := noteKey(n.tc, n.n)
		if other, dup := seen[k]; dup {
			t.Errorf("%q and %q share a key — one of them would be silently dropped", name, other)
		}
		seen[k] = name
	}
	// The model-drafted note carries the same TEXT as the human one, so it shares
	// the key deliberately: it is the same sentence landing in the same entry, and
	// filing it twice is exactly what the marker exists to prevent.
	if noteKey(tc, base) != noteKey(tc, ProposedNote("alice", "what happened?", "it was a spot reclaim")) {
		t.Error("identical filed text in one thread must share a key regardless of which route proposed it")
	}
}

// TestHandlePassesAStableKeyToTheForge is the wiring half: the key the forge is
// given must be the one noteKey computes, and it must repeat across a replayed
// delivery. A responder that passed "" (or a fresh value per call) would compile,
// pass every other test here, and leave the append with no idempotency at all.
func TestHandlePassesAStableKeyToTheForge(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", NoteURL: "https://github.com/o/r/pull/77"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The same message delivered twice — what a restart or a wiped dedup set
	// produces upstream.
	for i := range 2 {
		cur, _ := r.Registry.Get("111.222")
		if _, err := r.Handle(context.Background(), cur, "alice", "note: spot reclaim"); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if len(f.appends) != 2 {
		t.Fatalf("appends = %d, want 2 — this test needs both deliveries to reach the forge", len(f.appends))
	}
	want := noteKey(tc, HumanNote("alice", "spot reclaim"))
	if f.appends[0].key != want {
		t.Errorf("key = %q, want %q — the forge cannot dedup a key this layer never sends", f.appends[0].key, want)
	}
	if f.appends[0].key != f.appends[1].key {
		t.Errorf("a replayed delivery sent a different key (%q then %q); the forge would append the note twice",
			f.appends[0].key, f.appends[1].key)
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

// fakeSilenceRecorder is a SilenceRecorder that records every call it
// receives and returns silenceErr, if set.
type fakeSilenceRecorder struct {
	calls []struct {
		triggerKey string
		window     time.Duration
		user       string
		at         time.Time
	}
	silenceErr error
}

func (f *fakeSilenceRecorder) Silence(triggerKey string, window time.Duration, user string, at time.Time) error {
	f.calls = append(f.calls, struct {
		triggerKey string
		window     time.Duration
		user       string
		at         time.Time
	}{triggerKey, window, user, at})
	return f.silenceErr
}

// TestHandleSilenceNotEnabledStillReplies pins the rule stated on
// Responder.silence: a command that changes behaviour must never fail
// quietly. With no SilenceRecorder wired, the command must be answered with
// an explanation rather than falling through to some other intent's reply.
//
// The Responder is SHARED across transports (the same value backs both
// Slack's thread capture, in mention.go, and Matrix's mention handler), and
// silence() has no way to tell which one a given message arrived on. The
// reply must therefore name the shared config block (notify.silence) rather
// than either transport's own flag — an earlier draft named
// notify.matrix.silence_reactions here, which told a Slack user to set a key
// that has nothing to do with their transport. This pins the exact wording
// and explicitly asserts neither transport's name leaks into it, so a future
// edit cannot silently reintroduce a transport-specific hint.
func TestHandleSilenceNotEnabledStillReplies(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", TriggerKey: "trig-1"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> silence: 4h")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	const want = "Silencing isn't enabled here — ask an operator to turn on `notify.silence` for this transport."
	if reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}
	lower := strings.ToLower(reply)
	for _, transportSpecific := range []string{"matrix", "slack"} {
		if strings.Contains(lower, transportSpecific) {
			t.Errorf("reply names a specific transport (%q), but silence() cannot tell which one a message "+
				"arrived on: %q", transportSpecific, reply)
		}
	}
	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Error("a silence command must never write to the knowledge base")
	}
}

// TestHandleSilenceNoTriggerKeyReplies covers a thread whose Context carries
// no incident identity — nothing for the ledger to key the suppression on.
func TestHandleSilenceNoTriggerKeyReplies(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	rec := &fakeSilenceRecorder{}
	r.Silence = rec
	tc := Context{Root: "111.222"} // no TriggerKey

	reply, err := r.Handle(context.Background(), tc, "alice", "silence: 4h")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Error("Silence must not be called when there is no trigger key to record against")
	}
	if !strings.Contains(strings.ToLower(reply), "incident") {
		t.Errorf("reply must explain there is nothing to silence: %q", reply)
	}
}

// TestHandleSilenceUnparseableDurationReplies covers free text that does not
// parse as a Go duration, and pins that the reply states the configured cap.
func TestHandleSilenceUnparseableDurationReplies(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	rec := &fakeSilenceRecorder{}
	r.Silence = rec
	r.SilenceMax = 24 * time.Hour
	tc := Context{Root: "111.222", TriggerKey: "trig-1"}

	reply, err := r.Handle(context.Background(), tc, "alice", "silence: not-a-duration")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Error("Silence must not be called with an unparseable duration")
	}
	if !strings.Contains(reply, "up to 24h)") {
		t.Errorf("reply must state the configured cap as an operator writes it: %q", reply)
	}
	if strings.Contains(reply, "0m0s") {
		t.Errorf("reply renders the cap as a Go duration string: %q", reply)
	}
}

// TestHandleSilenceLedgerErrorIsReported covers the ledger refusing the
// window (e.g. over cap) — the human must see why, not a bare "ok".
func TestHandleSilenceLedgerErrorIsReported(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	rec := &fakeSilenceRecorder{silenceErr: errors.New("window exceeds notify.silence.max_window")}
	r.Silence = rec
	tc := Context{Root: "111.222", TriggerKey: "trig-1"}

	reply, err := r.Handle(context.Background(), tc, "alice", "silence: 999h")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(reply, "window exceeds notify.silence.max_window") {
		t.Errorf("reply must surface the ledger's own error: %q", reply)
	}
}

// TestHandleSilenceRecordsAndAcks is the success path: the ledger is called
// with exactly the parsed window and the reply is the shared SilenceAck text
// — the same one Slack's silence button produces, by construction.
func TestHandleSilenceRecordsAndAcks(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	rec := &fakeSilenceRecorder{}
	r.Silence = rec
	tc := Context{Root: "111.222", TriggerKey: "trig-1"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> silence: 4h")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("Silence calls = %d, want 1", len(rec.calls))
	}
	got := rec.calls[0]
	if got.triggerKey != "trig-1" || got.window != 4*time.Hour || got.user != "alice" || !got.at.Equal(noteAt) {
		t.Errorf("Silence called with %+v", got)
	}
	want := SilenceAck("alice", 4*time.Hour, noteAt.Add(4*time.Hour), r.FeedbackEnabled)
	if reply != want {
		t.Errorf("reply = %q, want the shared SilenceAck text %q", reply, want)
	}
	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Error("a silence command must never write to the knowledge base")
	}
}

// TestHandleUnanchoredSilenceNeverWrites pins the guard IntentNote already had
// and IntentSilence did not. Parse scans "silence:" before "note:" and matches it
// as a whole token ANYWHERE, so "note: we agreed on silence: 4h" parsed as
// IntentSilence with Text "4h" — and Handle routed it straight to r.silence,
// which wrote the ledger event and acked a suppression. The operator's note was
// never written and investigation was switched off for four hours by a sentence
// meant as prose.
//
// Parse's justification for the anywhere-match is that "the outcome is a refusal
// that writes nothing and spends nothing". True for reinvestigate:, false for
// silence:, which both writes and changes behaviour — so the unanchored case must
// be refused here, exactly as an unanchored note: is.
func TestHandleUnanchoredSilenceNeverWrites(t *testing.T) {
	for _, raw := range []string{
		"<@U0BOT> note: we agreed on silence: 4h",
		"the runbook says to silence: 4h next time",
		"hey <@U0BOT> please silence: 4h",
	} {
		t.Run(raw, func(t *testing.T) {
			f := &fakeForge{}
			r := newTestResponder(t, f)
			rec := &fakeSilenceRecorder{}
			r.Silence = rec
			r.SilenceMax = 24 * time.Hour
			tc := Context{Root: "111.222", TriggerKey: "trig-1", CuratedURL: "https://github.com/o/r/pull/42"}

			reply, err := r.Handle(context.Background(), tc, "alice", raw)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(rec.calls) != 0 {
				t.Errorf("Silence was called %d time(s) for an unanchored command: %+v", len(rec.calls), rec.calls)
			}
			if len(f.comments) != 0 || len(f.opened) != 0 {
				t.Error("an unanchored silence: must not write to the knowledge base either")
			}
			if reply != SilenceNotAnchoredReply {
				t.Errorf("reply = %q, want SilenceNotAnchoredReply %q", reply, SilenceNotAnchoredReply)
			}
		})
	}
}

// TestHandleAnchoredSilenceStillWorks is the other half: the guard must not
// disarm the command itself, including after a stripped mention.
func TestHandleAnchoredSilenceStillWorks(t *testing.T) {
	for _, raw := range []string{"silence: 4h", "<@U0BOT> silence: 4h", "<@U0BOT> <@U1> SILENCE: 4h"} {
		t.Run(raw, func(t *testing.T) {
			f := &fakeForge{}
			r := newTestResponder(t, f)
			rec := &fakeSilenceRecorder{}
			r.Silence = rec
			tc := Context{Root: "111.222", TriggerKey: "trig-1"}

			if _, err := r.Handle(context.Background(), tc, "alice", raw); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(rec.calls) != 1 {
				t.Fatalf("Silence calls = %d, want 1", len(rec.calls))
			}
		})
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
	// Appended, not commented: NoteURL is RunLore's own note PR whichever way the
	// routing reached it, so falling back to it must not fall back to the lossy
	// route as well.
	if len(f.appends) != 1 || f.appends[0].number != 77 {
		t.Fatalf("must fall back to the still-open NoteURL's entry; appends = %+v, comments = %+v", f.appends, f.comments)
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

// TestHandleNeverRewritesTheCuratorsEntry is the half of issue #493 that must
// NOT change, and it is the one with a real cost if it ever does.
//
// A curated PR has an author who is not RunLore, a body they wrote, and a review
// in progress. A note there is FEEDBACK on their draft — a human decides at
// merge time what of it belongs in the entry — so commenting is correct, and it
// stays correct however many notes a thread produces. Appending would have
// RunLore silently rewriting a human's file under them, mid-review, on the word
// of anyone who can type in a chat channel.
//
// Every note in the thread is checked, not only the first: the routing decision
// is taken per write, so a rule that held once and drifted on the second note
// would be exactly as damaging.
func TestHandleNeverRewritesTheCuratorsEntry(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, text := range []string{"first", "second", "third"} {
		cur, _ := r.Registry.Get("111.222")
		reply, err := r.Handle(context.Background(), cur, "alice", "note: "+text)
		if err != nil {
			t.Fatalf("Handle %q: %v", text, err)
		}
		if !strings.Contains(reply, "Noted on the knowledge-base PR #42") {
			t.Errorf("the reply for %q must say the note was recorded ON the PR, not written into it: %q", text, reply)
		}
	}
	if len(f.appends) != 0 {
		t.Fatalf("RunLore must never rewrite an entry somebody else drafted; appends = %+v", f.appends)
	}
	if len(f.comments) != 3 {
		t.Fatalf("comments = %d, want 3 — every note on a curated PR is review feedback", len(f.comments))
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
// operator could see throttling but not volume. EVERY landing route must
// increment ThreadNotesWritten, distinguished by the route label — including
// the append route, which was added precisely because nothing an operator could
// graph distinguished a note that becomes knowledge from one that does not.
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

	appendTC := Context{Root: "c", NoteURL: "https://github.com/o/r/pull/77"}
	if err := r.Registry.Put(appendTC); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Handle(context.Background(), appendTC, "carol", "note: via entry append"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.appends) != 1 {
		t.Fatalf("appends = %d, want 1 — this case is only meaningful if it took the append route", len(f.appends))
	}

	got, ok := read("runlore_thread_notes_written_total")
	if !ok || got != 3 {
		t.Errorf("runlore_thread_notes_written_total = %d (ok=%v), want 3 (one per landed route)", got, ok)
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
//
// The expectation GREW when the reply began quoting what it recorded
// (recordedBlock): this fixture's model proposes a note, so the reply now
// carries two more of each kind of byte — modelDraftedNotice, which is
// RunLore's own claim and stays outside every span, and the quoted note, which
// the model wrote and is inside one. That is the same boundary, over more of
// the message, not a weakened one.
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
		"📝 Noted on the knowledge-base PR #42 — «https://github.com/o/r/pull/42»\n" +
		"Drafted by RunLore from your message — not your own words, pending review:\n" +
		"> «the real cause was a spot reclaim»"
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

// putContext stores tc so write()'s post-lock registry re-read sees it, and
// returns it. write() is called directly in the TestWriteReports* group below:
// what it RETURNS is the whole subject, and record() currently discards
// everything but the reply.
func putContext(t *testing.T, r *Responder, tc Context) Context {
	t.Helper()
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return tc
}

// TestWriteReportsTheCommentRoute pins what write() hands back on the
// comment-on-a-linked-PR route: which route ran, which pull request took the
// note, its URL, and the text as filed. No caller could learn any of that from
// the (msg, landed, err) shape this replaced.
//
// The reply string is asserted byte-for-byte on purpose: growing the return
// value must not change a single byte of what the human is told.
func TestWriteReportsTheCommentRoute(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	const prURL = "https://github.com/o/r/pull/42"
	tc := putContext(t, r, Context{Root: "111.222", Title: "OOM", CuratedURL: prURL})

	reply, w, err := r.write(context.Background(), tc, HumanNote("alice", "spot reclaim, not OOM"), noteAt)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if w == nil {
		t.Fatal("write reported no result for a note that landed on PR #42")
	}
	if w.Route != RouteComment {
		t.Errorf("Route = %q, want %q", w.Route, RouteComment)
	}
	if w.PR != 42 {
		t.Errorf("PR = %d, want 42", w.PR)
	}
	if w.URL != prURL {
		t.Errorf("URL = %q, want %q", w.URL, prURL)
	}
	if w.Note != "spot reclaim, not OOM" {
		t.Errorf("Note = %q, want the note text as filed", w.Note)
	}
	// The comment route adds to an entry that already exists; it generates no
	// title of its own, and inventing one would name something nobody wrote.
	if w.Title != "" {
		t.Errorf("Title = %q, want empty on the comment route — no entry was generated", w.Title)
	}
	if want := "📝 Noted on the knowledge-base PR #42 — " + Untrusted(prURL); reply != want {
		t.Errorf("reply = %q, want %q — this change grows what write RETURNS, never what it says", reply, want)
	}
}

// TestWriteReportsTheOpenPRRoute pins the standalone-PR route, including the
// entry title ConceptEntry generated — the one value the human cannot see
// anywhere else without opening the pull request.
func TestWriteReportsTheOpenPRRoute(t *testing.T) {
	t.Run("numbered URL", func(t *testing.T) {
		f := &fakeForge{openURL: "https://github.com/o/r/pull/99"}
		r := newTestResponder(t, f)
		tc := putContext(t, r, Context{Root: "111.222", Title: "OOM in payments"})

		reply, w, err := r.write(context.Background(), tc, HumanNote("alice", "stale since Karpenter"), noteAt)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if w == nil {
			t.Fatal("write reported no result for a note that opened PR #99")
		}
		if w.Route != RouteOpenPR {
			t.Errorf("Route = %q, want %q", w.Route, RouteOpenPR)
		}
		if w.PR != 99 {
			t.Errorf("PR = %d, want 99", w.PR)
		}
		if w.URL != "https://github.com/o/r/pull/99" {
			t.Errorf("URL = %q, want the forge's own Ref URL", w.URL)
		}
		if len(f.opened) != 1 {
			t.Fatalf("opened = %d, want 1", len(f.opened))
		}
		if w.Title != f.opened[0].Title {
			t.Errorf("Title = %q, want %q — the title the entry was actually filed under", w.Title, f.opened[0].Title)
		}
		if w.Title == "" {
			t.Error("Title is empty; the open_pr route generates one and must report it")
		}
		if w.Note != "stale since Karpenter" {
			t.Errorf("Note = %q, want the note text as filed", w.Note)
		}
		if want := "📝 Opened knowledge-base PR #99 with your note — " + Untrusted("https://github.com/o/r/pull/99"); reply != want {
			t.Errorf("reply = %q, want %q", reply, want)
		}
	})

	// A Ref URL with no pull-request number in it is not an error — the write
	// landed, and the human still gets the URL. The result must say the same:
	// everything known, and 0 for the one thing that is not.
	t.Run("unnumbered URL", func(t *testing.T) {
		f := &fakeForge{openURL: "https://github.com/o/r/kb"}
		r := newTestResponder(t, f)
		tc := putContext(t, r, Context{Root: "111.222", Title: "OOM"})

		reply, w, err := r.write(context.Background(), tc, HumanNote("alice", "note text"), noteAt)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if w == nil {
			t.Fatal("write reported no result for a note that opened a PR")
		}
		if w.PR != 0 {
			t.Errorf("PR = %d, want 0 — the Ref URL carries no number", w.PR)
		}
		if w.URL != "https://github.com/o/r/kb" {
			t.Errorf("URL = %q, want the forge's own Ref URL", w.URL)
		}
		if w.Route != RouteOpenPR {
			t.Errorf("Route = %q, want %q", w.Route, RouteOpenPR)
		}
		if want := "📝 Opened a knowledge-base PR with your note — " + Untrusted("https://github.com/o/r/kb"); reply != want {
			t.Errorf("reply = %q, want %q", reply, want)
		}
	})
}

// TestWriteReportsTheNoteAsWritten is the security-relevant assertion of the
// group: what write() reports is the text that REACHED THE FORGE — redacted
// and capped — not the caller's raw input. A caller that quotes it back into a
// chat message (PR4's whole point) must not be able to unmask what the entry
// masked, nor republish the bytes the cap dropped.
//
// One message carries both hazards at once, deliberately: a secret AND more
// bytes than the cap allows. Separately they would each pass against an
// implementation that applied only the other transform.
func TestWriteReportsTheNoteAsWritten(t *testing.T) {
	const secret = "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWX"
	// No "![", no "<" and no "#": those are the FORGE-markdown defusals, which
	// run downstream of the shared redact+cap value (see
	// TestWriteReportsTheHumansTextNotItsForgeMarkdownDefusal). Keeping them out
	// of this input makes noteText an identity beyond redact+cap, so the
	// Contains check below is a byte-identity check on the whole block.
	raw := "the deploy token " + secret + " leaked into the log; " + strings.Repeat("padding ", 60)

	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.MaxNoteBytes = 200
	const prURL = "https://github.com/o/r/pull/42"
	tc := putContext(t, r, Context{Root: "111.222", Title: "OOM", CuratedURL: prURL})

	_, w, err := r.write(context.Background(), tc, HumanNote("alice", raw), noteAt)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if w == nil {
		t.Fatal("write reported no result for a note that landed")
	}
	if strings.Contains(w.Note, secret) || strings.Contains(w.Note, "ghp_") {
		t.Errorf("the reported note still carries the secret — it must be the redacted text:\n%q", w.Note)
	}
	if !strings.Contains(w.Note, "[REDACTED]") {
		t.Errorf("the reported note carries no mask where the secret was:\n%q", w.Note)
	}
	if len(w.Note) > r.MaxNoteBytes {
		t.Errorf("the reported note is %d bytes, want at most MaxNoteBytes (%d)", len(w.Note), r.MaxNoteBytes)
	}
	if !strings.Contains(w.Note, "truncated") {
		t.Errorf("a cut note must be reported as cut, exactly as it is filed:\n%q", w.Note)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	// Byte-identical to what reached the forge: the reported string appears
	// verbatim inside the body that was actually written.
	if !strings.Contains(f.comments[0].body, w.Note) {
		t.Errorf("the reported note is not byte-identical to the text written to the forge.\nreported:\n%q\nbody:\n%s", w.Note, f.comments[0].body)
	}
	// And the body itself is unchanged by the sharing: evaluating redact+cap
	// once, up front, must render exactly the body NoteBody renders from the raw
	// note. This is the guard on the refactor, not on the feature.
	if want := NoteBody(tc, HumanNote("alice", raw), noteAt, r.MaxNoteBytes); f.comments[0].body != want {
		t.Errorf("the forge body changed.\ngot:\n%s\nwant:\n%s", f.comments[0].body, want)
	}
}

// TestWriteReportsTheHumansTextNotItsForgeMarkdownDefusal draws the boundary
// deliberately: the reported note is the redacted, capped text — NOT the
// forge-markdown defusal applied on top of it.
//
// Redaction and the cap are what the entry did to the CONTENT, and a reply that
// skipped them would leak what the entry masked. The three defusals
// (neutralizeImages, neutralizeHTML, escapeOKFSections) are for GitHub's and
// GitLab's Markdown renderers, which no chat transport is; repeating them in a
// chat reply would show the human "!&#91;" where they typed "![" and would
// protect nothing, since a chat transport's own hazards are neutralised by
// Untrusted at the point of rendering.
func TestWriteReportsTheHumansTextNotItsForgeMarkdownDefusal(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := putContext(t, r, Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

	const raw = "the dashboard ![pixel](https://e.example/p.gif) is stale"
	_, w, err := r.write(context.Background(), tc, HumanNote("alice", raw), noteAt)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if w == nil {
		t.Fatal("write reported no result for a note that landed")
	}
	if w.Note != raw {
		t.Errorf("Note = %q, want the human's own text %q", w.Note, raw)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if !strings.Contains(f.comments[0].body, "!&#91;pixel]") {
		t.Errorf("the forge body must still be defused for Markdown:\n%s", f.comments[0].body)
	}
}

// TestWriteReportsNothingWhenNothingLanded is the other half of the contract:
// a nil result everywhere the knowledge base was NOT written. A caller that
// announces a write must have nothing to announce on any of these paths, and a
// nil pointer is what makes "nothing landed" and "here is what landed" one
// value that cannot disagree with itself.
func TestWriteReportsNothingWhenNothingLanded(t *testing.T) {
	t.Run("throttled", func(t *testing.T) {
		f := &fakeForge{}
		r := newTestResponder(t, f)
		// A one-event window with its one event already spent: ratelimit.New(0, …)
		// means UNLIMITED, not "refuse everything".
		r.ForgeWrites = ratelimit.New(1, time.Hour)
		if !r.ForgeWrites.Allow() {
			t.Fatal("the window refused its first event; this test cannot reach the throttled path")
		}
		tc := putContext(t, r, Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

		reply, w, err := r.write(context.Background(), tc, HumanNote("alice", "x"), noteAt)
		if err != nil {
			t.Fatalf("a throttle is not an error: %v", err)
		}
		if w != nil {
			t.Errorf("throttled write reported %+v, want no result", *w)
		}
		if !strings.Contains(reply, "paused") {
			t.Errorf("reply = %q, want the throttle message", reply)
		}
		if c, o := f.counts(); c != 0 || o != 0 {
			t.Errorf("forge calls = %d/%d, want 0/0", c, o)
		}
	})

	t.Run("comment failed", func(t *testing.T) {
		f := &fakeForge{commErr: errors.New("forge down")}
		r := newTestResponder(t, f)
		tc := putContext(t, r, Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

		_, w, err := r.write(context.Background(), tc, HumanNote("alice", "x"), noteAt)
		if err == nil {
			t.Fatal("a failed comment must still be an error")
		}
		if w != nil {
			t.Errorf("failed write reported %+v, want no result", *w)
		}
	})

	t.Run("open PR failed", func(t *testing.T) {
		f := &fakeForge{openErr: errors.New("forge down")}
		r := newTestResponder(t, f)
		tc := putContext(t, r, Context{Root: "111.222"})

		_, w, err := r.write(context.Background(), tc, HumanNote("alice", "x"), noteAt)
		if err == nil {
			t.Fatal("a failed OpenPR must still be an error")
		}
		if w != nil {
			t.Errorf("failed write reported %+v, want no result", *w)
		}
	})

	t.Run("open-check failed", func(t *testing.T) {
		f := &fakeForge{prOpenErr: errors.New("forge unreachable")}
		r := newTestResponder(t, f)
		tc := putContext(t, r, Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

		_, w, err := r.write(context.Background(), tc, HumanNote("alice", "x"), noteAt)
		if err == nil {
			t.Fatal("an unreachable forge must still be an error")
		}
		if w != nil {
			t.Errorf("failed write reported %+v, want no result", *w)
		}
	})

	// The per-thread cap is enforced in record(), UPSTREAM of write(), so a
	// capped note produces no result for the simplest possible reason: write()
	// never runs and the forge is never touched. Asserted here rather than
	// assumed, because that placement is what makes the cap a real ceiling on
	// anything a caller could later announce.
	t.Run("capped", func(t *testing.T) {
		f := &fakeForge{}
		r := newTestResponder(t, f)
		r.MaxNotesPerThread = 1
		tc := putContext(t, r, Context{Root: "111.222", Notes: 1, CuratedURL: "https://github.com/o/r/pull/42"})

		reply, err := r.record(context.Background(), tc, HumanNote("alice", "x"))
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		if !strings.Contains(reply, "note limit") {
			t.Errorf("reply = %q, want the per-thread cap message", reply)
		}
		if c, o := f.counts(); c != 0 || o != 0 {
			t.Errorf("forge calls = %d/%d, want 0/0 — a capped note never reaches write()", c, o)
		}
	})
}

// untrustedSpans returns the CONTENT of every span Untrusted marked in reply,
// in order — the test-side inverse of RenderReply. It is what a transport's
// escape function is handed, and nothing else: everything RunLore wrote itself
// falls on the even indices and is dropped here.
func untrustedSpans(reply string) []string {
	parts := strings.Split(reply, untrustedMark)
	spans := make([]string, 0, len(parts)/2)
	for i := 1; i < len(parts); i += 2 {
		spans = append(spans, parts[i])
	}
	return spans
}

// TestReplyQuotesTheRecordedNote is the feature: after a note lands the human
// is told WHAT was filed, not only where it went.
//
// Before this, the reply was a PR number and a URL. On the model-drafted route
// that meant a human read "Opened knowledge-base PR #12 with your note" about
// text RunLore's own model wrote in their name, and could not see a word of it
// without leaving the thread. The `note:` route was barely better: a human
// whose message was redacted or cut had no way to see that from the reply.
//
// Both routes are asserted, and the `note:` one with the chat layer OFF — the
// default deployment. The whole rendered reply is pinned byte-for-byte rather
// than probed with Contains, because the SHAPE is the property: RunLore's
// status line at the left margin, the quote blockquoted under it, and the
// untrusted spans exactly where they belong (see
// TestReplyQuotesTheNoteAsUntrustedAndNothingElse).
func TestReplyQuotesTheRecordedNote(t *testing.T) {
	t.Run("comment route, note: with chat off", func(t *testing.T) {
		f := &fakeForge{}
		r := newTestResponder(t, f)
		if r.Chat != nil {
			t.Fatal("test setup: this case must run with the chat layer off")
		}
		tc := Context{Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"}
		if err := r.Registry.Put(tc); err != nil {
			t.Fatalf("Put: %v", err)
		}

		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: spot reclaim, not OOM")
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		// No "Entry:" line: the comment route adds to an entry someone else
		// already titled, and the pull request it joined is named in the status
		// line above. KBWrite.Title is empty here by design, not by omission.
		want := "📝 Noted on the knowledge-base PR #42 — «https://github.com/o/r/pull/42»\n" +
			"> «spot reclaim, not OOM»"
		if got := RenderReply(reply, visibleEscape); got != want {
			t.Errorf("rendered reply =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("open_pr route names the entry it created", func(t *testing.T) {
		f := &fakeForge{}
		r := newTestResponder(t, f)
		tc := Context{Root: "111.222", Title: "OOM in payments"}
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
		// The entry is named after the NOTE, not after the finding the thread was
		// about ("OOM in payments") — see noteEntryTitle.
		want := "📝 Opened knowledge-base PR #99 with your note — «https://github.com/o/r/pull/99»\n" +
			"Entry: «Operator note: stale since Karpenter»\n" +
			"> «stale since Karpenter»"
		if got := RenderReply(reply, visibleEscape); got != want {
			t.Errorf("rendered reply =\n%q\nwant\n%q", got, want)
		}
		// The name in the reply is the name the entry was actually filed under,
		// not a second rendering of the same inputs that could drift from it.
		if !strings.Contains(reply, f.opened[0].Title) {
			t.Errorf("the reply names %q, but the entry was filed as %q", reply, f.opened[0].Title)
		}
	})
}

// TestReplyQuotesWithinAPreviewCeiling pins the bound that makes quoting safe
// to do at all: the quote is capped by notePreviewBytes, INDEPENDENTLY of
// MaxNoteBytes.
//
// The two bound different things. MaxNoteBytes bounds what is written to the
// knowledge base — 8 KiB by default, and an operator may set it higher
// (config.Validate rejects only a negative one). Without a second ceiling here,
// a note at that cap would be echoed back verbatim as a chat message of the
// same size, in a thread, on a path any channel member can trigger.
//
// The fixture therefore sets MaxNoteBytes far ABOVE the default and files a
// note that is comfortably under it: nothing about this bound can come from the
// note cap, because the note cap never fires.
func TestReplyQuotesWithinAPreviewCeiling(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.MaxNoteBytes = 64 << 10
	// Trimmed: Parse trims the text it hands on, so a fixture ending in a space
	// would not appear verbatim in the body the forge received.
	note := strings.TrimSpace(strings.Repeat("spot reclaim ", 4000)) // ~52 KiB, under MaxNoteBytes
	if len(note) >= r.MaxNoteBytes {
		t.Fatalf("test setup: the note (%d bytes) must stay under MaxNoteBytes (%d) so the note cap never fires", len(note), r.MaxNoteBytes)
	}
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: "+note)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	// The forge still got the whole note: the ceiling bounds what is SAID, never
	// what is WRITTEN.
	if !strings.Contains(f.comments[0].body, note) {
		t.Error("the preview ceiling must not shorten what reaches the knowledge base")
	}
	for i, span := range untrustedSpans(reply) {
		if len(span) > notePreviewBytes {
			t.Errorf("untrusted span %d is %d bytes, want at most notePreviewBytes (%d)", i, len(span), notePreviewBytes)
		}
	}
	// Framing headroom over the ceiling: a status line, a URL and the truncation
	// notice, and nothing that scales with the note.
	rendered := RenderReply(reply, nil)
	if ceiling := notePreviewBytes + 256; len(rendered) > ceiling {
		t.Errorf("reply is %d bytes, want at most %d — a %d-byte note must not become a %d-byte chat message",
			len(rendered), ceiling, len(note), len(rendered))
	}
	if !strings.Contains(rendered, "truncated") {
		t.Errorf("a cut quote must say it was cut:\n%q", rendered)
	}
	if !strings.Contains(rendered, "spot reclaim") {
		t.Errorf("the preview must still carry the start of the note:\n%q", rendered)
	}
	// A note that FITS says nothing about truncation — the notice has to be a
	// signal, not decoration.
	t.Run("a short note is quoted whole", func(t *testing.T) {
		f := &fakeForge{}
		r := newTestResponder(t, f)
		tc := Context{Root: "333.444", CuratedURL: "https://github.com/o/r/pull/42"}
		if err := r.Registry.Put(tc); err != nil {
			t.Fatalf("Put: %v", err)
		}
		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: short and complete")
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if strings.Contains(reply, "truncated") {
			t.Errorf("an uncut quote must not claim truncation:\n%q", reply)
		}
	})
}

// TestNotePreviewCeilingHasItsDocumentedValue pins the NUMBER, the way
// config.TestThreadDefaultsHaveTheirDocumentedValues pins the thread defaults.
//
// The test above cannot. It asserts len(span) <= notePreviewBytes and bounds the
// whole reply at notePreviewBytes+256, so both sides of every comparison are
// recomputed from the constant under test and the assertion holds for ANY value:
// raise notePreviewBytes from 512 to 8192 and it still passes, while a 650-byte
// note that used to produce a ~700-byte reply now produces an 8,331-byte one —
// the 8 KiB chat post that whole test exists to make impossible.
//
// 512 is derived rather than picked (see notePreviewBytes' doc comment): every
// untrusted span is escaped before it goes on the wire and the widest escape,
// Slack's "&" to "&amp;", expands 5x, so this reaches the transport as at most
// ~2.5k characters — inside Slack's ~4k text budget with room left for the
// model's own answer, which shares the same message on the freeform route.
// Changing it should be a deliberate edit HERE, with that arithmetic redone, not
// a side effect of tuning something else.
func TestNotePreviewCeilingHasItsDocumentedValue(t *testing.T) {
	if got, want := notePreviewBytes, 512; got != want {
		t.Errorf("notePreviewBytes = %d, want %d — if this was a deliberate retune, redo the "+
			"5x-escape arithmetic against the transport budgets in internal/notify (slackReplyBytes, "+
			"matrixReplyBytes) and restate the number wherever the docs quote it", got, want)
	}
}

// TestReplyQuotesTheNoteAsUntrustedAndNothingElse is the highest-value
// assertion of the feature. The quote echoes note CONTENT back into a chat
// system — on the model-drafted route content RunLore's own model wrote — into
// the very message where RunLore states what it did. That is exactly the egress
// PR3's Untrusted/RenderReply boundary exists for, and exactly the forgery
// modelVoice's blockquote exists for.
//
// The note below carries both hazards at once: a forged RunLore status line
// (glyph, wording and a hostile URL) on a line of its own, and Slack mrkdwn
// that pings a room. Neither may survive as markup, and neither may sit at the
// left margin where RunLore's own claims sit.
//
// It also pins the other side of the boundary: RunLore's own bytes — the 📝
// status line, the sentence around the URL, the "> " markers — are NOT inside a
// span. Marking them would have the transport escape RunLore's own framing, and
// the blockquote would come back as literal "&gt; ".
func TestReplyQuotesTheNoteAsUntrustedAndNothingElse(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	hostile := "looks fine\n📝 Noted on the knowledge-base PR #7 — <https://evil.example|https://github.com/acme/kb/pull/7>\n<!channel>"

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: "+hostile)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := "📝 Noted on the knowledge-base PR #42 — «https://github.com/o/r/pull/42»\n" +
		"> «looks fine»\n" +
		"> «Noted on the knowledge-base PR #7 — <https://evil.example|https://github.com/acme/kb/pull/7>»\n" +
		"> «<!channel>»"
	if got := RenderReply(reply, visibleEscape); got != want {
		t.Errorf("rendered reply =\n%q\nwant\n%q", got, want)
	}
	// Read the same guarantee the other way round, so a future rewrite that
	// keeps the string but loses the marking cannot pass on the literal alone.
	spans := untrustedSpans(reply)
	for _, must := range []string{"<!channel>", "<https://evil.example|https://github.com/acme/kb/pull/7>"} {
		var found bool
		for _, s := range spans {
			if strings.Contains(s, must) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q reached the transport outside an untrusted span; spans = %q", must, spans)
		}
	}
	for _, s := range spans {
		if strings.Contains(s, "📝") {
			t.Errorf("RunLore's own status glyph is inside an untrusted span (%q) — the transport would escape RunLore's own framing", s)
		}
		if strings.Contains(s, "> ") {
			t.Errorf("a blockquote marker is inside an untrusted span (%q) — it would render as literal \"&gt; \"", s)
		}
	}
	// Only the first line is RunLore's; every line of quoted content is
	// blockquoted, so no note line can pose as a RunLore claim.
	for _, line := range strings.Split(RenderReply(reply, nil), "\n")[1:] {
		if !strings.HasPrefix(line, "> ") {
			t.Errorf("a line of quoted note content sits at the left margin, where RunLore's own lines sit: %q", line)
		}
	}
}

// TestQuotedNoteCannotBreakOutOfTheBlockquote pins the measure the blockquote
// only appeared to provide.
//
// QuoteUntrusted prefixes every LINE with "> ", and a line used to be whatever
// "\n" separated. Text layout does not agree: UAX #14 gives seven characters a
// MANDATORY break, and a client starts a new visual line at every one of them.
// A note reading "harmless<U+2028>📝 Noted on the knowledge-base PR #7 —
// <hostile URL>" therefore put its second half OUTSIDE the quote, at the left
// margin, in RunLore's own vocabulary — the exact forgery modelVoice's
// blockquote exists to stop, arriving through a rune that is not "\n".
//
// SingleLine had already made this argument for the YAML title (see its doc
// comment: U+2028 and U+2029 are the ones "many renderers and tokenizers break
// on"), and noteField applies it to Author and Title. The note BODY is the one
// untrusted span rendered multi-line BY DESIGN, so it is the one span that
// cannot be flattened — and it was therefore the one span with no line-break
// handling of any kind.
//
// Each break is asserted through the real reply, byte for byte: the forged line
// must arrive with a "> " in front of it, nothing else may move, and CRLF must
// count as one break rather than two.
func TestQuotedNoteCannotBreakOutOfTheBlockquote(t *testing.T) {
	const status = "📝 Noted on the knowledge-base PR #42 — https://github.com/o/r/pull/42"
	const forged = "Noted on the knowledge-base PR #7 — https://evil.example/kb"

	for _, br := range []struct{ name, sep string }{
		{"LF U+000A", "\n"},
		{"CR U+000D", "\r"},
		{"CRLF is one break, not two", "\r\n"},
		{"VT U+000B", "\v"},
		{"FF U+000C", "\f"},
		{"NEL U+0085", "\u0085"},
		{"LS U+2028", "\u2028"},
		{"PS U+2029", "\u2029"},
	} {
		t.Run(br.name, func(t *testing.T) {
			r := newTestResponder(t, &fakeForge{})
			tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
			if err := r.Registry.Put(tc); err != nil {
				t.Fatalf("Put: %v", err)
			}
			reply, err := r.Handle(context.Background(), tc, "alice",
				"<@U0BOT> note: harmless"+br.sep+"📝 "+forged)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			want := status + "\n> harmless\n> " + forged
			if got := RenderReply(reply, nil); got != want {
				t.Errorf("a %s in the note escaped the blockquote.\n got %q\nwant %q", br.name, got, want)
			}
		})
	}

	// The other direction, and the reason the fix normalises BREAKS rather than
	// mapping whitespace the way SingleLine does: a tab, a no-break space and a
	// blank line are not line breaks, so the quote must carry them through
	// untouched. Flattening them would censor the note the human asked to read.
	t.Run("everything that is not a break survives byte for byte", func(t *testing.T) {
		r := newTestResponder(t, &fakeForge{})
		tc := Context{Root: "333.444", CuratedURL: "https://github.com/o/r/pull/42"}
		if err := r.Registry.Put(tc); err != nil {
			t.Fatalf("Put: %v", err)
		}
		note := "one\ttabbed\n\nblank line above, no-break\u00a0space, zero\u200bwidth"
		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: "+note)
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		want := status + "\n> one\ttabbed\n>\n> blank line above, no-break\u00a0space, zero\u200bwidth"
		if got := RenderReply(reply, nil); got != want {
			t.Errorf("ordinary note text was not quoted byte for byte.\n got %q\nwant %q", got, want)
		}
	})
}

// TestReplyQuotesTheModelDraftAsModelDrafted keeps the two routes apart in the
// one place the human actually reads. ProposedNote already carries the
// distinction into the ENTRY (see NoteBody), and quoting the text into the
// thread is precisely what could erase it there: a human who sees their own
// words quoted back reads them as their own, and a human who sees the MODEL's
// words quoted back under "your note" would read those as their own too.
//
// So the reply says who wrote them, immediately above the quote.
func TestReplyQuotesTheModelDraftAsModelDrafted(t *testing.T) {
	f := &fakeForge{}
	model := &fakeChatModel{resp: wellFormedReply("Noted — that changes the root cause.", "The real cause was a spot-node reclaim, not the CNI.")}
	r := newChatResponder(t, f, model)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> it was actually a spot reclaim")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := "> «Noted — that changes the root cause.»\n" +
		"📝 Noted on the knowledge-base PR #42 — «https://github.com/o/r/pull/42»\n" +
		"Drafted by RunLore from your message — not your own words, pending review:\n" +
		"> «The real cause was a spot-node reclaim, not the CNI.»"
	if got := RenderReply(reply, visibleEscape); got != want {
		t.Errorf("rendered reply =\n%q\nwant\n%q", got, want)
	}

	// The `note:` route must NOT carry that line: it files the human's own
	// words, and telling them RunLore drafted them would be a lie in the other
	// direction.
	t.Run("the note: route claims no such thing", func(t *testing.T) {
		f := &fakeForge{}
		r := newTestResponder(t, f)
		tc := Context{Root: "555.666", CuratedURL: "https://github.com/o/r/pull/42"}
		if err := r.Registry.Put(tc); err != nil {
			t.Fatalf("Put: %v", err)
		}
		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: it was a spot reclaim")
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if strings.Contains(reply, "Drafted by RunLore") {
			t.Errorf("the human's own words were reported as RunLore's draft:\n%q", reply)
		}
	})
}

// TestModelDraftedNoticeIntroducesTheQuoteItPromises pins the ONE thing
// modelDraftedNotice's trailing colon claims: that what follows it is the
// quote.
//
// On the open_pr route it was not. recordedBlock emitted the notice first and
// the "Entry:" line second, so the message read
//
//	Drafted by RunLore from your message — not your own words, pending review:
//	Entry: Operator note: the real cause was a spot-node reclaim
//	> the real cause was a spot-node reclaim
//
// — a colon promising a quote, answered by a title. Two colons in a row, the
// second one introducing something the first one did not name. This whole PR
// exists to make the feedback readable, so the notice sits immediately above
// the block it introduces.
//
// The second case is the same rule read the other way: a notice whose colon
// promises a quote that does not exist is the same defect with nothing after
// it at all, so an empty preview renders no notice. Nothing is lost by that —
// the status line's "a note drafted from your message" (see openedWith)
// already carries the provenance, and it is the half that stands alone.
func TestModelDraftedNoticeIntroducesTheQuoteItPromises(t *testing.T) {
	t.Run("the assembled open_pr reply puts the notice against the quote", func(t *testing.T) {
		f := &fakeForge{}
		model := &fakeChatModel{resp: wellFormedReply("Noted — that changes the root cause.", "The real cause was a spot-node reclaim, not the CNI.")}
		r := newChatResponder(t, f, model)
		tc := putContext(t, r, Context{Root: "111.222", Title: "OOM in payments"})

		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> it was actually a spot reclaim")
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		lines := strings.Split(RenderReply(reply, visibleEscape), "\n")
		at := -1
		for i, ln := range lines {
			if ln == modelDraftedNotice {
				at = i
			}
		}
		if at < 0 {
			t.Fatalf("the model-drafted notice is missing from the reply:\n%q", lines)
		}
		if at == len(lines)-1 {
			t.Fatalf("the notice promises a quote and is the last line:\n%q", lines)
		}
		if next := lines[at+1]; !strings.HasPrefix(next, "> ") {
			t.Errorf("the line under the model-drafted notice is %q, want the quote it introduces (a \"> \" line).\n"+
				"The notice ends in a colon; whatever answers that colon is what the human reads as the drafted text.\nfull reply:\n%q",
				next, lines)
		}
	})

	// recordedBlock directly: the reply path always has note text, so the
	// promise-with-nothing-after-it case is only reachable here — and this is
	// the function that must not emit it.
	t.Run("no quote, no notice", func(t *testing.T) {
		block := recordedBlock(ProposedNote("alice", "was it the CNI?", "drafted"),
			&KBWrite{Route: RouteOpenPR, PR: 99, URL: "https://github.com/o/r/pull/99", Title: "Operator note: OOM in payments"})
		if strings.Contains(block, modelDraftedNotice) {
			t.Errorf("the notice promises a quote, and there is none to show:\n%q", block)
		}
		if !strings.Contains(block, "Operator note: OOM in payments") {
			t.Errorf("the entry title must still be named:\n%q", block)
		}
	})
}

// TestOpenPRStatusLineNamesWhoseNoteItOpenedWith closes a contradiction the
// open_pr reply shipped: "📝 Opened knowledge-base PR #99 with your note" sat
// directly above "Drafted by RunLore from your message — not your own words".
// Two adjacent lines of one message disagreed about who wrote the thing that
// was filed, and the human read the false one first.
//
// The status line is the half that has to be right on its own. It is what a
// truncating transport keeps and what a human skims; a reader who stops after
// it must not come away believing RunLore filed their words under their name
// when it filed its own paraphrase.
//
// The `note:` route keeps "your note" unchanged, and that is asserted here too:
// the fix is to stop claiming authorship RunLore does not have, not to stop
// naming authorship at all. A reply that hedged both routes would be vaguer
// than the one it replaced, which on this path is a regression.
//
// The comment route is deliberately absent from this table. Its status line
// ("📝 Noted on the knowledge-base PR #42") makes no authorship claim to be
// wrong about, so it is already true on both routes — see openedWith.
func TestOpenPRStatusLineNamesWhoseNoteItOpenedWith(t *testing.T) {
	draft := ProposedNote("alice", "was it the CNI?", "The real cause was a spot-node reclaim, not the CNI.")
	typed := HumanNote("alice", "The real cause was a spot-node reclaim, not the CNI.")

	// write()-level, both URL shapes: the unnumbered branch is a separate return
	// with its own copy of the sentence, so it can drift from the numbered one.
	t.Run("write", func(t *testing.T) {
		for _, tt := range []struct {
			name    string
			note    Note
			openURL string
			want    string
		}{
			{
				name:    "model-drafted, numbered URL",
				note:    draft,
				openURL: "https://github.com/o/r/pull/99",
				want:    "📝 Opened knowledge-base PR #99 with a note drafted from your message — «https://github.com/o/r/pull/99»",
			},
			{
				name:    "model-drafted, unnumbered URL",
				note:    draft,
				openURL: "https://github.com/o/r/kb",
				want:    "📝 Opened a knowledge-base PR with a note drafted from your message — «https://github.com/o/r/kb»",
			},
			{
				name:    "note:, numbered URL",
				note:    typed,
				openURL: "https://github.com/o/r/pull/99",
				want:    "📝 Opened knowledge-base PR #99 with your note — «https://github.com/o/r/pull/99»",
			},
			{
				name:    "note:, unnumbered URL",
				note:    typed,
				openURL: "https://github.com/o/r/kb",
				want:    "📝 Opened a knowledge-base PR with your note — «https://github.com/o/r/kb»",
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := &fakeForge{openURL: tt.openURL}
				r := newTestResponder(t, f)
				tc := putContext(t, r, Context{Root: "111.222", Title: "OOM in payments"})

				reply, w, err := r.write(context.Background(), tc, tt.note, noteAt)
				if err != nil {
					t.Fatalf("write: %v", err)
				}
				if w == nil {
					t.Fatal("write reported no result for a note that opened a PR")
				}
				if got := RenderReply(reply, visibleEscape); got != tt.want {
					t.Errorf("status line =\n%q\nwant\n%q", got, tt.want)
				}
			})
		}
	})

	// End to end, because the contradiction was only visible in the assembled
	// message: neither line was wrong in isolation, and no test read both at
	// once. The whole rendered reply is pinned so a future edit to either line
	// has to look at the other.
	t.Run("the assembled model-drafted reply agrees with itself", func(t *testing.T) {
		f := &fakeForge{}
		model := &fakeChatModel{resp: wellFormedReply("Noted — that changes the root cause.", "The real cause was a spot-node reclaim, not the CNI.")}
		r := newChatResponder(t, f, model)
		tc := putContext(t, r, Context{Root: "111.222", Title: "OOM in payments"})

		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> it was actually a spot reclaim")
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		// The notice sits UNDER the entry title, against the quote its colon
		// promises — see TestModelDraftedNoticeIntroducesTheQuoteItPromises for
		// why that order moved.
		want := "> «Noted — that changes the root cause.»\n" +
			"📝 Opened knowledge-base PR #99 with a note drafted from your message — «https://github.com/o/r/pull/99»\n" +
			"Entry: «Operator note: The real cause was a spot-node reclaim, not the CNI.»\n" +
			"Drafted by RunLore from your message — not your own words, pending review:\n" +
			"> «The real cause was a spot-node reclaim, not the CNI.»"
		if got := RenderReply(reply, visibleEscape); got != want {
			t.Errorf("rendered reply =\n%q\nwant\n%q", got, want)
		}
		// The new clause is RunLore's own framing, so it must reach the transport
		// unmarked — exactly like the status line it is part of. Marked, the
		// transport would escape RunLore's own words.
		for _, s := range untrustedSpans(reply) {
			if strings.Contains(s, "drafted from your message") {
				t.Errorf("RunLore's own status wording is inside an untrusted span: %q", s)
			}
		}
	})
}

// TestReplyQuotesTheTextAsWrittenNotTheRawInput is the security half of
// quoting. What the reply shows must be the text that REACHED THE FORGE —
// redacted and capped by noteAsWritten — never the caller's raw input.
//
// Quoting n.Text instead would be a strictly worse egress than the entry it
// describes: the reply would republish, into a chat room, both the secret the
// entry masked and the bytes the entry's cap dropped. Task 1 put that value on
// KBWrite.Note for exactly this consumer; this pins that the consumer uses it.
//
// The fixture carries both hazards at once — a secret AND more bytes than
// MaxNoteBytes — and stays well under notePreviewBytes, so nothing here can be
// mistaken for the preview ceiling doing the work.
func TestReplyQuotesTheTextAsWrittenNotTheRawInput(t *testing.T) {
	const secret = "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWX"
	// No "![", "<" or "#": those trigger the forge-Markdown defusals, which are
	// deliberately NOT in the quoted text (see KBWrite.Note), and would make the
	// byte-identity check below fail for the wrong reason.
	raw := "the deploy token " + secret + " leaked into the log; " + strings.TrimSpace(strings.Repeat("padding ", 30))

	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.MaxNoteBytes = 200
	if len(raw) <= r.MaxNoteBytes || r.MaxNoteBytes >= notePreviewBytes {
		t.Fatalf("test setup: the note (%d bytes) must exceed MaxNoteBytes (%d), which must itself stay under notePreviewBytes (%d)",
			len(raw), r.MaxNoteBytes, notePreviewBytes)
	}
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: "+raw)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	rendered := RenderReply(reply, nil)
	if strings.Contains(rendered, secret) || strings.Contains(rendered, "ghp_") {
		t.Errorf("the reply quoted the secret the entry masked:\n%q", rendered)
	}
	if !strings.Contains(rendered, "[REDACTED]") {
		t.Errorf("the reply carries no mask where the secret was:\n%q", rendered)
	}
	if !strings.Contains(rendered, "input bytes dropped") {
		t.Errorf("the reply must carry the cap's own truncation mark, exactly as the entry does:\n%q", rendered)
	}
	// Byte-identical to what was written: unquoting the blockquote must yield a
	// string that appears verbatim in the body the forge received.
	var quoted []string
	for _, ln := range strings.Split(rendered, "\n") {
		if s, ok := strings.CutPrefix(ln, ">"); ok {
			quoted = append(quoted, strings.TrimPrefix(s, " "))
		}
	}
	got := strings.Join(quoted, "\n")
	if got == "" {
		t.Fatalf("the reply quoted nothing:\n%q", rendered)
	}
	if len(got) > r.MaxNoteBytes {
		t.Errorf("the quote is %d bytes, want at most MaxNoteBytes (%d) — it must be the capped text, not the raw input", len(got), r.MaxNoteBytes)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if !strings.Contains(f.comments[0].body, got) {
		t.Errorf("the quote is not byte-identical to the text written to the forge.\nquoted:\n%q\nbody:\n%s", got, f.comments[0].body)
	}
}

// ---- announcing a landed write to every configured sink --------------------

// fakeKBSink is a providers.KBUpdateNotifier that records every announcement it
// is handed.
//
// mu guards got because deliveries run DETACHED from the reply (see
// KBAnnouncer): the goroutine that asserts is never the goroutine that
// appended, so reading the slice unguarded would be a data race the -race
// detector catches independently of anything under test.
type fakeKBSink struct {
	mu  sync.Mutex
	got []providers.KBUpdate
	err error
	// entered and release are optional rendezvous channels, used by the tests
	// that must prove the reply never waits for a sink: when entered is
	// non-nil DeliverKBUpdate announces its arrival on it, and when release is
	// non-nil it then blocks until release is closed.
	entered chan struct{}
	release chan struct{}
}

func (s *fakeKBSink) DeliverKBUpdate(_ context.Context, up providers.KBUpdate) error {
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, up)
	return s.err
}

// updates returns a copy of what the sink has received so far, taken under the
// lock.
func (s *fakeKBSink) updates() []providers.KBUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]providers.KBUpdate(nil), s.got...)
}

// newAnnouncingResponder is newTestResponder with announcements wired to a
// recording sink.
func newAnnouncingResponder(t *testing.T, f *fakeForge) (*Responder, *fakeKBSink) {
	t.Helper()
	r := newTestResponder(t, f)
	sink := &fakeKBSink{}
	r.Announcer = NewKBAnnouncer(sink, providers.KBDeliverChannel, silentLog())
	return r, sink
}

// drainAnnouncements waits for every detached announcement to finish, so an
// assertion never races the delivery it is about. Bounded: a wedged sink must
// fail this test rather than hang the suite.
func drainAnnouncements(t *testing.T, r *Responder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.Announcer.Drain(ctx)
	if ctx.Err() != nil {
		t.Fatal("announcements did not drain within 10s")
	}
}

// TestAnnounceALandedWrite is the feature: one KBUpdate per successful write,
// carrying everything write() reported plus where it came from.
//
// Both routes are driven, and the whole event is compared as ONE value rather
// than field by field: a missing field is the failure this is for, and a
// per-field probe would pass over one that was never populated at all.
func TestAnnounceALandedWrite(t *testing.T) {
	t.Run("comment route", func(t *testing.T) {
		f := &fakeForge{}
		r, sink := newAnnouncingResponder(t, f)
		const prURL = "https://github.com/o/r/pull/42"
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", Channel: "C-ORIGIN", Title: "OOM", CuratedURL: prURL})

		if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: spot reclaim, not OOM"); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		drainAnnouncements(t, r)

		got := sink.updates()
		if len(got) != 1 {
			t.Fatalf("announcements = %d, want exactly 1 per landed write", len(got))
		}
		// Title is empty on this route by design — the note joined an entry
		// someone else already titled (see KBWrite.Title).
		want := providers.KBUpdate{
			Transport: "slack",
			Root:      "111.222",
			// Channel travels with Root, and the comparison below is the only
			// thing checking it does: a KBUpdate carrying a thread root with no
			// channel cannot be replied into, so every thread-routed delivery
			// would silently fall back to the channel — the destination an
			// operator configured away, restored without a word.
			Channel: "C-ORIGIN",
			Route:   providers.KBRouteComment,
			PR:      42,
			URL:     prURL,
			Author:  "alice",
			Note:    "spot reclaim, not OOM",
			At:      noteAt,
		}
		if got[0] != want {
			t.Errorf("announcement =\n%+v\nwant\n%+v", got[0], want)
		}
	})

	t.Run("open_pr route", func(t *testing.T) {
		f := &fakeForge{}
		r, sink := newAnnouncingResponder(t, f)
		tc := putContext(t, r, Context{Transport: "matrix", Root: "$evt:example.org", Channel: "!room:example.org", Title: "OOM in payments"})

		if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: stale since Karpenter"); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		drainAnnouncements(t, r)

		if len(f.opened) != 1 {
			t.Fatalf("opened = %d, want 1", len(f.opened))
		}
		got := sink.updates()
		if len(got) != 1 {
			t.Fatalf("announcements = %d, want exactly 1 per landed write", len(got))
		}
		want := providers.KBUpdate{
			Transport: "matrix",
			Root:      "$evt:example.org",
			Channel:   "!room:example.org",
			Route:     providers.KBRouteOpenPR,
			PR:        99,
			URL:       "https://github.com/o/r/pull/99",
			// The name the entry was actually filed under, not a second
			// rendering of the same inputs that could drift from it.
			Title:  f.opened[0].Title,
			Author: "alice",
			Note:   "stale since Karpenter",
			At:     noteAt,
		}
		if got[0] != want {
			t.Errorf("announcement =\n%+v\nwant\n%+v", got[0], want)
		}
		if got[0].Title == "" {
			t.Error("Title is empty; the open_pr route generates one and the announcement must name it")
		}
	})

	t.Run("append route", func(t *testing.T) {
		f := &fakeForge{}
		r, sink := newAnnouncingResponder(t, f)
		const prURL = "https://github.com/o/r/pull/77"
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", Title: "OOM", NoteURL: prURL})

		if _, err := r.Handle(context.Background(), tc, "bob", "<@U0BOT> note: and the fix was a taint"); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		drainAnnouncements(t, r)

		got := sink.updates()
		if len(got) != 1 {
			t.Fatalf("announcements = %d, want exactly 1 per landed write", len(got))
		}
		// Title is empty here for the same reason as on the comment route: the
		// entry already had a title, generated when the FIRST note in this thread
		// opened the PR, and this write generated none of its own.
		want := providers.KBUpdate{
			Transport: "slack",
			Root:      "111.222",
			Route:     providers.KBRouteAppend,
			PR:        77,
			URL:       prURL,
			Author:    "bob",
			Note:      "and the fix was a taint",
			At:        noteAt,
		}
		if got[0] != want {
			t.Errorf("announcement =\n%+v\nwant\n%+v", got[0], want)
		}
	})
}

// TestAnnouncerStampsItsOwnDestinationUnconditionally pins deliver()'s claim
// that a caller cannot compose a KBUpdate routed somewhere the configuration
// never selected.
//
// The stamp is what makes that true, and only if it is UNCONDITIONAL. Written as
// `if up.Delivery == "" { up.Delivery = a.delivery }` — the shape that reads as
// a harmless default — it becomes the opposite: a caller that sets the field
// wins, and the announcer's own configuration is what gets defaulted. Nothing
// else in either package notices, because every production caller leaves the
// field zero and every other test sets it on the announcer.
//
// It matters because the field is a DESTINATION. A caller able to set it can
// route an announcement into a thread on a deployment whose operator asked for
// channel-level delivery only, or the reverse — and the announcer is the only
// place that knows which was asked for.
func TestAnnouncerStampsItsOwnDestinationUnconditionally(t *testing.T) {
	for _, caller := range []providers.KBDelivery{
		"", providers.KBDeliverChannel, providers.KBDeliverThread, providers.KBDeliverBoth,
	} {
		t.Run("caller sets "+string(caller), func(t *testing.T) {
			sink := &fakeKBSink{}
			a := NewKBAnnouncer(sink, providers.KBDeliverThread, silentLog())
			a.deliver(context.Background(), providers.KBUpdate{
				Transport: "slack", Root: "111.222", Channel: "C-ORIGIN",
				Delivery: caller, Route: providers.KBRouteComment, URL: "https://github.com/o/r/pull/1",
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			a.Drain(ctx)

			got := sink.updates()
			if len(got) != 1 {
				t.Fatalf("announcements = %d, want 1", len(got))
			}
			if got[0].Delivery != providers.KBDeliverThread {
				t.Errorf("a caller-set Delivery %q survived to the sink as %q — the announcer's own "+
					"destination must overwrite it, or a caller can route an announcement somewhere the "+
					"operator never configured", string(caller), string(got[0].Delivery))
			}
		})
	}
}

// TestAnnounceNamesItsOriginatingTransportAndRoot pins the half of the event a
// consumer needs to tell where a write came from — and pins it as a value READ
// from the thread context rather than a constant.
//
// This is what the owner's "distinct destination, not exclusion" decision rests
// on: the announcement goes to each notifier's configured channel and never
// into a thread, so the origin travels with the event instead of being used to
// suppress it. A responder that stamped "slack" unconditionally would pass the
// test above, which runs one transport.
func TestAnnounceNamesItsOriginatingTransportAndRoot(t *testing.T) {
	for _, tt := range []struct{ transport, root string }{
		{"slack", "111.222"},
		{"matrix", "$evt:example.org"},
	} {
		t.Run(tt.transport, func(t *testing.T) {
			f := &fakeForge{}
			r, sink := newAnnouncingResponder(t, f)
			tc := putContext(t, r, Context{Transport: tt.transport, Root: tt.root, CuratedURL: "https://github.com/o/r/pull/42"})

			if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: x"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			drainAnnouncements(t, r)

			got := sink.updates()
			if len(got) != 1 {
				t.Fatalf("announcements = %d, want 1", len(got))
			}
			if got[0].Transport != tt.transport {
				t.Errorf("Transport = %q, want %q — the origin is read from the thread, never assumed", got[0].Transport, tt.transport)
			}
			if got[0].Root != tt.root {
				t.Errorf("Root = %q, want %q", got[0].Root, tt.root)
			}
		})
	}
}

// TestAnnounceNeverPostsIntoTheThread is the other half of that decision,
// asserted end to end through the real transport-facing entry point: an
// announcement is a SECOND destination, not a second message in the thread.
//
// Driven through Mention rather than Responder because Mention is the only
// thing in this package that can post into a thread at all. Exactly one reply
// goes to the Replier, and the announcement reaches the sink instead.
func TestAnnounceNeverPostsIntoTheThread(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	sink := &fakeKBSink{}
	m.Responder.Announcer = NewKBAnnouncer(sink, providers.KBDeliverChannel, silentLog())
	if err := m.Registry.Put(Context{Transport: "slack", Root: "111.222", Channel: "C1", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "<@U0BOT> note: spot reclaim", nil)
	drainAnnouncements(t, m.Responder)

	if len(rep.replies) != 1 {
		t.Fatalf("replies into the thread = %d, want exactly 1 — the announcement must not post a second one", len(rep.replies))
	}
	if got := sink.updates(); len(got) != 1 {
		t.Fatalf("announcements = %d, want exactly 1", len(got))
	}
}

// TestAnnounceNothingWhenNothingLanded is the assertion this package has
// shipped wrong three times: an operation that did not happen must not report
// as though it did. Every path that leaves the knowledge base untouched is
// asserted SEPARATELY — one of them silently regressing behind another's pass
// is exactly how that class of bug survives.
func TestAnnounceNothingWhenNothingLanded(t *testing.T) {
	assertSilent := func(t *testing.T, r *Responder, sink *fakeKBSink) {
		t.Helper()
		drainAnnouncements(t, r)
		if got := sink.updates(); len(got) != 0 {
			t.Fatalf("announced %+v; nothing was written, so there is nothing to announce", got)
		}
	}

	t.Run("throttled", func(t *testing.T) {
		f := &fakeForge{}
		r, sink := newAnnouncingResponder(t, f)
		// A one-event window with its one event already spent: ratelimit.New(0, …)
		// means UNLIMITED, not "refuse everything".
		r.ForgeWrites = ratelimit.New(1, time.Hour)
		if !r.ForgeWrites.Allow() {
			t.Fatal("the window refused its first event; this test cannot reach the throttled path")
		}
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: x")
		if err != nil {
			t.Fatalf("a throttle is not an error: %v", err)
		}
		if !strings.Contains(reply, "paused") {
			t.Fatalf("reply = %q, want the throttle message — this test did not reach the throttled path", reply)
		}
		assertSilent(t, r, sink)
	})

	t.Run("comment failed", func(t *testing.T) {
		f := &fakeForge{commErr: errors.New("forge down")}
		r, sink := newAnnouncingResponder(t, f)
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

		if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: x"); err == nil {
			t.Fatal("a failed comment must still be an error")
		}
		assertSilent(t, r, sink)
	})

	t.Run("open PR failed", func(t *testing.T) {
		f := &fakeForge{openErr: errors.New("forge down")}
		r, sink := newAnnouncingResponder(t, f)
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222"})

		if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: x"); err == nil {
			t.Fatal("a failed OpenPR must still be an error")
		}
		assertSilent(t, r, sink)
	})

	t.Run("open-check failed", func(t *testing.T) {
		f := &fakeForge{prOpenErr: errors.New("forge unreachable")}
		r, sink := newAnnouncingResponder(t, f)
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

		if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: x"); err == nil {
			t.Fatal("an unreachable forge must still be an error")
		}
		assertSilent(t, r, sink)
	})

	t.Run("capped", func(t *testing.T) {
		f := &fakeForge{}
		r, sink := newAnnouncingResponder(t, f)
		r.MaxNotesPerThread = 1
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", Notes: 1, CuratedURL: "https://github.com/o/r/pull/42"})

		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: x")
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if !strings.Contains(reply, "note limit") {
			t.Fatalf("reply = %q, want the per-thread cap message — this test did not reach the capped path", reply)
		}
		assertSilent(t, r, sink)
	})

	// The model answered but proposed nothing to file: "record nothing", not an
	// omission. There is no write, so there is no announcement — a channel must
	// not learn about a question that produced no knowledge.
	t.Run("no note proposed", func(t *testing.T) {
		f := &fakeForge{}
		r, sink := newAnnouncingResponder(t, f)
		r.Chat = &Chat{Model: &fakeChatModel{resp: wellFormedReply("The CNI was ruled out.", "")}, Log: silentLog()}
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

		if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> was it the CNI?"); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if c, o := f.counts(); c != 0 || o != 0 {
			t.Fatalf("forge calls = %d/%d, want 0/0 — this test did not reach the empty-kb_note path", c, o)
		}
		assertSilent(t, r, sink)
	})

	// The default deployment: no chat layer, so freeform records nothing at all.
	t.Run("freeform with chat off", func(t *testing.T) {
		f := &fakeForge{}
		r, sink := newAnnouncingResponder(t, f)
		tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> was it the CNI?")
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if reply != FreeformNotRecordedReply {
			t.Fatalf("reply = %q, want FreeformNotRecordedReply", reply)
		}
		assertSilent(t, r, sink)
	})
}

// TestAnnounceSurvivesAFailedCounterWriteBack is the other side of the test
// above: everything that DID land must be announced, including when a later
// bookkeeping step fails.
//
// record()'s own comment says the announce sits ahead of the counter write-back
// "because it must not depend on either: the entry is on the forge, and neither
// a truncated preview nor a failed counter changes that it landed." Nothing
// asserted it. Moving r.announce below the write-back and gating it on
// Registry.Update succeeding passed ./internal/thread in full, and that path is
// real rather than theoretical — an entry that aged out of the registry, or a
// disabled one, is exactly what TestHandleUncountableWriteIsSurfaced drives.
// Under that gate the note is permanently on the forge and no channel is ever
// told, which is the silent-write failure this whole feature exists to remove,
// arriving through the one path that also returns an error to the human.
//
// tc is deliberately never Put into the registry, so the Notes++ write-back
// misses and record() returns its error.
func TestAnnounceSurvivesAFailedCounterWriteBack(t *testing.T) {
	f := &fakeForge{}
	r, sink := newAnnouncingResponder(t, f)
	const prURL = "https://github.com/o/r/pull/42"
	tc := Context{Transport: "slack", Root: "111.222", CuratedURL: prURL}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: spot reclaim")
	if err == nil {
		t.Fatal("test setup: an uncountable write must still surface as an error, or this test is not on the path it exists for")
	}
	if len(f.comments) != 1 {
		t.Fatalf("the forge write itself must have landed; comments = %d", len(f.comments))
	}
	if !strings.Contains(reply, "I saved that") {
		t.Fatalf("reply = %q, want the counter warning — this test did not reach the uncountable path", reply)
	}

	drainAnnouncements(t, r)
	got := sink.updates()
	if len(got) != 1 {
		t.Fatalf("announcements = %d, want 1 — the entry is on the forge, and a counter that could not be written does not unwrite it", len(got))
	}
	if got[0].URL != prURL {
		t.Errorf("URL = %q, want %q — the announcement must still name the write that landed", got[0].URL, prURL)
	}
	if got[0].Note != "spot reclaim" {
		t.Errorf("Note = %q, want the text that reached the forge", got[0].Note)
	}
}

// TestAnnounceWithNoSinkIsOffAndDoesNotPanic pins the package's nil-safe
// contract — the one Budget, Metrics and Log on this same struct already
// follow. Announcements are opt-in (notify.thread.announce_kb_updates defaults
// to false), so the UNWIRED responder is the common deployment and must not be
// the one that panics.
func TestAnnounceWithNoSinkIsOffAndDoesNotPanic(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	if r.Announcer != nil {
		t.Fatal("test setup: newTestResponder must leave announcements unwired")
	}
	tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

	// A landed write, the throttled path, and the open_pr path: every branch
	// that touches the announcement, with nothing to announce to.
	if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	tc2 := putContext(t, r, Context{Transport: "slack", Root: "333.444"})
	if _, err := r.Handle(context.Background(), tc2, "alice", "<@U0BOT> note: y"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	// "Off" is ONE value, not a flag beside a sink: constructing an announcer
	// with no sink yields no announcer, so there is no half-wired state in
	// which announcements are enabled with nowhere to go.
	if a := NewKBAnnouncer(nil, providers.KBDeliverChannel, silentLog()); a != nil {
		t.Errorf("NewKBAnnouncer(nil) = %+v, want nil — a missing sink means announcements are off", a)
	}
	// And every method is safe on that nil, so a caller never needs its own
	// nil check before draining at shutdown.
	var off *KBAnnouncer
	off.deliver(context.Background(), providers.KBUpdate{})
	off.Drain(context.Background())
}

// TestAnnounceFailureDoesNotChangeTheReply pins that the announcement is
// best-effort in the strongest sense available: the bytes the human reads are
// IDENTICAL with a failing sink and with no sink at all, and the error the
// caller sees is unchanged.
//
// The baseline is computed by running the same message through a responder
// with announcements off, rather than by writing the expected string out —
// there is then no expectation to quietly adjust if the reply ever changes.
func TestAnnounceFailureDoesNotChangeTheReply(t *testing.T) {
	const msg = "<@U0BOT> note: spot reclaim, not OOM"
	newCase := func(t *testing.T) (*Responder, Context) {
		t.Helper()
		r := newTestResponder(t, &fakeForge{})
		return r, putContext(t, r, Context{Transport: "slack", Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"})
	}

	quiet, quietCtx := newCase(t)
	baseline, baselineErr := quiet.Handle(context.Background(), quietCtx, "alice", msg)
	if baselineErr != nil {
		t.Fatalf("baseline Handle: %v", baselineErr)
	}

	loud, loudCtx := newCase(t)
	sink := &fakeKBSink{err: errors.New("channel_not_found")}
	loud.Announcer = NewKBAnnouncer(sink, providers.KBDeliverChannel, silentLog())
	reply, err := loud.Handle(context.Background(), loudCtx, "alice", msg)
	if err != nil {
		t.Errorf("Handle returned %v; a failing announcement must never fail the write that already landed", err)
	}
	if reply != baseline {
		t.Errorf("reply changed when the sink failed.\ngot:\n%q\nwant:\n%q", reply, baseline)
	}
	drainAnnouncements(t, loud)
	if got := sink.updates(); len(got) != 1 {
		t.Fatalf("announcements = %d, want 1 — the sink must still have been tried", len(got))
	}
}

// TestAnnounceDoesNotDelayTheReply is why the delivery is detached rather than
// inline. A sink is a network call to a chat system; a slow or wedged one must
// not hold up the acknowledgement to the human whose note already landed.
//
// Proven without a timing threshold: the sink blocks until this test releases
// it, and the release happens only AFTER Handle has returned. A synchronous
// implementation could not reach that point, and fails on the deadline below
// rather than deadlocking the suite.
func TestAnnounceDoesNotDelayTheReply(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	sink := &fakeKBSink{entered: make(chan struct{}), release: make(chan struct{})}
	r.Announcer = NewKBAnnouncer(sink, providers.KBDeliverChannel, silentLog())
	tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

	type result struct {
		reply string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: spot reclaim")
		done <- result{reply, err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Handle did not return while the sink was blocked — the announcement is sitting on the reply path")
	}
	if got.err != nil {
		t.Fatalf("Handle: %v", got.err)
	}
	if !strings.Contains(got.reply, "#42") {
		t.Errorf("reply = %q, want the human told where their note landed", got.reply)
	}

	// The delivery really was in flight, not skipped. Bounded, so an
	// announcement that never happens fails here rather than wedging the suite.
	select {
	case <-sink.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the sink was never called — the write landed and nothing announced it")
	}
	close(sink.release)
	drainAnnouncements(t, r)
	if u := sink.updates(); len(u) != 1 {
		t.Fatalf("announcements = %d, want 1", len(u))
	}
}

// TestAnnounceIsDrainable pins that detached does not mean unaccounted for: an
// in-flight delivery is waited on, so shutdown does not drop an announcement
// for a write that already reached the forge.
func TestAnnounceIsDrainable(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	sink := &fakeKBSink{release: make(chan struct{})}
	r.Announcer = NewKBAnnouncer(sink, providers.KBDeliverChannel, silentLog())
	tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

	if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	go func() { time.Sleep(50 * time.Millisecond); close(sink.release) }()
	drainAnnouncements(t, r)

	if got := sink.updates(); len(got) != 1 {
		t.Fatalf("Drain returned with %d/1 deliveries finished", len(got))
	}
}

// TestAnnounceIsBoundedByTheForgeWriteWindow is the bounding analysis stated as
// a test rather than as an assumption in a comment.
//
// The claim is that announcements need no ceiling of their own because every
// one of them follows a forge write, and ForgeWrites already caps those
// globally per hour. What that reduces to is a 1:1 invariant — announcements
// equal landed forge writes, never more — and this drives more messages than
// the window allows to pin exactly that: the throttled attempts produce a reply
// and nothing else.
func TestAnnounceIsBoundedByTheForgeWriteWindow(t *testing.T) {
	f := &fakeForge{}
	r, sink := newAnnouncingResponder(t, f)
	r.ForgeWrites = ratelimit.New(2, time.Hour)
	// The per-thread cap must not be what stops this: tc is passed unchanged
	// each time, so tc.Notes stays 0 and only the window can refuse.
	tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

	for i := 0; i < 5; i++ {
		if _, err := r.Handle(context.Background(), tc, "alice", fmt.Sprintf("<@U0BOT> note: attempt %d", i)); err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	drainAnnouncements(t, r)

	comments, opened := f.counts()
	if comments != 2 || opened != 0 {
		t.Fatalf("forge writes = %d comments / %d opened, want 2/0 — the window must have refused the rest", comments, opened)
	}
	if got := sink.updates(); len(got) != comments {
		t.Errorf("announcements = %d, want %d — one per LANDED forge write, and none for a throttled attempt", len(got), comments)
	}
}

// TestAnnounceReportsTheNoteAsWritten pins the redaction guarantee at what is a
// NEW egress: the announcement leaves RunLore for every configured sink, so it
// must carry the post-redaction, post-cap text the entry carries — never the
// caller's raw input.
//
// The author name goes through the same masking, because it is transport-
// reported text that this event now publishes somewhere the entry's own
// rendering does not reach.
func TestAnnounceReportsTheNoteAsWritten(t *testing.T) {
	const secret = "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWX"
	f := &fakeForge{}
	r, sink := newAnnouncingResponder(t, f)
	r.MaxNoteBytes = 200
	tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})
	raw := "the deploy token " + secret + " leaked; " + strings.Repeat("padding ", 60)

	if _, err := r.Handle(context.Background(), tc, "alice "+secret, "<@U0BOT> note: "+raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainAnnouncements(t, r)

	got := sink.updates()
	if len(got) != 1 {
		t.Fatalf("announcements = %d, want 1", len(got))
	}
	if strings.Contains(got[0].Note, "ghp_") {
		t.Errorf("the announced note still carries the secret:\n%q", got[0].Note)
	}
	if !strings.Contains(got[0].Note, "[REDACTED]") {
		t.Errorf("the announced note carries no mask where the secret was:\n%q", got[0].Note)
	}
	if len(got[0].Note) > r.MaxNoteBytes {
		t.Errorf("the announced note is %d bytes, want at most MaxNoteBytes (%d)", len(got[0].Note), r.MaxNoteBytes)
	}
	if strings.Contains(got[0].Author, "ghp_") {
		t.Errorf("the announced author still carries the secret: %q", got[0].Author)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	// Byte-identical to what reached the forge: an announcement that described a
	// different string than the entry holds would be a second, drifting account
	// of the same write.
	if !strings.Contains(f.comments[0].body, got[0].Note) {
		t.Errorf("the announced note is not byte-identical to the text written to the forge.\nannounced:\n%q\nbody:\n%s", got[0].Note, f.comments[0].body)
	}
}

// TestAnnounceCarriesTheModelDraftedNote drives the route the announcement
// matters most on: with model.chat wired, RunLore's own model wrote the note,
// and the people who were not reading that thread learn about it only through
// this event.
func TestAnnounceCarriesTheModelDraftedNote(t *testing.T) {
	f := &fakeForge{}
	r, sink := newAnnouncingResponder(t, f)
	const drafted = "The real cause was a spot-node reclaim, not the CNI."
	r.Chat = &Chat{Model: &fakeChatModel{resp: wellFormedReply("Noted.", drafted)}, Log: silentLog()}
	tc := putContext(t, r, Context{Transport: "slack", Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"})

	if _, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> it was actually a spot reclaim"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainAnnouncements(t, r)

	got := sink.updates()
	if len(got) != 1 {
		t.Fatalf("announcements = %d, want 1", len(got))
	}
	if got[0].Note != drafted {
		t.Errorf("Note = %q, want the model's drafted text %q", got[0].Note, drafted)
	}
	if got[0].Author != "alice" {
		t.Errorf("Author = %q, want the human whose message produced the draft", got[0].Author)
	}
}

// TestKBRouteVocabulariesAgree pins the mapping between this package's route
// names and the providers vocabulary the event uses. They spell the routes
// identically today; a direct string conversion would keep compiling — and
// start emitting a route no consumer recognises — the moment either side is
// renamed.
//
// Both sides are pinned to their LITERAL spelling, not merely to each other.
// kbRoute switches on RouteComment and returns providers.KBRouteComment, so
// `kbRoute(RouteComment) == providers.KBRouteComment` is true for any pair of
// values whatever: rename RouteComment to "commented" and the switch still
// matches it, still returns the providers constant, and the assertion still
// holds while every dashboard series and every consumer reading the old word
// goes quiet. These strings are a wire vocabulary — RouteComment, RouteOpenPR
// and RouteAppend are also the "route" attribute ThreadNotesWritten is labelled
// with (see their doc comment), and the providers trio is what a KBUpdate
// consumer switches on — so changing one is an operator-visible break that
// belongs in a diff.
func TestKBRouteVocabulariesAgree(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"RouteComment", RouteComment, "comment"},
		{"RouteOpenPR", RouteOpenPR, "open_pr"},
		{"RouteAppend", RouteAppend, "append"},
		{"providers.KBRouteComment", string(providers.KBRouteComment), "comment"},
		{"providers.KBRouteOpenPR", string(providers.KBRouteOpenPR), "open_pr"},
		{"providers.KBRouteAppend", string(providers.KBRouteAppend), "append"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q — this word is on a metric label or in a delivered event, "+
				"so renaming it silently retires whatever was reading the old one", tc.name, tc.got, tc.want)
		}
	}
	if got := kbRoute(RouteComment); got != providers.KBRouteComment {
		t.Errorf("kbRoute(%q) = %q, want %q", RouteComment, got, providers.KBRouteComment)
	}
	if got := kbRoute(RouteOpenPR); got != providers.KBRouteOpenPR {
		t.Errorf("kbRoute(%q) = %q, want %q", RouteOpenPR, got, providers.KBRouteOpenPR)
	}
	if got := kbRoute(RouteAppend); got != providers.KBRouteAppend {
		t.Errorf("kbRoute(%q) = %q, want %q", RouteAppend, got, providers.KBRouteAppend)
	}
}

// forgeErrorText returns what the FORGE contributed to a "could not save" reply:
// RunLore's own lead-in and the inline code span it wraps the reason in are
// stripped, and the span marks are left in place so a test can count them.
//
// Requiring the code span here rather than asserting it separately is
// deliberate: it is the fourth measure — the one that keeps a soft-wrapped
// continuation line visibly out of RunLore's own voice — so a reply that lost it
// must fail every test built on this helper, not only one named for it.
func forgeErrorText(t *testing.T, reply string) string {
	t.Helper()
	const lead = "⚠️ I could not save that to the knowledge base: "
	if !strings.HasPrefix(reply, lead) {
		t.Fatalf("reply is not a forge-failure reply: %q", reply)
	}
	rest := strings.TrimPrefix(reply, lead)
	if len(rest) < 2 || !strings.HasPrefix(rest, "`") || !strings.HasSuffix(rest, "`") {
		t.Fatalf("the forge reason is not inside an inline code span: %q", rest)
	}
	return rest[1 : len(rest)-1]
}

// TestForgeFailureReplyIsRedactedFlattenedAndBounded pins the treatment a forge
// error gets before it is posted into a chat thread — on BOTH failure routes,
// because the two are separate literals and the one that gets fixed alone is the
// one that stops being tested.
//
// It used to get none. Untrusted(err.Error()) marks a span for the transport's
// markup escaper and does nothing else, and a forge error is a SERVER-SUPPLIED
// body: a GitHub 403 echoes the credential it rejected, and every escaper in
// this repo leaves a line break alone, so a multi-line JSON body rendered its
// continuation lines at the same left margin as RunLore's own status claims.
//
// The four measures are the ones internal/notify's curateFailureReason already
// applied to the identical class of text, and they are asserted here rather than
// assumed: redaction, backtick neutralisation (the reply wraps the reason in an
// inline code span, which one backtick would close early), flattening, and a
// bound.
func TestForgeFailureReplyIsRedactedFlattenedAndBounded(t *testing.T) {
	const token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	// The backtick is the forge quoting an identifier back at RunLore, which real
	// forge errors do ("branch `main` is protected") — one is all it takes to close
	// the inline code span the reply wraps this in.
	body := "github POST /repos/o/r/pulls: status 403: branch `main`: {\n \"message\": \"Bad credentials\",\n \"token\": \"" + token + "\"\n}"

	routes := map[string]func(*fakeForge){
		// The standalone-entry route: OpenPR itself failed.
		"open_pr": func(f *fakeForge) { f.openErr = errors.New(body) },
		// The existing-PR route: the comment (and, before it, the append) failed.
		"comment": func(f *fakeForge) { f.commErr, f.appendErr = errors.New(body), errors.New(body) },
	}
	for name, setup := range routes {
		t.Run(name, func(t *testing.T) {
			f := &fakeForge{}
			setup(f)
			r := newTestResponder(t, f)
			tc := Context{Transport: "slack", Root: "root-1"}
			if name == "comment" {
				tc.CuratedURL = "https://github.com/o/r/pull/7"
			}
			reply, err := r.Handle(context.Background(), tc, "alice", "note: the cause was a spot reclaim")
			if err == nil {
				t.Fatalf("expected the forge failure to be reported, got reply %q", reply)
			}
			got := forgeErrorText(t, reply)
			if strings.Contains(got, token) {
				t.Errorf("the credential the forge echoed reached the thread verbatim: %q", got)
			}
			if strings.ContainsAny(got, "\n\r\v\f") {
				t.Errorf("a break rune survived, so a forged status line can sit at RunLore's own left margin: %q", got)
			}
			if strings.Contains(got, "`") {
				t.Errorf("a backtick survived and would close the inline code span early: %q", got)
			}
			if !strings.Contains(got, "status 403") {
				t.Errorf("the diagnosis an operator acts on was dropped: %q", got)
			}
		})
	}
}

// TestForgeFailureReplyIsCapped keeps the bound a property of the reply rather
// than of whatever the forge happened to return. A forge body is server-supplied
// and unbounded — a proxy banner, an HTML error page — and an unbounded one is
// an unbounded chat post on a path any channel member can trigger.
func TestForgeFailureReplyIsCapped(t *testing.T) {
	f := &fakeForge{openErr: errors.New("open PR: " + strings.Repeat("x", 4000))}
	r := newTestResponder(t, f)
	reply, err := r.Handle(context.Background(), Context{Root: "root-1"}, "alice", "note: the cause was a spot reclaim")
	if err == nil {
		t.Fatalf("expected a forge failure, got %q", reply)
	}
	if got := len(forgeErrorText(t, reply)); got > forgeErrorBytes+2*len(untrustedMark)+8 {
		t.Errorf("the published forge error is %d bytes, past the %d-byte bound", got, forgeErrorBytes)
	}
}

// TestForgeFailureCannotNarrowWhatTheReplyEscapes is the composed guard behind
// RenderReply's parity, and the reason that function's doc comment no longer
// claims a stray mark is harmless.
//
// RenderReply splits on a single mark and escapes the odd-indexed segments, so
// the segments simply alternate. One EXTRA mark anywhere flips that parity for
// everything after it: a genuinely untrusted span downstream lands on an even
// index and is handed to the transport unescaped. The forge error is the one
// piece of transport-bound text this package interpolates straight from a
// server, so it is where an injected mark would arrive — and on the freeform
// route a reply carries model prose in the very same message.
func TestForgeFailureCannotNarrowWhatTheReplyEscapes(t *testing.T) {
	f := &fakeForge{openErr: errors.New("open PR: forbidden" + untrustedMark + " and then")}
	r := newTestResponder(t, f)
	reply, err := r.Handle(context.Background(), Context{Root: "root-1"}, "alice", "note: the cause was a spot reclaim")
	if err == nil {
		t.Fatalf("expected a forge failure, got %q", reply)
	}
	if n := strings.Count(reply, untrustedMark); n%2 != 0 {
		t.Fatalf("the reply carries %d span marks — an odd count flips RenderReply's parity: %q", n, reply)
	}
	rendered := RenderReply(reply+"\n"+Untrusted("<!channel>"), func(s string) string {
		return strings.ReplaceAll(s, "<", "&lt;")
	})
	if strings.Contains(rendered, "<!channel>") {
		t.Errorf("a mark smuggled through the forge error narrowed the escaping and let <!channel> reach the wire: %q", rendered)
	}
}

// TestForgeOutageDoesNotBurnTheGlobalWriteBudget closes the disagreement between
// this feature's two budgets about what a FAILURE costs.
//
// ForgeWrites is consumed at the top of write(), upstream of the route branch,
// which is right: a comment must spend it exactly as an OpenPR does. But no
// failure path handed the token back, so a forge outage spent the whole hour on
// writes that never happened — 20 failed attempts, zero entries, and the 21st
// caller told "I have made too many knowledge-base writes recently" (reproduced).
// The per-thread count is deliberately NOT charged for a failure (see record),
// so the two ceilings were answering the same question differently.
//
// The 21st write here is the assertion: the forge is healthy again, and the only
// thing that could still refuse it is a budget spent on nothing.
func TestForgeOutageDoesNotBurnTheGlobalWriteBudget(t *testing.T) {
	f := &fakeForge{openErr: errors.New("503 service unavailable")}
	r := newTestResponder(t, f)
	r.ForgeWrites = ratelimit.New(20, time.Hour)
	r.MaxNotesPerThread = 1000
	tc := Context{Transport: "slack", Root: "root-1"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("put: %v", err)
	}
	ctx := context.Background()
	for i := range 20 {
		if _, err := r.Handle(ctx, tc, "alice", "note: attempt during the outage"); err == nil {
			t.Fatalf("attempt %d: expected the forge failure to be reported", i)
		}
	}
	if got := r.ForgeWrites.Count(); got != 0 {
		t.Errorf("%d tokens still spent after 20 writes that never landed", got)
	}

	f.openErr = nil
	f.openURL = "https://github.com/o/r/pull/7"
	fresh, _ := r.Registry.Get("root-1")
	reply, err := r.Handle(ctx, fresh, "alice", "note: the forge is healthy again")
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if strings.Contains(reply, "too many knowledge-base writes") {
		t.Fatalf("throttled on a healthy forge, having landed nothing: %q", reply)
	}
	if len(f.opened) != 1 {
		t.Errorf("opened %d PRs, want the one that succeeded", len(f.opened))
	}
}

// TestGlobalWriteBudgetStillChargesEveryLandedWrite is the other direction, and
// it is why the refund is placed on the failure paths rather than on the whole
// call: a write that LANDED must still cost a token, on either route, or the
// refund would quietly turn the one global ceiling this feature has into no
// ceiling at all.
func TestGlobalWriteBudgetStillChargesEveryLandedWrite(t *testing.T) {
	for _, tt := range []struct {
		name string
		tc   Context
	}{
		{"open_pr", Context{Transport: "slack", Root: "root-1"}},
		{"comment", Context{Transport: "slack", Root: "root-1", CuratedURL: "https://github.com/o/r/pull/7"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForge{openURL: "https://github.com/o/r/pull/9"}
			r := newTestResponder(t, f)
			r.ForgeWrites = ratelimit.New(2, time.Hour)
			r.MaxNotesPerThread = 1000
			if err := r.Registry.Put(tt.tc); err != nil {
				t.Fatalf("put: %v", err)
			}
			ctx := context.Background()
			for i := range 2 {
				fresh, _ := r.Registry.Get("root-1")
				if _, err := r.Handle(ctx, fresh, "alice", fmt.Sprintf("note: landed write %d", i)); err != nil {
					t.Fatalf("write %d: %v", i, err)
				}
			}
			fresh, _ := r.Registry.Get("root-1")
			reply, err := r.Handle(ctx, fresh, "alice", "note: one past the budget")
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			if !strings.Contains(reply, "too many knowledge-base writes") {
				t.Errorf("the third write was not throttled: %q", reply)
			}
		})
	}
}

// TestConcurrentNotesCannotExceedThePerThreadCap closes the gap between the cap
// and the lock that was already there for the sibling race.
//
// The cap used to be read in record() from the caller's OWN snapshot of the
// thread context — taken before write() acquired lockRoot — and the counter was
// incremented after that guard had already been released. So the check, the
// write and the increment sat in three different critical sections, none of
// which was the one protecting the thread: eight concurrent notes on a thread
// two writes into a cap of three produced TEN forge writes and refused none,
// under -race.
//
// The guard needed for this is the same guard write() already takes for the
// duplicate-PR race — one thread's writes were already serialised, the cap was
// simply being decided outside that. So the assertion is exact rather than
// "fewer than before": with one write of headroom, exactly one caller may write
// and every other must be refused, whatever order they arrive in.
func TestConcurrentNotesCannotExceedThePerThreadCap(t *testing.T) {
	const (
		noteCap  = 3 // not `cap`: revive rejects an identifier that shadows a builtin
		already  = 2
		writers  = 8
		headroom = noteCap - already
	)
	f := &fakeForge{openURL: "https://github.com/o/r/pull/7"}
	r := newTestResponder(t, f)
	r.MaxNotesPerThread = noteCap
	r.ForgeWrites = ratelimit.New(0, time.Hour) // unlimited; the global budget is not what this pins
	root := "root-1"
	if err := r.Registry.Put(Context{Transport: "slack", Root: root, Notes: already}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	refused := 0
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A stale snapshot, exactly as the mention handler takes one: every
			// writer reads the registry before any of them has written, so all eight
			// observe Notes == already. Nothing but the guard can separate them.
			tc, ok := r.Registry.Get(root)
			if !ok {
				t.Errorf("Get[%d]: registry lost the root mid-test", i)
				return
			}
			reply, err := r.Handle(context.Background(), tc, "alice", fmt.Sprintf("note: concurrent %d", i))
			if err != nil {
				t.Errorf("Handle[%d]: %v", i, err)
				return
			}
			if strings.Contains(reply, "note limit") {
				mu.Lock()
				refused++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	comments, opened := f.counts()
	if got := comments + opened; got != headroom {
		t.Errorf("%d forge writes with %d of headroom left — the cap was decided outside the guard that serialises this thread", got, headroom)
	}
	if refused != writers-headroom {
		t.Errorf("refused = %d, want %d — every writer past the cap must be told so, not silently written", refused, writers-headroom)
	}
	if tc, ok := r.Registry.Get(root); !ok || tc.Notes != noteCap {
		t.Errorf("Notes = %d (tracked %v), want %d — the counter must end at the cap, not past it or short of it", tc.Notes, ok, noteCap)
	}
}

// TestOpenPRMarksTheFirstNoteSoAReplayCannotDuplicateIt closes the one gap in
// the idempotency this package already built.
//
// noteKey and okf.NoteMarker exist because the layers above replay: internal/
// server dedups Slack deliveries through a bounded, PER-PROCESS set that is
// wiped wholesale at capacity, so a busy channel, a restart or a leader failover
// all deliver the same message twice. Both APPEND paths honoured that — an entry
// already carrying the key is left alone. The OPEN_PR path did not: it wrote
// note 1 into a brand-new entry with no marker at all, so a redelivery found
// nothing to match, took the append route (NoteURL is set by then) and wrote the
// same note into the entry a second time. Permanent catalog content, indexed and
// recalled twice, with no signal that either is a duplicate.
//
// The oracle is the CONTRACT between the two layers rather than a simulated
// forge: the marker the entry carries must be the one the replay's append then
// looks for. github.Client and gitlab.Client both implement HasNoteMarker as a
// substring search for exactly that literal, and both have their own tests for
// the skip; faking that here would prove only that the fake agrees with itself.
func TestOpenPRMarksTheFirstNoteSoAReplayCannotDuplicateIt(t *testing.T) {
	f := &fakeForge{openURL: "https://github.com/o/r/pull/7"}
	r := newTestResponder(t, f)
	tc := Context{Transport: "slack", Root: "root-1", Channel: "C1"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ctx := context.Background()
	const text = "the real cause was a spot-node reclaim"

	if _, err := r.record(ctx, tc, HumanNote("alice", text)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want 1", len(f.opened))
	}

	// The replay: same thread, same author, same text — everything that does not
	// move between a delivery and its redelivery.
	fresh, _ := r.Registry.Get("root-1")
	if _, err := r.record(ctx, fresh, HumanNote("alice", text)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(f.appends) != 1 {
		t.Fatalf("appends = %d, want the replay to have taken the append route", len(f.appends))
	}

	key := f.appends[0].key
	if key == "" {
		t.Fatal("the replay's append carries no key, so the forge has nothing to match")
	}
	if marker := okf.NoteMarker(key); !strings.Contains(f.opened[0].Body, marker) {
		t.Errorf("the entry opened for note 1 does not carry %s, so the replay's HasNoteMarker finds "+
			"nothing and writes the same note into the catalog twice.\nentry body:\n%s", marker, f.opened[0].Body)
	}
}

// TestOpenPRNoteMarkerIsTheKeyTheAppendPathDerives keeps the two halves of that
// contract from drifting apart in the harmless-looking direction: an entry that
// carries SOME marker, derived differently from the one an append looks for,
// reads as correct and dedups nothing.
func TestOpenPRNoteMarkerIsTheKeyTheAppendPathDerives(t *testing.T) {
	f := &fakeForge{openURL: "https://github.com/o/r/pull/7"}
	r := newTestResponder(t, f)
	tc := Context{Transport: "slack", Root: "root-1"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	n := HumanNote("alice", "the real cause was a spot-node reclaim")
	if _, err := r.record(context.Background(), tc, n); err != nil {
		t.Fatalf("record: %v", err)
	}
	// noteKey is derived from the note AS WRITTEN — redacted and capped — which is
	// what write() files, so the expectation has to go through the same step.
	written, _ := r.noteAsWritten(n)
	want := okf.NoteMarker(noteKey(tc, written))
	if !strings.Contains(f.opened[0].Body, want) {
		t.Errorf("entry carries no %s\nbody:\n%s", want, f.opened[0].Body)
	}
}

// TestModelDraftedReplayIsNotDeduplicated states the limit of all this, because
// a guarantee that quietly does not hold on one route is worse than one that
// says where it stops.
//
// noteKey hashes the note's own TEXT, and on the freeform route that text is the
// model's draft rather than the human's message. A replayed delivery re-invokes
// the model, which is not deterministic, so the redelivery arrives with
// different bytes, hashes to a different key, and appends as a genuinely new
// note. Nothing at this layer can fix that: the only stable identity a replay
// shares is the human's original message, and keying on THAT would collapse two
// deliberate follow-ups drafted from similar messages into one — silently
// dropping a note somebody meant to file, which is the worse failure of the two.
//
// What bounds it instead is upstream and already there: the per-thread cap (now
// enforced under the thread's own guard), the global ForgeWrites window, and
// internal/server's delivery dedup, which catches the common replay before a
// model is ever called. A duplicated model draft is visible prose in an entry a
// human still has to merge, not silent catalog corruption.
func TestModelDraftedReplayIsNotDeduplicated(t *testing.T) {
	tc := Context{Transport: "slack", Root: "root-1"}
	const message = "was it the CNI?"
	first := ProposedNote("alice", message, "It was a spot-node reclaim.")
	replay := ProposedNote("alice", message, "The cause was a spot node being reclaimed.")

	if noteKey(tc, first) == noteKey(tc, replay) {
		t.Fatal("two different drafts hashed to one key — noteKey no longer keys on the note text")
	}
	// The human's own message is identical across the two, which is exactly the
	// identity that is NOT used, and the comment above says why.
	if first.DraftedFrom != replay.DraftedFrom {
		t.Fatal("fixture error: the replayed delivery must carry the same human message")
	}
}

// TestAnnouncementCarriesWhoActuallyWroteTheNote closes the last surface on
// which model-authored prose was filed under a named human.
//
// Every other surface already made the distinction. NoteBody heads a drafted
// entry "🤖 Proposed operator note — drafted by RunLore" and states "@alice did
// not write it"; conceptDescription leads with the provenance clause so a
// listing that clips the line cannot clip it; openedWith exists ONLY to stop the
// thread reply saying "with your note" for text the human did not type. The
// announcement had no provenance field at all, so it published
// {author: "alice", note: "<the model's text>"} to every configured sink and
// rendered "By alice in a slack thread" — the exact claim openedWith exists to
// prevent, arriving through the one door nobody had checked.
//
// It is worst under announce_kb_updates: thread, where the reduced form is two
// lines and one of them IS the attribution — and where, if the best-effort reply
// fails, it is the only thing in the thread.
func TestAnnouncementCarriesWhoActuallyWroteTheNote(t *testing.T) {
	for _, tt := range []struct {
		name string
		note Note
		want bool
	}{
		{"an explicit note: is the human's own words", HumanNote("alice", "the real cause was a spot reclaim"), false},
		{"a freeform answer's note is the model's", ProposedNote("alice", "was it the CNI?", "It was a spot reclaim."), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeForge{openURL: "https://github.com/o/r/pull/7"}
			r, sink := newAnnouncingResponder(t, f)
			tc := Context{Transport: "slack", Root: "root-1", Channel: "C1"}
			if err := r.Registry.Put(tc); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if _, err := r.record(context.Background(), tc, tt.note); err != nil {
				t.Fatalf("record: %v", err)
			}
			drainAnnouncements(t, r)

			ups := sink.updates()
			if len(ups) != 1 {
				t.Fatalf("announced %d writes, want 1", len(ups))
			}
			if got := ups[0].ModelDrafted; got != tt.want {
				t.Errorf("KBUpdate.ModelDrafted = %v, want %v — the announcement reports %q as the note's "+
					"provenance and carries %q as its text", got, tt.want, ups[0].Author, ups[0].Note)
			}
			// Author stays the human either way: they are still whose message
			// produced the note, which is what a reader needs to follow it up. What
			// changes is whether the announcement may present the TEXT as theirs.
			if ups[0].Author != "alice" {
				t.Errorf("Author = %q, want the human whose message produced the note", ups[0].Author)
			}
		})
	}
}

// noteFactsOnTheAnnouncement names, for every field of Note, the KBUpdate field
// that carries it to a sink. Stated here rather than derived, so a fact added to
// Note has to be deliberately routed or deliberately withheld.
//
// It exists because the two field-classification guards that already cover
// KBUpdate — the untrusted/trusted split in internal/providers and the webhook
// payload's coverage check — both WALK KBUPDATE. A fact that was never put on
// the struct is invisible to both: they can only ask whether an existing field
// is handled, never whether a field is missing. Note.DraftedFrom was exactly
// that for as long as the announcement existed, and every one of those guards
// passed the whole time.
//
// This walks the SOURCE type instead, which is the direction that bites.
var noteFactsOnTheAnnouncement = map[string]string{
	"Author": "Author",
	"Text":   "Note",
	// The FACT, not the text. The human's original message is already in the
	// entry, where a reviewer weighs the draft against it (see NoteBody); an
	// announcement is a short chat post to sinks the thread never reached, and
	// republishing someone's raw message into all of them widens egress for no
	// reader who needs it. What must travel is that the note is not their words.
	"DraftedFrom": "ModelDrafted",
}

// TestKBUpdateCarriesEveryNoteFact makes the map above bite in both directions,
// and adds the behavioural half a name-matching test cannot give: two notes that
// differ ONLY in provenance must not produce indistinguishable announcements.
func TestKBUpdateCarriesEveryNoteFact(t *testing.T) {
	note := reflect.TypeOf(Note{})
	update := reflect.TypeOf(providers.KBUpdate{})

	for i := range note.NumField() {
		name := note.Field(i).Name
		carrier, ok := noteFactsOnTheAnnouncement[name]
		if !ok {
			t.Errorf("thread.Note.%s reaches no announcement and is not classified. Route it to a "+
				"providers.KBUpdate field, or record why a sink does not need it — a fact never put on "+
				"the struct is one neither KBUpdate guard can see is missing.", name)
			continue
		}
		if _, found := update.FieldByName(carrier); !found {
			t.Errorf("thread.Note.%s is classified as reaching providers.KBUpdate.%s, which does not exist — "+
				"stale entry, so nothing is checking whatever replaced it", name, carrier)
		}
	}
	for name := range noteFactsOnTheAnnouncement {
		if _, ok := note.FieldByName(name); !ok {
			t.Errorf("noteFactsOnTheAnnouncement names %q, which thread.Note no longer has — stale entry", name)
		}
	}

	// The behavioural half. Same author, same filed text, same thread: the only
	// difference is who wrote it, and the two events must not be equal.
	r := &Responder{Now: func() time.Time { return noteAt }}
	human := r.kbUpdateFor(Context{Transport: "slack", Root: "r"}, HumanNote("alice", "a spot reclaim"),
		&KBWrite{Route: RouteOpenPR, PR: 7, Note: "a spot reclaim"}, noteAt)
	drafted := r.kbUpdateFor(Context{Transport: "slack", Root: "r"}, ProposedNote("alice", "was it the CNI?", "a spot reclaim"),
		&KBWrite{Route: RouteOpenPR, PR: 7, Note: "a spot reclaim"}, noteAt)
	if human == drafted {
		t.Error("a note the human typed and a note RunLore's model drafted announce identically — " +
			"the classification above is satisfied by a field nothing actually sets")
	}
}

// TestOpenPRRouteReportsWhatTheDraftedEntryGetsWrong closes the thread half of
// #518. The curator's PR path runs a draft-time diagnostic before the pull
// request exists; the standalone-note route opened one with nothing in the log
// at all. So a note filed under a `resource` recall can never match died exactly
// as silently as the curated entry that started #518 — except that here the
// human was told, in their own thread, that it was saved.
//
// It is a report, never a gate: the entry is filed either way, because losing a
// human's correction over a frontmatter defect is the worse trade.
func TestOpenPRRouteReportsWhatTheDraftedEntryGetsWrong(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.Log = slog.New(slog.NewTextHandler(&logs, nil))
	// A separator EntryResourceRef does not cut at, so the value ships whole: it
	// carries no whitespace (the merge gate passes it) and can never equal a
	// Workload.Ref() (recall can never match it) — #518's silent half.
	tc := putContext(t, r, Context{
		Root: "111.222", Transport: "slack",
		Title: "Core Argo CD Applications stuck OutOfSync", Resource: "argocd/essentials|monitoring",
	})

	if _, _, err := r.write(context.Background(), tc, HumanNote("alice", "the poisoned chart cache theory is wrong"), noteAt); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("the report must never cost the note: opened = %d, want 1", len(f.opened))
	}
	out := logs.String()
	if !strings.Contains(out, "argocd/essentials|monitoring") {
		t.Errorf("expected a visible warning naming the unusable recall index, got logs:\n%s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("the draft-time report must warn, never fail the write, got logs:\n%s", out)
	}
}

// TestOpenPRRouteDoesNotWarnAboutAConceptsAbsentResource keeps that report
// honest about the type distinction it inherits from the validator.
// ConceptEntry types its entry Concept precisely so the Incident-only rules do
// not apply: OKF omits `resource` for abstract knowledge, kbvalidate requires it
// for Incident only, and an ordinary operator note has none. Warning about it
// would fire on every note ever captured, which is the same as not warning at all.
func TestOpenPRRouteDoesNotWarnAboutAConceptsAbsentResource(t *testing.T) {
	var logs bytes.Buffer
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.Log = slog.New(slog.NewTextHandler(&logs, nil))
	tc := putContext(t, r, Context{Root: "111.222", Transport: "slack", Title: "OOM in payments"})

	if _, _, err := r.write(context.Background(), tc, HumanNote("alice", "this recurs after every spot-node reclaim"), noteAt); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want 1", len(f.opened))
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("a clean operator note must warn about nothing, or the signal is worthless; got:\n%s", out)
	}
}

// TestOpenPRRouteCountsADefectiveDraftUnderItsDefect is the thread half of "both
// entry writers are counted", and the reason the label vocabulary is written in
// terms of the ENTRY rather than the curator: this route has no investigation, no
// verdict and no confidence — it files a human's correction — yet it opens a KB
// pull request exactly as the curator does, and an entry filed here with an
// unusable recall index dies exactly as silently. The one metric an operator
// alerts on has to cover it, under the same label.
func TestOpenPRRouteCountsADefectiveDraftUnderItsDefect(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.Metrics = telemetry.NewMetrics()
	// No whitespace, so the merge gate passes it; not shaped namespace/name, so
	// recall can never match it — #518's silent half.
	tc := putContext(t, r, Context{
		Root: "111.222", Transport: "slack",
		Title: "Core Argo CD Applications stuck OutOfSync", Resource: "argocd/essentials|monitoring",
	})

	if _, _, err := r.write(context.Background(), tc, HumanNote("alice", "the poisoned chart cache theory is wrong"), noteAt); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("counting a defect must never cost the note: opened = %d, want 1", len(f.opened))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{"runlore_kb_draft_defects_total", `defect="unrecallable_resource"`} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape does not contain %q; got:\n%s", want, body)
		}
	}
}
