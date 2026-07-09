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

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

func init() {
	Register(Descriptor{
		Name: "slack",
		Build: func(d Deps) (providers.Notifier, error) {
			// Bot token (chat.postMessage) takes precedence over an incoming webhook.
			if sl := d.Cfg.Notify.Slack; sl.BotTokenEnv != "" && sl.Channel != "" {
				if tok := os.Getenv(sl.BotTokenEnv); tok != "" {
					b := NewSlackBot(tok, sl.Channel)
					b.FeedbackButtons = sl.FeedbackButtons
					return b, nil
				}
			} else if sl := d.Cfg.Notify.Slack; sl.WebhookURLEnv != "" {
				if url := os.Getenv(sl.WebhookURLEnv); url != "" {
					s := NewSlack(url)
					s.FeedbackButtons = sl.FeedbackButtons
					return s, nil
				}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack post: %w", err)
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
}

// NewSlackBot builds a bot-token Slack notifier posting to channel (ID or name).
func NewSlackBot(token, channel string) *SlackBot {
	return &SlackBot{token: token, channel: channel, baseURL: "https://slack.com", http: httpx.SecureClient(15 * time.Second)}
}

var (
	_ providers.Notifier         = (*SlackBot)(nil)
	_ providers.ProgressNotifier = (*SlackBot)(nil)
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

// post targets the message at the configured channel and sends it via
// chat.postMessage, surfacing transport and Slack API (ok:false) errors, and
// returns the posted message's ts — the handle a threaded reply keys on ("" on
// the empty-body 2xx path). Shared by Deliver and DeliverProgress.
func (s *SlackBot) post(ctx context.Context, msg map[string]any) (string, error) {
	msg["channel"] = s.channel
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
// (header → verdict → metadata fields → why → next steps → ruled-out/data-gaps →
// recurrence → confidence footer → approval buttons) followed by an optional
// detail section (full evidence + the complete open-questions / data-gaps /
// ruled-out lists). This is the single-message composition used by the webhook
// path; threading is a later concern. The "text" field is fallbackText — the
// one-line notification/accessibility summary.
//
// Escape invariant: the fallback is no longer escapeMrkdwn(Format(inv)) — it is
// fallbackText, which escapes its one untrusted field (the model title) itself.
// Every untrusted string interpolated into an mrkdwn block (title in the verdict
// section, alert metadata + ChangeRef in the fields, evidence, ruled-out /
// data-gap items, PrevCuratedURL inside the recurrence link) is passed through
// escapeMrkdwn at the point of use. Headers are plain_text (never escaped) and
// slackDate emits a raw <!date^…> token that is blocks-only — it must never enter
// the escaped fallback text.
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

// summaryBlocks renders the triage summary as Block Kit, top-down as an on-call
// reads it: what/where (header) → verdict → key facts (fields) → seen-before KB
// recall (when present) → why → what to do → honest limits → recurrence →
// confidence footer → approval buttons. The full
// analysis (every hypothesis with all evidence, the complete open-questions /
// data-gaps / ruled-out lists) lives in detailBlocks; slackMessage appends the
// two for the single-message webhook path.
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

	// 2. Verdict owns the second slot — the headline actionability call. When the
	// model omitted a verdict (old / recall investigations) fall back to the
	// confidence context line so the layout stays complete and readable.
	if vEmoji, label := verdictBadge(inv.Verdict); label != "" {
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
			"text": truncate(fmt.Sprintf("%s *%s* — %s", vEmoji, label, escapeMrkdwn(title)), 2900)}})
	} else {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
			{"type": "mrkdwn", "text": fmt.Sprintf("%s *%s confidence* · %d%%  ·  🤖 RunLore SRE agent", emoji, level, pct)}}})
	}

	// 3. Metadata fields — the trigger-time facts an on-call scans first.
	if fields := metadataFields(inv); len(fields) > 0 {
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	}

	// 3b. Prior knowledge — on a recurring incident with a merged KB entry, quote
	// what the KB already says (cause + human-reviewed resolution + track record)
	// before the current analysis: history frames how the on-call reads what
	// follows, with zero clicks. The entry excerpts are untrusted (model prose,
	// human edits) and escaped; when this block renders, the legacy Recurrence
	// field and the previously-investigated context pointer are suppressed —
	// count, date and link all live here.
	if p := inv.Prior; p != nil {
		var s strings.Builder
		fmt.Fprintf(&s, "📚 *Seen before ×%d* — last %s", inv.Occurrences, slackDate(inv.LastOccurrence))
		if p.Cause != "" {
			fmt.Fprintf(&s, "\n*Prior cause:* %s", escapeMrkdwn(p.Cause))
		}
		if p.Resolution != "" {
			fmt.Fprintf(&s, "\n*Prior resolution:* %s", escapeMrkdwn(p.Resolution))
		}
		foot := make([]string, 0, 2)
		if inv.PrevCuratedURL != "" {
			foot = append(foot, fmt.Sprintf("<%s|previous entry>", escapeMrkdwn(inv.PrevCuratedURL)))
		}
		if p.Recalls > 0 {
			foot = append(foot, fmt.Sprintf("resolve rate %d/%d", p.Resolved, p.Recalls))
		}
		if len(foot) > 0 {
			fmt.Fprintf(&s, "\n%s", strings.Join(foot, " · "))
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}

	// 4. Top root cause: the single most-likely why, with up to three evidence
	// bullets. Deeper hypotheses and full evidence move to the detail section.
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
			blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
				{"type": "mrkdwn", "text": fmt.Sprintf("_…%d more hypotheses below_", n)}}})
		}
	}

	// 5. Suggested next steps — the resolution guide (per-root-cause suggestions +
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

	// 6. Ruled out — honest limits, capped at three (the full list is in the detail
	// section). Each item is untrusted and escaped.
	if len(inv.RuledOut) > 0 {
		var s strings.Builder
		s.WriteString("❌ *Ruled out:*")
		for i, r := range inv.RuledOut {
			if i >= 3 {
				fmt.Fprintf(&s, "\n• _…%d more_", len(inv.RuledOut)-i)
				break
			}
			fmt.Fprintf(&s, "\n• %s", escapeMrkdwn(r))
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}

	// 7. Data gaps — signals we could not obtain, as a compact context line.
	if len(inv.DataGaps) > 0 {
		escaped := make([]string, len(inv.DataGaps))
		for i, d := range inv.DataGaps {
			escaped[i] = escapeMrkdwn(d)
		}
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
			{"type": "mrkdwn", "text": truncate("⚠️ Data gaps: "+strings.Join(escaped, " · "), 2900)}}})
	}

	// 8. Recurrence pointer to the previous investigation's conclusion. The link is
	// formatter-constructed; escaping the URL inside it is what Slack's docs
	// prescribe (a raw & / < / > would corrupt the link).
	if inv.PrevCuratedURL != "" && inv.Prior == nil {
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
			{"type": "mrkdwn", "text": fmt.Sprintf("🔁 Previously investigated — <%s|previous conclusion>", escapeMrkdwn(inv.PrevCuratedURL))}}})
	}

	// 9. Confidence footer — verdict owns the top, so confidence, verification, the
	// agent tag, the KB link, and the usage one-liner all live at the bottom.
	foot := []string{fmt.Sprintf("%s %s confidence · %d%%", emoji, level, pct)}
	if inv.Verified {
		foot = append(foot, "✓ verified")
	}
	foot = append(foot, "🤖 RunLore SRE agent")
	if inv.CuratedURL != "" {
		foot = append(foot, fmt.Sprintf("📚 <%s|view entry>", escapeMrkdwn(inv.CuratedURL)))
	}
	if u := usageFooter(inv.Usage); u != "" {
		foot = append(foot, u) // trusted scaffolding — digits/labels only, no mrkdwn meta
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
		{"type": "mrkdwn", "text": truncate(strings.Join(foot, "  ·  "), 2900)}}})

	// 10. Interactive Approve/Reject for any action awaiting approval (rung-2).
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
	if len(inv.RootCauses) > 0 {
		if ch := inv.RootCauses[0].ChangeRef; ch != "" {
			add("What changed", truncate(escapeMrkdwn(ch), 200))
		} else {
			add("What changed", "none")
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

// confidenceBadge returns a visual confidence indicator. The headline confidence
// is the max of the overall score and the top root cause's — models frequently
// leave the top-level field at 0 while ranking a high-confidence root cause, and
// showing "0%" next to an 80% root cause reads as broken.
func confidenceBadge(inv providers.Investigation) (emoji, level string, pct int) {
	c := inv.Confidence
	for _, rc := range inv.RootCauses {
		if rc.Confidence > c {
			c = rc.Confidence
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
