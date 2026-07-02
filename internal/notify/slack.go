package notify

import (
	"bytes"
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
					return NewSlackBot(tok, sl.Channel), nil
				}
			} else if env := d.Cfg.Notify.Slack.WebhookURLEnv; env != "" {
				if url := os.Getenv(env); url != "" {
					return NewSlack(url), nil
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
}

// NewSlack builds a Slack webhook notifier.
func NewSlack(webhookURL string) *Slack {
	return &Slack{webhookURL: webhookURL, http: httpx.SecureClient(15 * time.Second)}
}

var _ providers.Notifier = (*Slack)(nil)

// Deliver posts the formatted investigation to the webhook. When an action carries
// an ApprovalID, it renders interactive Approve/Reject buttons (Block Kit).
func (s *Slack) Deliver(ctx context.Context, inv providers.Investigation) error {
	body, err := json.Marshal(slackMessage(inv))
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
}

// NewSlackBot builds a bot-token Slack notifier posting to channel (ID or name).
func NewSlackBot(token, channel string) *SlackBot {
	return &SlackBot{token: token, channel: channel, baseURL: "https://slack.com", http: httpx.SecureClient(15 * time.Second)}
}

var _ providers.Notifier = (*SlackBot)(nil)

// Deliver posts the formatted investigation to the configured channel via
// chat.postMessage, surfacing both transport and Slack API (ok:false) errors.
func (s *SlackBot) Deliver(ctx context.Context, inv providers.Investigation) error {
	msg := slackMessage(inv)
	msg["channel"] = s.channel
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/api/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack status %d", resp.StatusCode)
	}
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("slack read response: %w", err)
	}
	if len(bytes.TrimSpace(respBody)) == 0 {
		return nil // a 2xx with an empty body is a successful post, not a failure
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("slack decode response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("slack chat.postMessage: %s", result.Error)
	}
	return nil
}

// Slack interaction action_ids — must match the server's /slack/interactions handler.
const (
	approveActionID = "runlore_approve"
	rejectActionID  = "runlore_reject"
)

// slackMessage builds the Slack payload: a Block Kit layout (header, confidence
// badge, ranked root causes with evidence, suggested next steps, open questions,
// KB link) plus a plain-text fallback (used for notifications/accessibility and by
// clients that don't render blocks). Actions carrying an ApprovalID also get
// interactive Approve/Reject buttons (rung-2).
func slackMessage(inv providers.Investigation) map[string]any {
	// The fallback text is parsed as mrkdwn too (notifications, block-less
	// clients), so untrusted content embedded in it must be escaped. Escaping the
	// composed Format output is safe because Format's own scaffolding (bold
	// markers, bullets) contains none of mrkdwn's three control characters —
	// TestSlackMessageFallbackEscaped guards that invariant. Matrix and the
	// generic webhook call Format themselves and are unaffected.
	return map[string]any{"text": escapeMrkdwn(Format(inv)), "blocks": slackBlocks(inv)}
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

// slackBlocks renders the investigation as Block Kit. Designed to read top-down as
// an on-call would triage: what happened → how sure → why → what to do → what's
// still open.
func slackBlocks(inv providers.Investigation) []map[string]any {
	title := inv.Title
	if title == "" {
		title = "Investigation"
	}
	emoji, level, pct := confidenceBadge(inv)
	blocks := []map[string]any{
		// The header is a plain_text object: Slack renders it literally (no mrkdwn
		// parsing), so the untrusted title needs no escaping here — escaping would
		// display raw &lt; entities instead.
		{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate("🔍 "+title, 150), "emoji": true}},
		{"type": "context", "elements": []map[string]any{
			{"type": "mrkdwn", "text": fmt.Sprintf("%s *%s confidence* · %d%%  ·  🤖 RunLore SRE agent", emoji, level, pct)}}},
	}

	// Ranked root causes, each with what-changed + evidence.
	for i, rc := range inv.RootCauses {
		if i >= 5 {
			break
		}
		var s strings.Builder
		fmt.Fprintf(&s, "*%d. %s*  `%.0f%%`", i+1, escapeMrkdwn(rc.Summary), rc.Confidence*100)
		if rc.ChangeRef != "" {
			fmt.Fprintf(&s, "\n📦 *What changed:* `%s`", escapeMrkdwn(rc.ChangeRef))
		}
		for j, e := range rc.Evidence {
			if j >= 4 {
				fmt.Fprintf(&s, "\n• _…%d more_", len(rc.Evidence)-j)
				break
			}
			fmt.Fprintf(&s, "\n• %s", escapeMrkdwn(e))
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}

	// Suggested next steps — the resolution guide. Combines per-root-cause
	// suggestions and any policy-surfaced actions, de-duplicated, reversibility flagged.
	if steps := nextSteps(inv); len(steps) > 0 {
		var s strings.Builder
		s.WriteString("*🛠 Suggested next steps*  _(read-only — RunLore won't apply these)_")
		for _, st := range steps {
			fmt.Fprintf(&s, "\n• %s", st)
		}
		blocks = append(blocks,
			map[string]any{"type": "divider"},
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}

	if len(inv.Unresolved) > 0 {
		var s strings.Builder
		s.WriteString("*❓ Open questions* _(needs a human)_")
		for i, u := range inv.Unresolved {
			if i >= 5 {
				fmt.Fprintf(&s, "\n• _…%d more_", len(inv.Unresolved)-i)
				break
			}
			fmt.Fprintf(&s, "\n• %s", escapeMrkdwn(u))
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}

	if inv.CuratedURL != "" {
		// The link itself is formatter-constructed; escaping the URL inside it is
		// what Slack's docs prescribe (a raw & / < / > would corrupt the link).
		blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
			{"type": "mrkdwn", "text": fmt.Sprintf("📚 Logged to the knowledge base — <%s|view entry>", escapeMrkdwn(inv.CuratedURL))}}})
	}

	// Interactive Approve/Reject for any action awaiting approval (rung-2).
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

var _ providers.Notifier = (*Multi)(nil)

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

// Len reports how many notifiers are configured.
func (m *Multi) Len() int { return len(m.notifiers) }
