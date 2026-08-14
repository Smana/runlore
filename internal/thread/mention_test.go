// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

type fakeReplier struct {
	replies []string
	err     error
}

func (f *fakeReplier) ReplyInThread(_ context.Context, _, _, text string) error {
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
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestMentionKnownThreadWritesAndReplies(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	if err := m.Registry.Put(Context{Root: "111.222", Channel: "C1", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "<@U0BOT> note: spot reclaim")

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

	m.HandleMention(context.Background(), "C1", "999.888", "alice", "note: x")

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

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x")

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

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x")

	if len(f.comments) != 1 {
		t.Fatal("the KB write must not be rolled back when the reply fails to post")
	}
}

func TestMentionWithNoReplierStillWrites(t *testing.T) {
	f := &fakeForge{}
	m := newTestMention(t, f, nil)
	m.Replier = nil
	if err := m.Registry.Put(Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x")

	if len(f.comments) != 1 {
		t.Fatal("a missing replier must not lose the note")
	}
}
