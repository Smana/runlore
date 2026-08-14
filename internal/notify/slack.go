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

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
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

// ThreadSink records the thread root a delivered investigation was posted to.
// Implemented by *thread.Registry; declared here as an interface so the notifier
// never imports the thread package and stays ignorant of how the mapping is
// stored. Nil-safe by contract at every call site: registration is best-effort
// and must never affect delivery.
type ThreadSink interface {
	Register(root, channel string, inv providers.Investigation)
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
				if sl.ThreadCapture {
					b.Threads = d.Threads
				}
				return b, nil
			case slackTargetWebhook:
				s := NewSlack(os.Getenv(sl.WebhookURLEnv))
				s.FeedbackButtons = sl.FeedbackButtons
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
}

// NewSlack builds a Slack webhook notifier.
func NewSlack(webhookURL string) *Slack {
	return &Slack{webhookURL: webhookURL, http: httpx.SecureClient(15 * time.Second)}
}

var (
	_ providers.Notifier         = (*Slack)(nil)
	_ providers.ProgressNotifier = (*Slack)(nil)
)

// Deliver posts the formatted investigation to the webhook. When an action carries
// an ApprovalID, it renders interactive Approve/Reject buttons (Block Kit).
func (s *Slack) Deliver(ctx context.Context, inv providers.Investigation) error {
	return s.post(ctx, slackMessageWith(inv, s.FeedbackButtons))
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
)

// Deliver posts the compact summary to the channel, then the full analysis as a
// thread reply so the channel stays a scannable triage feed. The summary IS the
// notification; the detail reply is secondary, so a failed thread post returns a
// wrapped error that records the summary already landed — Multi logs it without
// implying the alert went undelivered. Nothing is threaded when the summary post
// yields no ts (empty-body path) or the investigation has no detail beyond it.
func (s *SlackBot) Deliver(ctx context.Context, inv providers.Investigation) error {
	summary := summaryBlocks(inv)
	if s.FeedbackButtons {
		summary = append(summary, feedbackBlocks(inv)...)
	}
	ts, err := s.post(ctx, map[string]any{"text": fallbackText(inv), "blocks": summary})
	if err != nil {
		return err
	}
	// Record the thread root so a reply here can be attributed. Best-effort and
	// nil-safe: capture is an opt-in extra, delivery is the contract.
	if s.Threads != nil && ts != "" {
		s.Threads.Register(ts, s.channel, inv)
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
func (s *SlackBot) ReplyInThread(ctx context.Context, root, channel, text string) error {
	msg := map[string]any{"text": text, "thread_ts": root}
	if channel != "" {
		msg["channel"] = channel
	}
	_, err := s.post(ctx, msg)
	return err
}

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
)

// slackMessage builds the Slack payload: a verdict-first Block Kit summary
// (header → verdict, alone → confidence, shown once → seen-before/recall
// context → matched-known-runbook → why, generous — it's the answer → suggested
// next steps → metadata fields, now BELOW the answer/action → footer:
// provenance only — verified, model calls, cost, the one view-entry link →
// approval buttons) followed by an optional detail section (full evidence + the
// complete open-questions / ruled-out / data-gap lists — ruled-out and
// data-gaps render ONLY here, never in the summary, so a phone reader hits the
// answer before any "Show more"). This is the single-message composition used
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
	return slackMessageWith(inv, false)
}

// slackMessageWith is slackMessage plus the opt-in 👍/👎 feedback block appended
// last (after the detail section) when withFeedback is set — the single-message
// webhook path's equivalent of the bot path's buttons-on-summary.
func slackMessageWith(inv providers.Investigation, withFeedback bool) map[string]any {
	blocks := append(summaryBlocks(inv), detailBlocks(inv)...)
	if withFeedback {
		blocks = append(blocks, feedbackBlocks(inv)...)
	}
	return map[string]any{
		"text":   fallbackText(inv),
		"blocks": blocks,
	}
}

// feedbackBlocks renders the 👍/👎 actions block — the human end of the learning
// loop: a click lands in the outcome ledger and weighs the recalled entry's trust
// like a resolve signal does (the only ground-truth channel for sources with no
// resolve webhook, e.g. GitOps failures). The button value is the TriggerKey
// (incident identity — ratings survive re-worded re-investigations), falling back
// to the alert fingerprint; with neither there is nothing for the ledger to
// attribute, so no buttons render. Labels are plain_text (never escaped); the
// value is opaque to Slack.
func feedbackBlocks(inv providers.Investigation) []map[string]any {
	key := cmp.Or(inv.TriggerKey, inv.Fingerprint)
	if key == "" {
		return nil
	}
	return []map[string]any{{"type": "actions", "elements": []map[string]any{
		{"type": "button", "action_id": feedbackUpActionID, "value": key,
			"text": map[string]any{"type": "plain_text", "text": "👍 Accurate", "emoji": true}},
		{"type": "button", "action_id": feedbackDownActionID, "value": key,
			"text": map[string]any{"type": "plain_text", "text": "👎 Off-base", "emoji": true}},
	}}}
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
	_, level, pct := confidenceBadge(inv)
	s := "🔍 " + title
	if _, label := verdictBadge(inv.Verdict); label != "" {
		s += " — " + label
	}
	return escapeMrkdwn(fmt.Sprintf("%s (%s confidence · %d%%)", s, level, pct))
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
func slackProgressMessage(up providers.ProgressUpdate) map[string]any {
	return map[string]any{"text": escapeMrkdwn(FormatProgress(up)), "blocks": slackProgressBlocks(up)}
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
	emoji, level, pct := confidenceBadge(inv)

	// 1. Header (plain_text — Slack renders it literally, no mrkdwn parsing, so the
	// untrusted alert name / title needs no escaping). When the source named the
	// alert, anchor on it and append the tenant/cluster scope + affected resource.
	head := "🔍 "
	if inv.AlertName != "" {
		head += inv.AlertName
		scope := inv.Tenant
		if scope == "" {
			scope = inv.Cluster
		}
		loc := make([]string, 0, 2)
		if scope != "" {
			loc = append(loc, scope)
		}
		if ref := inv.Resource.Ref(); ref != "" {
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
	confText := fmt.Sprintf("%s *%s confidence* · %d%%", emoji, level, pct)
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
		for j, e := range rc.Evidence {
			if j >= 3 {
				fmt.Fprintf(&s, "\n• _…%d more_", len(rc.Evidence)-j)
				break
			}
			fmt.Fprintf(&s, "\n• %s", escapeMrkdwn(e))
		}
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

	// 6. Suggested next steps — the resolution guide (per-root-cause suggestions +
	// policy actions, de-duplicated, reversibility-flagged), capped at three.
	if steps := nextSteps(inv); len(steps) > 0 {
		var s strings.Builder
		s.WriteString("*🛠 Suggested next steps*  _(read-only — RunLore won't apply these)_")
		for i, st := range steps {
			if i >= 3 {
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

	// 8. Footer — provenance only: verified, model calls/cost, and the single
	// link to view the entry (this investigation's own curated entry, or — on
	// a recall/seen-before card with nothing freshly curated — the prior/
	// recalled/matched entry; see entryLink). Confidence and the agent identity
	// are NOT repeated here: confidence already owns the header (2b), and the
	// Slack app's own identity already says who posted this — showing either
	// twice reads as the card disagreeing with itself, not reinforcing it.
	var foot []string
	if inv.Verified {
		foot = append(foot, "✓ verified")
	}
	if link := entryLink(inv); link != "" {
		foot = append(foot, link)
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
	scope := make([]string, 0, 2)
	if inv.Tenant != "" {
		scope = append(scope, escapeMrkdwn(inv.Tenant))
	}
	if inv.Cluster != "" {
		scope = append(scope, escapeMrkdwn(inv.Cluster))
	}
	add("Cluster", strings.Join(scope, " · "))
	if ref := inv.Resource.Ref(); ref != "" {
		add("Resource", escapeMrkdwn(strings.TrimSpace(inv.Resource.Kind+" "+ref)))
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
			add("What changed", truncate(escapeMrkdwn(ch), 200))
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
		fmt.Fprintf(&s, "*%d. %s*  `%.0f%%`", i+1, escapeMrkdwn(rc.Summary), rc.Confidence*100)
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
func confidenceBadge(inv providers.Investigation) (emoji, level string, pct int) {
	c := inv.Confidence
	if !inv.Recalled {
		for _, rc := range inv.RootCauses {
			c = max(c, rc.Confidence)
		}
	}
	pct = int(c*100 + 0.5)
	switch {
	case c >= 0.7:
		return "🟢", "High", pct
	case c >= 0.4:
		return "🟡", "Medium", pct
	default:
		return "🔴", "Low", pct
	}
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

// Len reports how many notifiers are configured.
func (m *Multi) Len() int { return len(m.notifiers) }

// ThreadReplier returns the first configured notifier that can carry a reply
// back into a thread, or nil when none can. The wiring needs one concrete
// replier and Multi is where the built notifiers live; this is the same
// capability discovery Deliver does for ProgressNotifier, exposed.
func (m *Multi) ThreadReplier() providers.ThreadNotifier {
	for _, n := range m.notifiers {
		if tn, ok := n.(providers.ThreadNotifier); ok {
			return tn
		}
	}
	return nil
}
