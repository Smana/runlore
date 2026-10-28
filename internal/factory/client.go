// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/httpx"
)

// maxKeyLen is the longest Idempotency-Key the factory accepts.
const maxKeyLen = 255

// maxBody caps every response body read.
const maxBody = 64 << 10

// tokenSkew is how long before expiry a cached access token is refreshed.
const tokenSkew = 60 * time.Second

// retryDelays are the waits before the second and third attempt of a CreateTask.
var retryDelays = []time.Duration{time.Second, 3 * time.Second}

// retrySleep waits between attempts and returns early, with the context's error, when ctx ends;
// tests replace it with a no-op.
var retrySleep = defaultRetrySleep

func defaultRetrySleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Incident is the finding's identity, as the factory's task API names it.
type Incident struct {
	Alert      string  `json:"alert"`
	Resource   string  `json:"resource"`
	Severity   string  `json:"severity"`
	Cluster    string  `json:"cluster"`
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
}

// TaskRequest is the body of POST /v1/tasks.
type TaskRequest struct {
	Repository string   `json:"repository"`
	Text       string   `json:"text"`
	DataClass  string   `json:"dataClass"`
	MaxCostUSD float64  `json:"maxCostUSD"`
	Incident   Incident `json:"incident"`
}

// Created is the factory's answer to a task: 201 a new task, 200 a duplicate of one, 202 a
// refusal that is not an error (Reason), 429 a limit (Reason); any other status is returned as is.
type Created struct {
	Status    int
	Task      string
	Room      string
	Issue     int
	Duplicate bool
	Reason    string
}

// Client is the factory's API client: a ZITADEL client-credentials token, POST /v1/tasks.
type Client struct {
	cfg      config.Factory
	clientID string
	secret   string
	project  string
	hc       *http.Client
	now      func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewClient reads the three credentials through getenv, once. The client is off (Enabled false)
// unless the URL and all three are set.
func NewClient(cfg config.Factory, getenv func(string) string, hc *http.Client, now func() time.Time) *Client {
	return &Client{
		cfg:      cfg,
		clientID: getenv(cfg.ClientIDEnv),
		secret:   getenv(cfg.ClientSecretEnv),
		project:  getenv(cfg.ProjectIDEnv),
		hc:       hc,
		now:      now,
	}
}

// Enabled reports whether the URL and every credential are set.
func (c *Client) Enabled() bool {
	return c != nil && c.cfg.URL != "" && c.clientID != "" && c.secret != "" && c.project != ""
}

// IdempotencyKey is the TriggerKey, or its SHA-256 hex when it is past 255 bytes.
func IdempotencyKey(triggerKey string) string {
	if len(triggerKey) <= maxKeyLen {
		return triggerKey
	}
	sum := sha256.Sum256([]byte(triggerKey))
	return hex.EncodeToString(sum[:])
}

// accessToken returns the cached token, fetching a new one within 60 s of its expiry. retry is
// true for a failure worth another attempt: a transport error or a 5xx from the token endpoint.
func (c *Client) accessToken(ctx context.Context) (tok string, retry bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Add(tokenSkew).Before(c.expires) {
		return c.token, false, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.secret},
		"scope":         {"openid urn:zitadel:iam:org:project:id:" + c.project + ":aud"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", false, httpx.SanitizeURLError(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("factory token: %w", httpx.SanitizeURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", true, fmt.Errorf("factory token: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode >= 500, fmt.Errorf("factory token: status %d", resp.StatusCode)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", false, errors.New("factory token: malformed response")
	}
	c.token = tr.AccessToken
	c.expires = c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, false, nil
}

// CreateTask posts the task. A 503 or a transport error is retried twice more with the same key,
// after 1 s and 3 s; any other status is returned in Created, its error or reason in Reason.
func (c *Client) CreateTask(ctx context.Context, key string, req TaskRequest) (Created, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return Created{}, fmt.Errorf("factory: marshal task: %w", err)
	}
	var lastErr error
	refreshed := false
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			if err := retrySleep(ctx, retryDelays[attempt-1]); err != nil {
				return Created{}, fmt.Errorf("factory: %w (last: %w)", err, lastErr)
			}
		}
		if err := ctx.Err(); err != nil {
			return Created{}, err
		}
		got, retry, err := c.postTask(ctx, key, payload)
		if err == nil && got.Status == http.StatusUnauthorized && !refreshed {
			// The cached token was revoked or rotated before its expiry: drop it and go again
			// once, without spending one of the 503 attempts.
			refreshed = true
			c.dropToken()
			attempt--
			continue
		}
		if !retry {
			return got, err
		}
		lastErr = err
	}
	return Created{}, fmt.Errorf("factory: task not accepted after %d attempts: %w", len(retryDelays)+1, lastErr)
}

// dropToken forgets the cached access token so the next call fetches a fresh one.
func (c *Client) dropToken() {
	c.mu.Lock()
	c.token, c.expires = "", time.Time{}
	c.mu.Unlock()
}

// postTask makes one attempt; retry is true for a 503 or a transport error.
func (c *Client) postTask(ctx context.Context, key string, payload []byte) (Created, bool, error) {
	tok, retry, err := c.accessToken(ctx)
	if err != nil {
		return Created{}, retry, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.URL, "/")+"/v1/tasks", bytes.NewReader(payload))
	if err != nil {
		return Created{}, false, httpx.SanitizeURLError(err)
	}
	hreq.Header.Set("Authorization", "Bearer "+tok)
	hreq.Header.Set("Idempotency-Key", key)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(hreq)
	if err != nil {
		return Created{}, true, fmt.Errorf("factory: %w", httpx.SanitizeURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Created{}, true, fmt.Errorf("factory: read: %w", err)
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return Created{}, true, errors.New("factory: 503")
	}
	var a struct {
		Task      string `json:"task"`
		Room      string `json:"room"`
		Issue     int    `json:"issue"`
		Duplicate bool   `json:"duplicate"`
		Error     string `json:"error"`
		Reason    string `json:"reason"`
	}
	_ = json.Unmarshal(body, &a) // a body that is not JSON leaves the fields empty; the status stands
	out := Created{Status: resp.StatusCode, Reason: a.Reason}
	if out.Reason == "" {
		out.Reason = a.Error
	}
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		out.Task, out.Room, out.Issue, out.Duplicate = a.Task, a.Room, a.Issue, a.Duplicate
	}
	return out, false, nil
}
