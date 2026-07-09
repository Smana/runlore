package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordSink collects Feedback calls and signals doneAt when the expected
// number has landed, so tests wait on real progress instead of sleeping.
type recordSink struct {
	mu     sync.Mutex
	got    []string // "key/rating/user"
	doneAt int
	done   chan struct{}
}

func (r *recordSink) Feedback(key, rating, user string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, key+"/"+rating+"/"+user)
	if len(r.got) == r.doneAt {
		close(r.done)
	}
	return nil
}

// reactionJSON builds one m.reaction timeline event.
func reactionJSON(sender, target, key string) string {
	return fmt.Sprintf(`{"type":"m.reaction","sender":%q,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":%q,"key":%q}}}`, sender, target, key)
}

// TestMatrixFeedbackRun scripts a homeserver: the first /sync is a position
// handshake whose (historical) events must be SKIPPED; the second batch carries
// a 👍 (variation selector included), a 👎, a foreign emoji, and a reaction to a
// message without the trigger field — only the two votes reach the sink, with
// the Matrix user ids as identities. ctx cancellation stops Run.
func TestMatrixFeedbackRun(t *testing.T) {
	const room = "!r:hs"
	sink := &recordSink{doneAt: 2, done: make(chan struct{})}
	var syncCalls int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/account/whoami":
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@runlore:hs"})
		case strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/sync"):
			mu.Lock()
			syncCalls++
			n := syncCalls
			mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("sync auth = %q", got)
			}
			timeline := ""
			switch n {
			case 1:
				// Handshake response carrying a HISTORICAL reaction that must be skipped.
				if r.URL.Query().Get("since") != "" {
					t.Errorf("first sync must carry no since, got %q", r.URL.Query().Get("since"))
				}
				timeline = reactionJSON("@old:hs", "$msg1", "👍")
			case 2:
				if r.URL.Query().Get("since") != "s1" {
					t.Errorf("second sync since = %q, want s1", r.URL.Query().Get("since"))
				}
				timeline = strings.Join([]string{
					reactionJSON("@alice:hs", "$msg1", "👍️"), // variation selector
					reactionJSON("@bob:hs", "$msg1", "👎"),
					reactionJSON("@carol:hs", "$msg1", "🎉"), // foreign emoji: ignored
					reactionJSON("@dave:hs", "$human", "👍"), // keyless target: ignored
					reactionJSON("@eve:hs", "$spoof", "👍"),  // forged-field target: ignored
				}, ",")
			default:
				// Quiet long-poll: nothing new. (The real server would hold; the test
				// returns immediately — Run just re-polls.)
			}
			_, _ = fmt.Fprintf(w, `{"next_batch":"s%d","rooms":{"join":{%q:{"timeline":{"events":[%s]}}}}}`, n, room, timeline)
		case strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"):
			switch {
			case strings.HasSuffix(r.URL.Path, "/event/$msg1"):
				_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@runlore:hs",
					"content": map[string]any{triggerKeyContentField: "trig-1"}})
			case strings.HasSuffix(r.URL.Path, "/event/$spoof"):
				// The attack the self-check closes: a room member posts their OWN
				// message carrying the trigger field, then votes on it.
				_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@eve:hs",
					"content": map[string]any{triggerKeyContentField: "victim-trigger"}})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@human:hs",
					"content": map[string]any{"body": "a human message"}})
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := NewMatrixFeedback(srv.URL, room, "tok", sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { f.Run(ctx); close(stopped) }()

	select {
	case <-sink.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the two votes")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	want := []string{"trig-1/up/@alice:hs", "trig-1/down/@bob:hs"}
	if len(sink.got) != 2 || sink.got[0] != want[0] || sink.got[1] != want[1] {
		t.Fatalf("recorded = %v, want %v", sink.got, want)
	}
}

// TestMatrixFeedbackSyncErrorRetries: a failing homeserver is logged and
// retried, never fatal — Run keeps polling and records once the server heals.
func TestMatrixFeedbackSyncErrorRetries(t *testing.T) {
	const room = "!r:hs"
	sink := &recordSink{doneAt: 1, done: make(chan struct{})}
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/account/whoami" {
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@runlore:hs"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@runlore:hs",
				"content": map[string]any{triggerKeyContentField: "k"}})
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1:
			w.WriteHeader(http.StatusBadGateway) // transient failure
		case 2:
			_, _ = fmt.Fprintf(w, `{"next_batch":"s1","rooms":{}}`) // handshake
		default:
			_, _ = fmt.Fprintf(w, `{"next_batch":"s%d","rooms":{"join":{%q:{"timeline":{"events":[%s]}}}}}`,
				n, room, reactionJSON("@a:hs", "$m", "👍"))
		}
	}))
	defer srv.Close()

	f := NewMatrixFeedback(srv.URL, room, "tok", sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Shrink the retry pause for the test via a tiny wrapper: not configurable by
	// design (operators shouldn't tune it), so just tolerate the one 5s backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go f.Run(ctx)
	select {
	case <-sink.done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for recovery after a transient sync failure")
	}
}
