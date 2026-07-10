// SPDX-License-Identifier: Apache-2.0

package source

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// fakeDecoder is a WebhookSource that returns a fixed DecodeResult.
type fakeDecoder struct {
	result DecodeResult
	err    error
}

func (f fakeDecoder) Decode(_ []byte, _ http.Header) (DecodeResult, error) {
	return f.result, f.err
}

func webhookBuilt(d fakeDecoder) Built {
	return Built{
		Desc: Descriptor{
			Name:      "test-webhook",
			Kind:      Webhook,
			Admission: MatchGated,
			Path:      "/webhook/test",
		},
		Impl: d,
	}
}

func oneRequestResult() DecodeResult {
	return DecodeResult{
		Requests: []investigate.Request{
			{
				Title:       "test alert",
				Severity:    "critical",
				Workload:    providers.Workload{Namespace: "default", Name: "myapp"},
				Fingerprint: "fp-webhook-1",
			},
		},
	}
}

func TestHandlerBadAuth(t *testing.T) {
	enq := &capEnq{}
	pipe := NewPipeline(matchAllCfg(), enq, nil, nil)
	b := webhookBuilt(fakeDecoder{result: oneRequestResult()})
	auth := func(*http.Request) bool { return false }

	h := b.Handler(auth, 1<<20, pipe)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued after bad auth, got %d", len(enq.reqs))
	}
}

func TestHandlerBodyTooLarge(t *testing.T) {
	enq := &capEnq{}
	pipe := NewPipeline(matchAllCfg(), enq, nil, nil)
	b := webhookBuilt(fakeDecoder{result: oneRequestResult()})

	// bodyCap of 5 bytes, body is larger
	h := b.Handler(nil, 5, pipe)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader(`{"very":"long body that exceeds cap"}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", rec.Code)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued after body too large, got %d", len(enq.reqs))
	}
}

func TestHandlerValidRequest(t *testing.T) {
	enq := &capEnq{}
	pipe := NewPipeline(matchAllCfg(), enq, nil, nil)
	b := webhookBuilt(fakeDecoder{result: oneRequestResult()})

	h := b.Handler(nil, 1<<20, pipe)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader(`{"alerts":[]}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued, got %d", len(enq.reqs))
	}
}

// fakeAuthDecoder is a WebhookSource that also self-authenticates (Authenticator),
// like the pagerduty source's HMAC signature check.
type fakeAuthDecoder struct {
	fakeDecoder
	ok bool
}

func (f fakeAuthDecoder) Authenticate(_ []byte, _ http.Header) bool { return f.ok }

// TestHandlerSelfAuthRejects: a source-level Authenticate failure is a 401 and
// nothing reaches the pipeline.
func TestHandlerSelfAuthRejects(t *testing.T) {
	enq := &capEnq{}
	pipe := NewPipeline(matchAllCfg(), enq, nil, nil)
	b := webhookBuilt(fakeDecoder{result: oneRequestResult()})
	b.Impl = fakeAuthDecoder{fakeDecoder{result: oneRequestResult()}, false}

	h := b.Handler(nil, 1<<20, pipe)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued after failed self-auth, got %d", len(enq.reqs))
	}
}

// TestHandlerSelfAuthReplacesSharedAuth: a source that implements Authenticator
// owns its authentication — the shared bearer-token auth must NOT apply to it
// (PagerDuty signs requests; it cannot send the operator's bearer token).
func TestHandlerSelfAuthReplacesSharedAuth(t *testing.T) {
	enq := &capEnq{}
	pipe := NewPipeline(matchAllCfg(), enq, nil, nil)
	b := webhookBuilt(fakeDecoder{result: oneRequestResult()})
	b.Impl = fakeAuthDecoder{fakeDecoder{result: oneRequestResult()}, true}
	sharedAuthRejectsAll := func(*http.Request) bool { return false }

	h := b.Handler(sharedAuthRejectsAll, 1<<20, pipe)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 (self-auth replaces shared auth), got %d", rec.Code)
	}
	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued, got %d", len(enq.reqs))
	}
}

func TestMountWebhooks(t *testing.T) {
	enq := &capEnq{}
	pipe := NewPipeline(matchAllCfg(), enq, nil, nil)
	built := []Built{
		{
			Desc: Descriptor{
				Name:      "wh1",
				Kind:      Webhook,
				Admission: MatchGated,
				Path:      "/webhook/wh1",
			},
			Impl: fakeDecoder{result: oneRequestResult()},
		},
		{
			Desc: Descriptor{
				Name: "watch1",
				Kind: Watcher,
			},
			Impl: fakeDecoder{}, // not a watcher, but Kind=Watcher so should be skipped
		},
	}

	mux := http.NewServeMux()
	MountWebhooks(mux, built, nil, pipe)

	// Send a request to the registered webhook path
	req := httptest.NewRequest(http.MethodPost, "/webhook/wh1", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}

	// The watcher-kind Built should NOT register a path
	req2 := httptest.NewRequest(http.MethodPost, "/webhook/watch1", strings.NewReader(`{}`))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusAccepted {
		t.Fatal("watcher-kind source should not register a webhook path")
	}
}

// Confirm matchAllCfg + MatchGated admits a "critical" request (smoke).
func TestHandlerAdmissionIntegration(t *testing.T) {
	enq := &capEnq{}
	pipe := NewPipeline(matchAllCfg(), enq, nil, nil)
	result := DecodeResult{
		Requests: []investigate.Request{
			{Title: "firing", Severity: "critical", Fingerprint: "fp-adm-1"},
		},
	}
	b := webhookBuilt(fakeDecoder{result: result})
	h := b.Handler(nil, 1<<20, pipe)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued, got %d", len(enq.reqs))
	}
	_ = time.Now() // keep import used
}
