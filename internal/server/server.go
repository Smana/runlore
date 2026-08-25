// SPDX-License-Identifier: Apache-2.0

// Package server exposes RunLore's HTTP endpoints (incident webhooks).
package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/source"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/thread"
)

const (
	// maxConcurrentMentions bounds detached Slack mention handlers running at once
	// (s.eventDispatcher). Each slot holds one forge round-trip — read the thread,
	// comment or open a PR — not a heavy computation, so a wide pool stays cheap.
	// What is NOT cheap is exceeding it: the ack has already gone out, so a mention
	// dropped here is unrecoverable (Slack will never retry it). 16 gives real
	// headroom over the previous 4 while still bounding an internet-facing endpoint
	// against unbounded goroutines.
	maxConcurrentMentions = 16
	// maxConcurrentBusyNotices bounds the best-effort "I'm too busy, try again"
	// replies sent when maxConcurrentMentions is saturated (s.busyDispatcher).
	// Deliberately separate and small: telling humans about an overload is itself
	// a Slack round-trip, and it must never become the next unbounded-goroutine
	// problem. A burst wide enough to exhaust even this budget just means those
	// humans see nothing beyond the (Error-level) log line — no worse than before
	// this fix, never worse than that.
	maxConcurrentBusyNotices = 4
	// mentionHandlerTimeout bounds how long a single detached Slack mention
	// handler (see handleSlackEvent) may run before its context is cancelled.
	//
	// Justified against the WORST case, not the common one. github.Client.OpenPR
	// — the path a standalone operator-note PR takes — makes ~6 sequential forge
	// calls (get the base ref, create the branch, PUT the entry file, PUT
	// index.md, PUT log.md, open the PR), each riding httpx.SecureClient's own
	// 30s HTTP timeout. A degraded or rate-limited forge can hold every one of
	// those calls to its full 30s, which is close to 3 minutes for one mention —
	// and, unbounded (the previous context.WithoutCancel behaviour, which strips
	// the deadline along with cancellation), that mention would hold one of
	// maxConcurrentMentions slots for as long as the forge stays degraded, not
	// for as long as the mention itself takes. 2 minutes leaves the common case
	// (a healthy forge; every call returns in well under a second) enormous
	// headroom while still guaranteeing the whole pool drains through a forge
	// outage in bounded time instead of being held hostage indefinitely.
	mentionHandlerTimeout = 2 * time.Minute
)

// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	ready            func() bool
	approvals        *action.Approvals  // nil unless action mode "approve" is configured
	pauser           Pauser             // nil unless action mode "auto" is configured (kill-switch)
	feedback         FeedbackRecorder   // nil unless notify.slack.feedback_buttons is on (with an enabled ledger)
	silence          SilenceRecorder    // nil unless notify.slack.silence_button is on (with an enabled ledger)
	token            string             // shared secret for the approval/control endpoints (required when actions enabled)
	slackSecret      string             // Slack signing secret; verifies interactive button clicks
	webhookToken     string             // optional bearer token required on POST /webhook/alertmanager
	approvers        map[string]bool    // Slack user IDs permitted to approve actions (empty = none)
	guard            *authGuard         // failed-auth backoff for the shared-token endpoints
	threads          ThreadHandler      // nil unless notify.slack.thread_capture is on
	seenEvents       *seenSet           // Slack event_id dedup for delivery retries
	telemetryMetrics *telemetry.Metrics // nil unless telemetry is configured; nil-safe throughout
	// eventDispatcher and busyDispatcher run detached mention handlers and the
	// "pool saturated" busy reply respectively, each under its own concurrency
	// bound and timeout (see internal/thread.Dispatcher). Separate pools:
	// telling a human the mention pool is busy is itself a Slack round-trip, and
	// it must never become the next unbounded-goroutine problem.
	eventDispatcher *thread.Dispatcher
	busyDispatcher  *thread.Dispatcher

	metrics http.Handler // optional; GET /metrics (OTel Prometheus exposition)
	log     *slog.Logger
	handler http.Handler
}

// Pauser is the rung-3 auto-execution kill-switch.
type Pauser interface {
	Pause()
	Resume()
	Paused() bool
}

// FeedbackRecorder persists a human 👍/👎 rating on a delivered investigation —
// the ground-truth signal the learning loop weighs recalled knowledge by
// (implemented by *outcome.Ledger).
type FeedbackRecorder interface {
	Feedback(triggerKey, rating, user string, at time.Time) error
}

// SilenceRecorder persists a human 🔕 silence on a delivered investigation — the
// verdict that suppresses re-investigating the same trigger for a window
// (implemented by *outcome.Ledger). Kept SEPARATE from FeedbackRecorder rather
// than widening it: a deployment may enable ratings without silencing, and the
// server's nil-means-off guards must be able to express that per capability.
type SilenceRecorder interface {
	Silence(triggerKey string, window time.Duration, user string, at time.Time) error
}

// ThreadHandler processes a human message addressed to RunLore inside a thread.
// Implemented by *thread.Mention. It returns nothing: the endpoint acks Slack
// within its 3s deadline and the handler runs detached, replying in the thread
// itself.
type ThreadHandler interface {
	// HandleMention's fallback carries a Context a transport decoded some other
	// way than the registry, for use only when the registry has no entry for
	// root. Slack has no such source, so this call site always passes nil.
	HandleMention(ctx context.Context, channel, root, author, text string, fallback *thread.Context)
	// Busy tells the human their message could not be accepted right now, so they
	// know to send it again rather than assume it was recorded. Called when the
	// detached handler pool is saturated; must be best-effort and safe to call
	// with no reply transport configured.
	Busy(ctx context.Context, channel, root string)
}

// nilableKinds are the reflect kinds for which Value.IsNil is defined. Value.IsNil
// PANICS on any other kind, so it must never be called without this check.
var nilableKinds = map[reflect.Kind]bool{
	reflect.Chan: true, reflect.Func: true, reflect.Interface: true, reflect.Map: true,
	reflect.Pointer: true, reflect.Slice: true, reflect.UnsafePointer: true,
}

// liveIface normalises a TYPED-NIL interface value to a true nil interface, so
// that "not wired" is representable in exactly one way.
//
// Every optional field of Actions is an INTERFACE, but the builders that fill
// them in production return CONCRETE POINTERS and return nil to mean "not
// configured" — app.BuildThreadMention alone does so for three separate
// misconfigurations: no forge configured, no bot-token delivery target resolved,
// no thread-capable notifier resolved. A nil concrete pointer stored in an
// interface is a NON-NIL interface value, so handleSlackEvent's `s.threads ==
// nil` test read "wired" in all three states. The endpoint then answered 401
// rather than 404 to an operator's pre-flight probe — reading as "live, you just
// signed it wrong" — and acked a real signed app_mention 200 before
// dereferencing the nil receiver, losing the human's note to a recovered panic
// in a detached goroutine.
//
// Normalising HERE rather than at the call site is deliberate, and it is applied
// to ALL THREE optional fields rather than only the one that was reported.
// `s.threads == nil`, `s.pauser == nil` and `s.feedback == nil` each decide
// whether an internet-facing endpoint exists or an action can be vetoed; a
// safety guard whose correctness depends on every present and future caller
// remembering to nil-check before filling an exported struct field is not a
// safety guard, and that argument is not specific to Threads. The call sites are
// still expected to assign guarded — see RunServe, and the
// TestActionsInterfaceFieldsAreNeverAssignedRawBuilderResults guard in
// internal/app that enforces it for the whole class — but the invariant no
// longer depends on them.
//
// Cost is one reflect call per field per process, in New.
func liveIface[T any](v T) T {
	rv := reflect.ValueOf(v)
	if nilableKinds[rv.Kind()] && rv.IsNil() {
		var zero T
		return zero
	}
	return v
}

// Actions bundles the optional rung-2/rung-3 wiring: the approval queue, the auto
// kill-switch, the shared control token, the Slack signing secret, and the opt-in
// feedback recorder.
type Actions struct {
	Approvals    *action.Approvals
	Pauser       Pauser
	Feedback     FeedbackRecorder // opt-in 👍/👎 recording (notify.slack.feedback_buttons)
	Silence      SilenceRecorder  // opt-in 🔕 silencing (notify.slack.silence_button)
	Token        string
	SlackSecret  string
	WebhookToken string             // optional bearer token required on POST /webhook/alertmanager
	ApproverIDs  []string           // Slack user IDs permitted to approve actions
	Threads      ThreadHandler      // opt-in thread capture (notify.slack.thread_capture)
	Metrics      *telemetry.Metrics // optional; nil-safe — powers the mentions-dropped-on-saturation counter
}

// New builds a Server. ready reports whether this replica should serve; nil =
// always ready. The caller composes what readiness means (e.g. a warm catalog —
// since #264 deliberately NOT leadership, so every warm replica is Ready and
// Helm --wait/kstatus succeeds with replicaCount>1). acts (optional) enables the
// rung-2 approval endpoints + the rung-3 kill-switch, gated by acts.Token
// (X-Approval-Token). /healthz is liveness; /readyz is readiness (gated by the
// caller-supplied ready func). built + pipe wire the registered event sources:
// webhook sources are mounted at their paths and feed the ingest pipeline.
// metricsHandler (optional) serves OTel Prometheus metrics on GET /metrics when
// non-nil. fwd (optional) is the leader-forwarding policy: every WORK-BEARING
// route (source webhooks, slack interactions, action control) goes through it
// so a follower proxies the request to the leader; nil serves everything
// locally.
func New(ready func() bool, acts Actions, built []source.Built, pipe *source.Pipeline, metricsHandler http.Handler, fwd *Forward, log *slog.Logger) *Server {
	approvers := make(map[string]bool, len(acts.ApproverIDs))
	for _, id := range acts.ApproverIDs {
		approvers[id] = true
	}
	s := &Server{
		ready:     ready,
		approvals: acts.Approvals, pauser: liveIface(acts.Pauser), feedback: liveIface(acts.Feedback),
		silence: acts.Silence,
		token:   acts.Token, slackSecret: acts.SlackSecret,
		webhookToken: acts.WebhookToken, approvers: approvers, metrics: metricsHandler, log: log,
		guard:            newAuthGuard(),
		threads:          liveIface(acts.Threads),
		seenEvents:       newSeenSet(1024),
		telemetryMetrics: acts.Metrics,
		eventDispatcher:  thread.NewDispatcher(maxConcurrentMentions, mentionHandlerTimeout, log),
		busyDispatcher:   thread.NewDispatcher(maxConcurrentBusyNotices, mentionHandlerTimeout, log),
	}
	mux := http.NewServeMux()
	// work marks a route as work-bearing: on a follower the request is proxied
	// to the leader (single hop, see Forward). Authentication happens on the
	// leader — headers and body are relayed byte-identical, so the bearer token
	// and HMAC signatures verify there exactly as they would here. The
	// local-only endpoints below (/healthz, /readyz, /metrics) are NEVER
	// wrapped: probes and scrapes are always about THIS replica.
	work := fwd.middleware // nil-receiver safe: identity when fwd == nil
	mux.Handle("POST /slack/interactions", work(http.HandlerFunc(s.handleSlackInteraction)))
	mux.Handle("POST /slack/events", work(http.HandlerFunc(s.handleSlackEvent)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.ready != nil && !s.ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	// The action-control endpoints are work-bearing too: the approval queue and
	// the auto kill-switch live in the LEADER process (registered by its
	// investigation loop), so a follower answering locally would list an empty
	// queue or pause a pauser nothing consults.
	mux.Handle("GET /actions", work(http.HandlerFunc(s.handleListActions)))
	mux.Handle("POST /actions/{id}/approve", work(http.HandlerFunc(s.handleApprove)))
	mux.Handle("POST /actions/{id}/reject", work(http.HandlerFunc(s.handleReject)))
	mux.Handle("POST /actions/pause", work(http.HandlerFunc(s.handlePause)))
	mux.Handle("POST /actions/resume", work(http.HandlerFunc(s.handleResume)))
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
	source.MountWebhooks(mux, built, s.webhookAuthorized, pipe, work)
	// Wrap the whole mux so a panic in any handler (current or future) returns a
	// structured 500 and a logged stack instead of net/http's silent connection drop.
	s.handler = recoverPanic(mux, log)
	return s
}

// recoverPanic wraps next so a panic in any handler is recovered: it logs the
// panic value, request method/path/remote-addr, and a stack trace at error level,
// then writes a 500 if nothing has been written yet. Without this, net/http's
// per-connection recover closes the connection with no response and no log.
func recoverPanic(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if log != nil {
					log.Error("recovered from handler panic",
						"panic", rec,
						"method", r.Method,
						"path", r.URL.Path,
						"remote_addr", r.RemoteAddr,
						"stack", string(debug.Stack()),
					)
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if s.pauser == nil {
		http.Error(w, "auto-execution not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.pauser.Pause()
	s.log.Warn("auto-execution paused via kill-switch")
	_, _ = fmt.Fprintln(w, "auto-execution paused")
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if s.pauser == nil {
		http.Error(w, "auto-execution not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.pauser.Resume()
	s.log.Info("auto-execution resumed")
	_, _ = fmt.Fprintln(w, "auto-execution resumed")
}

// authorized enforces the approval token (constant-time compare). It FAILS
// CLOSED: with no token configured the control endpoints are denied, never open.
// (main refuses to start with actions enabled and an empty token, so a running
// rung-2/3 server always has one.) Repeated failures from one host arm an
// exponential backoff (see authGuard) — checked BEFORE the compare.
func (s *Server) authorized(r *http.Request) bool {
	host := remoteHost(r.RemoteAddr)
	if s.guard.blocked(host) {
		s.log.Warn("auth backoff engaged; rejecting without compare", "host", host)
		return false
	}
	if s.token == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Approval-Token")), []byte(s.token)) == 1 {
		s.guard.success(host)
		return true
	}
	s.guard.fail(host)
	return false
}

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		http.Error(w, "actions not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.approvals.List())
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		http.Error(w, "actions not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	act, err := s.approvals.Approve(r.Context(), id, "approve:http")
	if err != nil {
		s.log.Warn("action approval failed", "id", id, "err", err)
		code := http.StatusInternalServerError
		if errors.Is(err, action.ErrNoPending) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	s.log.Info("action approved and executed", "id", id, "op", act.Op)
	_, _ = fmt.Fprintf(w, "executed: %s %s\n", act.Op, act.Description)
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		http.Error(w, "actions not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if err := s.approvals.Reject(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, _ = fmt.Fprintln(w, "rejected")
}

// slackInteraction is the subset of the Block Kit interaction payload we need.
type slackInteraction struct {
	ResponseURL string `json:"response_url"`
	User        struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// handleSlackInteraction processes Block Kit button clicks: it verifies the Slack
// request signature, then approves (executing) / rejects the referenced action
// (privileged — approver allowlist) or records a 👍/👎 feedback rating
// (unprivileged — an opinion, not a cluster mutation), updating the message via
// response_url.
func (s *Server) handleSlackInteraction(w http.ResponseWriter, r *http.Request) {
	// The endpoint drives Approve/Reject on queued actions, the opt-in 👍/👎
	// feedback, and the opt-in 🔕 silence; it stays 404 unless at least one is
	// wired, so a deployment that enabled none of them exposes no interactive
	// callback at all. The signing secret stays mandatory: signature verification
	// is never optional.
	if (s.approvals == nil && s.feedback == nil && s.silence == nil) || s.slackSecret == "" {
		http.Error(w, "slack interactions not enabled", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !s.verifySlack(r.Header, body) {
		s.log.Warn("rejected slack interaction: bad signature")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	var p slackInteraction
	if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil || len(p.Actions) == 0 {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	act := p.Actions[0]
	var msg string
	switch act.ActionID {
	case "runlore_approve":
		if s.approvals == nil {
			msg = "❌ approvals not enabled"
			break
		}
		// Authorize the *user*, not just the Slack workspace: the signature proves
		// origin, the allowlist proves the clicker may approve cluster mutations.
		if !s.approvers[p.User.ID] {
			msg = "❌ not authorized to approve (user not in approver allowlist)"
			s.log.Warn("slack approve denied: user not in allowlist", "user_id", p.User.ID, "user", p.User.Username)
			break
		}
		executed, aerr := s.approvals.Approve(r.Context(), act.Value, "approve:slack:"+p.User.ID)
		if aerr != nil {
			msg = "❌ approval failed: " + aerr.Error()
			s.log.Warn("slack approve failed", "id", act.Value, "user", p.User.Username, "err", aerr)
		} else {
			msg = fmt.Sprintf("✅ approved by @%s — executed: %s %s", p.User.Username, executed.Op, executed.Description)
			s.log.Info("slack approval executed", "id", act.Value, "user_id", p.User.ID, "user", p.User.Username, "op", executed.Op)
		}
	case "runlore_reject":
		if s.approvals == nil {
			msg = "❌ approvals not enabled"
			break
		}
		// Gate reject on the same approver allowlist as approve: a signature-valid but
		// unlisted user must not be able to cancel a pending remediation (denial-of-
		// remediation). Rejecting is the safe direction, but it's still a privileged
		// decision over a queued cluster action.
		if !s.approvers[p.User.ID] {
			msg = "❌ not authorized to reject (user not in approver allowlist)"
			s.log.Warn("slack reject denied: user not in allowlist", "user_id", p.User.ID, "user", p.User.Username)
			break
		}
		if rerr := s.approvals.Reject(act.Value); rerr != nil {
			msg = "⚠️ " + rerr.Error()
		} else {
			msg = fmt.Sprintf("🚫 rejected by @%s", p.User.Username)
		}
	case "runlore_feedback_up", "runlore_feedback_down":
		// Feedback is deliberately unprivileged (no approver allowlist): the
		// signature proves the workspace, and a rating is an opinion feeding the
		// learning loop, not a cluster mutation. Anti-gaming lives in the ledger —
		// one live vote per (TriggerKey, user), latest wins.
		if s.feedback == nil {
			msg = "⚠️ feedback recording not enabled (notify.slack.feedback_buttons is off)"
			break
		}
		rating := "up"
		if act.ActionID == "runlore_feedback_down" {
			rating = "down"
		}
		if ferr := s.feedback.Feedback(act.Value, rating, p.User.ID, time.Now()); ferr != nil {
			msg = "⚠️ recording feedback failed: " + ferr.Error()
			s.log.Warn("slack feedback failed", "key", act.Value, "err", ferr)
		} else {
			msg = fmt.Sprintf("🙏 feedback recorded (%s) — thanks @%s", rating, p.User.Username)
			s.log.Info("slack feedback recorded", "key", act.Value, "rating", rating, "user_id", p.User.ID, "user", p.User.Username)
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK) // ack the click; update the message best-effort
	// Approve/reject replace the interaction message with the outcome; a feedback
	// ack must NOT — replacing would wipe the investigation the rating is about.
	replace := act.ActionID == "runlore_approve" || act.ActionID == "runlore_reject"
	s.updateSlack(r.Context(), p.ResponseURL, msg, replace)
}

// slackEvent is the subset of Slack's Events API envelope this server reads.
type slackEvent struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	EventID   string `json:"event_id"`
	Event     struct {
		Type     string `json:"type"`
		User     string `json:"user"`
		BotID    string `json:"bot_id"`
		Text     string `json:"text"`
		Channel  string `json:"channel"`
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"event"`
}

// handleSlackEvent receives Events API deliveries for the opt-in thread capture:
// a human writing `@runlore …` inside an investigation thread.
//
// Only app_mention is subscribed — never message.channels. That is explicit
// consent: RunLore reads nothing in a channel it was not addressed in, and there
// is no firehose to filter.
//
// The endpoint acks BEFORE doing any work. Slack retries anything it does not
// see acked within 3 seconds, and a knowledge write is a forge round-trip; a
// synchronous handler would be retried mid-write and file the note repeatedly.
func (s *Server) handleSlackEvent(w http.ResponseWriter, r *http.Request) {
	if s.threads == nil || s.slackSecret == "" {
		http.Error(w, "slack events not enabled", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !s.verifySlack(r.Header, body) {
		s.log.Warn("rejected slack event: bad signature")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ev slackEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	// The subscription handshake: Slack posts a challenge to the Request URL and
	// expects it echoed. Signature-verified like everything else on this endpoint.
	if ev.Type == "url_verification" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(ev.Challenge))
		return
	}

	// Ack first, unconditionally: every path below this line is a reason to IGNORE
	// the event, and an ignored event still has to be acked or Slack keeps retrying it.
	w.WriteHeader(http.StatusOK)

	if ev.Type != "event_callback" || ev.Event.Type != "app_mention" {
		return
	}
	// Loop guard: never act on our own messages, or any other app's.
	if ev.Event.BotID != "" || ev.Event.User == "" {
		return
	}
	// Only replies inside a thread carry a root to attribute knowledge to. A
	// top-level mention has ts but no thread_ts.
	if ev.Event.ThreadTS == "" {
		return
	}
	if ev.EventID != "" && !s.seenEvents.add(ev.EventID) {
		s.log.Debug("slack event: duplicate delivery ignored", "event_id", ev.EventID)
		return
	}

	// Detached from the request: the response is already written. s.eventDispatcher
	// strips cancellation and applies its own timeout so the work outlives this
	// handler's return instead of dying with it (see thread.Dispatcher.Go).
	if s.eventDispatcher.Go(r.Context(), func(ctx context.Context) {
		// Slack carries no context a fallback could be decoded from — nil here
		// always means "consult the registry only", never "skip the registry".
		s.threads.HandleMention(ctx, ev.Event.Channel, ev.Event.ThreadTS, ev.Event.User, ev.Event.Text, nil)
	}) {
		return
	}

	// The pool is saturated. The ack already went out (top of this handler), so
	// Slack will never retry this delivery — without the two lines below, the
	// human's note would be gone with nothing to show for it but a log line. Error,
	// not Warn: this loss is permanent and unrecoverable, for a feature whose entire
	// premise is not losing the operator's knowledge.
	s.log.Error("slack event: mention dropped, handler pool saturated", "channel", ev.Event.Channel, "root", ev.Event.ThreadTS)
	if s.telemetryMetrics != nil {
		s.telemetryMetrics.MentionsDroppedOnSaturation.Add(r.Context(), 1)
	}
	// Telling the human is itself another Slack round-trip, so it gets the same
	// treatment as the mention handler itself: detached and bounded, under its own
	// small budget, never blocking this request goroutine.
	s.busyDispatcher.Go(r.Context(), func(ctx context.Context) {
		s.threads.Busy(ctx, ev.Event.Channel, ev.Event.ThreadTS)
	})
}

// Drain waits for every currently in-flight detached handler (mention
// processing, and the "busy" reply sent when the pool is saturated) to finish,
// bounded by ctx. Call it during shutdown AFTER the HTTP listener has stopped
// accepting new requests (so neither dispatcher can be handed new work) and
// before tearing down anything a handler might still depend on.
//
// The two dispatchers are drained CONCURRENTLY, each against its own goroutine
// racing the SAME ctx, so each gets independent access to the full ctx budget
// from the moment Drain is called. Draining them one after another would let
// an expired deadline silently starve the second pool of its grace period: a
// context's Done() channel stays closed forever once its deadline passes, so
// if the first dispatcher's wait ran out the clock (mention work outlasting
// the grace period — precisely the degraded-forge scenario this drain exists
// for), the second would be handed an already-expired context and return
// almost instantly without waiting for real in-flight work, while still
// logging a timeout for work it never actually awaited.
//
// Mirrors investigate.Queue.Drain's shape: if ctx's deadline fires first,
// Drain returns anyway rather than blocking shutdown forever. A handler still
// running at that point is not killed — its own dispatcher-timeout-bounded
// context (see handleSlackEvent) is what eventually stops it — Drain simply
// stops waiting on it.
func (s *Server) Drain(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.eventDispatcher.Drain(ctx) }()
	go func() { defer wg.Done(); s.busyDispatcher.Drain(ctx) }()
	wg.Wait()
}

// seenSet is a bounded set of recently-seen ids. It exists so Slack's delivery
// retries are ignored rather than filing the same note twice. Reset past the cap
// rather than evicted individually: the live working set is the last few
// minutes of events, so the crude bound is exact enough and has no ordering cost.
type seenSet struct {
	mu   sync.Mutex
	cap  int
	seen map[string]struct{}
}

func newSeenSet(capacity int) *seenSet {
	return &seenSet{cap: capacity, seen: make(map[string]struct{}, capacity)}
}

// add records id and reports whether it was NEW (true = act on it).
func (s *seenSet) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.seen[id]; dup {
		return false
	}
	if len(s.seen) >= s.cap {
		s.seen = make(map[string]struct{}, s.cap)
	}
	s.seen[id] = struct{}{}
	return true
}

// verifySlack validates the Slack request signature (HMAC-SHA256 over
// "v0:{ts}:{body}") and rejects stale (replayable) requests.
func (s *Server) verifySlack(h http.Header, body []byte) bool {
	tsStr := h.Get("X-Slack-Request-Timestamp")
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	if d := time.Since(time.Unix(ts, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.slackSecret))
	_, _ = mac.Write([]byte("v0:" + tsStr + ":" + string(body)))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(h.Get("X-Slack-Signature")))
}

// updateSlack answers the interaction via its response_url: approve/reject
// overwrite the interaction message with the outcome (replaceOriginal=true),
// feedback appends an ephemeral note instead. The URL is attacker-influenceable
// (it arrives in the interaction payload), so it is restricted to https
// *.slack.com and posted with a bounded client — no SSRF to arbitrary internal
// services, no unbounded hang on http.DefaultClient.
func (s *Server) updateSlack(ctx context.Context, responseURL, text string, replaceOriginal bool) {
	if responseURL == "" {
		return
	}
	if u, err := url.Parse(responseURL); err != nil || u.Scheme != "https" ||
		(u.Hostname() != "slack.com" && !strings.HasSuffix(u.Hostname(), ".slack.com")) {
		s.log.Warn("refusing slack response_url: not an https *.slack.com host", "url", responseURL)
		return
	}
	body := slackResponseBody(text, replaceOriginal)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := httpx.SecureClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		s.log.Warn("slack response_url: request failed (best-effort)", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		s.log.Warn("slack response_url: non-2xx status (best-effort)", "status", resp.StatusCode)
	}
}

// slackResponseBody builds the response_url payload. A feedback ack
// (replaceOriginal=false) is additionally marked ephemeral so the thanks note is
// visible to the clicker only — the channel keeps the untouched investigation.
func slackResponseBody(text string, replaceOriginal bool) []byte {
	m := map[string]any{"replace_original": replaceOriginal, "text": text}
	if !replaceOriginal {
		m["response_type"] = "ephemeral"
	}
	b, _ := json.Marshal(m)
	return b
}

// Handler returns the HTTP handler (built once at construction; Go 1.22+ method routing).
func (s *Server) Handler() http.Handler {
	return s.handler
}

// webhookAuthorized checks the optional alert-webhook bearer token (constant-time).
// When no token is configured the webhook is open — Validate forbids that once
// actions.mode=auto, so an auto-executing server always authenticates it. With a
// token configured, repeated failures from one host arm the same backoff as the
// control endpoints.
func (s *Server) webhookAuthorized(r *http.Request) bool {
	if s.webhookToken == "" {
		return true
	}
	host := remoteHost(r.RemoteAddr)
	if s.guard.blocked(host) {
		s.log.Warn("webhook auth backoff engaged; rejecting without compare", "host", host)
		return false
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), []byte(s.webhookToken)) == 1 {
		s.guard.success(host)
		return true
	}
	s.guard.fail(host)
	return false
}
