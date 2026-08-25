// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// Delivery targets SlackDeliveryTarget can resolve to.
const (
	slackTargetBot     = "bot"     // chat.postMessage with a bot token
	slackTargetWebhook = "webhook" // incoming webhook
)

// SlackDeliveryTarget reports which delivery path the Slack notifier resolves to
// for sl against the current environment: "bot" (chat.postMessage), "webhook"
// (incoming webhook), or "" when neither is usable — nothing configured, or the
// env var holding the credential is present but EMPTY (an unmounted secret). ""
// means the builder below returns no notifier and Slack delivery is silently
// skipped, which is why this is exported: a startup guard must be able to detect
// that without re-deriving the precedence rules and drifting from them.
func SlackDeliveryTarget(sl config.SlackNotify) string {
	// Bot token (chat.postMessage) takes precedence over an incoming webhook: when
	// it is configured, an empty token does NOT fall back to the webhook.
	if sl.BotTokenEnv != "" && sl.Channel != "" {
		if os.Getenv(sl.BotTokenEnv) != "" {
			return slackTargetBot
		}
		return ""
	}
	if sl.WebhookURLEnv != "" && os.Getenv(sl.WebhookURLEnv) != "" {
		return slackTargetWebhook
	}
	return ""
}

// SlackBotDelivery reports whether Slack delivery resolves to the BOT-token path
// (chat.postMessage) rather than an incoming webhook. Thread capture requires
// exactly that path — a webhook returns no message ts, so there is no thread
// root — and asking here keeps the target vocabulary in one package.
func SlackBotDelivery(sl config.SlackNotify) bool {
	return SlackDeliveryTarget(sl) == slackTargetBot
}

// ThreadSink records the thread root a delivered investigation was posted to.
// Implemented by *thread.Registry; declared here as an interface so the notifier
// never imports the thread package and stays ignorant of how the mapping is
// stored. Nil-safe by contract at every call site: registration is best-effort
// and must never affect delivery.
type ThreadSink interface {
	// Register records inv against root, on channel, as delivered over
	// transport (the caller's own Transport() — "slack", "matrix", …), so the
	// stored entry is attributed to the transport that actually delivered it
	// rather than assumed.
	Register(transport, root, channel string, inv providers.Investigation)
}

func init() {
	Register(Descriptor{
		Name: "slack",
		Build: func(d Deps) (providers.Notifier, error) {
			sl := d.Cfg.Notify.Slack
			switch SlackDeliveryTarget(sl) {
			case slackTargetBot:
				b := NewSlackBot(os.Getenv(sl.BotTokenEnv), sl.Channel)
				b.FeedbackButtons = sl.FeedbackButtons
				if sl.SilenceButton {
					b.SilenceWindows = d.Cfg.Notify.Silence.Std()
				}
				if sl.ThreadCapture {
					b.Threads = d.Threads
				}
				return b, nil
			case slackTargetWebhook:
				s := NewSlack(os.Getenv(sl.WebhookURLEnv))
				s.FeedbackButtons = sl.FeedbackButtons
				if sl.SilenceButton {
					s.SilenceWindows = d.Cfg.Notify.Silence.Std()
				}
				return s, nil
			}
			return nil, nil
		},
	})
}

// Slack delivers via a Slack incoming webhook.
type Slack struct {
	webhookURL string
	http       *http.Client
	// FeedbackButtons (opt-in, notify.slack.feedback_buttons) appends 👍/👎 buttons
	// so the on-call can rate the diagnosis; clicks land in the outcome ledger via
	// the exposed /slack/interactions endpoint.
	FeedbackButtons bool
	// SilenceWindows are the presets offered by the 🔕 overflow
	// (notify.silence.windows); empty disables the control.
	SilenceWindows []time.Duration
}

// NewSlack builds a Slack webhook notifier.
func NewSlack(webhookURL string) *Slack {
	return &Slack{webhookURL: webhookURL, http: httpx.SecureClient(15 * time.Second)}
}

var (
	_ providers.Notifier         = (*Slack)(nil)
	_ providers.ProgressNotifier = (*Slack)(nil)
	_ providers.KBUpdateNotifier = (*Slack)(nil)
)

// Deliver posts the formatted investigation to the webhook. When an action carries
// an ApprovalID, it renders interactive Approve/Reject buttons (Block Kit).
func (s *Slack) Deliver(ctx context.Context, inv providers.Investigation) error {
	return s.post(ctx, slackMessageWith(inv, s.FeedbackButtons, s.SilenceWindows))
}

// DeliverProgress posts an interim progress ping to the webhook (ProgressNotifier).
func (s *Slack) DeliverProgress(ctx context.Context, up providers.ProgressUpdate) error {
	return s.post(ctx, slackProgressMessage(up))
}

// post marshals a Slack payload and POSTs it to the incoming webhook, surfacing
// transport and non-2xx errors. Shared by Deliver and DeliverProgress.
func (s *Slack) post(ctx context.Context, msg map[string]any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	// Both error paths are sanitized: an incoming-webhook URL IS the credential (the
	// secret is its path), net/http reports it verbatim inside a *url.Error — it masks
	// only a userinfo password — and this error is logged at Error level on the way
	// out. Otherwise a single DNS blip writes a live posting credential into the
	// operator's log store, which is one of the stores RunLore itself reads back.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return httpx.SanitizeURLError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack post: %w", httpx.SanitizeURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack status %d", resp.StatusCode)
	}
	return nil
}

// SlackBot delivers via the Slack Web API (chat.postMessage) using a bot token,
// for workspaces that provision a bot app instead of an incoming webhook. Unlike
// a webhook, chat.postMessage targets an explicit channel and returns HTTP 200
// with {"ok":false,"error":...} on logical failures (e.g. not_in_channel).
type SlackBot struct {
	token   string
	channel string
	baseURL string
	http    *http.Client
	// FeedbackButtons — see Slack.FeedbackButtons; on the bot path the buttons sit
	// on the channel summary message, never on the detail thread reply.
	FeedbackButtons bool
	// SilenceWindows are the presets offered by the 🔕 overflow
	// (notify.silence.windows); empty disables the control.
	SilenceWindows []time.Duration
	// Threads, when set (notify.slack.thread_capture), receives the summary
	// message's ts so a later reply in that thread can be attributed to this
	// investigation. Never set from the detail reply — the root is the handle.
	Threads ThreadSink
}

// NewSlackBot builds a bot-token Slack notifier posting to channel (ID or name).
func NewSlackBot(token, channel string) *SlackBot {
	return &SlackBot{token: token, channel: channel, baseURL: "https://slack.com", http: httpx.SecureClient(15 * time.Second)}
}

var (
	_ providers.Notifier         = (*SlackBot)(nil)
	_ providers.ProgressNotifier = (*SlackBot)(nil)
	_ providers.ThreadNotifier   = (*SlackBot)(nil)
	_ providers.KBUpdateNotifier = (*SlackBot)(nil)
)

// Deliver posts the compact summary to the channel, then the full analysis as a
// thread reply so the channel stays a scannable triage feed. The summary IS the
// notification; the detail reply is secondary, so a failed thread post returns a
// wrapped error that records the summary already landed — Multi logs it without
// implying the alert went undelivered. Nothing is threaded when the summary post
// yields no ts (empty-body path) or the investigation has no detail beyond it.
func (s *SlackBot) Deliver(ctx context.Context, inv providers.Investigation) error {
	summary := summaryBlocks(inv)
	summary = append(summary, feedbackBlocks(inv, s.FeedbackButtons, s.SilenceWindows)...)
	ts, err := s.post(ctx, map[string]any{"text": fallbackText(inv), "blocks": summary})
	if err != nil {
		return err
	}
	// Record the thread root so a reply here can be attributed. Best-effort and
	// nil-safe: capture is an opt-in extra, delivery is the contract.
	if s.Threads != nil && ts != "" {
		s.Threads.Register(s.Transport(), ts, s.channel, inv)
	}
	detail := detailBlocks(inv)
	if ts == "" || len(detail) == 0 {
		return nil
	}
	msg := map[string]any{"text": "Full analysis: " + escapeMrkdwn(truncate(inv.Title, 120)), "blocks": detail, "thread_ts": ts}
	if _, err := s.post(ctx, msg); err != nil {
		return fmt.Errorf("slack detail thread (summary delivered): %w", err)
	}
	return nil
}

// DeliverProgress posts an interim progress ping to the channel (ProgressNotifier).
func (s *SlackBot) DeliverProgress(ctx context.Context, up providers.ProgressUpdate) error {
	_, err := s.post(ctx, slackProgressMessage(up))
	return err
}

// ReplyInThread posts text as a reply in the thread rooted at root
// (providers.ThreadNotifier). It targets the channel it is given rather than the
// configured default: a reply can only go where the message it answers was sent.
//
// text is a composed reply, not a single untrusted string, so it goes through
// thread.RenderReply rather than through escapeMrkdwn directly: only the spans
// thread.Untrusted marked — model prose, a forge's own error text, a URL that
// arrived from somewhere — are escaped, and RunLore's own framing is posted
// verbatim. Escaping the whole message instead would close one hole and open
// another: mrkdwnEscaper replaces ">" with "&gt;", and the ">" that prefixes
// every line of model prose is what lets a human tell the model's words from
// RunLore's claims about what it did (see thread.modelVoice). The angle
// brackets in FreeformNotRecordedReply's backticked "`note: <text>`" example
// are RunLore's own too.
//
// The rendered result is then bounded at slackReplyBytes — AFTER escaping,
// because escaping is what can make it overflow. Without that bound a long
// enough model answer makes the post itself fail, and the human is told nothing
// about a note that was already written; see boundPostedReply.
func (s *SlackBot) ReplyInThread(ctx context.Context, root, channel, text string) error {
	msg := map[string]any{"text": boundPostedReply(thread.RenderReply(text, escapeMrkdwn), slackReplyBytes), "thread_ts": root}
	if channel != "" {
		msg["channel"] = channel
	}
	_, err := s.post(ctx, msg)
	return err
}

// Transport identifies this notifier's chat system (providers.ThreadNotifier),
// the key Multi.ThreadRepliers scopes thread replies by.
func (s *SlackBot) Transport() string { return "slack" }

// post targets the message at the configured channel and sends it via
// chat.postMessage, surfacing transport and Slack API (ok:false) errors, and
// returns the posted message's ts — the handle a threaded reply keys on ("" on
// the empty-body 2xx path). Shared by Deliver and DeliverProgress.
func (s *SlackBot) post(ctx context.Context, msg map[string]any) (string, error) {
	if _, ok := msg["channel"]; !ok {
		msg["channel"] = s.channel
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/api/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("slack post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("slack status %d", resp.StatusCode)
	}
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("slack read response: %w", err)
	}
	if len(bytes.TrimSpace(respBody)) == 0 {
		return "", nil // a 2xx with an empty body is a successful post, not a failure
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("slack decode response: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack chat.postMessage: %s", result.Error)
	}
	return result.TS, nil
}

// Slack interaction action_ids — must match the server's /slack/interactions handler.
const (
	approveActionID      = "runlore_approve"
	rejectActionID       = "runlore_reject"
	feedbackUpActionID   = "runlore_feedback_up"
	feedbackDownActionID = "runlore_feedback_down"
	silenceActionID      = "runlore_silence"
)

// silenceBlockIDPrefix namespaces the actions block whose block_id carries the
// TriggerKey for the silence overflow, and slackBlockIDMax is Slack's cap on that
// field.
//
// The key rides in block_id rather than in the overflow's option values because
// Slack caps an option value at 75 characters (a button value gets 2000), and a
// GitOps TriggerKey is `namespace/name:Reason` — routinely 60-70 characters, and
// unbounded in principle since Kubernetes names run to 253. An over-long option
// value makes Slack reject the ENTIRE message, so the failure would take out the
// notification, not just the control.
const (
	silenceBlockIDPrefix = "sil:"
	slackBlockIDMax      = 255
)

// slackMessage builds the Slack payload: a verdict-first Block Kit summary
// (header → verdict, alone → confidence, shown once → seen-before/recall
// context → matched-known-runbook → why, generous — it's the answer → suggested
// next steps → metadata fields, now BELOW the answer/action → footer:
// provenance only — verified, model calls, cost, the one view-entry link →
// approval buttons) followed by an optional detail section (full evidence + the
// complete open-questions / ruled-out / data-gap lists — ruled-out and
// data-gaps render only here, so a phone reader hits the answer before any
// "Show more", with ONE carve-out: an `inconclusive` card with no root cause has
// no answer to push below the fold, so block 5b promotes the blocker into the
// summary instead of leaving the card empty — see inconclusiveAccount). This is
// the single-message composition used
// by the webhook path; threading is a later concern. The "text" field is
// fallbackText — the one-line notification/accessibility summary.
//
// Escape invariant: the fallback is no longer escapeMrkdwn(Format(inv)) — it is
// fallbackText, which escapes its one untrusted field (the model title) itself.
// Every untrusted string interpolated into an mrkdwn block (the verdict/why
// sections, alert metadata + ChangeRef in the fields, evidence, ruled-out
// items, PrevCuratedURL inside the entry link) is passed through escapeMrkdwn
// at the point of use. Headers are plain_text (never escaped) and slackDate
// emits a raw <!date^…> token that is blocks-only — it must never enter the
// escaped fallback text.
func slackMessage(inv providers.Investigation) map[string]any {
	return slackMessageWith(inv, false, nil)
}

// slackMessageWith is slackMessage plus the opt-in 👍/👎/🔕 feedback block
// appended last (after the detail section) — the single-message webhook path's
// equivalent of the bot path's buttons-on-summary. withFeedback and
// silenceWindows are independent facts, both forwarded to feedbackBlocks
// unconditionally: it decides for itself which half (if either) to render, so
// this free function (unlike the Slack/SlackBot methods that call it, which
// read their own struct fields) just passes both through.
func slackMessageWith(inv providers.Investigation, withFeedback bool, silenceWindows []time.Duration) map[string]any {
	blocks := append(summaryBlocks(inv), detailBlocks(inv)...)
	blocks = append(blocks, feedbackBlocks(inv, withFeedback, silenceWindows)...)
	return map[string]any{
		"text":   fallbackText(inv),
		"blocks": blocks,
	}
}

// feedbackBlocks renders the human end of the learning loop: 👍/👎 when
// withFeedback is set, a 🔕 overflow offering each configured window when
// silenceWindows is non-empty — INDEPENDENTLY of each other.
//
// The three are one row and one verdict vocabulary — 👍 accurate, 👎 off-base,
// 🔕 accurate but known — but they are NOT one capability: a rating weighs a
// recalled entry's trust, while a silence suppresses re-investigation. They are
// enabled by separate config flags (notify.slack.feedback_buttons,
// notify.slack.silence_button), and a deployment may enable either without the
// other — config.Validate allows it, and the server nil-checks each recorder
// independently. Rendering a control whose capability is off would dead-end at
// handleSlackInteraction's "not enabled" ack, so each half is gated on its own
// flag rather than on "is either on".
//
// Attribution for 👍/👎 is the TriggerKey (incident identity — ratings survive
// re-worded re-investigations), falling back to the alert fingerprint; with
// neither there is nothing for the ledger to attribute, so nothing renders.
//
// The 🔕 silence overflow requires the TriggerKey specifically — NOT the
// fallback above. RecurrenceGate.decide reads l.silences[req.TriggerKey] and
// bails out to recurrenceOff outright when req.TriggerKey == "", so a silence
// recorded under a bare fingerprint (e.g. a budget/timeout/refusal result,
// which stamps Fingerprint but never TriggerKey) could never be read back:
// the click would be acked as success and then permanently ignored — the
// worst failure mode this feature can cause. A rating has no such read path
// (it is recorded for analytics regardless of key shape), so 👍/👎 keep the
// fingerprint fallback while 🔕 does not.
//
// The silence element's TriggerKey travels in the block's block_id, not in the
// option values — see silenceBlockIDPrefix for why. If the key is too long for
// even that, the silence element alone is dropped: a pathological resource name
// must degrade ONE control, never the card.
//
// Labels are plain_text (never escaped); values are opaque to Slack.
func feedbackBlocks(inv providers.Investigation, withFeedback bool, silenceWindows []time.Duration) []map[string]any {
	key := cmp.Or(inv.TriggerKey, inv.Fingerprint)
	if key == "" {
		return nil
	}
	var elements []map[string]any
	if withFeedback {
		elements = append(elements,
			map[string]any{"type": "button", "action_id": feedbackUpActionID, "value": key,
				"text": map[string]any{"type": "plain_text", "text": "👍 Accurate", "emoji": true}},
			map[string]any{"type": "button", "action_id": feedbackDownActionID, "value": key,
				"text": map[string]any{"type": "plain_text", "text": "👎 Off-base", "emoji": true}},
		)
	}
	blockID := silenceBlockIDPrefix + inv.TriggerKey
	overflowFits := inv.TriggerKey != "" && len(silenceWindows) > 0 && len(blockID) <= slackBlockIDMax
	if overflowFits {
		opts := make([]map[string]any, 0, len(silenceWindows))
		for _, w := range silenceWindows {
			opts = append(opts, map[string]any{
				// The LABEL is human text and reads the way the docs and values.yaml
				// write the preset (thread.ShortDuration); the VALUE is a machine token
				// the interactions handler parses back with time.ParseDuration, and
				// keeps the canonical spelling.
				"text":  map[string]any{"type": "plain_text", "text": "🔕 Silence " + thread.ShortDuration(w), "emoji": true},
				"value": w.String(),
			})
		}
		elements = append(elements, map[string]any{
			"type": "overflow", "action_id": silenceActionID, "options": opts,
		})
	}
	if len(elements) == 0 {
		return nil
	}
	block := map[string]any{"type": "actions", "elements": elements}
	if overflowFits {
		block["block_id"] = blockID
	}
	return []map[string]any{block}
}

// fallbackText renders the one-line notification/accessibility summary Slack
// shows in push notifications and block-less clients:
//
//	🔍 <title> — <verdict label> (<level> confidence · <pct>%)
//
// It parses as mrkdwn, so the one untrusted field it carries — the model title —
// is escaped; the verdict label is omitted when the model gave no verdict. It
// never embeds a slackDate token (raw <>), so it stays a single safe line.
func fallbackText(inv providers.Investigation) string {
	title := displayTitle(inv.Title)
	_, level, pct, stated := confidenceBadge(inv)
	s := "🔍 " + title
	if _, label := verdictBadge(inv.Verdict); label != "" {
		s += " — " + label
	}
	return escapeMrkdwn(fmt.Sprintf("%s (%s)", s, confidenceText(level, pct, stated)))
}

// displayTitle falls back to a generic label when the model/trigger gave no title.
func displayTitle(title string) string {
	if title == "" {
		return "Investigation"
	}
	return title
}

// slackDate renders t as a Slack date token that displays in the reader's local
// timezone, with the RFC3339 UTC form as the no-JS fallback. Slack-blocks-only:
// the token uses raw <>, so it must never enter the escaped fallback text.
func slackDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>", t.Unix(), t.UTC().Format(time.RFC3339))
}

// slackProgressMessage builds the Slack payload for an interim progress ping: a
// compact header + context line + the model's interim text. The fallback text is
// the escaped shared FormatProgress output (parsed as mrkdwn by block-less
// clients); the blocks escape each untrusted field the same way delivery does, so
// a hostile interim line like <https://evil|x> renders inert, never a live link.
//
// The fallback text is then bounded, which the blocks beside it already were (150
// runes for the header, 2900 for the section) and it was not. FormatProgress
// splices in ProgressUpdate.Interim — the model's raw completion text, capped by
// nothing between the loop and here — and escapeMrkdwn expands "&" fivefold on
// top of that, so past Slack's ~40,000-character limit the whole POST is
// rejected. An operator turns progress pings on precisely so a long
// investigation is not silent; unbounded, the longest investigations are the
// ones it silences. See boundPostedHead for why the HEAD is what survives:
// FormatProgress leads with the status line, and that IS the ping.
func slackProgressMessage(up providers.ProgressUpdate) map[string]any {
	return map[string]any{
		"text":   boundPostedHead(escapeMrkdwn(FormatProgress(up)), slackReplyBytes),
		"blocks": slackProgressBlocks(up),
	}
}

// slackProgressBlocks renders an interim progress update as Block Kit.
func slackProgressBlocks(up providers.ProgressUpdate) []map[string]any {
	title := displayTitle(up.Title)
	// The header is plain_text (Slack renders it literally, no mrkdwn parsing), so
	// the untrusted title needs no escaping here.
	status := fmt.Sprintf("⏳ *Investigating* · step %d/%d", up.Step, up.MaxSteps)
	if s := progressToolsSummary(up.ToolsUsed); s != "" {
		status += " · " + escapeMrkdwn(s)
	}
	blocks := []map[string]any{
		{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate("🔍 "+title, 150), "emoji": true}},
		{"type": "context", "elements": []map[string]any{{"type": "mrkdwn", "text": status}}},
	}
	if t := strings.TrimSpace(up.Interim); t != "" {
		blocks = append(blocks, map[string]any{"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": truncate(escapeMrkdwn(t), 2900)}})
	}
	return blocks
}

// mrkdwnEscaper implements Slack's documented mrkdwn escaping: exactly three
// characters act as control characters and must be replaced with HTML entities
// (& first). strings.Replacer substitutes in a single left-to-right pass, so
// the ampersands introduced by &lt;/&gt; are never re-escaped.
var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// escapeMrkdwn neutralises untrusted text (model output, evidence quoting
// cluster logs or alert annotations) before it is interpolated into Slack
// mrkdwn, so a hostile log line like <https://evil.example|innocent text>
// renders as literal text instead of a clickable phishing link. Mirrors the
// escape-first approach of the Matrix notifier's mrkdwnToHTML.
func escapeMrkdwn(s string) string { return mrkdwnEscaper.Replace(s) }

// summaryBlocks renders the triage summary as Block Kit, optimized for a
// woken-up on-call reading on a phone: everything actionable sits above the
// first "Show more" collapse. Top-down: what/where (header) → verdict, alone →
// the delivered confidence, shown exactly once → seen-before KB recall (when
// present) → matched-known-runbook (when a full loop reused a known entry) →
// why, rendered generously — it's the answer the reader came for → suggested
// next steps → trigger-time metadata (resource/started/what-changed/
// recurrence) — orienting detail, so it drops BELOW the answer and the
// action, not above it → footer: provenance only (verified, model calls,
// cost, the one "view entry" link) → approval buttons. Ruled-out and
// data-gaps are deliberately absent from the summary — both are already
// carried in full in detailBlocks' thread reply, so repeating them here would
// only cost fold space the answer needs. The full analysis (every hypothesis
// with all evidence, the complete open-questions / data-gaps / ruled-out
// lists) lives in detailBlocks; slackMessage appends the two for the
// single-message webhook path.
func summaryBlocks(inv providers.Investigation) []map[string]any {
	title := displayTitle(inv.Title)
	emoji, level, pct, stated := confidenceBadge(inv)

	// 1. Header (plain_text — Slack renders it literally, no mrkdwn parsing, so the
	// untrusted alert name / title needs no escaping). When the source named the
	// alert, anchor on it and append the tenant/cluster scope + affected resource.
	head := "🔍 "
	if inv.AlertName != "" {
		head += inv.AlertName
		// scopeIdentity here too, so the header collapses a tenant that only repeats
		// the cluster exactly as the Cluster field does. cmp.Or then prefers the
		// tenant, which is the narrower of the two names when both survive.
		cluster, tenant := scopeIdentity(inv.Cluster, inv.Tenant)
		scope := cmp.Or(tenant, cluster)
		loc := make([]string, 0, 2)
		if scope != "" {
			loc = append(loc, scope)
		}
		// resourceRef, not Resource.Ref() — see resourceRef. The header is the
		// most-read line, so it is the last place to print a namespace an object
		// was never in.
		if ref := resourceRef(inv.Resource); ref != "" {
			loc = append(loc, ref)
		}
		if len(loc) > 0 {
			head += " — " + strings.Join(loc, "/")
		}
	} else {
		head += title
	}
	blocks := []map[string]any{
		{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate(head, 150), "emoji": true}},
	}

	// 2. Verdict owns the second slot — the headline actionability call, plus the
	// investigation's own title WHEN THE HEADER DID NOT ALREADY SHOW IT.
	//
	// The header shows the title verbatim only when the source named no alert.
	// On the alert-driven path — Alertmanager, the primary trigger — it shows the
	// ALERT NAME and scope instead, which is the question, not the answer. Dropping
	// the title unconditionally therefore removed the agent's one-line conclusion
	// from exactly the incidents it matters most for: "KubePodCrashLooping —
	// prod/payments/api" is what woke the on-call; "api crash-looping after the
	// payments/api chart bump" is what RunLore worked out.
	//
	// So it is restated only when it would not be a duplicate. Untrusted model
	// output, hence escaped, and truncated to stay inside Slack's block limit.
	// Absent when the model omitted a verdict (old / recall investigations);
	// confidence (2b, below) renders either way, so the layout stays complete.
	if vEmoji, label := verdictBadge(inv.Verdict); label != "" {
		line := fmt.Sprintf("%s *%s*", vEmoji, label)
		if inv.AlertName != "" && inv.Title != "" && !strings.EqualFold(strings.TrimSpace(inv.Title), strings.TrimSpace(inv.AlertName)) {
			line += " — " + escapeMrkdwn(title)
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
			"text": truncate(line, 2900)}})
	} else if inv.AlertName != "" && inv.Title != "" && !strings.EqualFold(strings.TrimSpace(inv.Title), strings.TrimSpace(inv.AlertName)) {
		// No verdict to anchor it to, but the conclusion still has to appear.
		//
		// Both branches skip when Title is just the alert name again. inv.Title falls
		// back to req.Title (loop.go), and for Alertmanager and Grafana req.Title IS
		// labels.alertname — and every synthesised terminal result (non-convergence,
		// budget, timeout, refusal) sets Title: req.Title unconditionally. Without this
		// the header and the conclusion line print the same string twice, which is
		// #399's original finding, on precisely the inconclusive cards where restating
		// the alert as the answer misleads most.
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
			"text": truncate(escapeMrkdwn(title), 2900)}})
	}

	// 2b. Confidence — shown exactly ONCE, here in the header area, whether or
	// not a verdict rendered above. It never repeats in the footer (provenance
	// only, block 8) and the agent identity isn't restated here either — the
	// Slack app's own name/icon already says who posted this. A recalled
	// investigation's own top-hypothesis confidence can legitimately differ
	// from this DELIVERED number (see confidenceBadge); when it does,
	// recallConfidenceNote spells out both so the reader never has to
	// reconcile two bare, seemingly-contradictory percentages alone.
	// Bold covers the level only, never the percentage — byte-identical to what the
	// README's shipped card captures show. The unstated variant has no level to bold,
	// so it bolds the whole phrase.
	confText := fmt.Sprintf("%s *%s confidence* · %d%%", emoji, level, pct)
	if !stated {
		confText = fmt.Sprintf("%s *%s*", emoji, confidenceText(level, pct, stated))
	}
	if note := recallConfidenceNote(inv, pct); note != "" {
		confText += "\n_" + note + "_"
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
		{"type": "mrkdwn", "text": confText}}})

	// 3. Prior knowledge — on a recurring incident with a merged KB entry, quote
	// what the KB already says (cause + human-reviewed resolution + track record)
	// before the current analysis: history frames how the on-call reads what
	// follows, with zero clicks. The entry excerpts are untrusted (model prose,
	// human edits) and escaped.
	if p := inv.Prior; p != nil {
		var s strings.Builder
		// Two shapes share this block. A RECALL short-circuited the loop: without an
		// explicit marker the message reads as a low-confidence fresh investigation, so
		// lead with "⚡ Instant recall" and label the quoted sections as the KNOWN answer.
		// A recurring FRESH investigation instead leads with the "Seen before ×N" counter.
		causeLabel, resLabel := "Prior cause", "Prior resolution"
		if inv.Recalled {
			s.WriteString("⚡ *Instant recall* — answered from your knowledge base, no investigation was run")
			causeLabel, resLabel = "Known cause", "Validated resolution"
		} else {
			fmt.Fprintf(&s, "📚 *Seen before ×%d* — last %s", inv.Occurrences, slackDate(inv.LastOccurrence))
		}
		if p.Cause != "" {
			fmt.Fprintf(&s, "\n*%s:* %s", causeLabel, escapeMrkdwn(p.Cause))
		}
		if p.Resolution != "" {
			fmt.Fprintf(&s, "\n*%s:* %s", resLabel, escapeMrkdwn(p.Resolution))
		}
		// The entry itself (filename or link) is deliberately NOT inlined here —
		// it is the single most-clickable thing on the card, and a section this
		// long is exactly what Slack's client collapses/truncates mid-word
		// behind "Show more". It lives once, in the footer (entryLink), on its
		// own short line that is never truncated.
		if p.Recalls > 0 {
			fmt.Fprintf(&s, "\nresolve rate %d/%d", p.Resolved, p.Recalls)
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}

	// 4. Existing-KB match — a full investigation whose kb_search matched a known
	// runbook/entry at clear-match strength. Make it VISIBLE that RunLore already had
	// knowledge for this incident: the live gap was a 95%-confidence kb_search hit that
	// the on-call could not see. Suppressed when Prior is set — the recurrence block
	// above already says "seen before" with richer context, so don't double-render.
	// Title is untrusted (entry frontmatter) and escaped; path/URL are not inlined
	// here — same reasoning as the Prior block above — entryLink surfaces the
	// reference once, in the footer.
	if mk := inv.MatchedKnowledge; mk != nil && inv.Prior == nil {
		text := fmt.Sprintf("📚 *Matches known runbook:* %s — RunLore has prior knowledge for this incident.",
			escapeMrkdwn(mk.Title))
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(text, 2900)}})
	}

	// 5. Top root cause: the single most-likely why — the answer the reader came
	// for, so it gets the full 2900-char block budget like every other section
	// (nothing here is capped tighter), with up to three evidence bullets.
	// Deeper hypotheses and full evidence move to the detail section.
	if len(inv.RootCauses) > 0 {
		rc := inv.RootCauses[0]
		var s strings.Builder
		fmt.Fprintf(&s, "*Why:* %s", escapeMrkdwn(rc.Summary))
		appendCappedBullets(&s, rc.Evidence)
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
		if n := len(inv.RootCauses) - 1; n > 0 {
			word := "hypotheses"
			if n == 1 {
				word = "hypothesis"
			}
			blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
				{"type": "mrkdwn", "text": fmt.Sprintf("_…%d more %s below_", n, word)}}})
		}
	}

	// 5b. What blocked the investigation — the account an `inconclusive` verdict owes
	// the reader when block 5 rendered nothing to be the answer instead. It stands in
	// the same slot as the Why, because that is what it is for this card, and it
	// renders for no other verdict (see inconclusiveAccount).
	//
	// Without it an inconclusive-with-no-cause card was a verdict badge, a confidence
	// line and metadata — which is what shipped live on 2026-08-18. The blocker text
	// existed for every stop RunLore synthesises itself (budget kill, timeout,
	// refusal, non-convergence all write one into Unresolved/DataGaps) and reached
	// only the threaded detail reply, where the channel reader never sees it; when the
	// model supplied no blocker at all, nothing anywhere said so.
	//
	// Untrusted model output, escaped, and capped at summaryBullets like the evidence
	// list above it — a run stopped by a ceiling names one blocker, and a card that
	// spends its fold space enumerating gaps is the failure mode the summary/detail
	// split exists to prevent.
	//
	// This PROMOTES the blocker rather than moving it: detailBlocks still renders the
	// same list in full below. On the bot path those are two messages (channel card,
	// thread reply) and the reader sees each once; on the single-message webhook path
	// they are adjacent, which is the pre-existing shape of this pair — block 5 and
	// detailBlocks already double-render the top root cause and its evidence the same
	// way, because the detail section has to stand alone when it is posted alone.
	if lead, bullets := inconclusiveAccount(inv); lead != "" {
		var s strings.Builder
		s.WriteString(lead)
		appendCappedBullets(&s, bullets)
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}

	// 6. Suggested next steps — the resolution guide (per-root-cause suggestions +
	// policy actions, de-duplicated, reversibility-flagged), capped at three.
	//
	// Rendered even with nothing to show, saying so — see
	// providers.Investigation.ActionWithoutRemedy for the payload that motivated it,
	// and notify.remedyMissingForReader for why the gate is not that predicate
	// directly. The two branches are mutually exclusive: the predicate requires that
	// no root cause and no action carried a remedy, and nextSteps builds its list
	// from exactly those fields, trimmed the same way.
	steps := nextSteps(inv)
	if remedyMissingForReader(inv) {
		blocks = append(blocks,
			map[string]any{"type": "divider"},
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
				"text": noRemedyNotice}})
	}
	if len(steps) > 0 {
		var s strings.Builder
		s.WriteString("*🛠 Suggested next steps*  _(read-only — RunLore won't apply these)_")
		// Own loop, not appendCappedBullets: nextSteps has already escaped these
		// (it de-duplicates on the raw string first) and mrkdwnEscaper is not
		// idempotent. Only the cap is shared.
		for i, st := range steps {
			if i >= summaryBullets {
				fmt.Fprintf(&s, "\n• _…%d more_", len(steps)-i)
				break
			}
			fmt.Fprintf(&s, "\n• %s", st)
		}
		blocks = append(blocks,
			map[string]any{"type": "divider"},
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}

	// 7. Metadata fields — trigger-time facts (resource, started, what-changed,
	// recurrence counter). Orienting detail, not the thing to lead with, so it
	// sits BELOW the answer (5) and the action (6) rather than above them.
	if fields := metadataFields(inv); len(fields) > 0 {
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	}

	// 8. Footer — provenance only: verified, the single link to view the entry
	// (this investigation's own curated entry, or — on a recall/seen-before card
	// with nothing freshly curated — the prior/recalled/matched entry; see
	// entryLink), why that link is missing when a write failed, and model
	// calls/cost. Confidence and the agent identity are NOT repeated here:
	// confidence already owns the header (2b), and the Slack app's own identity
	// already says who posted this — showing either twice reads as the card
	// disagreeing with itself, not reinforcing it.
	var foot []string
	if inv.Verified {
		foot = append(foot, "✓ verified")
	}
	if link := entryLink(inv); link != "" {
		foot = append(foot, link)
	}
	// Beside the entry link, because it is the same fact's other arm: this is what
	// there is to say when the write that would have produced that link failed. An
	// empty CuratedURL is ambiguous — it is also the normal state for a finding
	// below curate.min_confidence or carrying a skip_verdicts verdict — so without
	// this the card renders a broken learning loop exactly like a deliberately
	// skipped one (#506, observed live: a 403 ran unnoticed until an operator
	// happened to write a thread note, the one path that already reported it). On a
	// recurring incident entryLink still resolves the PRIOR entry, so the two
	// genuinely coexist, and the warning is what stops that stale link from reading
	// as this run's.
	//
	// escapeMrkdwn is MANDATORY here, not defensive: the reason is forge-supplied
	// text, so without it this would be the only unescaped untrusted span on the
	// card and a body echoing <https://evil.example|click here> would render as a
	// live phishing link in the incident channel. The inline code span is
	// curateFailureReason's contract — backticks are already stripped from the
	// reason, so it cannot close early.
	if reason := curateFailureReason(inv); reason != "" {
		foot = append(foot, "⚠️ could not save to the knowledge base: `"+escapeMrkdwn(reason)+"`")
	}
	if u := usageFooter(inv.Usage); u != "" {
		foot = append(foot, u) // trusted scaffolding — digits/labels only, no mrkdwn meta
	}
	if len(foot) > 0 {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
			{"type": "mrkdwn", "text": truncate(strings.Join(foot, "  ·  "), 2900)}}})
	}

	// 9. Interactive Approve/Reject for any action awaiting approval (rung-2).
	for _, a := range inv.Actions {
		if a.ApprovalID == "" {
			continue
		}
		blocks = append(blocks,
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*Proposed action:* " + escapeMrkdwn(a.Description)}},
			map[string]any{"type": "actions", "elements": []map[string]any{
				{"type": "button", "style": "primary", "action_id": approveActionID, "value": a.ApprovalID,
					"text": map[string]any{"type": "plain_text", "text": "Approve"}},
				{"type": "button", "style": "danger", "action_id": rejectActionID, "value": a.ApprovalID,
					"text": map[string]any{"type": "plain_text", "text": "Reject"}},
			}},
		)
	}
	return blocks
}

// metadataFields renders the trigger-time facts as a Block Kit fields array (two
// columns), only non-empty entries, capped at Slack's ten-field limit and each
// value truncated to the 2000-char per-field cap. Every value is untrusted (alert
// labels, model-supplied change refs) and escaped; the labels are trusted
// scaffolding. slackDate tokens (Started/Recurrence) are Slack-emitted, not
// escaped.
func metadataFields(inv providers.Investigation) []map[string]any {
	var fields []map[string]any
	add := func(label, val string) {
		if val == "" || len(fields) >= 10 {
			return
		}
		fields = append(fields, map[string]any{"type": "mrkdwn", "text": truncate(fmt.Sprintf("*%s:*\n%s", label, val), 2000)})
	}
	if inv.AlertName != "" {
		name := escapeMrkdwn(inv.AlertName)
		if inv.Severity != "" {
			name += " (" + escapeMrkdwn(inv.Severity) + ")"
		}
		add("Alert", name)
	}
	// One name, not the same name twice — see scopeIdentity, which the header and
	// Format's metadata line also go through.
	cluster, tenant := scopeIdentity(inv.Cluster, inv.Tenant)
	scope := make([]string, 0, 2)
	if tenant != "" {
		scope = append(scope, escapeMrkdwn(tenant))
	}
	if cluster != "" {
		scope = append(scope, escapeMrkdwn(cluster))
	}
	add("Cluster", strings.Join(scope, " · "))
	// resourceLine, not Resource.Ref() — see resourceRef.
	if line := resourceLine(inv.Resource); line != "" {
		add("Resource", escapeMrkdwn(line))
	}
	add("Started", slackDate(inv.StartedAt))
	// Only rendered when the investigation actually established a change. change_ref
	// is OPTIONAL in submit_findings, and WhatChangedTool is only registered when a
	// GitOps provider is configured — so in a deployment without Flux/Argo the field
	// is empty on every card, and on a recall card no investigation ran at all.
	//
	// The previous else-branch printed "No Git change identified — likely
	// infrastructure-induced" in exactly those cases. An absent optional field is not
	// evidence of absence, and "likely infrastructure-induced" is an inference the
	// investigation never made: the same speculation-from-silence that #420 forbade on
	// the prompt side. Operationally it points a woken-up on-call AWAY from the recent
	// deploy, which is the first thing they should check.
	if len(inv.RootCauses) > 0 {
		if ch := inv.RootCauses[0].ChangeRef; ch != "" {
			// Capped BEFORE escaping, which is the reverse of this file's other two
			// truncate sites and deliberate. Those cap at 2900 — a last-resort guard on
			// a pathological payload — where it does not matter that "&" costs 5 of the
			// budget. This cap fires routinely, so measuring escaped runes would hand a
			// change_ref describing "1.14.2 -> 1.15.0 && <prod>" a materially smaller
			// allowance than one without meta characters. Capping the source also means
			// truncateWords only ever sees real prose, so no cut can sever an entity.
			add("What changed", escapeMrkdwn(truncateWords(ch, slackWhatChangedRunes)))
		}
	}
	if inv.Occurrences > 1 && inv.Prior == nil {
		add("Recurrence", fmt.Sprintf("🔁 #%d · last %s", inv.Occurrences, slackDate(inv.LastOccurrence)))
	}
	return fields
}

// detailBlocks renders the full analysis the summary elides: every root cause with
// all its evidence, and the complete open-questions / data-gaps / ruled-out lists.
// Returns nil when there is nothing beyond the summary — all three honest-limit
// slices empty and at most one root cause with no more than three evidence bullets.
func detailBlocks(inv providers.Investigation) []map[string]any {
	topEvidence := 0
	if len(inv.RootCauses) > 0 {
		topEvidence = len(inv.RootCauses[0].Evidence)
	}
	if len(inv.Unresolved) == 0 && len(inv.DataGaps) == 0 && len(inv.RuledOut) == 0 &&
		len(inv.RootCauses) <= 1 && topEvidence <= 3 {
		return nil
	}

	blocks := []map[string]any{
		{"type": "divider"},
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*Full analysis*"}},
	}
	for i, rc := range inv.RootCauses {
		var s strings.Builder
		// An unstated per-cause confidence renders as nothing rather than a `0%`
		// chip — same reason as the headline; see confidenceText.
		if rc.Confidence > 0 {
			fmt.Fprintf(&s, "*%d. %s*  `%.0f%%`", i+1, escapeMrkdwn(rc.Summary), rc.Confidence*100)
		} else {
			fmt.Fprintf(&s, "*%d. %s*", i+1, escapeMrkdwn(rc.Summary))
		}
		if rc.ChangeRef != "" {
			fmt.Fprintf(&s, "\n📦 *What changed:* `%s`", escapeMrkdwn(rc.ChangeRef))
		}
		for _, e := range rc.Evidence {
			fmt.Fprintf(&s, "\n• %s", escapeMrkdwn(e))
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}
	blocks = appendListSection(blocks, "*❓ Open questions* _(needs a human)_", inv.Unresolved)
	blocks = appendListSection(blocks, "*⚠️ Data gaps:*", inv.DataGaps)
	blocks = appendListSection(blocks, "*❌ Ruled out:*", inv.RuledOut)
	return blocks
}

// summaryBullets is how many bullets any one SUMMARY section spends before it
// hands off to a "…N more" pointer. One named constant because the cap is a
// fold-space policy, not a per-section taste: three sections apply it, and the
// failure mode is them disagreeing so one list quietly pushes the next below
// "Show more". The detail section is deliberately uncapped (appendListSection).
const summaryBullets = 3

// appendCappedBullets writes items to s as escaped mrkdwn bullets, at most
// summaryBullets of them, replacing the tail with a "…N more" pointer. Shared by the
// summary's evidence and inconclusive-account lists so the glyph, the wording and
// the cap cannot drift between two sections of the same card.
//
// The cap is not a parameter on purpose: a per-caller cap is exactly the drift
// summaryBullets exists to prevent, and there is no section that wants a different
// one.
//
// It escapes each item, so callers must pass RAW model output: mrkdwnEscaper is not
// idempotent (it would turn an escaped &amp; into &amp;amp;). That is why nextSteps,
// which pre-escapes while de-duplicating, keeps its own loop.
func appendCappedBullets(s *strings.Builder, items []string) {
	for i, it := range items {
		if i >= summaryBullets {
			fmt.Fprintf(s, "\n• _…%d more_", len(items)-i)
			return
		}
		fmt.Fprintf(s, "\n• %s", escapeMrkdwn(it))
	}
}

// appendListSection appends one mrkdwn section listing every item as an escaped
// bullet under header, or returns blocks unchanged when items is empty. Used for
// the detail section's full (uncapped) honest-limit lists.
func appendListSection(blocks []map[string]any, header string, items []string) []map[string]any {
	if len(items) == 0 {
		return blocks
	}
	var s strings.Builder
	s.WriteString(header)
	for _, it := range items {
		fmt.Fprintf(&s, "\n• %s", escapeMrkdwn(it))
	}
	return append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
}

// confidenceBadge returns the DELIVERED headline confidence. For most
// investigations this is the max of the overall score and the top root
// cause's — models frequently leave the top-level field at 0 while ranking a
// high-confidence root cause, and showing "0%" next to an 80% root cause reads
// as broken. A RECALLED investigation is the one exception: inv.Confidence
// there is a deliberately computed, outcome-weighted number (the recall
// pipeline's own match confidence, decayed by the entry's track record) and
// must render AS-IS — maxing it against the top hypothesis's raw match
// confidence would silently hide a bad track record behind the larger number.
// See recallConfidenceNote for how the two are disclosed when they diverge.
// The fourth return, `stated`, separates "the model assessed this as very low" from
// "the model assessed nothing at all" — see confidenceText for why those two must not
// render alike.
func confidenceBadge(inv providers.Investigation) (emoji, level string, pct int, stated bool) {
	c := inv.Confidence
	if !inv.Recalled {
		for _, rc := range inv.RootCauses {
			c = max(c, rc.Confidence)
		}
	}
	// A recalled investigation's confidence is derived by the recall pipeline, never
	// left to a model, so it is always stated — even at 0.
	stated = inv.Recalled || c > 0
	pct = int(c*100 + 0.5)
	switch {
	case c >= 0.7:
		return "🟢", "High", pct, stated
	case c >= 0.4:
		return "🟡", "Medium", pct, stated
	case stated:
		return "🔴", "Low", pct, stated
	default:
		return "⚪", "Unstated", pct, stated
	}
}

// confidenceText renders the confidence fragment every delivery path headlines, so the
// four of them can never disagree about the same investigation.
//
// When the model stated no confidence anywhere it reads "confidence not stated" rather
// than "Low confidence · 0%". An absent self-assessment is not a zero one, and the
// difference is not cosmetic: `confidence` was optional in the findings schema, so a
// model that simply omitted it turned a sound, fully-evidenced, verify-passed finding
// into a red 0% card. Observed live — a NodeSystemSaturation investigation that
// correctly pinned a workspace pod consuming 7 of a node's 8 cores, metrics quoted,
// shipped as "🔴 Low confidence · 0%". Per-root-cause confidence is now required in the
// schema; this is the honest fallback for a model that ignores it.
func confidenceText(level string, pct int, stated bool) string {
	if !stated {
		return "confidence not stated"
	}
	return fmt.Sprintf("%s confidence · %d%%", level, pct)
}

// recallConfidenceNote discloses a recalled entry's own match confidence
// (the top hypothesis's, before outcome/track-record weighting) when it
// differs, post-rounding, from the delivered confidence confidenceBadge
// headlines — so the two never sit on the card as two bare percentages that
// look like a contradiction. Returns "" when there is nothing to disambiguate
// (not a recall, no root cause, or the two already round to the same number).
func recallConfidenceNote(inv providers.Investigation, deliveredPct int) string {
	if !inv.Recalled || len(inv.RootCauses) == 0 {
		return ""
	}
	ownPct := int(inv.RootCauses[0].Confidence*100 + 0.5)
	if ownPct == deliveredPct {
		return ""
	}
	return fmt.Sprintf("entry's own confidence %d%%, adjusted to %d%% by its track record", ownPct, deliveredPct)
}

// entryLink resolves the single "view entry" reference for the footer: this
// investigation's own curated entry when it produced one, else the
// previous/recalled entry's link, else — only when no URL is derivable — the
// bare catalog path as inline code. Centralising it here is what fixes a card
// that inlined a truncated entry filename mid-sentence: a KB entry path is the
// single most-clickable thing on the card, and Slack's client can collapse or
// truncate a long paragraph mid-word — so the reference belongs on its own
// short line, exactly once, not woven into prose. "" when there is nothing to
// link (a fresh, uncurated investigation with no recall/match context).
func entryLink(inv providers.Investigation) string {
	url := cmp.Or(inv.CuratedURL, inv.PrevCuratedURL)
	if url == "" && inv.MatchedKnowledge != nil {
		url = inv.MatchedKnowledge.URL
	}
	if url != "" {
		return fmt.Sprintf("📚 <%s|view entry>", escapeMrkdwn(url))
	}
	var path string
	if inv.Prior != nil {
		path = inv.Prior.EntryPath
	}
	path = cmp.Or(path, inv.RecalledEntry)
	if path == "" && inv.MatchedKnowledge != nil {
		path = inv.MatchedKnowledge.Path
	}
	if path == "" {
		return ""
	}
	return fmt.Sprintf("📚 `%s`", escapeMrkdwn(path))
}

// nextSteps collects the actionable remediations (root-cause suggestions + policy
// actions), de-duplicated and reversibility-flagged, preserving order. The
// untrusted descriptions are mrkdwn-escaped before the formatter's own italics
// suffix is appended, so only the payload is neutralised.
func nextSteps(inv providers.Investigation) []string {
	var steps []string
	seen := map[string]bool{}
	add := func(desc string, reversible bool) {
		// Trimmed, so this agrees with providers.Investigation.ActionWithoutRemedy,
		// which trims too. Gating on desc == "" instead let a whitespace-only
		// suggested_action produce a step, suppressing the no-remedy notice while
		// rendering an empty bullet — the promised remedy, spelled as one space.
		desc = strings.TrimSpace(desc)
		if desc == "" || seen[desc] {
			return
		}
		seen[desc] = true
		desc = escapeMrkdwn(desc)
		if reversible {
			desc += "  _(reversible)_"
		}
		steps = append(steps, desc)
	}
	for _, rc := range inv.RootCauses {
		add(rc.SuggestedAction, rc.Reversible)
	}
	for _, a := range inv.Actions {
		add(a.Description, a.Reversible)
	}
	return steps
}

// truncate caps a string to n runes, appending an ellipsis when cut (Slack section
// text is limited to 3000 chars).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// slackWhatChangedRunes caps the "What changed" metadata field. 200 was tighter
// than what models actually put in change_ref: it carries a revision range on some
// cards and a sentence explaining the change on others, and the explanatory form is
// the more useful of the two. 600 holds it while still leaving the two-column
// metadata grid readable, and stays well inside the 2000-rune per-field cap add()
// applies.
const slackWhatChangedRunes = 600

// truncateWords caps a string to n runes like truncate, but backs the cut off to
// the last space so the ellipsis never lands inside a word. Use it for prose the
// model wrote; use truncate for values that are one token (a ref, a URL, a sha)
// or where the cap is a hard protocol limit.
//
// A mid-word cut reads as a rendering fault rather than an intentional elision —
// live, a change_ref ended "…outside Argo/Fl…", which looks like the card broke.
// The cap itself is still enforced: a tail with no space in its final half is one
// long token, and that gets the hard cut rather than losing half the value.
//
// Call it on RAW text and escape the result, not the other way round. It counts
// runes of content, and a cut through escaped text can sever an "&amp;" into a
// visible "&am" — the same rendering fault, in the one branch the back-off cannot
// reach, since a long token has no space to back off to.
//
// internal/thread.truncateWords is the byte-budget twin of this function, applying
// the same rule to the KB validator's byte limits (and trimming dangling
// punctuation, which Slack does not need). Keep the two in step.
func truncateWords(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return "" // no room for content and a mark both
	}
	// The scan starts at the FIRST DROPPED rune, not the last kept one. That is what
	// makes the word-boundary case ordinary rather than special: when r[n-1] is
	// itself a space the prefix already ends on a boundary, and starting there finds
	// it instead of scanning past a whole word that fitted. Bounded at half the
	// allowance — a tail with no space in it is one long token (a sha, a URL), and
	// backing off to a distant space would lose more than the cut does.
	cut := r[:n-1]
	for i := n - 1; i > n/2; i-- {
		if unicode.IsSpace(r[i]) {
			cut = r[:i]
			break
		}
	}
	return strings.TrimRightFunc(string(cut), unicode.IsSpace) + "…"
}

// Multi delivers to several notifiers, best-effort: a failing notifier is logged,
// not propagated, so one bad sink doesn't block the others.
type Multi struct {
	notifiers []providers.Notifier
	log       *slog.Logger
}

// NewMulti builds a fan-out notifier.
func NewMulti(log *slog.Logger, notifiers ...providers.Notifier) *Multi {
	return &Multi{notifiers: notifiers, log: log}
}

var (
	_ providers.Notifier         = (*Multi)(nil)
	_ providers.ProgressNotifier = (*Multi)(nil)
	_ providers.KBUpdateNotifier = (*Multi)(nil)
)

// Deliver fans out to every notifier (best-effort: one bad sink never blocks the
// others), logs each failure, and returns the joined errors so the caller can tell
// delivery was incomplete. Returns nil when all sinks succeed.
func (m *Multi) Deliver(ctx context.Context, inv providers.Investigation) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.Deliver(ctx, inv); err != nil {
			m.log.Error("delivery failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DeliverProgress fans an interim progress ping out to every wrapped notifier
// that implements ProgressNotifier (the type-assert capability check), skipping
// those that don't (Matrix/webhook may no-op for now). It is best-effort by
// contract: a failing sink is logged and swallowed, never propagated — a progress
// ping must never fail an investigation. Returns nil always.
func (m *Multi) DeliverProgress(ctx context.Context, up providers.ProgressUpdate) error {
	for _, n := range m.notifiers {
		pn, ok := n.(providers.ProgressNotifier)
		if !ok {
			continue
		}
		if err := pn.DeliverProgress(ctx, up); err != nil {
			m.log.Error("progress delivery failed (swallowed)", "err", err)
		}
	}
	return nil
}

// DeliverKBUpdate announces a knowledge-base write to every wrapped notifier
// that implements KBUpdateNotifier (the same type-assert capability check
// DeliverProgress does), skipping those that don't — an announcement is not an
// Investigation, and a sink that cannot render one is skipped rather than
// erroring.
//
// It is best-effort by contract, exactly like DeliverProgress: a failing sink is
// logged and swallowed, never propagated, and it returns nil always. The write
// being announced has ALREADY landed on the forge before this is called, so a
// broadcast that could fail would report a write that in fact succeeded as
// failed — and there is nothing here to roll back.
//
// Nil-safe: an unwired Multi is a no-op, so the KB-write path can hold a nil
// sink to mean "announcements are off" without a nil check at every call site.
func (m *Multi) DeliverKBUpdate(ctx context.Context, up providers.KBUpdate) error {
	if m == nil {
		return nil
	}
	for _, n := range m.notifiers {
		kn, ok := n.(providers.KBUpdateNotifier)
		if !ok {
			continue
		}
		if err := kn.DeliverKBUpdate(ctx, up); err != nil {
			m.log.Error("knowledge-base update delivery failed (swallowed)", "err", err, "url", up.URL, "route", string(up.Route))
		}
	}
	return nil
}

// Len reports how many notifiers are configured.
func (m *Multi) Len() int { return len(m.notifiers) }

// ThreadRepliers returns the configured notifiers that can carry a reply back
// into a thread, keyed by Transport(). The wiring needs the concrete replier
// for the specific transport a thread lives on — Multi is where the built
// notifiers live, so this is the same capability discovery Deliver does for
// ProgressNotifier, exposed and transport-scoped: with two thread-capable
// transports configured (Slack, Matrix), picking just the first one found
// would answer one transport's threads on the other, or not at all.
//
// When two notifiers report the same transport, the first registered wins
// and the collision is logged — silently dropping one would be the same
// class of bug this scoping fixes.
func (m *Multi) ThreadRepliers() map[string]providers.ThreadNotifier {
	repliers := make(map[string]providers.ThreadNotifier)
	for _, n := range m.notifiers {
		tn, ok := n.(providers.ThreadNotifier)
		if !ok {
			continue
		}
		transport := tn.Transport()
		if _, collide := repliers[transport]; collide {
			m.log.Warn("multiple thread-capable notifiers registered for the same transport; keeping the first, dropping the rest",
				"transport", transport)
			continue
		}
		repliers[transport] = tn
	}
	return repliers
}
