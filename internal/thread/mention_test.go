// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/ratelimit"
)

// mu guards replies: TestMentionConcurrentFirstMessagesRehydrateRegistryOnceAndCountEveryWrite
// drives HandleMention from real goroutines, all of which reply.
type fakeReplier struct {
	mu      sync.Mutex
	replies []string
	err     error
}

func (f *fakeReplier) ReplyInThread(_ context.Context, _, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, text)
	return f.err
}

func newTestMention(t *testing.T, f *fakeForge, rep *fakeReplier) *Mention {
	t.Helper()
	r := newTestResponder(t, f)
	return &Mention{
		Responder: r,
		Registry:  r.Registry,
		Replier:   rep,
		Log:       silentLog(),
	}
}

func TestMentionKnownThreadWritesAndReplies(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	if err := m.Registry.Put(Context{Root: "111.222", Channel: "C1", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "<@U0BOT> note: spot reclaim", nil)

	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if len(rep.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(rep.replies))
	}
	if !strings.Contains(rep.replies[0], "42") {
		t.Errorf("reply must name the PR: %q", rep.replies[0])
	}
}

func TestMentionUnknownThreadRepliesAndWritesNothing(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)

	m.HandleMention(context.Background(), "C1", "999.888", "alice", "note: x", nil)

	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatal("an unknown thread must write nothing to the KB")
	}
	if len(rep.replies) != 1 {
		t.Fatalf("an unknown thread must still get a reply, got %d", len(rep.replies))
	}
	if !strings.Contains(strings.ToLower(rep.replies[0]), "don't have") &&
		!strings.Contains(strings.ToLower(rep.replies[0]), "do not have") {
		t.Errorf("the reply must name the limitation: %q", rep.replies[0])
	}
}

func TestMentionRepliesEvenWhenTheWriteFails(t *testing.T) {
	f, rep := &fakeForge{commErr: errors.New("403 forbidden")}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	if err := m.Registry.Put(Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x", nil)

	if len(rep.replies) != 1 {
		t.Fatalf("replies = %d, want 1 — a failed write must still be reported", len(rep.replies))
	}
	if !strings.Contains(rep.replies[0], "403 forbidden") {
		t.Errorf("the reply must carry the reason: %q", rep.replies[0])
	}
}

func TestMentionSurvivesAReplyFailure(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{err: errors.New("channel_not_found")}
	m := newTestMention(t, f, rep)
	if err := m.Registry.Put(Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x", nil)

	if len(f.comments) != 1 {
		t.Fatal("the KB write must not be rolled back when the reply fails to post")
	}
}

func TestMentionBusyRepliesInThreadWithoutTouchingTheKB(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)

	m.Busy(context.Background(), "C1", "111.222")

	if len(rep.replies) != 1 {
		t.Fatalf("replies = %d, want 1 — Busy must post an in-thread notice", len(rep.replies))
	}
	if rep.replies[0] == "" {
		t.Fatal("Busy must not post an empty reply")
	}
	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatal("Busy must never touch the knowledge base — it only tells the human to retry")
	}
}

func TestMentionBusyWithNoReplierIsNoop(t *testing.T) {
	f := &fakeForge{}
	m := newTestMention(t, f, nil)
	m.Replier = nil

	m.Busy(context.Background(), "C1", "111.222") // must not panic
}

func TestMentionWithNoReplierStillWrites(t *testing.T) {
	f := &fakeForge{}
	m := newTestMention(t, f, nil)
	m.Replier = nil
	if err := m.Registry.Put(Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x", nil)

	if len(f.comments) != 1 {
		t.Fatal("a missing replier must not lose the note")
	}
}

// TestMentionDrivesTheChatLayerEndToEnd is the wiring test this route did not
// have: nothing drove HandleMention with a chat model configured, so nothing
// proved the model's answer is actually POSTED. Every other chat test stops at
// Responder.Handle's return value and every mention test runs with Chat nil —
// between them, a chat layer that answered perfectly and a transport entry
// point that dropped the answer would both look correct. That is precisely the
// class that shipped a non-functional feature in PR2.
//
// It is driven through HandleMention rather than Handle so the whole path is
// real: the registry lookup, the responder, the model call, the forge write,
// and the reply that goes back to the Replier.
func TestMentionDrivesTheChatLayerEndToEnd(t *testing.T) {
	setUp := func(t *testing.T, f *fakeForge, rep *fakeReplier, model *fakeChatModel) *Mention {
		t.Helper()
		m := newTestMention(t, f, rep)
		m.Responder.Chat = &Chat{Model: model, Log: silentLog()}
		if err := m.Registry.Put(Context{
			Root: "111.222", Channel: "C1", Title: "pod crash-looping",
			CuratedURL: "https://github.com/o/r/pull/42",
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		return m
	}

	t.Run("the answer is posted and the note it proposed is filed", func(t *testing.T) {
		f, rep := &fakeForge{}, &fakeReplier{}
		model := &fakeChatModel{resp: wellFormedReply(
			"The CNI was ruled out — it was a spot reclaim.",
			"The real cause was a spot-node reclaim, not the CNI.",
		)}
		m := setUp(t, f, rep, model)

		m.HandleMention(context.Background(), "C1", "111.222", "alice", "<@U0BOT> was it the CNI?", nil)

		if model.calls != 1 {
			t.Fatalf("Complete called %d times, want exactly 1 — a freeform mention with model.chat configured must reach the model", model.calls)
		}
		// The model answered from the thread's own context rather than from
		// nothing: a wiring that reached the model with an empty context would
		// still produce a reply, and would still be broken.
		prompt := model.lastReq.Messages[0].Content
		for _, want := range []string{"pod crash-looping", "was it the CNI?", "alice"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("the assembled prompt is missing %q — the thread's context never reached the model:\n%s", want, prompt)
			}
		}

		if len(rep.replies) != 1 {
			t.Fatalf("replies = %d, want 1", len(rep.replies))
		}
		// Read as the human sees it: the untrusted-span marks are the transport's
		// business (see RenderReply), not this contract's.
		posted := RenderReply(rep.replies[0], nil)
		if !strings.Contains(posted, "> The CNI was ruled out — it was a spot reclaim.") {
			t.Fatalf("the model's answer never reached the thread: %q", posted)
		}
		if !strings.Contains(posted, "📝 Noted on the knowledge-base PR #42") {
			t.Fatalf("the human was not told where their note landed: %q", posted)
		}

		if len(f.comments) != 1 || f.comments[0].number != 42 {
			t.Fatalf("comments = %+v, want exactly one on #42 — the proposed note must be filed through the thread's own routing", f.comments)
		}
		if !strings.Contains(f.comments[0].body, "spot-node reclaim, not the CNI") {
			t.Fatalf("the filed note lost the model's own text:\n%s", f.comments[0].body)
		}
		stored, ok := m.Registry.Get("111.222")
		if !ok {
			t.Fatal("the thread went missing from the registry")
		}
		if stored.Notes != 1 {
			t.Fatalf("Notes = %d, want 1 — a note filed through the chat route spends the same per-thread allowance", stored.Notes)
		}
	})

	t.Run("a question with nothing to record is still answered", func(t *testing.T) {
		f, rep := &fakeForge{}, &fakeReplier{}
		model := &fakeChatModel{resp: wellFormedReply("Yes — the probe timeout was raised at 09:14.", "")}
		m := setUp(t, f, rep, model)

		m.HandleMention(context.Background(), "C1", "111.222", "alice", "<@U0BOT> did anyone change the probe?", nil)

		if len(rep.replies) != 1 {
			t.Fatalf("replies = %d, want 1", len(rep.replies))
		}
		if got, want := RenderReply(rep.replies[0], nil), "> Yes — the probe timeout was raised at 09:14."; got != want {
			t.Fatalf("posted %q, want %q — an empty kb_note is the model saying \"file nothing\", and the answer must still be posted", got, want)
		}
		if c, o := f.counts(); c != 0 || o != 0 {
			t.Fatalf("forge calls = %d comments / %d opened, want 0/0 — an empty kb_note writes nothing", c, o)
		}
	})
}

// TestMentionFallbackRehydratesRegistryForNextMessage pins the fix for the
// defect where a fallback context substituted on a registry miss was used for
// that one reply and then discarded: the registry never learned the thread, so
// its per-thread note counter stayed at 0 forever and the cap never engaged.
// A fallback must instead be persisted under its root so every later message
// in the same thread hits the registry directly and keeps counting from where
// the fallback write left off.
func TestMentionFallbackRehydratesRegistryForNextMessage(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	fallback := &Context{Root: "999.888", CuratedURL: "https://github.com/o/r/pull/42"}

	m.HandleMention(context.Background(), "C1", "999.888", "alice", "note: first", fallback)

	stored, ok := m.Registry.Get("999.888")
	if !ok {
		t.Fatal("a fallback context supplied for an unknown root must be persisted to the registry")
	}
	if stored.Notes != 1 {
		t.Fatalf("Notes = %d, want 1 — the write made on the fallback path must be counted", stored.Notes)
	}

	// No fallback supplied this time: the registry must now hit on its own.
	m.HandleMention(context.Background(), "C1", "999.888", "alice", "note: second", nil)

	stored2, ok := m.Registry.Get("999.888")
	if !ok {
		t.Fatal("the rehydrated entry must still be there")
	}
	if stored2.Notes != 2 {
		t.Fatalf("Notes = %d, want 2 after a second message with no fallback supplied", stored2.Notes)
	}
	if len(f.comments) != 2 {
		t.Fatalf("comments = %d, want 2", len(f.comments))
	}
}

// TestMentionFallbackPathEnforcesPerThreadCap proves the per-thread cap is no
// longer permanently inert on the fallback path — the exact "always Notes: 0"
// failure mode the audit found.
func TestMentionFallbackPathEnforcesPerThreadCap(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	m.Responder.MaxNotesPerThread = 1
	fallback := &Context{Root: "r1", CuratedURL: "https://github.com/o/r/pull/42"}

	m.HandleMention(context.Background(), "C1", "r1", "alice", "note: first", fallback)
	m.HandleMention(context.Background(), "C1", "r1", "alice", "note: second", nil)

	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1 — the per-thread cap must apply on the fallback path exactly as on a registry hit", len(f.comments))
	}
	if len(rep.replies) != 2 {
		t.Fatalf("replies = %d, want 2", len(rep.replies))
	}
	if !strings.Contains(strings.ToLower(rep.replies[1]), "limit") {
		t.Errorf("the capped reply must say so: %q", rep.replies[1])
	}
}

// TestMentionRegistryHitTakesPrecedenceOverFallback pins the stated
// precedence: a registry hit carries NoteURL/Notes state a caller-supplied
// fallback stamp cannot, so it must always win over a fallback when both are
// available.
func TestMentionRegistryHitTakesPrecedenceOverFallback(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	if err := m.Registry.Put(Context{Root: "r1", CuratedURL: "https://github.com/o/r/pull/42", Notes: 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fallback := &Context{Root: "r1", CuratedURL: "https://github.com/o/r/pull/999"}

	m.HandleMention(context.Background(), "C1", "r1", "alice", "note: x", fallback)

	if len(f.comments) != 1 || f.comments[0].number != 42 {
		t.Fatalf("must use the registry's CuratedURL (42), not the fallback's (999): comments=%+v", f.comments)
	}
}

// TestMentionFallbackOnDisabledRegistryDoesNotWriteWithZeroValueContext pins
// that GetOrCreate's ErrThreadNotEstablishable is handled explicitly rather
// than falling into the "concurrent winner" branch: a disabled registry
// cannot establish a fallback, and the zero-value Context that comes back
// alongside that error must never be carried into a write — a contextless
// "Operator note" PR with no title, trigger key, or resource, bounded only by
// the global hourly window.
func TestMentionFallbackOnDisabledRegistryDoesNotWriteWithZeroValueContext(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	disabled, err := NewRegistry("", time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m.Registry = disabled
	m.Responder.Registry = disabled
	fallback := &Context{Root: "r1", CuratedURL: "https://github.com/o/r/pull/42"}

	m.HandleMention(context.Background(), "C1", "r1", "alice", "note: x", fallback)

	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatalf("a registry that cannot establish a fallback must never reach a write: comments=%d opened=%d",
			len(f.comments), len(f.opened))
	}
	if len(rep.replies) != 1 {
		t.Fatalf("replies = %d, want 1 — the human must still be told", len(rep.replies))
	}
}

// TestMentionFallbackOnEmptyRootDoesNotWriteWithZeroValueContext mirrors the
// disabled-registry case for an empty root reaching GetOrCreate.
func TestMentionFallbackOnEmptyRootDoesNotWriteWithZeroValueContext(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	fallback := &Context{Root: "", CuratedURL: "https://github.com/o/r/pull/42"}

	m.HandleMention(context.Background(), "C1", "", "alice", "note: x", fallback)

	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatalf("an empty root must never reach a write: comments=%d opened=%d", len(f.comments), len(f.opened))
	}
}

// TestMentionConcurrentFirstMessagesRehydrateRegistryOnceAndCountEveryWrite
// drives HandleMention itself with real goroutines under -race: several
// concurrent first messages on one never-before-tracked thread, all supplying
// the same fallback. It pins three guarantees together: (1) the registry ends
// up with exactly ONE entry for the root — not several divergent ones each
// caller thought was "the" entry (Registry.GetOrCreate's atomicity); (2) the
// note counter on that one entry equals the number of writes that actually
// landed; and (3) exactly ONE of those writes opens a standalone PR — every
// other one comments on it (Responder.write's per-root guard).
//
// The fallback deliberately carries no CuratedURL: an earlier version of this
// test set one, which routed every goroutine to CommentOnPR on an
// already-open PR and never exercised OpenPR at all — the exact route this
// test is about — while its own comment argued at length that pinning
// opened == 1 would be premature. It is not premature once the per-root
// write guard exists: every writer re-reads the registry under that guard
// before choosing a route, so the second and later writers see the first
// writer's freshly-landed NoteURL.
func TestMentionConcurrentFirstMessagesRehydrateRegistryOnceAndCountEveryWrite(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	m.Responder.MaxNotesPerThread = 1000                  // the per-thread cap is not what this test pins
	m.Responder.ForgeWrites = ratelimit.New(0, time.Hour) // 0 = unlimited; the global budget is not what this test pins
	root := "unknown-thread"
	fallback := &Context{Root: root}

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.HandleMention(context.Background(), "C1", root, fmt.Sprintf("user%d", i), "note: concurrent", fallback)
		}(i)
	}
	wg.Wait()

	stored, ok := m.Registry.Get(root)
	if !ok {
		t.Fatal("registry lost the thread after concurrent first messages")
	}

	comments, opened := f.counts()
	if opened != 1 {
		t.Fatalf("opened = %d, want exactly 1 — every concurrent first message on the same thread must land on the ONE PR the first writer opens", opened)
	}
	if comments != n-1 {
		t.Fatalf("comments = %d, want %d — every writer after the first must comment on that one PR", comments, n-1)
	}
	landed := comments + opened
	if stored.Notes != landed {
		t.Fatalf("Notes = %d, want %d — the counter must reflect every write that actually landed, even under concurrency", stored.Notes, landed)
	}
}
