// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestLedgerClaimIsExclusiveUntilReleasedOrSeen(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), func() time.Time { return now })
	if !l.Claim("e1", now) {
		t.Fatal("first claim")
	}
	if l.Claim("e1", now) {
		t.Fatal("a concurrent duplicate does not get the claim")
	}
	l.Release("e1")
	if !l.Claim("e1", now) {
		t.Fatal("released after a failure: the retry may claim")
	}
	must(t, l.MarkSeen("e1", now))
	l.Release("e1")
	if l.Claim("e1", now) {
		t.Fatal("seen ids are not claimable")
	}
}

// blockingReplier holds the first ReplyInThread until released.
type blockingReplier struct {
	replies
	entered chan struct{}
	release chan struct{}
}

func (b *blockingReplier) ReplyInThread(ctx context.Context, root, channel, text string) error {
	close(b.entered)
	<-b.release
	return b.replies.ReplyInThread(ctx, root, channel, text)
}

func TestReceiverDropsADuplicateWhileTheFirstIsInFlight(t *testing.T) {
	g := newRig(t)
	br := &blockingReplier{entered: make(chan struct{}), release: make(chan struct{})}
	g.rc.Replier = br
	h := sign(g.key, "inflight-1", g.now, []byte(endedBody))
	first := make(chan int)
	go func() { first <- g.post(h, endedBody) }()
	<-br.entered
	if got := g.post(h, endedBody); got != http.StatusNoContent {
		t.Fatalf("the duplicate is acknowledged: %d", got)
	}
	close(br.release)
	if got := <-first; got != http.StatusNoContent {
		t.Fatal(got)
	}
	if len(br.got) != 1 {
		t.Fatalf("one post for one event, got %d", len(br.got))
	}
}
