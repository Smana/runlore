# PR4 — tell the human what landed, and tell the other sinks

Stacked on PR3 (#484). Two features, decided with the owner:

1. The thread reply shows **what was recorded**, not just a PR link.
2. A KB write is **announced to every configured notifier**, not only the thread it came from.

Constraints carried from `.superpowers/sdd/pr4-scope.md`. Read that file first.

## Why these two are one PR

Both need the same thing that does not exist today: a description of what a write actually
did. `write()` returns `(msg string, landed bool, err error)` and builds `NoteBody` /
`ConceptEntry` inline, so nothing downstream can name the entry, quote the note, or say
which route was taken. Task 1 produces that value once; tasks 2 and 4 consume it.

## Non-goals

- No new forge calls. The reply and the announcement describe what was already written; they
  never re-read the forge to enrich it.
- No diff of the KB. `providers.Ref` is `{URL string}`; asking the forge what files changed is
  a second round trip on the incident path.
- No new model calls anywhere in this PR.

---

### Task 1: Return what the write actually did

**Files:** `internal/thread/responder.go`, `internal/thread/responder_test.go`

- [ ] **Step 1: Write the failing test**

- `write()` reports the route taken (`comment` on a linked PR, or `open_pr`), the PR number,
  the URL, and — on the `open_pr` route — the entry title `ConceptEntry` generated.
- It reports the note text **as written**: redacted and capped, not the caller's raw input.
  Assert with a message carrying a secret and exceeding the cap: the reported text must be
  masked and capped, byte-identical to what reached the forge.
- Every existing return path keeps its current reply string byte-for-byte. This task changes
  what `write()` RETURNS, not what it says.
- A throttled, capped, or failed write reports no result — a caller must not be able to
  announce a write that did not happen.

- [ ] **Step 2: Run test to verify it fails**

`go test ./internal/thread/ -run TestWriteReports -v` → FAIL.

- [ ] **Step 3: Implement**

Add a `KBWrite` struct and return `(string, *KBWrite, error)` — a nil pointer where `landed`
was false, so "nothing landed" and "here is what landed" are one value that cannot disagree
with itself. That is deliberate: the current `(msg, landed, err)` shape lets a caller read
`landed` and ignore `err`, and this package has shipped that class of bug three times.

The note text must come from ONE evaluation of the redact+cap pipeline, shared with the body
that goes to the forge. Do not re-derive it separately — two call sites drift.

- [ ] **Step 4: Run tests and commit**

```bash
git commit -m "refactor(thread): report what a knowledge write did, not just that it did

write() built its body inline and returned a bool, so no caller could name the
entry, quote what was filed, or say which route was taken. The reply strings are
unchanged; only the return value grows." -- internal/thread/responder.go internal/thread/responder_test.go
```

---

### Task 2: Quote what was recorded, back into the thread

**Files:** `internal/thread/responder.go`, `internal/thread/responder_test.go`

- [ ] **Step 1: Write the failing test**

- After a note lands, the reply quotes the recorded text and names the entry (`open_pr` route)
  or the PR it was added to (`comment` route). The URL stays.
- **The quote is bounded independently of `MaxNoteBytes`.** A note at the 8 KiB cap must not
  produce an 8 KiB chat message. Assert a preview ceiling and that a truncated preview says so.
- **The quoted text is marked `Untrusted`** (PR3's mechanism) so each transport escapes it.
  This is the highest-value assertion in the task: the quote is note content echoed back into
  a chat system, and on the model-drafted route the model wrote it.
- The model-drafted route is still marked as model-drafted in the reply — quoting the text must
  not make it read as the human's words. `ProposedNote` already carries the distinction.
- A `note:` write with chat off quotes it too: the human should see what was captured either way.

- [ ] **Step 2: Run test to verify it fails**

`go test ./internal/thread/ -run TestReplyQuotes -v` → FAIL.

- [ ] **Step 3: Implement**

Render from Task 1's `KBWrite`. Keep RunLore's own framing (the `📝` status line, the URL)
outside the untrusted mark, exactly as `modelVoice` does — the marks wrap CONTENT, never
RunLore's own bytes.

- [ ] **Step 4: Run tests and commit**

---

### Task 3: A KB-update event, and the capability to receive it

**Files:** `internal/providers/providers.go`, `internal/providers/providers_test.go`,
`internal/notify/slack.go` (Multi), `internal/notify/slack_test.go`

- [ ] **Step 1: Write the failing test**

- `Multi.DeliverKBUpdate` fans out to every notifier implementing the new capability and
  **skips those that do not**, exactly as `DeliverProgress` does its `ProgressNotifier` check.
- Best-effort: a failing sink is logged and swallowed, never propagated. A broadcast must never
  fail or roll back a KB write that already happened.
- A nil/empty `Multi` is a no-op.

- [ ] **Step 2: Run test to verify it fails**

- [ ] **Step 3: Implement**

Add `providers.KBUpdate` (transport and thread root of origin, route, entry title, URL, author,
the recorded note text, timestamp) and `providers.KBUpdateNotifier` as an OPTIONAL interface.
**Do not widen `providers.Notifier`** — every existing sink would have to change, and the repo's
own pattern for "some sinks can do this" is the capability check.

Document on `KBUpdate` that `Note`, `Title` and `Author` are UNTRUSTED — redacted upstream, but
still model- or human-authored — so every implementer escapes them like any other untrusted text.

- [ ] **Step 4: Run tests and commit**

---

### Task 4: Announce a write through the notifier fan-out

**Files:** `internal/thread/responder.go`, `internal/thread/responder_test.go`

- [ ] **Step 1: Write the failing test**

- On a successful write, the responder emits one `KBUpdate` carrying Task 1's values.
- **Nothing is emitted when nothing landed** — throttled, capped, failed, or no note proposed.
  Assert each.
- A nil sink is off and must not panic (the package's nil-safe contract).
- The emit is **best-effort and must not affect the reply**: a sink that errors or blocks must
  not change what the human is told, nor delay it. Decide sync vs async deliberately and say why
  in the doc comment; if async, it must be bounded and drainable like `thread.Dispatcher`.
- The `KBUpdate` names its origin transport and thread root, so a consumer can tell where it
  came from.

- [ ] **Step 2: Run test to verify it fails**

- [ ] **Step 3: Implement**

`Responder` gains a sink field (nil ⇒ off). Keep `internal/thread` transport-agnostic: it emits
a `providers.KBUpdate`, it does not know what a room or a channel is.

**Bounding:** every emit follows a forge write, which `ForgeWrites` already caps globally per
hour, so the announcement rate is bounded by that ceiling and needs no second one. State this in
the doc comment — and if the analysis turns out to be wrong, add the ceiling instead of assuming.

- [ ] **Step 4: Run tests and commit**

---

### Task 5: Config and wiring

**Files:** `internal/config/config.go`, `internal/config/config_test.go`,
`internal/app/notify.go`, `internal/app/notify_test.go`, `internal/app/serve.go`,
`internal/app/serve_thread_wiring_test.go`

- [ ] **Step 1: Write the failing test**

- `notify.thread.announce_kb_updates` (bool, **default false**). Opt-in: it adds notification
  volume to channels that did not ask for it, and the thread reply is already the direct
  acknowledgement.
- Absent ⇒ off; explicitly false ⇒ off; true ⇒ the responder gets a real sink.
- **The wiring is pinned through the real construction path**, not by building the struct by
  hand. Extend `serve_thread_wiring_test.go`'s static guard — this series has four times shipped
  something green and inert, and this is another closure-assigned dependency.
- With the key on but no notifier configured, startup must not claim the feature is active.
  Consider a `*Warning` (the repo's convention, and `warnings_wired_test.go` will require it to
  be emitted).

- [ ] **Step 2: Run test to verify it fails**

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run tests and commit**

---

### Task 6: Implement the capability on the real sinks, and document it

**Files:** `internal/notify/slack.go`, `internal/notify/matrix.go`,
`internal/notify/webhook/`, `internal/notify/templated/`, their tests, `website/content/docs/`

- [ ] **Step 1: Write the failing test**

- Slack and Matrix render a KB-update announcement and **escape its untrusted spans** — assert
  `<!channel>` and `@room` are neutralised, the same guarantee PR3 established for replies. This
  is a NEW egress for model-authored text; it needs the guarantee from its first commit, not
  from a later review.
- Webhook/templated: either implement the capability or deliberately do not. Whichever you
  choose, a test states it — an unimplemented capability is a silent skip in `Multi`, which must
  be intentional rather than accidental.
- Docs: extend the "Four ways knowledge gets in" section (`concepts/reviewing-knowledge.md`) and
  the transport pages with the new key and what it broadcasts. Note that the announcement carries
  note content, so a private note reaches every configured sink — that is the operator's call to
  make knowingly.
- `internal/docsguard` covers any new default that appears in the docs.

- [ ] **Step 2: Run test to verify it fails**

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run full gate, build the site, commit**

Site build: `/usr/bin/hugo` (0.164.0 — the one on PATH is 0.139.0 and cannot build this site),
`--destination` to a scratch dir outside the repo, never edit `website/public/`.
