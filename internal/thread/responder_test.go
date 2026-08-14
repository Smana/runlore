// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
)

type fakeForge struct {
	comments []struct {
		number int
		body   string
	}
	opened  []providers.KBEntry
	openURL string
	openErr error
	commErr error
}

func (f *fakeForge) CommentOnPR(_ context.Context, number int, body string) error {
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
		OpenPRs:           ratelimit.New(10, time.Hour),
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
	r.OpenPRs = ratelimit.New(1, time.Hour)

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
		t.Fatalf("opened = %d, want 1 — the global OpenPR budget caps the second", len(f.opened))
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

func TestHandleFreeformIsCapturedWhenNoModelIsWired(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> the cause was a spot reclaim")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("freeform must still be captured; comments = %d", len(f.comments))
	}
	if !strings.Contains(strings.ToLower(reply), "note:") {
		t.Errorf("the reply should teach the explicit prefix: %q", reply)
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
