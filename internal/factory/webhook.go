// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

const (
	// maxWebhookBody caps the request body: events are id-only.
	maxWebhookBody = 64 << 10
	// timestampTolerance is how far a webhook-timestamp may be from the receiver's clock, either way.
	timestampTolerance = 5 * time.Minute
	// minKeyBytes is the shortest signing key accepted, decoded.
	minKeyBytes = 32
	// maxIDLen bounds a webhook-id before it is stored.
	maxIDLen = 256
	// maxSignatures bounds how many signatures of one header are checked.
	maxSignatures = 8
)

// Why a request was refused. Only the log distinguishes them: the caller always gets a bare 401.
var (
	errNoKeys       = errors.New("no signing key configured")
	errMissingHdr   = errors.New("missing webhook header")
	errBadTimestamp = errors.New("timestamp unparseable")
	errStale        = errors.New("timestamp outside the tolerance")
	errBadSignature = errors.New("no signature matches")
)

// Event is the broker's webhook payload (agent-platform's webhook.Event): ids and short codes only.
type Event struct {
	Type   string `json:"type"`
	Room   string `json:"room"`
	Repo   string `json:"repo,omitempty"`
	Run    string `json:"run,omitempty"`
	Reason string `json:"reason,omitempty"`
	Limit  string `json:"limit,omitempty"`
	State  string `json:"state,omitempty"`
	Origin string `json:"origin,omitempty"`
	PR     int    `json:"pr,omitempty"`
}

// ReadKeys parses the signing keys: one or two non-empty lines of standard base64, each at least
// 32 bytes decoded (two so a rotation can overlap). The error never echoes a key.
func ReadKeys(raw string) ([][]byte, error) {
	var keys [][]byte
	for _, ln := range strings.Split(raw, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		k, err := base64.StdEncoding.DecodeString(ln)
		if err != nil {
			return nil, errors.New("factory webhook key: not valid base64")
		}
		if len(k) < minKeyBytes {
			return nil, fmt.Errorf("factory webhook key: shorter than %d bytes", minKeyBytes)
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 || len(keys) > 2 {
		return nil, fmt.Errorf("factory webhook key: want one or two keys, got %d", len(keys))
	}
	return keys, nil
}

// Verify checks a Standard Webhooks request: the timestamp within five minutes of now, and one of
// the header's v1 signatures equal, in constant time, to HMAC-SHA256(key, id.timestamp.body) for
// some key. It returns the webhook-id. It fails closed without a key.
func Verify(keys [][]byte, h http.Header, body []byte, now time.Time) (string, error) {
	if len(keys) == 0 {
		return "", errNoKeys
	}
	id, ts, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || ts == "" || sigs == "" || len(id) > maxIDLen {
		return "", errMissingHdr
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errBadTimestamp
	}
	if d := now.Sub(time.Unix(sec, 0)); d > timestampTolerance || d < -timestampTolerance {
		return "", errStale
	}
	var got [][]byte
	for _, s := range strings.Fields(sigs) {
		if len(got) == maxSignatures {
			break
		}
		v, ok := strings.CutPrefix(s, "v1,")
		if !ok {
			continue
		}
		if b, err := base64.StdEncoding.DecodeString(v); err == nil {
			got = append(got, b)
		}
	}
	// Every key against every signature, no early exit: the time taken does not say which matched.
	ok := false
	for _, key := range keys {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(id + "." + ts + "."))
		m.Write(body)
		want := m.Sum(nil)
		for _, g := range got {
			if hmac.Equal(g, want) {
				ok = true
			}
		}
	}
	if !ok {
		return "", errBadSignature
	}
	return id, nil
}

// Receiver is the http.Handler of POST /factory/webhook: it verifies the broker's signed events,
// drops replays, maps each to the incident it belongs to and posts a fixed line in its thread.
type Receiver struct {
	// Keys returns the signing keys at the time of the request (read from the environment each time).
	Keys   func() [][]byte
	Ledger *Ledger
	// Replier posts into the incident's Slack thread; nil posts nothing.
	Replier providers.ThreadNotifier
	Now     func() time.Time
	Log     *slog.Logger
	// OnMerged is called, in a goroutine, once when a fix room's merge is first recorded.
	OnMerged func(ctx context.Context, key string)
}

var _ http.Handler = (*Receiver)(nil)

func (rc *Receiver) now() time.Time {
	if rc.Now != nil {
		return rc.Now()
	}
	return time.Now()
}

func (rc *Receiver) log() *slog.Logger {
	if rc.Log != nil {
		return rc.Log
	}
	return slog.Default()
}

// ServeHTTP implements http.Handler. Order matters: the signature is verified over the raw bytes
// before anything is parsed or recorded, so an unauthenticated caller can neither write the
// ledger nor reach the JSON decoder.
func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var keys [][]byte
	if rc.Keys != nil {
		keys = rc.Keys()
	}
	now := rc.now()
	id, err := Verify(keys, r.Header, body, now)
	if err != nil {
		// The reason is for the operator's log; the caller learns nothing. Never the key, the signature or the body.
		rc.log().Warn("factory webhook refused", "reason", err.Error())
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if rc.Ledger != nil && rc.Ledger.HasSeen(id, now) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := rc.handle(r.Context(), ev, now); err != nil {
		// Not marked seen: the broker's retry of this id must be handled, not dropped.
		rc.log().Error("factory webhook: handling failed", "err", err, "type", ev.Type)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if rc.Ledger != nil {
		if err := rc.Ledger.MarkSeen(id, now); err != nil {
			// The event is handled; a replay would only repeat an idempotent update.
			rc.log().Error("factory webhook: recording the id failed", "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handle updates the ledger for ev and posts its thread line. An event for a room the ledger does
// not know is ignored; the caller still gets 204.
func (rc *Receiver) handle(ctx context.Context, ev Event, now time.Time) error {
	if rc.Ledger == nil || ev.Room == "" || ev.Type == "" {
		return nil
	}
	rec, ok := rc.Ledger.ByRoom(ev.Room)
	if !ok && ev.Origin != "" {
		rec, ok = rc.Ledger.ByRoom(ev.Origin)
	}
	if !ok {
		return nil
	}
	if ev.Room != rec.Room && rec.FixRoom != "" && rec.FixRoom != ev.Room {
		return nil // a second room naming this origin: the first fix stays the incident's fix
	}
	var newlyMerged bool
	err := rc.Ledger.Upsert(rec.TriggerKey, func(r *Record) {
		if ev.Room != r.Room && r.FixRoom == "" {
			r.FixRoom = ev.Room // the origin named a task we know: this room is its fix
		}
		r.Events = append(r.Events, ev.Type)
		if ev.PR > 0 {
			r.PR = ev.PR
		}
		if ev.Type == "room.merged" && ev.Room == r.FixRoom && r.MergedAt.IsZero() {
			r.MergedAt, newlyMerged = now, true
		}
		rec = *r
	})
	if err != nil {
		return fmt.Errorf("updating the ledger: %w", err)
	}
	if line := threadLine(ev, rec); line != "" && rec.SlackRoot != "" && rc.Replier != nil {
		if err := rc.Replier.ReplyInThread(ctx, rec.SlackRoot, rec.SlackChannel, line); err != nil {
			rc.log().Warn("factory webhook: the thread post failed", "err", err, "type", ev.Type)
		}
	}
	if newlyMerged && rc.OnMerged != nil {
		key := rec.TriggerKey
		go func() {
			c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			rc.OnMerged(c, key)
		}()
	}
	return nil
}

// safeCode admits only the short identifier-like values the broker sends. Room text must never
// reach a shared Slack channel, so anything else is replaced.
var safeCode = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

func code(s string) string {
	if safeCode.MatchString(s) {
		return s
	}
	return "unspecified"
}

// threadLine is the fixed English line for ev, built from ids and codes only; "" posts nothing.
func threadLine(ev Event, rec Record) string {
	fix := ev.Room == rec.FixRoom && ev.Room != rec.Room
	room := code(ev.Room)
	switch ev.Type {
	case "room.task_received":
		if fix {
			return fmt.Sprintf("The fix was published and started as task `%s`.", room)
		}
		return fmt.Sprintf("Agent factory task `%s` took this incident; its public stub is issue #%d.", room, rec.Issue)
	case "room.ended":
		if fix {
			return fmt.Sprintf("The fix's task ended: %s.", code(ev.State))
		}
		return fmt.Sprintf("Agent factory task `%s` ended: %s. A maintainer publishes its proposal with `factory/publish`.", room, code(ev.State))
	case "room.pr_opened":
		return fmt.Sprintf("The fix's pull request is #%d.", ev.PR)
	case "room.waiting_for_human":
		return fmt.Sprintf("The fix waits for a human: %s.", code(ev.Reason))
	case "room.merged":
		return fmt.Sprintf("The fix #%d is merged.", ev.PR)
	case "room.reverted":
		return fmt.Sprintf("The fix #%d was reverted.", ev.PR)
	}
	return ""
}
