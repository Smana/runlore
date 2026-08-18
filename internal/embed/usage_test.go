// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// usageServer answers every /embeddings request with one vector and the given
// prompt_tokens, and counts the requests it served. The counter is atomic: net/http
// serves each request on its own goroutine.
func usageServer(t *testing.T, promptTokens int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0,0.0]}],` +
			`"usage":{"prompt_tokens":` + strconv.Itoa(promptTokens) + `}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &served
}

// TestUsageSinkCollectsWhatTheEndpointBilled is the accounting half of bounding the
// embeddings endpoint: a caller that installs a sink on the context gets back the
// provider-reported prompt tokens of every embedding call made under it, which is
// what lets an investigation fold its own hybrid-recall query embeds into the total
// its spend ceiling compares against.
//
// The metric wiring made this spend VISIBLE; a per-caller sink is what makes it
// ATTRIBUTABLE, and only an attributable figure can be bounded.
func TestUsageSinkCollectsWhatTheEndpointBilled(t *testing.T) {
	srv, _ := usageServer(t, 4242)
	var sink UsageSink
	ctx := WithUsageSink(context.Background(), &sink)

	c := New(srv.URL, "text-embed", "")
	if _, err := c.Embed(ctx, []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := c.Embed(ctx, []string{"b"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	calls, tokens := sink.Totals()
	if calls != 2 || tokens != 8484 {
		t.Fatalf("sink recorded %d calls / %d tokens, want 2 / 8484 — the sink must accumulate "+
			"every /embeddings round trip made under its context", calls, tokens)
	}
}

// TestUsageSinkChargesEveryChunkOfALargeBatch pins that chunking is invisible to the
// accounting: Embed splits an oversized input into maxEmbedBatch-sized requests and
// the endpoint bills each one, so the sink must see each one.
func TestUsageSinkChargesEveryChunkOfALargeBatch(t *testing.T) {
	srv, served := usageServer(t, 100)
	var sink UsageSink
	texts := make([]string, maxEmbedBatch+1)
	for i := range texts {
		texts[i] = "t"
	}
	// The fake returns ONE vector per request, so the count check rejects the call —
	// irrelevant here: the request was still sent, and still billed.
	_, _ = New(srv.URL, "text-embed", "").Embed(WithUsageSink(context.Background(), &sink), texts)
	if served.Load() < 1 {
		t.Fatal("the server was never called")
	}
	calls, _ := sink.Totals()
	if int64(calls) != served.Load() {
		t.Fatalf("sink recorded %d calls for %d requests actually served: chunking must not "+
			"hide round trips from the caller charging for them", calls, served.Load())
	}
}

// TestUsageSinkChargesAFailedCall pins the same rule the investigation loop's model
// calls now follow: a request the endpoint accepted and then failed to answer usably
// is still a call. It is recorded with ZERO tokens — "unknown", never a claim that it
// was free — because a failure before the usage block leaves nothing honest to report.
func TestUsageSinkChargesAFailedCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	var sink UsageSink
	if _, err := New(srv.URL, "text-embed", "").Embed(WithUsageSink(context.Background(), &sink), []string{"a"}); err == nil {
		t.Fatal("a 500 must fail the call")
	}
	calls, tokens := sink.Totals()
	if calls != 1 || tokens != 0 {
		t.Fatalf("sink recorded %d calls / %d tokens for one failed request, want 1 / 0", calls, tokens)
	}
}

// TestUsageSinkIsOptionalAndNilSafe pins that every path without a sink — the CLI, the
// catalog syncer, any test — behaves exactly as before, and that reading a sink that
// was never installed is safe rather than a nil dereference at an incident's worst
// moment.
func TestUsageSinkIsOptionalAndNilSafe(t *testing.T) {
	srv, _ := usageServer(t, 7)
	if _, err := New(srv.URL, "text-embed", "").Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed without a sink: %v", err)
	}
	if calls, tokens := UsageSinkFrom(context.Background()).Totals(); calls != 0 || tokens != 0 {
		t.Fatalf("a context with no sink must total 0/0, got %d/%d", calls, tokens)
	}
	var nilSink *UsageSink
	if calls, tokens := nilSink.Totals(); calls != 0 || tokens != 0 {
		t.Fatalf("a nil sink must total 0/0, got %d/%d", calls, tokens)
	}
}

// TestUsageSinkRecordAndTotalsRace hammers the sink's own two operations directly —
// interleaved writes and reads from many goroutines, which the HTTP-driven test below
// cannot express because it only reads Totals once at the end.
//
// It exists as its own test because the first -race run of this package DID report a
// race, and the report named this file's HTTP fixture (a plain `served++` counter),
// with every frame in both stacks inside net/http and none inside usage.go. "It was
// only the test" is exactly what a real sink race looks like from the inside, so the
// sink's guarantee is asserted here on its own, with no HTTP server near it.
func TestUsageSinkRecordAndTotalsRace(t *testing.T) {
	var sink UsageSink
	var wg sync.WaitGroup
	const workers, iterations = 32, 200
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				sink.record(3)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = sink.Totals()
			}
		}()
	}
	wg.Wait()
	calls, tokens := sink.Totals()
	if calls != workers*iterations || tokens != workers*iterations*3 {
		t.Fatalf("sink lost updates under contention: %d calls / %d tokens, want %d / %d",
			calls, tokens, workers*iterations, workers*iterations*3)
	}
}

// TestUsageSinkIsConcurrencySafe: one sink is shared by every embedding call made
// under one investigation's context, and nothing promises those are sequential.
func TestUsageSinkIsConcurrencySafe(t *testing.T) {
	srv, _ := usageServer(t, 10)
	var sink UsageSink
	ctx := WithUsageSink(context.Background(), &sink)
	c := New(srv.URL, "text-embed", "")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Embed(ctx, []string{"a"})
		}()
	}
	wg.Wait()
	if calls, tokens := sink.Totals(); calls != 16 || tokens != 160 {
		t.Fatalf("sink recorded %d calls / %d tokens, want 16 / 160", calls, tokens)
	}
}
