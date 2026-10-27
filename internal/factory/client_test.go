// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
)

// fakeFactory is ZITADEL's token endpoint and the factory's API in one server: tokens expire after
// ttl, and /v1/tasks answers from script, one reply per call, recording each Idempotency-Key.
type fakeFactory struct {
	ttl     time.Duration
	tokens  atomic.Int32
	keys    []string
	bodies  []TaskRequest
	newRoom string // when set, 201 answers carry this room
	script  []int
}

func (f *fakeFactory) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/v2/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "client_credentials" || !strings.Contains(r.Form.Get("scope"), "urn:zitadel:iam:org:project:id:proj1:aud") {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			n := f.tokens.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("tok%d", n), "expires_in": f.ttl.Milliseconds() / 1000})
		case "/v1/tasks":
			f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
			var tr TaskRequest
			_ = json.NewDecoder(r.Body).Decode(&tr)
			f.bodies = append(f.bodies, tr)
			code := f.script[0]
			f.script = f.script[1:]
			w.WriteHeader(code)
			switch code {
			case http.StatusCreated:
				room := "abcdefgh"
				if f.newRoom != "" {
					room = f.newRoom
				}
				_, _ = w.Write([]byte(`{"task":"` + room + `","room":"` + room + `","issue":42}`))
			case http.StatusOK:
				_, _ = w.Write([]byte(`{"task":"abcdefgh","room":"abcdefgh","issue":42,"duplicate":true}`))
			case http.StatusAccepted:
				_, _ = w.Write([]byte(`{"accepted":false,"reason":"below_threshold"}`))
			case http.StatusTooManyRequests:
				_, _ = w.Write([]byte(`{"error":"principal_daily_tasks"}`))
			default:
				_, _ = w.Write([]byte(`{"error":"tasks_unavailable"}`))
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func init() { retrySleep = func(context.Context, time.Duration) error { return nil } }

func newTestClient(t *testing.T, f *fakeFactory, clock *time.Time) *Client {
	srv := f.server(t)
	env := map[string]string{"ID": "runlore", "SECRET": "s3cret", "PROJ": "proj1"}
	return NewClient(config.Factory{URL: srv.URL, TokenURL: srv.URL + "/oauth/v2/token", ClientIDEnv: "ID",
		ClientSecretEnv: "SECRET", ProjectIDEnv: "PROJ", Repository: "Smana/cloud-native-ref", MaxCostUSD: 2}, // pragma: allowlist secret
		func(k string) string { return env[k] }, srv.Client(), func() time.Time { return *clock })
}

var req = TaskRequest{Repository: "Smana/cloud-native-ref", Text: "t", DataClass: "internal", MaxCostUSD: 2,
	Incident: Incident{Alert: "KubePodCrashLooping", Resource: "apps/web", Verdict: "action_required", Confidence: 0.9}}

func TestCreateTaskAnswers(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	f := &fakeFactory{ttl: time.Hour, script: []int{201, 200, 202, 429, 503, 503, 201}}
	c := newTestClient(t, f, &clock)
	ctx := context.Background()
	for _, want := range []Created{
		{Status: 201, Task: "abcdefgh", Room: "abcdefgh", Issue: 42},
		{Status: 200, Task: "abcdefgh", Room: "abcdefgh", Issue: 42, Duplicate: true},
		{Status: 202, Reason: "below_threshold"},
		{Status: 429, Reason: "principal_daily_tasks"},
		{Status: 201, Task: "abcdefgh", Room: "abcdefgh", Issue: 42}, // two 503s, retried
	} {
		got, err := c.CreateTask(ctx, "k1", req)
		if err != nil || got != want {
			t.Fatalf("want %+v, got %+v %v", want, got, err)
		}
	}
	for _, k := range f.keys {
		if k != "k1" {
			t.Fatalf("a retry changed the key: %v", f.keys)
		}
	}
	if f.tokens.Load() != 1 {
		t.Fatalf("one token for an hour's calls, got %d", f.tokens.Load())
	}
	clock = clock.Add(59*time.Minute + 30*time.Second) // within 60 s of expiry: refresh
	f.script = []int{201}
	if _, err := c.CreateTask(ctx, "k2", req); err != nil || f.tokens.Load() != 2 {
		t.Fatalf("refresh: %v, %d tokens", err, f.tokens.Load())
	}
}

func TestCreateTaskGivesUpAfterThree503(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	c := newTestClient(t, &fakeFactory{ttl: time.Hour, script: []int{503, 503, 503}}, &clock)
	if _, err := c.CreateTask(context.Background(), "k", req); err == nil {
		t.Fatal("three 503s are an error")
	}
}

func TestIdempotencyKey(t *testing.T) {
	short := "kubepodcrashlooping|apps|deployment|web|gcp-0"
	if IdempotencyKey(short) != short {
		t.Fatal("a short key is the TriggerKey itself")
	}
	long := strings.Repeat("x", 256)
	if k := IdempotencyKey(long); len(k) != 64 || k == long {
		t.Fatalf("past 255 bytes the key is its SHA-256 hex: %q", k)
	}
}

func TestClientOffWithoutItsEnv(t *testing.T) {
	c := NewClient(config.Factory{URL: "http://f", TokenURL: "https://a", ClientIDEnv: "ID", ClientSecretEnv: "S", ProjectIDEnv: "P"},
		func(string) string { return "" }, &http.Client{}, time.Now)
	if c.Enabled() {
		t.Fatal("empty credentials: off")
	}
}

func TestRetrySleepHonoursTheContext(t *testing.T) {
	saved := retrySleep
	t.Cleanup(func() { retrySleep = saved })
	retrySleep = defaultRetrySleep
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := retrySleep(ctx, time.Minute); err == nil || time.Since(start) > time.Second {
		t.Fatalf("a cancelled context ends the wait at once: %v", err)
	}
	clock := time.Unix(1_800_000_000, 0)
	c := newTestClient(t, &fakeFactory{ttl: time.Hour, script: []int{503, 503, 503}}, &clock)
	retrySleep = defaultRetrySleep
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	start = time.Now()
	if _, err := c.CreateTask(ctx2, "k", req); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("CreateTask gives up when its context ends, not after the 1 s and 3 s waits: %v after %s", err, time.Since(start))
	}
}
