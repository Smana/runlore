// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Notifier delivers an investigation to a destination. Pluggable: Slack and
// Matrix first; PagerDuty and incident.io later. Several notifiers can be wired
// at once (e.g. chat for humans + an incident platform for the on-call record).
type Notifier interface {
	Deliver(ctx context.Context, inv Investigation) error
}

// ProgressUpdate is a lightweight interim status ping for a still-running
// investigation. It is NOT an Investigation (there are no findings yet) — it
// exists so a long (up to 20-step) investigation is not silent until the final
// message. Interim is model-derived text that may quote tool output; the loop
// redacts it (redact.Secrets) before it leaves, and notifiers must still escape
// it as untrusted like any other model text.
type ProgressUpdate struct {
	Title     string         // incident title (untrusted alert text)
	Step      int            // current step (1-based)
	MaxSteps  int            // step ceiling
	ToolsUsed map[string]int // investigation tool name → call count so far
	Interim   string         // model's latest interim assistant text (already secret-redacted), if any
}

// ProgressNotifier is an OPTIONAL capability a Notifier may implement to receive
// interim progress pings during a long investigation. It is separate from
// Notifier so a progress ping (not an Investigation) never widens the Notifier
// contract: the app type-asserts for it and wires progress delivery only to the
// notifiers that support it (Slack first; Matrix/webhook may no-op for now).
// Delivery of a progress ping is best-effort — a failure is logged and swallowed,
// never failing the investigation.
type ProgressNotifier interface {
	DeliverProgress(ctx context.Context, up ProgressUpdate) error
}

// LineNotifier is the optional capability of posting one plain line of text to the channel,
// with no investigation behind it: the Knowledge queue's "rewritten, here is the link".
// Implemented by the Slack notifiers and by the fan-out over them.
type LineNotifier interface {
	DeliverLine(ctx context.Context, text string) error
}

// ThreadNotifier is the optional capability of carrying a conversation back into
// a delivered notification's thread. A notifier that cannot — an incoming
// webhook, the generic HTTP sink — simply does not implement it, and thread
// interaction is unavailable on that transport. Same contract as an unset data
// source disabling its tool: no capability is ever faked.
type ThreadNotifier interface {
	Notifier
	// ReplyInThread posts text into the thread rooted at root, in channel. Both
	// handles are transport-specific opaque strings (Slack: thread_ts and a
	// channel id).
	ReplyInThread(ctx context.Context, root, channel, text string) error
	// Transport names the chat system this notifier replies on ("slack",
	// "matrix"). It is what lets a deployment running several transports route
	// each thread's reply back to the system the human is actually in — the
	// alternative, picking the first notifier that can reply, silently answers
	// one transport's threads on another.
	Transport() string
}

// KBRoute names how a knowledge-base write landed on the forge. The values
// mirror the labels the thread responder already reports on its
// ThreadNotesWritten counter, so an announcement and the metric describing the
// same write cannot disagree about which route it took.
type KBRoute string

// The routes a knowledge-base write can take.
const (
	// KBRouteComment added the note as a comment on a KB pull request SOMEONE
	// ELSE drafted — the curator's. There the note is review feedback on another
	// author's draft: a human reconciles it at merge time, and until they do it
	// is deliberately not part of the entry.
	KBRouteComment KBRoute = "comment"
	// KBRouteOpenPR opened a new standalone pull request for the note.
	KBRouteOpenPR KBRoute = "open_pr"
	// KBRouteAppend appended the note to the ENTRY FILE of a standalone note PR
	// RunLore itself opened earlier in the same thread. It is a route of its own
	// rather than a second spelling of KBRouteComment because the two land
	// somewhere materially different: what this route writes is inside the entry
	// the catalog gains when the PR merges, and what a comment writes is not.
	KBRouteAppend KBRoute = "append"
)

// KBDelivery says WHERE a KBUpdate announcement is to be delivered. It is set
// once from configuration (config.AnnounceMode) and read by every sink.
//
// # A sink that is not the originating transport falls back to the channel
//
// Only ONE sink can ever deliver into the thread a note was typed in: the
// transport that thread lives on. A Matrix room cannot reply into a Slack
// thread, an incoming-webhook Slack cannot reply into any thread, and an HTTP
// webhook has no thread at all. The choice for those sinks is fall back to the
// channel, or skip them; this contract is FALL BACK, and the sinks that cannot
// thread-route are the reason.
//
// Skipping would make thread-routing silently un-configure an operator's other
// sinks. The echo KBDeliverThread exists to remove is specific to the
// originating transport, where the thread already lives inside the channel the
// announcement would post to; a Matrix room that never saw the Slack thread has
// no echo to remove, and the announcement is the only way it learns anything at
// all. Dropping it there would answer "stop repeating yourself in one channel"
// with "stop telling the other rooms", which is a different feature and a worse
// one. The same argument holds harder for a webhook sink feeding an index or an
// incident tool: it wants the record either way.
//
// The fallback is also what makes a missing handle safe. A KBUpdate whose Root
// or Channel is empty — a thread context rebuilt after a restart, an origin that
// was never a thread — cannot be replied into by anyone, and the write it
// describes has already landed. Falling back to the channel reports it; skipping
// would lose an announcement for a write that really happened, which is the
// failure this whole path is built to avoid.
type KBDelivery string

// Where an announcement lands.
const (
	// KBDeliverChannel posts to each sink's own configured channel or room and
	// nowhere else.
	//
	// It is the ZERO VALUE deliberately: a KBUpdate built without deciding
	// behaves exactly as every announcement did before routing existed, so no
	// producer that predates this field can change destination by omission.
	KBDeliverChannel KBDelivery = ""
	// KBDeliverThread delivers into the originating thread where the sink can
	// (see above), and to the sink's channel where it cannot.
	KBDeliverThread KBDelivery = "thread"
	// KBDeliverBoth delivers into the originating thread AND to every sink's
	// channel.
	KBDeliverBoth KBDelivery = "both"
)

// IntoThread reports whether this delivery wants the originating thread. It does
// NOT report that a given sink can honour that — only the sink knows whether it
// is the originating transport and holds both handles.
func (d KBDelivery) IntoThread() bool { return d == KBDeliverThread || d == KBDeliverBoth }

// KBUpdate announces that a knowledge-base write ALREADY LANDED on the forge. It
// is emitted after the fact and describes what was written — it is never a
// request to write, and a consumer failing to deliver it changes nothing about
// the entry, which is already on the forge.
//
// It names its origin (Transport + Root + Channel) so a consumer can tell which
// thread produced it — and so the transport that already replied in that thread
// can avoid announcing the same write to the same people twice, or deliver into
// that thread rather than beside it (see Delivery).
//
// It also names WHO WROTE the note as distinct from whose message produced it:
// on the freeform route RunLore's own chat model drafted the text and the human
// only prompted it. See ModelDrafted — a consumer that renders Author without
// consulting it will attribute model prose to a named engineer.
//
// # Untrusted fields
//
// Note, Title and Author are UNTRUSTED. They are secret-redacted upstream
// (redact.Secrets) and length-capped, but they remain model- or human-authored
// text: on the model-drafted route RunLore's own chat model wrote Note, and
// Author is whatever the chat transport reported. Every implementer MUST escape
// them with its transport's escaper before rendering — unescaped they can inject
// links and mass-ping a channel (Slack <!channel>, Matrix @room), the same
// hazard ProgressUpdate.Interim carries.
//
// URL, Root and Channel are untrusted for the same reason at one remove: they
// come back from the forge and from the chat transport rather than from RunLore,
// which is why the thread reply already wraps the URL in thread.Untrusted.
// Transport, Route, PR, At and Delivery are RunLore's own values and need no
// escaping.
type KBUpdate struct {
	// Transport is the chat system the note came from ("slack", "matrix"),
	// matching ThreadNotifier.Transport; "" when the write had no thread origin.
	Transport string
	// Root is the opaque thread-root handle on that transport (Slack: thread_ts).
	Root string
	// Channel is where that thread lives (Slack: channel id; Matrix: room id) —
	// the second half of the handle ReplyInThread needs, since a thread root
	// alone does not say which channel to post it in. "" when the write had no
	// thread origin, or when the origin was rebuilt without one.
	Channel string
	// Delivery says where this announcement is to be delivered. It is a routing
	// instruction rather than a fact about the write, and the only field here
	// that is: everything else describes what landed on the forge.
	Delivery KBDelivery
	// Route is how the write landed (comment on an open PR, or a new PR).
	Route KBRoute
	// PR is the forge pull/merge request number the knowledge landed in; 0 when
	// the forge URL did not name one.
	PR int
	// URL is the forge URL of that pull request.
	URL string
	// Title is the knowledge entry's title. Set on the KBRouteOpenPR route,
	// which generates one; empty on KBRouteComment, which adds to an entry that
	// was already titled. UNTRUSTED.
	Title string
	// Author is the human whose thread message produced the note, as the chat
	// transport reported it. Provenance, not proof of identity. UNTRUSTED.
	//
	// It names whose message PRODUCED the note, which is not the same as who
	// wrote its words — see ModelDrafted, which every renderer must consult
	// before presenting Note as this person's own.
	Author string
	// ModelDrafted reports that RunLore's own chat model wrote Note from Author's
	// message, rather than Author having typed it after an explicit "note:".
	//
	// It is the announcement's half of a distinction every other surface already
	// makes, and the reason it has to exist HERE rather than be inferred: a sink
	// receives this struct and nothing else. Without it the event carried
	// {author: "alice", note: "<the model's text>"} with no way to tell the two
	// routes apart, so a chat sink rendered "By alice in a slack thread" over
	// prose alice never wrote and a webhook receiver stored it as her statement.
	// The knowledge base's merge gate reads exactly that signal — a human
	// attestation is what a reviewer weighs a note by — so filing model prose
	// under a named engineer is the specific failure the thread reply's
	// openedWith, NoteBody's "@alice did not write it" heading and
	// conceptDescription's leading provenance clause were each written to close.
	//
	// TRUSTED: RunLore sets it from the note's own provenance (thread.Note.
	// DraftedFrom), it is never reported by a chat system, and it is a boolean
	// rather than text, so no renderer escapes it. It is nonetheless the field
	// most likely to be forgotten by a renderer, which is why the announcement
	// surfaces have tests asserting the two routes cannot render identically.
	ModelDrafted bool
	// Note is the note text AS WRITTEN to the forge — post-redaction and
	// post-cap, never the caller's raw input, so an announcement cannot leak
	// what the entry itself masked. UNTRUSTED.
	Note string
	// At is when the write landed.
	At time.Time
}

// KBUpdateNotifier is an OPTIONAL capability a Notifier may implement to receive
// KBUpdate announcements, so a knowledge-base write reaches every configured
// destination rather than only the thread that produced it.
//
// It is separate from Notifier — and, unlike ThreadNotifier, does not embed it —
// for the same reason ProgressNotifier is: a KB-update announcement is not an
// Investigation, and widening Notifier would force every existing sink to grow a
// method it has no use for. Consumers type-assert for it (see notify.Multi) and
// skip the sinks that do not implement it.
//
// Delivery is best-effort by contract: a failure is logged and swallowed, never
// propagated. The write being announced has already landed on the forge, so a
// broadcast must never be able to report it as failed or roll it back.
type KBUpdateNotifier interface {
	DeliverKBUpdate(ctx context.Context, up KBUpdate) error
}

// CurationForge is the forge surface the curator's file-time gate needs: open a
// drafted PR, list open KB PRs (dedup), and comment to coalesce duplicates.
type CurationForge interface {
	OpenPR(ctx context.Context, entry KBEntry) (Ref, error)
	ListPRsByLabel(ctx context.Context, label string) ([]CuratedIssue, error)
	// CommentOnPR posts a comment on the PULL REQUEST / MERGE REQUEST numbered
	// `number`. See the scoped-comment note on ReinvestForge.CommentOnIssue for
	// why the artifact kind is in the method name rather than left to the forge.
	CommentOnPR(ctx context.Context, number int, body string) error
}

// CuratedIssue is a minimal view of a curated KB issue, used by the re-investigate
// loop to re-run and post results back.
type CuratedIssue struct {
	Number    int
	URL       string // the web URL; set by the GitHub forge only
	Title     string
	Body      string
	Labels    []string
	UpdatedAt time.Time // forge last-update time; used by the curate lifecycle sweep
	CreatedAt time.Time // forge creation time; the Knowledge queue compares it with its last rewrite
	MergedAt  time.Time // zero unless the forge reported the merge time (the merged-PR search does)
}

// ReinvestForge lists curated issues flagged for re-investigation and posts results
// back to them. RunLore polls the forge (outbound) — it receives no inbound GitHub
// webhooks — so a human checking the "reinvestigate" label triggers a fresh run.
type ReinvestForge interface {
	// ListIssuesByLabel returns open issues carrying the given label.
	ListIssuesByLabel(ctx context.Context, label string) ([]CuratedIssue, error)
	// CommentOnIssue posts a comment on the ISSUE numbered `number`.
	//
	// The artifact kind is part of the method NAME, not a runtime guess by the
	// forge, because on GitLab a bare number does not identify one artifact:
	// merge requests and issues have INDEPENDENT iid sequences, both starting at
	// 1, so in a busy KB project issue #3 and merge request !3 both exist and are
	// unrelated. A forge that had to infer the kind (say, "try MRs first, fall
	// back to issues on 404") would post the re-investigation findings onto a
	// random merge request, get a 200 back, and leave no trace of the misroute.
	// GitHub has no such ambiguity — issues and PRs share one number sequence and
	// one comments endpoint — so both of its scoped methods are the same call;
	// the split exists so the COMPILER, not a code reviewer, enforces the
	// distinction on the forges where it is load-bearing.
	CommentOnIssue(ctx context.Context, number int, body string) error
	// ReplaceLabel removes one label and adds another (lifecycle transition);
	// either side may be empty to only add or only remove.
	ReplaceLabel(ctx context.Context, number int, remove, add string) error
}

// KBEntry is an OKF knowledge entry the curator drafts from an investigation.
type KBEntry struct {
	Type        string // OKF type, one of the validator vocabulary: Incident | Playbook | Concept
	Title       string
	Description string
	Resource    string
	// AlertResource is the resource the originating ALERT fired on, when it differs
	// from Resource (the fault locus). Recall matches by alert resource; without this
	// an entry whose fault sits deeper than its alert is permanently unrecallable.
	AlertResource string
	Tags          []string
	// ExtraLabels are additional FORGE labels (GitHub/GitLab issue/PR labels)
	// OpenPR appends to its standard lifecycle labels ("runlore", "triggered").
	// Unlike Tags above — which renders into the committed OKF entry's
	// frontmatter and never reaches the forge — ExtraLabels never renders into
	// the entry file; it exists purely so a later pass can recognise and
	// exclude specific PRs from auto-closing (e.g. a standalone operator note
	// — see internal/curate's isOperatorNote). Additive only: OpenPR must
	// APPEND these alongside the lifecycle labels, never replace them with it.
	// Empty/nil for an ordinary curated finding.
	ExtraLabels []string
	Body        string   // markdown
	Fingerprint string   // deterministic dedup fingerprint (see curator.DupFingerprint)
	Confidence  float64  // overall investigation confidence; queryable extension frontmatter (0 = unset)
	Provenance  []string // distinct causing-change refs; queryable extension frontmatter
	// Origin is who wrote the entry ("runlore"); queryable extension frontmatter.
	Origin string
	// Resolution is the verified fix; okf.Render writes it for a "resolved" verdict only.
	Resolution *AppliedResolution
	// Verdict and EvidenceLine render in the PR body review card only, never in
	// the committed entry.
	Verdict      string
	EvidenceLine string
	// Reviewer context, rendered in the PR BODY only — never in the committed
	// entry file (renderEntry ignores these), so the catalog and validator are
	// untouched. Related is the draft-time BM25 neighborhood; the recurrence
	// facts mirror Investigation.Occurrences/PrevCuratedURL.
	Related        []RelatedEntry
	Occurrences    int
	PrevCuratedURL string
	// SuspectedDuplicate is the catalog entry a dedup decider flagged as a probable
	// (but not confident-enough-to-skip) duplicate. Review metadata, rendered into
	// the PR/MR description only — like Related, never into Body — because Body is
	// committed into the catalog file (okf.Render) and a merged entry must not carry
	// a stale review note a human already decided was a false positive. Nil unless
	// the annotate tier fired.
	SuspectedDuplicate *RelatedEntry
}

// RelatedEntry is a nearby catalog entry surfaced to the KB PR reviewer so
// "is this a duplicate / what do we already know?" is answerable in the PR.
type RelatedEntry struct {
	Path     string // bundle-relative entry path (the forge renders the web link)
	Title    string
	Resource string  // affected resource, when the entry names one
	Score    float64 // BM25 score at draft time (corpus-relative — a hint, not a ranking guarantee)
}

// Ref is a URL handle to a created issue or PR.
type Ref struct{ URL string }

// CommitFile describes one file modified by a single Git commit.
type CommitFile struct {
	Path         string
	Status       string // "added" | "modified" | "removed" | "renamed"
	PreviousPath string // populated when Status == "renamed"
}

// CommitDetail describes a single Git commit fetched from a forge API.
type CommitDetail struct {
	SHA     string
	Parents []string
	Message string
	Files   []CommitFile
}

// RevertFileAction describes one atomic file mutation in a deterministic revert commit.
type RevertFileAction struct {
	Path    string
	Delete  bool   // true => delete Path; false => restore Content at Path
	Content []byte // exact parent blob bytes when Delete is false
}

// RevertPRRequest carries the parameters to atomically create a revert branch,
// single inverse commit, and Draft PR/MR on a forge.
type RevertPRRequest struct {
	Repo          string
	BaseBranch    string
	BaseSHA       string
	HeadBranch    string
	CommitMessage string
	Title         string
	Body          string
	Files         []RevertFileAction
}

// RevertForge is the minimal forge API surface required by the pure-Go
// deterministic revert_pr executor. Implemented by both GitHub and GitLab.
type RevertForge interface {
	DefaultBranch(ctx context.Context, repo string) (branch, headSHA string, err error)
	CommitDetail(ctx context.Context, repo, sha string) (CommitDetail, error)
	CompareChangedFiles(ctx context.Context, repo, baseSHA, headSHA string) ([]string, error)
	FileBlob(ctx context.Context, repo, ref, path string) ([]byte, bool, error)
	FindOpenPRByHead(ctx context.Context, repo, headBranch string) (url string, number int, found bool, err error)
	CreateRevertDraftPR(ctx context.Context, req RevertPRRequest) (url string, number int, err error)
}

// PossibleDuplicateHeading opens the section a forge writes into a KB PR's body when a
// dedup decider flags the finding (KBEntry.SuspectedDuplicate). The curate Queue reads it
// as a refusal, so a PR carrying it never gets ready-to-merge on evidence alone.
const PossibleDuplicateHeading = "## Possible duplicate"

// cardEvidencePrefix opens the evidence line a forge writes right after the card marker.
const cardEvidencePrefix = "\n\n**Evidence:** "

// ParseCardEvidence reads the evidence line of a PR body: the one that follows the LAST card
// marker directly, which is where RunLore writes it. A body with no evidence there returns "".
// A line elsewhere is not RunLore's: a newline in a model-written related-entry resource can
// forge a "**Evidence:**" line anywhere before the marker, so the last such line is no proof.
func ParseCardEvidence(body string) string {
	i := strings.LastIndex(body, cardMarkerPrefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(cardMarkerPrefix):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return ""
	}
	rest = rest[j+len(" -->"):]
	if !strings.HasPrefix(rest, cardEvidencePrefix) {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimPrefix(rest, cardEvidencePrefix), "\n")
	return strings.TrimSpace(line)
}

const fingerprintMarkerPrefix = "<!-- runlore-fingerprint: "

// FingerprintMarker renders a hidden PR-body marker carrying the dedup fingerprint,
// so an open PR's fingerprint is recoverable from the PR listing without fetching
// file contents. It returns "" for an empty fingerprint so callers may append it
// unconditionally.
func FingerprintMarker(fp string) string {
	if fp == "" {
		return ""
	}
	return fingerprintMarkerPrefix + fp + " -->"
}

// ParseFingerprintMarker extracts the fingerprint from a PR body, or "" if absent.
func ParseFingerprintMarker(body string) string {
	i := strings.Index(body, fingerprintMarkerPrefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(fingerprintMarkerPrefix):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

const cardMarkerPrefix = "<!-- runlore:card verdict="

// CardMarker renders the hidden PR-body review card marker. The verdict is
// checked against ValidVerdict (the text is model-influenced); anything else is
// written as "unknown" so it cannot break out of the comment.
func CardMarker(verdict string, confidence float64) string {
	if !ValidVerdict(Verdict(verdict)) {
		verdict = "unknown"
	}
	return fmt.Sprintf("%s%s confidence=%.2f -->", cardMarkerPrefix, verdict, confidence)
}

// ParseCardMarker reads the review card marker back from a PR body.
func ParseCardMarker(body string) (verdict string, confidence float64, ok bool) {
	// The LAST marker: RunLore writes its own after every untrusted segment.
	i := strings.LastIndex(body, cardMarkerPrefix)
	if i < 0 {
		return "", 0, false
	}
	rest := body[i+len(cardMarkerPrefix):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return "", 0, false
	}
	v, c, found := strings.Cut(rest[:j], " confidence=")
	if !found {
		return "", 0, false
	}
	f, err := strconv.ParseFloat(c, 64)
	if err != nil || math.IsNaN(f) || f < 0 || f > 1 || (v != "unknown" && !ValidVerdict(Verdict(v))) {
		return "", 0, false
	}
	return v, f, true
}
