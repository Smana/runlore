// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	ttl        time.Duration
	tokens     atomic.Int32
	keys       []string
	bodies     []TaskRequest
	tokenFails int    // the first N token requests answer 503
	newRoom    string // when set, 201 answers carry this room
	script     []int
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
			if f.tokenFails > 0 {
				f.tokenFails--
				http.Error(w, "down", http.StatusServiceUnavailable)
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

func TestCreateTaskRefreshesTheTokenOnce401(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	f := &fakeFactory{ttl: time.Hour, script: []int{401, 201}}
	c := newTestClient(t, f, &clock)
	got, err := c.CreateTask(context.Background(), "k1", req)
	if err != nil || got.Status != 201 || f.tokens.Load() != 2 {
		t.Fatalf("a revoked token is dropped and the call retried once: %+v %v, %d tokens", got, err, f.tokens.Load())
	}
	f.script = []int{401, 401}
	got, err = c.CreateTask(context.Background(), "k2", req)
	if err != nil || got.Status != 401 || f.tokens.Load() != 3 {
		t.Fatalf("a second 401 is returned, not looped on: %+v %v, %d tokens", got, err, f.tokens.Load())
	}
}

func TestCreateTaskRetriesAFailingTokenEndpoint(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	f := &fakeFactory{ttl: time.Hour, tokenFails: 2, script: []int{201}}
	c := newTestClient(t, f, &clock)
	if got, err := c.CreateTask(context.Background(), "k1", req); err != nil || got.Status != 201 {
		t.Fatalf("a 5xx from the token endpoint is retried: %+v %v", got, err)
	}
	f = &fakeFactory{ttl: time.Hour, tokenFails: 3, script: []int{201}}
	c = newTestClient(t, f, &clock)
	if _, err := c.CreateTask(context.Background(), "k1", req); err == nil {
		t.Fatal("three token failures are an error")
	}
	// A transport error on the token call is retried too.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // nothing listens: connection refused
	c = NewClient(config.Factory{URL: srv.URL, TokenURL: srv.URL, ClientIDEnv: "ID", ClientSecretEnv: "S", ProjectIDEnv: "P"},
		func(string) string { return "x" }, &http.Client{}, func() time.Time { return clock })
	var dials int
	c.hc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { dials++; return nil, errors.New("refused") })
	if _, err := c.CreateTask(context.Background(), "k1", req); err == nil || dials != 3 {
		t.Fatalf("a token transport error is retried like a task one: %d dials, %v", dials, err)
	}
}

// verifyServer is the factory's verification endpoint behind the same token endpoint: it records
// each body and path and answers from script, one status per call.
type verifyServer struct {
	script []int
	bodies []string
	paths  []string
	auth   []string
}

func (v *verifyServer) client(t *testing.T, clock *time.Time) *Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v2/token" {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		v.bodies, v.paths, v.auth = append(v.bodies, string(b)), append(v.paths, r.URL.Path), append(v.auth, r.Header.Get("Authorization"))
		code := v.script[0]
		v.script = v.script[1:]
		w.WriteHeader(code)
		switch code {
		case 201:
			_, _ = w.Write([]byte(`{"task":"fixt0001","verdict":"resolved","recordedAt":"2026-10-27T10:00:00Z"}`))
		case 200:
			_, _ = w.Write([]byte(`{"task":"fixt0001","verdict":"resolved","duplicate":true}`))
		case 409:
			_, _ = w.Write([]byte(`{"error":"window_closed"}`))
		default:
			_, _ = w.Write([]byte(`{"error":"tasks_unavailable"}`))
		}
	}))
	t.Cleanup(srv.Close)
	env := map[string]string{"ID": "runlore", "SECRET": "s3cret", "PROJ": "proj1"}
	return NewClient(config.Factory{URL: srv.URL, TokenURL: srv.URL + "/oauth/v2/token", ClientIDEnv: "ID",
		ClientSecretEnv: "SECRET", ProjectIDEnv: "PROJ", Repository: "Smana/cloud-native-ref", MaxCostUSD: 2}, // pragma: allowlist secret
		func(k string) string { return env[k] }, srv.Client(), func() time.Time { return *clock })
}

func TestPostVerification(t *testing.T) {
	clock := time.Unix(1_800_000_000, 0)
	vv := Verification{Verdict: "resolved", Alert: VerifiedAlert{Name: "KubePodCrashLooping", FiringAtMerge: true},
		CheckedAt: clock.UTC(), Evidence: "pods stable for 30m"}
	want := `{"verdict":"resolved","alert":{"name":"KubePodCrashLooping","firingAtMerge":true,"firingNow":false},"checkedAt":"` +
		clock.UTC().Format(time.RFC3339) + `","evidence":"pods stable for 30m"}`
	for name, tc := range map[string]struct {
		script []int
		status int
		reason string
		calls  int
	}{
		"created":          {[]int{201}, 201, "", 1},
		"duplicate":        {[]int{200}, 200, "", 1},
		"window closed":    {[]int{409}, 409, "window_closed", 1},
		"503 then created": {[]int{503, 201}, 201, "", 2},
	} {
		t.Run(name, func(t *testing.T) {
			vs := &verifyServer{script: tc.script}
			c := vs.client(t, &clock)
			status, reason, err := c.PostVerification(context.Background(), "fixt0001", vv)
			if err != nil || status != tc.status || reason != tc.reason {
				t.Fatalf("got %d %q %v", status, reason, err)
			}
			if len(vs.bodies) != tc.calls {
				t.Fatalf("calls %d, want %d", len(vs.bodies), tc.calls)
			}
			for i := range vs.bodies {
				if vs.bodies[i] != want+"\n" && vs.bodies[i] != want {
					t.Fatalf("body %s\nwant %s", vs.bodies[i], want)
				}
				if vs.paths[i] != "/v1/tasks/fixt0001/verification" || vs.auth[i] != "Bearer tok" {
					t.Fatalf("path %q auth %q", vs.paths[i], vs.auth[i])
				}
			}
		})
	}
	// A 503 that never clears is an error, so the entry stays due.
	vs := &verifyServer{script: []int{503, 503, 503}}
	if _, _, err := vs.client(t, &clock).PostVerification(context.Background(), "fixt0001", vv); err == nil {
		t.Fatal("three 503s are an error")
	}
}
