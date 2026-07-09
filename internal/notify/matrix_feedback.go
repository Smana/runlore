package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
)

// triggerKeyContentField is the custom event-content field Deliver stamps on
// investigation messages and the reaction listener reads back — the join between
// a 👍/👎 reaction and the incident it rates. Namespaced per the Matrix
// convention for custom keys.
const triggerKeyContentField = "io.runlore.trigger_key"

// FeedbackSink records a human 👍/👎 rating on a delivered investigation
// (implemented by *outcome.Ledger — the same method the Slack path records
// through, so dedup, trust weighting and the recurrence-cooldown re-arm are
// shared by construction).
type FeedbackSink interface {
	Feedback(triggerKey, rating, user string, at time.Time) error
}

// MatrixFeedback is the opt-in reaction listener
// (notify.matrix.feedback_reactions): it long-polls the homeserver's /sync for
// m.reaction events in the configured room and records 👍/👎 on RunLore's own
// investigation messages into the outcome ledger.
//
// Unlike the Slack feedback path, NOTHING is exposed: /sync is an outbound
// HTTPS long-poll authenticated by the notifier's access token — no inbound
// endpoint, no signing secret, no NetworkPolicy change. The trade is a
// long-lived poll loop, which runs leader-only (started in startWork, cancelled
// with the leadership context) so an HA deployment records each reaction once.
//
// The first /sync response is a position handshake only: historical reactions
// are deliberately skipped, so feedback counts from startup onward — replaying
// a room's history through the ledger on every restart would re-stamp old
// votes with fresh timestamps.
type MatrixFeedback struct {
	homeserver string
	roomID     string
	token      string
	sink       FeedbackSink
	log        *slog.Logger
	http       *http.Client

	// self is the bot's own Matrix user id (from /whoami at Run start). It is the
	// attribution trust anchor: a vote only counts when the REACTED-TO event was
	// sent by self. Without this check any room member could post a message
	// carrying an io.runlore.trigger_key field of their choosing and vote on it —
	// attributing ratings to an arbitrary incident (trust poisoning / vote
	// misdirection). Slack has no equivalent hole (interaction payloads only ever
	// reference buttons the app itself posted); Matrix must enforce it explicitly.
	self string

	// keyByEvent caches target-event → trigger-key lookups so N reactions to the
	// same message cost one fetch. Bounded crudely (reset past cap): the working
	// set is "messages still being reacted to", a handful.
	keyByEvent map[string]string
}

// syncTimeout is the server-side long-poll hold; the HTTP client timeout must
// comfortably exceed it or every quiet poll would surface as a client error.
const syncTimeout = 30 * time.Second

// retryBackoff is the pause after a failed sync before re-polling.
const retryBackoff = 5 * time.Second

// keyByEventCap bounds the target-event cache (reset, not evicted — simplicity
// over LRU for a cache whose working set is a handful of recent messages).
const keyByEventCap = 256

// NewMatrixFeedback builds the reaction listener. homeserver/roomID/token are
// the notifier's own settings; sink is where ratings land (the outcome ledger).
func NewMatrixFeedback(homeserver, roomID, token string, sink FeedbackSink, log *slog.Logger) *MatrixFeedback {
	return &MatrixFeedback{
		homeserver: strings.TrimRight(homeserver, "/"),
		roomID:     roomID,
		token:      token,
		sink:       sink,
		log:        log,
		http:       httpx.SecureClient(syncTimeout + 15*time.Second),
		keyByEvent: map[string]string{},
	}
}

// Run long-polls /sync until ctx is cancelled (leadership loss / shutdown).
// Errors are logged and retried after a fixed backoff — a flaky homeserver
// must never crash the agent; at worst feedback pauses.
func (f *MatrixFeedback) Run(ctx context.Context) {
	// Resolve our own identity FIRST — it anchors target-event attribution (see
	// the self field). No identity, no listening: fail towards recording nothing.
	for f.self == "" {
		self, err := f.whoami(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.log.Warn("matrix feedback whoami failed; retrying", "err", err)
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return
			}
			continue
		}
		f.self = self
	}
	since := ""
	for ctx.Err() == nil {
		next, events, err := f.sync(ctx, since)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			f.log.Warn("matrix feedback sync failed; retrying", "err", err)
			select {
			case <-time.After(retryBackoff):
			case <-ctx.Done():
				return
			}
			continue
		}
		// First response = position handshake: adopt the batch token, skip the
		// (historical) events. Everything after is live.
		if since != "" {
			for _, e := range events {
				f.handleReaction(ctx, e)
			}
		}
		since = next
	}
}

// matrixEvent is the subset of a timeline event the listener needs.
type matrixEvent struct {
	Type    string `json:"type"`
	Sender  string `json:"sender"`
	Content struct {
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
			Key     string `json:"key"`
		} `json:"m.relates_to"`
	} `json:"content"`
}

// sync performs one filtered /sync long-poll and returns the next batch token
// plus the room's new timeline events. The inline filter narrows the response
// to m.reaction events in the configured room — no presence, state, or other
// rooms' traffic ever crosses the wire.
func (f *MatrixFeedback) sync(ctx context.Context, since string) (string, []matrixEvent, error) {
	filter := fmt.Sprintf(`{"presence":{"types":[]},"account_data":{"types":[]},"room":{"rooms":[%q],"state":{"types":[]},"ephemeral":{"types":[]},"account_data":{"types":[]},"timeline":{"types":["m.reaction"],"limit":50}}}`, f.roomID)
	q := url.Values{"filter": {filter}, "timeout": {fmt.Sprintf("%d", syncTimeout.Milliseconds())}}
	if since != "" {
		q.Set("since", since)
	}
	endpoint := f.homeserver + "/_matrix/client/v3/sync?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("matrix sync: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", nil, fmt.Errorf("matrix sync status %d", resp.StatusCode)
	}
	var res struct {
		NextBatch string `json:"next_batch"`
		Rooms     struct {
			Join map[string]struct {
				Timeline struct {
					Events []matrixEvent `json:"events"`
				} `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&res); err != nil {
		return "", nil, fmt.Errorf("matrix sync decode: %w", err)
	}
	if res.NextBatch == "" {
		return "", nil, fmt.Errorf("matrix sync: empty next_batch")
	}
	return res.NextBatch, res.Rooms.Join[f.roomID].Timeline.Events, nil
}

// handleReaction maps one m.reaction event to a ledger rating: 👍→up, 👎→down
// (every other emoji is ignored — reactions are a general mechanism and 🎉 on a
// resolved incident must not count as an endorsement of the diagnosis). The
// reaction names its target event; the trigger identity is read back from the
// target's content — a target without the field is not one of RunLore's
// investigation messages and is skipped.
func (f *MatrixFeedback) handleReaction(ctx context.Context, e matrixEvent) {
	if e.Type != "m.reaction" || e.Content.RelatesTo.RelType != "m.annotation" {
		return
	}
	rating := ""
	// Clients commonly append the emoji variation selector (U+FE0F); strip it so
	// "👍" and "👍️" are the same vote.
	switch strings.ReplaceAll(e.Content.RelatesTo.Key, "️", "") {
	case "👍":
		rating = "up"
	case "👎":
		rating = "down"
	default:
		return
	}
	key, err := f.triggerKeyFor(ctx, e.Content.RelatesTo.EventID)
	if err != nil {
		f.log.Warn("matrix feedback: target event lookup failed", "event_id", e.Content.RelatesTo.EventID, "err", err)
		return
	}
	if key == "" {
		return // not one of our investigation messages
	}
	if err := f.sink.Feedback(key, rating, e.Sender, time.Now()); err != nil {
		f.log.Warn("matrix feedback recording failed", "key", key, "err", err)
		return
	}
	f.log.Info("matrix feedback recorded", "key", key, "rating", rating, "user", e.Sender)
}

// whoami resolves the access token's own user id — the sender every legitimate
// investigation message carries.
func (f *MatrixFeedback) whoami(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.homeserver+"/_matrix/client/v3/account/whoami", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("matrix whoami status %d", resp.StatusCode)
	}
	var res struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&res); err != nil {
		return "", err
	}
	if res.UserID == "" {
		return "", fmt.Errorf("matrix whoami: empty user_id")
	}
	return res.UserID, nil
}

// triggerKeyFor fetches the reaction's target event and returns its embedded
// trigger identity ("" when absent, when the target was NOT sent by the bot
// itself — a room member could otherwise stamp the field onto their own message
// and misdirect votes — or for one of ours from before the field existed).
// Cached per event id.
func (f *MatrixFeedback) triggerKeyFor(ctx context.Context, eventID string) (string, error) {
	if eventID == "" {
		return "", nil
	}
	if key, ok := f.keyByEvent[eventID]; ok {
		return key, nil
	}
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/event/%s",
		f.homeserver, url.PathEscape(f.roomID), url.PathEscape(eventID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("matrix event fetch status %d", resp.StatusCode)
	}
	var ev struct {
		Sender  string         `json:"sender"`
		Content map[string]any `json:"content"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ev); err != nil {
		return "", err
	}
	key, _ := ev.Content[triggerKeyContentField].(string)
	if ev.Sender != f.self {
		key = "" // not our message: whatever the field claims, it attributes nothing
	}
	if len(f.keyByEvent) >= keyByEventCap {
		f.keyByEvent = map[string]string{} // crude bound; the live working set is tiny
	}
	f.keyByEvent[eventID] = key
	return key, nil
}
