// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// fakeSlackForge is thread.Forge, recording whether either write entry point
// was ever called — used to prove a reserved command never reaches the
// knowledge base, on the REAL Slack pipeline (handleSlackEvent →
// eventDispatcher → thread.Mention.HandleMention → thread.Responder.Handle),
// not a hand-built thread.Parse call. A regression anywhere in that chain,
// not only in the grammar, would be caught here.
type fakeSlackForge struct {
	mu        sync.Mutex
	opened    int
	commented int
	appended  int
}

func (f *fakeSlackForge) OpenPR(context.Context, providers.KBEntry) (providers.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return providers.Ref{URL: "https://github.com/o/r/pull/1"}, nil
}

func (f *fakeSlackForge) CommentOnPR(context.Context, int, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commented++
	return nil
}

func (f *fakeSlackForge) IsPROpen(context.Context, int) (bool, error) { return true, nil }

func (f *fakeSlackForge) AppendToEntryOnPR(context.Context, int, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended++
	return nil
}

// counts sums EVERY knowledge-base write entry point, not only the two that
// existed when this fake was written. A reserved command must reach none of
// them, so a third route added later must be counted here or this fake would
// keep reporting zero writes for a route it simply is not looking at.
func (f *fakeSlackForge) counts() (opened, commented int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, f.commented + f.appended
}

// fakeSlackReplier is thread.Replier: it records every reply so the
// detached mention handler's output can be observed without a live Slack
// endpoint. done fires once the expected number of replies has landed, so a
// test can wait deterministically instead of sleeping.
type fakeSlackReplier struct {
	mu     sync.Mutex
	calls  []string
	doneAt int
	done   chan struct{}
}

func (f *fakeSlackReplier) ReplyInThread(_ context.Context, _, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, text)
	if f.done != nil && len(f.calls) == f.doneAt {
		close(f.done)
	}
	return nil
}

func (f *fakeSlackReplier) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// newRealThreadPipeline wires a real thread.Registry + thread.Responder +
// thread.Mention (with a fake Forge and fake Replier) into a *Server exactly
// as production wiring does — the "real pipeline" the task calls for, as
// opposed to server_test.go/events_test.go's capturingThreadHandler, which is
// a ThreadHandler fake that never touches Responder/Parse/Forge at all and so
// cannot catch a grammar regression like the one this file pins.
func newRealThreadPipeline(t *testing.T, root string) (*Server, *fakeSlackForge, *fakeSlackReplier) {
	t.Helper()
	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.Put(thread.Context{Root: root, Transport: "slack", Channel: "C1"}); err != nil {
		t.Fatalf("registry Put: %v", err)
	}
	forge := &fakeSlackForge{}
	responder := &thread.Responder{Forge: forge, Registry: reg, Log: discardLog}
	rep := &fakeSlackReplier{doneAt: 1, done: make(chan struct{})}
	mention := &thread.Mention{Responder: responder, Registry: reg, Replier: rep, Log: discardLog}

	s := New(nil, Actions{SlackSecret: testSigningSecret, Threads: mention}, nil, nil, nil, nil, discardLog)
	return s, forge, rep
}

func waitForReply(t *testing.T, rep *fakeSlackReplier) {
	t.Helper()
	select {
	case <-rep.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the reply to be posted")
	}
}

// TestSlackEventReservedPrefixAnywhereIsRefused is the end-to-end regression
// test for the defect this commit fixes: thread.Parse used to match
// "reinvestigate:" only at position 0 of the mention-stripped text, so a
// single filler word between the "<@U0BOT>" mention and the command —
// "<@U0BOT> please reinvestigate: the network issue" — fell through to
// IntentFreeform, and thread.Responder.Handle treats IntentFreeform
// identically to an explicit "note:": the operator's re-run request was
// silently written to the knowledge base as a standalone Concept PR and
// reported back as "Noted", leaving them believing a re-run started, or that
// their words were saved, when neither happened — a false success worse than
// the noise a refusal would have caused. This is Slack's own delivery shape —
// a bracketed "<@U…>" mention token, exactly what handleSlackEvent hands to
// thread.Mention.HandleMention verbatim. Driven through the REAL
// handleSlackEvent → eventDispatcher → thread.Mention.HandleMention →
// thread.Responder.Handle pipeline (a real Registry entry, a real Responder,
// a fake Forge), asserting ZERO forge calls — not just the parsed intent.
func TestSlackEventReservedPrefixAnywhereIsRefused(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"leading filler word", "<@U0BOT> please reinvestigate: the network issue"},
		{"different leading filler", "<@U0BOT> can you reinvestigate: this"},
		{"position 0, unchanged from before this fix", "<@U0BOT> reinvestigate: go look"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := fmt.Sprintf("111.%d", i)
			s, forge, rep := newRealThreadPipeline(t, root)

			body := fmt.Sprintf(`{"type":"event_callback","event_id":"E%d","event":{"type":"app_mention","user":"U1","text":%q,"channel":"C1","ts":"333.444","thread_ts":%q}}`,
				i, tt.text, root)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
			if rec.Code != 200 {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			waitForReply(t, rep)

			if opened, commented := forge.counts(); opened != 0 || commented != 0 {
				t.Fatalf("forge: opened=%d commented=%d, want 0/0 — a reserved command must never write to the knowledge base no matter what precedes it (text: %q)",
					opened, commented, tt.text)
			}
			if got := rep.snapshot(); len(got) != 1 || got[0] != thread.ReinvestigateNotSupportedReply {
				t.Fatalf("replies = %+v, want exactly one reply %q", got, thread.ReinvestigateNotSupportedReply)
			}
		})
	}
}

// TestSlackEventNoteContainingReservedWordWithoutColonIsCaptured is the
// narrowness counterpart, on the same real Slack pipeline: an explicit note
// whose text happens to contain the bare word "reinvestigate" — no trailing
// ':' — must still be captured, not refused as the reserved command. This is
// what proves the fix is a colon-anchored token match, not a blanket refusal
// of the word wherever it appears.
//
// This test used to drive the pipeline with NO "note:" prefix at all — under
// the old contract, freeform text was captured exactly like an explicit note,
// so that was sufficient to exercise the reserved-word boundary. Freeform no
// longer writes to the knowledge base at all (see
// thread.FreeformNotRecordedReply), so an explicit "note:" prefix is now
// required to reach the write path; the reserved-word boundary this test
// pins is otherwise unchanged.
func TestSlackEventNoteContainingReservedWordWithoutColonIsCaptured(t *testing.T) {
	const root = "111.222"
	s, forge, rep := newRealThreadPipeline(t, root)

	body := fmt.Sprintf(`{"type":"event_callback","event_id":"E-narrow","event":{"type":"app_mention","user":"U1","text":%q,"channel":"C1","ts":"333.444","thread_ts":%q}}`,
		"<@U0BOT> note: we had to reinvestigate the DNS path and it was stale", root)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	waitForReply(t, rep)

	if opened, _ := forge.counts(); opened != 1 {
		t.Fatalf("forge.opened = %d, want 1 — a note whose text contains the bare word without a colon must still be recorded", opened)
	}
	if got := rep.snapshot(); len(got) != 1 || got[0] == thread.ReinvestigateNotSupportedReply {
		t.Fatalf("replies = %+v, want the note recorded, not refused", got)
	}
}

// TestSlackEventFreeformWritesNothing is the sibling regression test for the
// second grammar defect a security audit found on this same pipeline:
// thread.Responder.Handle used to treat IntentFreeform identically to an
// explicit "note:", so ANY addressed message with no recognised prefix —
// including something as ordinary as "anyone checked what runlore said about
// the CNI?" — silently wrote to the knowledge base. Driven through the REAL
// handleSlackEvent → eventDispatcher → thread.Mention.HandleMention →
// thread.Responder.Handle pipeline, asserting ZERO forge calls.
func TestSlackEventFreeformWritesNothing(t *testing.T) {
	const root = "111.333"
	s, forge, rep := newRealThreadPipeline(t, root)

	body := fmt.Sprintf(`{"type":"event_callback","event_id":"E-freeform","event":{"type":"app_mention","user":"U1","text":%q,"channel":"C1","ts":"333.444","thread_ts":%q}}`,
		"<@U0BOT> anyone checked what runlore said about the CNI?", root)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	waitForReply(t, rep)

	if opened, commented := forge.counts(); opened != 0 || commented != 0 {
		t.Fatalf("forge: opened=%d commented=%d, want 0/0 — freeform text must never write to the knowledge base", opened, commented)
	}
	if got := rep.snapshot(); len(got) != 1 || got[0] != thread.FreeformNotRecordedReply {
		t.Fatalf("replies = %+v, want exactly one reply %q", got, thread.FreeformNotRecordedReply)
	}
}
