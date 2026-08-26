# Slack silence-control UX Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the 🔕 silence control visible, its acknowledgement public, and its effect durable on the card.

**Architecture:** Swap Slack's unlabelled `overflow` element for a `static_select` carrying a placeholder, then rewrite the card in place on click — dropping the silence control and appending a marker — via the already-SSRF-guarded `response_url`. A rebuild that cannot be done safely falls back to today's ephemeral note rather than blanking the finding.

**Tech Stack:** Go, Slack Block Kit, `internal/notify` (render) and `internal/server` (interaction handling), which deliberately do not import each other.

**Spec:** `docs/superpowers/specs/2026-08-26-slack-silence-control-ux-design.md`

---

## Conventions — read before Task 1

- **No Makefile.** Tests: `go test -race ./...`. Lint: `golangci-lint run ./...`. Format: `gofmt -l .` must be empty.
- Every file starts with `// SPDX-License-Identifier: Apache-2.0` then a blank line.
- **Conventional Commits.** **Never add a `Co-Authored-By` trailer.**
- Comments explain *why*, at length, citing durable verifiable facts. See `internal/investigate/cloud_tools.go:55-95`. A comment restating the code is a defect. Do not write comments narrating "the plan" or "Task N".
- Test names are full sentences describing the invariant. Table-driven with a prose `name` field.
- `internal/server` deliberately does NOT import `internal/notify`. Shared string constants are duplicated as literals on both sides and pinned by a guard test in `notify` — see `TestSilenceBlockIDPrefixMatchesTheServerHandler`.

## File structure

| File | Responsibility |
|---|---|
| `internal/notify/slack.go` *(modify)* | Render the silence control as a labelled `static_select` |
| `internal/notify/slack_test.go` *(modify)* | Pin the rendered element shape |
| `internal/config/config.go` *(modify)* | Window-count validation messages stop citing a mechanism no longer used |
| `internal/thread/silence.go` *(modify)* | `SilenceMarker` — the one-line card stamp, beside the existing `SilenceAck` |
| `internal/server/server.go` *(modify)* | Parse `message.blocks`; rebuild the card; post blocks via `response_url` |
| `internal/server/server_test.go` *(modify)* | Rebuild behaviour, and the fallback that must never blank the card |
| `website/content/docs/integrations/notifications/slack.md` *(modify)* | Describe the control as it now renders |
| `deploy/helm/runlore/values.yaml` *(modify)* | Comment stops citing overflow limits |

---

## Task 1: Render the silence control as a labelled `static_select`

**Files:**
- Modify: `internal/notify/slack.go` (constant at `:342-348`, comment at `:412-431`, builder at `:449-473`)
- Test: `internal/notify/slack_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestSilenceControlIsLabelledNotABareOverflow pins the fix for the first live
// use of the 🔕 control: Slack's overflow element takes no text, so it rendered
// as a bare "···" beside 👍/👎 and nobody could tell what it was. A static_select
// carries a placeholder, which is the only actions-block element that shows a
// label AND opens a menu.
func TestSilenceControlIsLabelledNotABareOverflow(t *testing.T) {
	inv := providers.Investigation{Title: "t", TriggerKey: "k"}
	blocks := feedbackBlocks(inv, true, []time.Duration{4 * time.Hour, 24 * time.Hour})
	if len(blocks) != 1 {
		t.Fatalf("want one actions block, got %d", len(blocks))
	}
	elements, _ := blocks[0]["elements"].([]map[string]any)
	var silence map[string]any
	for _, e := range elements {
		if e["action_id"] == silenceActionID {
			silence = e
		}
		if e["type"] == "overflow" {
			t.Errorf("the silence control must not be an overflow: it renders as an unlabelled ···")
		}
	}
	if silence == nil {
		t.Fatal("no silence element rendered")
	}
	if silence["type"] != "static_select" {
		t.Errorf("silence element type = %v, want static_select", silence["type"])
	}
	ph, _ := silence["placeholder"].(map[string]any)
	if ph == nil || ph["text"] == "" {
		t.Fatalf("static_select needs a non-empty placeholder, got %v", silence["placeholder"])
	}
	if !strings.Contains(ph["text"].(string), "🔕") {
		t.Errorf("placeholder = %q, want it to carry the 🔕 the menu options use", ph["text"])
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/notify/ -run TestSilenceControlIsLabelledNotABareOverflow -v`
Expected: FAIL — `the silence control must not be an overflow` and `silence element type = overflow, want static_select`.

- [ ] **Step 3: Swap the element**

In `feedbackBlocks`, replace the append at `:463-465`:

```go
		elements = append(elements, map[string]any{
			"type": "static_select", "action_id": silenceActionID,
			// An overflow takes no text and renders as a bare "···". Observed live
			// on 2026-08-26: nobody could tell what the control was, because the 🔕
			// only appears once the menu is already open. A static_select is the one
			// actions-block element that carries both a visible label and a menu.
			"placeholder": map[string]any{"type": "plain_text", "text": "🔕 Silence…", "emoji": true},
			"options":     opts,
		})
```

Rename the local `overflowFits` to `silenceFits` at `:449` and its two uses at `:452` and `:471`.

- [ ] **Step 4: Rename the constant and correct its rationale**

Replace `:342-348`:

```go
	// slackSilenceMinOptions is a DELIBERATE UX floor, no longer a Slack limit.
	// It was named for Slack's overflow element, which rejects a 1-option menu by
	// rejecting the WHOLE message — but the control is a static_select now, and
	// Slack accepts a select with one option. The floor stays because a one-entry
	// dropdown is a button wearing a menu: it reads as broken. Config validation
	// rejects that combination at startup; this is the render site refusing to
	// build it anyway, since it is the only place that counts the options it is
	// about to emit.
	slackSilenceMinOptions = 2
```

Update the reference at `:427` and the doc-comment paragraph at `:412-431` so it no longer says Slack would reject the message.

- [ ] **Step 5: Pin the claim that no migration is needed**

The whole "old cards keep working" argument rests on `static_select` and
`overflow` delivering the chosen option at the same payload path. Verified during
design, but unpinned. Add to `internal/server/silence_test.go`:

```go
// TestSilenceReadsAStaticSelectPayload pins the reason this change needed no
// migration: Slack sends the chosen option at actions[0].selected_option.value
// for static_select exactly as it did for overflow, so cards posted before the
// element swap stay clickable after it. If Slack ever diverges, this fails here
// rather than as a silently dead control on old cards.
func TestSilenceReadsAStaticSelectPayload(t *testing.T) {
	rec := &recordSilence{}
	const secret = "shh"
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)

	if rr := sendSilence(t, srv, secret, "sil:k", "24h"); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	want := recordedSilence{key: "k", window: 24 * time.Hour, user: "U9"}
	if len(rec.got) != 1 || rec.got[0] != want {
		t.Fatalf("recorded = %+v, want exactly one %+v", rec.got, want)
	}
}
```

- [ ] **Step 6: Run the whole notify package**

Run: `go test -race ./internal/notify/ && gofmt -l internal/notify`
Expected: PASS, no gofmt output. Existing silence tests that assert `"overflow"` will fail — update them to the new shape; do not delete them.

- [ ] **Step 7: Commit**

```bash
git add internal/notify/slack.go internal/notify/slack_test.go internal/server/silence_test.go
git commit -m "fix(notify): give the silence control a label instead of a bare ···"
```

---

## Task 2: Stop the validation messages citing a mechanism no longer used

**Files:**
- Modify: `internal/config/config.go:2227-2233`, `:2247-2263`, and the comment at `:1203`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestSilenceWindowBoundsDoNotBlameSlack pins that the window-count errors stop
// citing Slack's overflow element. The bounds survive the move to static_select
// as a UX floor and ceiling, but an error explaining itself with a mechanism the
// code no longer uses sends the reader to the wrong place.
func TestSilenceWindowBoundsDoNotBlameSlack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		windows []Duration
	}{
		{"one preset is below the floor", []Duration{Duration(time.Hour)}},
		{"six presets are above the ceiling", []Duration{
			Duration(time.Hour), Duration(2 * time.Hour), Duration(3 * time.Hour),
			Duration(4 * time.Hour), Duration(5 * time.Hour), Duration(6 * time.Hour)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := silenceTestConfig(t, tc.windows)
			err := c.Validate()
			if err == nil {
				t.Fatal("want a validation error, got nil")
			}
			if strings.Contains(err.Error(), "overflow") {
				t.Errorf("error still cites the overflow element: %v", err)
			}
			if !strings.Contains(err.Error(), "notify.silence.windows") {
				t.Errorf("error should name the field it is about: %v", err)
			}
		})
	}
}
```

Write `silenceTestConfig` as a helper returning a minimal `*Config` with `notify.slack.silence_button` on, a bot token, a channel, a signing secret and `outcome.ledger_path` set — mirroring whatever the existing silence validation tests in this file already build. Reuse their helper if one exists.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/config/ -run TestSilenceWindowBoundsDoNotBlameSlack -v`
Expected: FAIL — both cases report `error still cites the overflow element`.

- [ ] **Step 3: Rewrite the two messages**

At `:2233`:

```go
			return fmt.Errorf("notify.silence.windows lists %d entries; at most 5 are offered: the 🔕 menu is a quick choice on a card someone is reading at 3am, and a six-item dropdown is a form — trim the list to 5 or fewer presets", n)
```

At `:2263`:

```go
			return fmt.Errorf("notify.silence.windows lists %d entry; at least 2 are needed: a one-option dropdown is a button wearing a menu and reads as broken — add a second preset, or turn off notify.slack.silence_button", n)
```

Update the comments above each (`:2227`, `:2247`) and the field comment at `:1203` so none of them attribute the bounds to Slack's overflow element.

- [ ] **Step 4: Run and commit**

Run: `go test -race ./internal/config/ && gofmt -l internal/config`
Expected: PASS, no gofmt output.

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "fix(config): silence-window bounds are a UX choice, not a Slack limit"
```

---

## Task 3: A marker line for the card

**Files:**
- Modify: `internal/thread/silence.go` (beside `SilenceAck` at `:83`)
- Test: `internal/thread/silence_test.go`

- [ ] **Step 1: Write the failing test**

```go
// TestSilenceMarkerNamesWhoAndUntilWhen pins the one-line stamp left on the card
// itself. It is deliberately not SilenceAck: the ack explains the consequences to
// the person who just clicked, while the marker is scannable evidence for someone
// scrolling the channel a day later.
func TestSilenceMarkerNamesWhoAndUntilWhen(t *testing.T) {
	until := time.Date(2026, 8, 26, 14, 40, 0, 0, time.UTC)
	got := SilenceMarker("smaine.kahlouch", 48*time.Hour, until)
	for _, want := range []string{"🔕", "smaine.kahlouch", "48h"} {
		if !strings.Contains(got, want) {
			t.Errorf("marker %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the marker is a single context line, got %q", got)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/thread/ -run TestSilenceMarkerNamesWhoAndUntilWhen -v`
Expected: FAIL — `undefined: SilenceMarker`.

- [ ] **Step 3: Implement it**

```go
// SilenceMarker is the one-line stamp left on the card after a silence, as
// distinct from SilenceAck's full explanation to the clicker. Two different
// readers: the ack answers "what did I just do?", the marker answers "did anyone
// already deal with this?" for someone scrolling the channel a day later. It is a
// single line because it renders in a context block, which does not wrap well.
func SilenceMarker(user string, window time.Duration, until time.Time) string {
	return fmt.Sprintf("🔕 Silenced by @%s until %s · %s",
		user, until.Format(silenceExpiryLayout), ShortDuration(window))
}
```

- [ ] **Step 4: Run and commit**

Run: `go test -race ./internal/thread/ && gofmt -l internal/thread`
Expected: PASS, no gofmt output.

```bash
git add internal/thread/silence.go internal/thread/silence_test.go
git commit -m "feat(thread): add the one-line silence marker for the card"
```

---

## Task 4: Rebuild the card, and refuse to blank it

**Files:**
- Modify: `internal/server/server.go` — payload struct at `:404-425`, plus a new function
- Test: `internal/server/server_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// TestSilencedCardDropsTheControlAndKeepsTheFinding pins the rebuild: the silence
// menu goes (it has served its purpose and a second click would be confusing),
// 👍/👎 stay (a 👎 is the documented way to lift a silence early), and the marker
// is appended.
func TestSilencedCardDropsTheControlAndKeepsTheFinding(t *testing.T) {
	blocks := []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*Why:* something broke"}},
		{"type": "actions", "block_id": "sil:k", "elements": []any{
			map[string]any{"type": "button", "action_id": "runlore_feedback_up"},
			map[string]any{"type": "button", "action_id": "runlore_feedback_down"},
			map[string]any{"type": "static_select", "action_id": "runlore_silence"},
		}},
	}
	got, ok := silencedCard(blocks, "🔕 Silenced by @x until y · 48h")
	if !ok {
		t.Fatal("rebuild refused a well-formed card")
	}
	dump, _ := json.Marshal(got)
	if strings.Contains(string(dump), "runlore_silence") {
		t.Error("the silence control survived the rebuild")
	}
	for _, keep := range []string{"runlore_feedback_up", "runlore_feedback_down", "something broke"} {
		if !strings.Contains(string(dump), keep) {
			t.Errorf("the rebuild dropped %q — it must only remove the silence control", keep)
		}
	}
	if !strings.Contains(string(dump), "Silenced by @x") {
		t.Error("the marker was not appended")
	}
}

// TestSilencedCardRefusesRatherThanBlanksTheCard is the regression test for the
// one hard invariant. replace_original: true overwrites the message, so a rebuild
// that cannot be done safely must report failure and let the caller fall back to
// an ephemeral note. A blanked card loses the investigation the silence is about,
// which is strictly worse than having no marker.
func TestSilencedCardRefusesRatherThanBlanksTheCard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []map[string]any
	}{
		{"no blocks in the payload at all", nil},
		{"an empty block list", []map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := silencedCard(tc.blocks, "marker"); ok {
				t.Errorf("rebuild claimed success on %s: %v", tc.name, got)
			}
		})
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/server/ -run TestSilencedCard -v`
Expected: FAIL — `undefined: silencedCard`.

- [ ] **Step 3: Add `message.blocks` to the payload struct**

Append inside `slackInteraction` (after the `Actions` field):

```go
	// Message carries the card the click came from. It is the only way to rewrite
	// that card without a chat:write scope and a chat.update call: the blocks are
	// echoed back to us, so the rebuild can be a pure function of the payload.
	Message struct {
		Blocks []map[string]any `json:"blocks"`
	} `json:"message"`
```

- [ ] **Step 4: Implement the rebuild**

```go
// silencedCard rewrites a card after a silence: the 🔕 control is removed and a
// marker context block is appended. 👍/👎 are deliberately kept — a 👎 lifts a
// silence early, so removing it would strand the escape hatch the ack promises.
//
// It reports false when it cannot produce a card it is sure of. That matters more
// than it looks: the caller posts with replace_original: true, which OVERWRITES
// the message, so a rebuild that guessed would blank the investigation the marker
// is about. Refusing costs a marker; guessing costs the finding.
//
// "runlore_silence" is a literal because internal/server does not import
// internal/notify — the same split as the "sil:" block_id prefix above, and
// pinned from the notify side by the same style of guard test.
func silencedCard(blocks []map[string]any, marker string) ([]map[string]any, bool) {
	if len(blocks) == 0 {
		return nil, false
	}
	out := make([]map[string]any, 0, len(blocks)+1)
	for _, b := range blocks {
		elems, isActions := b["elements"].([]any)
		if !isActions {
			out = append(out, b)
			continue
		}
		kept := make([]any, 0, len(elems))
		for _, e := range elems {
			if m, ok := e.(map[string]any); ok && m["action_id"] == "runlore_silence" {
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			continue // an actions block with nothing left in it is invalid to Slack
		}
		nb := make(map[string]any, len(b))
		for k, v := range b {
			nb[k] = v
		}
		nb["elements"] = kept
		out = append(out, nb)
	}
	if len(out) == 0 {
		return nil, false
	}
	return append(out, map[string]any{
		"type": "context",
		"elements": []any{map[string]any{"type": "mrkdwn", "text": marker}},
	}), true
}
```

- [ ] **Step 5: Run and commit**

Run: `go test -race ./internal/server/ && gofmt -l internal/server`
Expected: PASS, no gofmt output.

```bash
git add internal/server/server.go internal/server/server_test.go
git commit -m "feat(server): rebuild a silenced card, or refuse rather than blank it"
```

---

## Task 5: Post the rebuilt card

**Files:**
- Modify: `internal/server/server.go` — `updateSlack` at `:765`, `slackResponseBody` at `:793`, and the silence branch at `:558-566`
- Test: `internal/server/server_test.go`

- [ ] **Step 1: Extend the existing helper, then write the failing tests**

`internal/server/silence_test.go:38` already has `sendSilence`, which posts a
signed interaction. It carries neither a `response_url` nor `message.blocks`, so
add a sibling beside it rather than a second signing helper:

```go
// sendSilenceCard is sendSilence plus the two payload fields the card rewrite
// needs: where to post the rebuilt card, and the card it is rebuilding. Passing
// blocks == "" omits message.blocks entirely, which is the shape that must fall
// back rather than blank the card.
func sendSilenceCard(t *testing.T, srv *Server, secret, blockID, value, responseURL, blocks string) *httptest.ResponseRecorder {
	t.Helper()
	msg := ""
	if blocks != "" {
		msg = `,"message":{"blocks":` + blocks + `}`
	}
	payload := `{"user":{"id":"U9","username":"bob"},"response_url":"` + responseURL + `",` +
		`"actions":[{"action_id":"runlore_silence","block_id":"` + blockID +
		`","selected_option":{"value":"` + value + `"}}]` + msg + `}`
	body := "payload=" + url.QueryEscape(payload)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", slackSign(secret, ts, body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

const testSilenceCardBlocks = `[
  {"type":"section","text":{"type":"mrkdwn","text":"*Why:* something broke"}},
  {"type":"actions","block_id":"sil:k","elements":[
    {"type":"button","action_id":"runlore_feedback_up"},
    {"type":"static_select","action_id":"runlore_silence"}]}]`
```

The response_url guard only accepts `https://*.slack.com`, so a plain `httptest`
server would be refused before any POST. Capture the body by pointing the
`Server`'s HTTP client at a stub instead — follow whatever the existing
`updateSlack` tests in this package do; if none exist, assert on the guard's
refusal path and cover the body shape by calling `updateSlackBlocks` directly.

```go
// TestSilenceRewritesTheCardPublicly pins both halves of what the screenshot
// exposed: the acknowledgement must not be ephemeral (nobody else learned the
// alert was handled) and the card must carry the marker afterwards (scrollback
// could not tell a handled finding from an unhandled one).
func TestSilenceRewritesTheCardPublicly(t *testing.T) {
	var got map[string]any
	srv, capture := serverWithCapturedSlackResponse(t, &got)

	rr := sendSilenceCard(t, srv, capture.secret, "sil:k", "48h", capture.url, testSilenceCardBlocks)
	if rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	if got["replace_original"] != true {
		t.Errorf("replace_original = %v, want true — the card must be rewritten in place", got["replace_original"])
	}
	if got["response_type"] == "ephemeral" {
		t.Error("the acknowledgement is still ephemeral: nobody but the clicker sees it")
	}
	dump, _ := json.Marshal(got["blocks"])
	if got["blocks"] == nil {
		t.Fatal("no blocks posted — the rebuild did not reach the response_url")
	}
	if !strings.Contains(string(dump), "Silenced by @bob") {
		t.Errorf("the posted card carries no marker: %s", dump)
	}
	if strings.Contains(string(dump), "runlore_silence") {
		t.Error("the silence control survived onto the rewritten card")
	}
	if !strings.Contains(string(dump), "something broke") {
		t.Error("the rewrite dropped the finding it was marking")
	}
}

// TestSilenceWithoutBlocksLeavesTheCardIntact is the end-to-end half of the hard
// invariant: a payload with no blocks must degrade to the old ephemeral note, not
// post an empty card. Task 4 pins the pure function; this pins the wiring.
func TestSilenceWithoutBlocksLeavesTheCardIntact(t *testing.T) {
	var got map[string]any
	srv, capture := serverWithCapturedSlackResponse(t, &got)

	if rr := sendSilenceCard(t, srv, capture.secret, "sil:k", "48h", capture.url, ""); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	if got["blocks"] != nil {
		t.Errorf("posted blocks with nothing to rebuild from: %v", got["blocks"])
	}
	if got["replace_original"] != false {
		t.Errorf("replace_original = %v, want false — the card must be left alone", got["replace_original"])
	}
	if got["response_type"] != "ephemeral" {
		t.Errorf("response_type = %v, want ephemeral fallback", got["response_type"])
	}
}

// TestSilenceLedgerFailureLeavesTheCardUnmarked pins the ordering the spec
// requires: record first, then decorate. A marker on a card whose silence was
// never stored is a lie the reader has no way to detect.
func TestSilenceLedgerFailureLeavesTheCardUnmarked(t *testing.T) {
	var got map[string]any
	srv, capture := serverWithCapturedSlackResponseFailing(t, &got) // Silence() returns an error

	if rr := sendSilenceCard(t, srv, capture.secret, "sil:k", "48h", capture.url, testSilenceCardBlocks); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	dump, _ := json.Marshal(got)
	if strings.Contains(string(dump), "Silenced by") {
		t.Errorf("the card was marked despite the ledger write failing: %s", dump)
	}
}
```

Write `serverWithCapturedSlackResponse` (and its `…Failing` variant, whose
recorder returns an error from `Silence`) once, beside `recordSilence` at
`silence_test.go:27`. It returns a `*Server` wired with `Actions{Silence: rec,
SlackSecret: secret}` and a capture handle carrying the secret and an
`https://…slack.com`-shaped URL the guard accepts.

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/server/ -run TestSilence -v`
Expected: FAIL — `replace_original = false` and `no blocks posted`.

- [ ] **Step 3: Add a blocks-aware post**

```go
// updateSlackBlocks rewrites the interaction's message with a rebuilt card. It
// shares updateSlack's response_url validation — the URL arrives in the payload
// and is therefore attacker-influenceable, so it stays restricted to https
// *.slack.com and a bounded client.
func (s *Server) updateSlackBlocks(ctx context.Context, responseURL string, blocks []map[string]any) {
	body, err := json.Marshal(map[string]any{"replace_original": true, "blocks": blocks})
	if err != nil {
		s.log.Warn("slack card rewrite: marshal failed (best-effort)", "err", err)
		return
	}
	s.postSlackResponse(ctx, responseURL, body)
}
```

Extract the existing validate-and-POST tail of `updateSlack` into `postSlackResponse(ctx, responseURL string, body []byte)` and have both callers use it, so the SSRF guard has exactly one implementation.

- [ ] **Step 4: Wire it into the silence branch**

Replace the tail of `handleSlackInteraction` (`:571-575`) so silence takes the rewrite path when it can:

```go
	w.WriteHeader(http.StatusOK) // ack the click; update the message best-effort
	// Approve/reject replace the interaction message with the outcome; a feedback
	// ack must NOT — replacing would wipe the investigation the rating is about.
	//
	// Silence is the third case: it rewrites the card too, but by REBUILDING it
	// (control removed, marker appended) rather than replacing it with text. When
	// the rebuild refuses — no blocks in the payload — it degrades to the feedback
	// behaviour rather than posting a card it is unsure of.
	if silenced && p.ResponseURL != "" {
		if rebuilt, ok := silencedCard(p.Message.Blocks, marker); ok {
			s.updateSlackBlocks(r.Context(), p.ResponseURL, rebuilt)
			return
		}
		s.log.Warn("slack silence: no usable blocks in payload; leaving the card intact")
	}
	replace := act.ActionID == "runlore_approve" || act.ActionID == "runlore_reject"
	s.updateSlack(r.Context(), p.ResponseURL, msg, replace)
```

Declare `var silenced bool` and `var marker string` beside `msg` at the top of the switch, and set both in the silence branch immediately after `s.silence.Silence(...)` succeeds:

```go
		silenced = true
		marker = thread.SilenceMarker(p.User.Username, window, now.Add(window))
```

Setting them only after the ledger write succeeds is what keeps the ordering the spec requires: **record first, then decorate.** A marker on a card whose silence was never stored is a lie the reader cannot detect.

- [ ] **Step 5: Run the full gate and commit**

Run: `go build ./... && go vet ./... && go test -race ./... ; gofmt -l . ; golangci-lint run ./...`
Expected: build/vet clean, all packages pass, no gofmt output, 0 lint issues.

```bash
git add internal/server/server.go internal/server/server_test.go
git commit -m "feat(server): make the silence acknowledgement public and durable"
```

---

## Task 6: Documentation

**Files:**
- Modify: `website/content/docs/integrations/notifications/slack.md:77-79`
- Modify: `deploy/helm/runlore/values.yaml:355`

- [ ] **Step 1: Update the Slack integration page**

`:77` currently reads "a 🔕 overflow menu offering the durations listed in `notify.silence.windows`". Replace the overflow wording with the control as it now renders — a labelled `🔕 Silence…` dropdown — and rewrite `:79`, which attributes the 2–5 bound to "Slack answers a…". State the bound as a UX floor and ceiling instead. Add one sentence recording what a click now does: the card is rewritten in place with the control removed and a marker naming who silenced it and until when, visible to the channel.

- [ ] **Step 2: Update the chart comment**

`values.yaml:355` says the 2–5 range exists because of "Slack's overflow limits — outside that Slack rejects the". Rewrite so it does not cite a mechanism the code no longer uses, matching the new validation error text from Task 2.

- [ ] **Step 3: Verify no stale references remain**

Run: `grep -rn "overflow" --include='*.go' --include='*.md' --include='*.yaml' internal/ website/ deploy/ | grep -iv "overflow-x\|text-overflow"`
Expected: no hit describes the silence control as an overflow. Hits in unrelated contexts are fine.

- [ ] **Step 4: Commit**

```bash
git add website/content/docs/integrations/notifications/slack.md deploy/helm/runlore/values.yaml
git commit -m "chore(docs): describe the silence control as it now renders"
```

---

## Definition of done

- [ ] Full gate green: `go build ./... && go vet ./... && go test -race ./... ; gofmt -l . ; golangci-lint run ./...`
- [ ] `grep -rn "overflow" internal/ website/ deploy/` returns nothing describing the silence control
- [ ] The two invariant tests exist and have been observed failing before passing: the card is never blanked, and the ack is never ephemeral when a rebuild succeeded
