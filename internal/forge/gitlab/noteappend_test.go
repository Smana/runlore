// SPDX-License-Identifier: Apache-2.0

package gitlab

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

const noteEntry = "---\ntype: Concept\ntitle: Operator note: OOM\n---\n\n### 📝 Operator note\n\nthe first note\n"

// emptyEntry asks the stub to serve a ZERO-BYTE file; "" means "serve the
// default entry", so the two need different spellings.
const emptyEntry = "\x00"

// stubMR is a configurable GitLab stub serving ONE note merge request, mirroring
// github's stubPR knob for knob so both clients' guards are exercised the same
// way rather than each drifting to whatever its own tests happened to cover.
type stubMR struct {
	state    string // "opened" unless set
	branch   string // source_branch; "runlore/kb-oom-1" unless set
	srcProj  int64  // source_project_id; defaults equal to target (same-project MR)
	changed  []string
	entry    string
	encoding string // "base64" unless set

	// overflow is GitLab's truncation signal on /changes: true when the diff
	// limits (max files, lines, patch bytes) cut the listing being returned.
	overflow bool
	// changesCount overrides changes_count, which defaults to agreeing with the
	// changes array. "-" serves it EMPTY, which is what GitLab answers while a
	// freshly created merge request's diff is still being computed.
	changesCount string
	// noProjects omits source_project_id / target_project_id entirely, the shape
	// in which a zero-value id can reach the fork guard.
	noProjects bool
	// noLastCommitID serves the entry with an empty last_commit_id, which is the
	// only input that can turn the Commits API write into an unconditional
	// overwrite.
	noLastCommitID bool

	// commitStatus, when non-zero, is the status the Commits API answers with —
	// 400 is what a stale last_commit_id gets.
	commitStatus int
	// commitLands models a lost RESPONSE to a commit GitLab already applied: the
	// content is updated, then the call fails. The only way to reach appendLanded.
	commitLands bool

	mu         sync.Mutex
	content    string
	lastCommit string
	commits    []map[string]any
}

func (s *stubMR) start(t *testing.T) *Client {
	t.Helper()
	if s.content == emptyEntry {
		s.content = ""
	} else {
		s.content = cmp.Or(s.content, noteEntry)
	}
	if !s.noLastCommitID {
		s.lastCommit = "commit0"
	}
	if s.changed == nil {
		s.changed = []string{"index.md", "log.md", "concepts/oom-1.md"}
	}
	s.entry = cmp.Or(s.entry, "concepts/oom-1.md")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.HasSuffix(path, "/merge_requests/84/changes"):
			changes := make([]map[string]string, 0, len(s.changed))
			for _, f := range s.changed {
				changes = append(changes, map[string]string{"new_path": f})
			}
			src := s.srcProj
			if src == 0 {
				src = 7
			}
			count := cmp.Or(s.changesCount, strconv.Itoa(len(s.changed)))
			if count == "-" {
				count = ""
			}
			payload := map[string]any{
				"state": cmp.Or(s.state, "opened"), "source_branch": cmp.Or(s.branch, "runlore/kb-oom-1"),
				"source_project_id": src, "target_project_id": 7, "changes": changes,
				"changes_count": count, "overflow": s.overflow,
			}
			if s.noProjects {
				delete(payload, "source_project_id")
				delete(payload, "target_project_id")
			}
			_ = json.NewEncoder(w).Encode(payload)
		case strings.Contains(path, "/repository/files/"):
			s.mu.Lock()
			defer s.mu.Unlock()
			// The file path must arrive URL-encoded as ONE segment and be read at
			// the merge request's own ref — GitLab 404s an unencoded "/" in a way
			// that looks like a permissions problem, so this is asserted rather
			// than assumed. Note the absence of a "/raw" suffix: the NON-raw
			// endpoint is the one carrying last_commit_id, which is why the client
			// stopped using /raw here.
			want := "/repository/files/" + url.PathEscape(s.entry)
			if !strings.HasSuffix(path, want) || r.URL.Query().Get("ref") != cmp.Or(s.branch, "runlore/kb-oom-1") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			enc := cmp.Or(s.encoding, "base64")
			content := base64.StdEncoding.EncodeToString([]byte(s.content))
			if enc != "base64" {
				content = ""
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"encoding": enc, "content": content, "last_commit_id": s.lastCommit,
			})
		case strings.HasSuffix(path, "/repository/commits"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.commits = append(s.commits, body)
			if s.commitStatus == 0 {
				if actions, ok := body["actions"].([]any); ok && len(actions) == 1 {
					if a, ok := actions[0].(map[string]any); ok {
						s.content, _ = a["content"].(string)
						s.lastCommit += "x"
					}
				}
			}
			if s.commitStatus != 0 || s.commitLands {
				status := s.commitStatus
				if status == 0 {
					status = http.StatusBadGateway
				}
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(`{"id":"abc123"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "o/r", "main", staticToken("tok"))
}

func (s *stubMR) writes() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.commits...)
}

func (s *stubMR) entryContent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.content
}

// action returns the single file action of commit i.
func (s *stubMR) action(t *testing.T, i int) map[string]any {
	t.Helper()
	commits := s.writes()
	if len(commits) <= i {
		t.Fatalf("commits = %d, want more than %d", len(commits), i)
	}
	actions, _ := commits[i]["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v, want exactly one (the entry update)", commits[i]["actions"])
	}
	a, _ := actions[0].(map[string]any)
	return a
}

// TestAppendToEntryOnPRUpdatesTheEntryOnTheSourceBranch is the GitLab half of
// issue #493: the note reaches the ENTRY FILE — on the merge request's own
// source branch, keeping every note already there — rather than a discussion
// note the catalog never indexes.
func TestAppendToEntryOnPRUpdatesTheEntryOnTheSourceBranch(t *testing.T) {
	s := &stubMR{}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "### 📝 Operator note\n\nthe second note", "k1"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	if got := s.writes()[0]["branch"]; got != "runlore/kb-oom-1" {
		t.Errorf("branch = %v, want the merge request's own source branch", got)
	}
	a := s.action(t, 0)
	if a["action"] != "update" {
		t.Errorf("action = %v, want update — a create would 400 on an existing file", a["action"])
	}
	if a["file_path"] != "concepts/oom-1.md" {
		t.Errorf("file_path = %v, want the entry — index.md and log.md are bundle upkeep", a["file_path"])
	}
	body, _ := a["content"].(string)
	if !strings.Contains(body, "the first note") || !strings.Contains(body, "the second note") {
		t.Errorf("the entry must keep every note, got:\n%s", body)
	}
	if strings.Index(body, "the first note") > strings.Index(body, "the second note") {
		t.Errorf("notes must accumulate in order, got:\n%s", body)
	}
}

// TestAppendToEntryOnPRSendsLastCommitID is the concurrency control, and the
// property this client CLAIMED to mirror from GitHub while not holding it.
//
// The Commits API overwrites unconditionally unless the update action carries
// last_commit_id. Without it this read-modify-write silently reverts anyone who
// committed to the entry in between — a reviewer fixing a typo in the web IDE
// has their change erased by the next note in the thread, with nothing anywhere
// reporting it. GitHub gets the same property free from the blob sha its
// contents API hands back; here it has to be asked for.
func TestAppendToEntryOnPRSendsLastCommitID(t *testing.T) {
	s := &stubMR{}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	if got := s.action(t, 0)["last_commit_id"]; got != "commit0" {
		t.Errorf("last_commit_id = %v, want the id just read — without it a racing writer is silently reverted", got)
	}
}

// TestAppendToEntryOnPRPropagatesAConflict: GitLab answers a stale
// last_commit_id with 400. The racing writer keeps their commit, and this note
// must surface as an error so the caller degrades to a comment rather than
// losing the human's words.
func TestAppendToEntryOnPRPropagatesAConflict(t *testing.T) {
	s := &stubMR{commitStatus: http.StatusBadRequest}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err == nil {
		t.Fatal("want an error so the caller falls back to a comment")
	}
	if strings.Contains(s.entryContent(), "the second note") {
		t.Errorf("the entry must be untouched after a conflict:\n%s", s.entryContent())
	}
}

// TestAppendToEntryOnPRRefusesAnUnreadableEntry: a read that reports success
// while handing back nothing would make okf.AppendBlock replace the entry with
// the single note being appended to it. See the GitHub sibling for the full
// account of how that shape arises.
func TestAppendToEntryOnPRRefusesAnUnreadableEntry(t *testing.T) {
	s := &stubMR{encoding: "none"}
	c := s.start(t)
	err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1")
	if err == nil {
		t.Fatal("want an error: a read that returns nothing must never be treated as an empty entry")
	}
	// Named, not merely non-nil. An unreadable body decodes to zero bytes, which
	// the frontmatter guard behind this one also refuses — so without naming the
	// encoding this test scores that guard and would keep passing with the read's
	// own check deleted.
	if !strings.Contains(err.Error(), "encoding") {
		t.Errorf("err = %v, want the read to refuse the encoding rather than lean on the guard behind it", err)
	}
	if commits := s.writes(); len(commits) != 0 {
		t.Fatalf("the entry was rewritten from an unreadable read: %+v", commits)
	}
}

// TestAppendToEntryOnPRRefusesAFileThatIsNotAnEntry is the independent half of
// the same guard: whatever the read yielded, it is written back only if it reads
// as an OKF entry. It also covers what okf.EntryFile cannot promise — that the
// one non-reserved .md in a request is the catalog entry.
func TestAppendToEntryOnPRRefusesAFileThatIsNotAnEntry(t *testing.T) {
	for name, content := range map[string]string{
		"empty file":     emptyEntry,
		"no frontmatter": "# Notes\n\nsome markdown a reviewer added\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := &stubMR{content: content}
			c := s.start(t)
			if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
				t.Fatal("want an error rather than a rewrite of a file that is not an entry")
			}
			if commits := s.writes(); len(commits) != 0 {
				t.Fatalf("rewrote a non-entry: %+v", commits)
			}
		})
	}
}

// TestAppendToEntryOnPRIsIdempotent: a replayed chat delivery must not append
// the same note twice — unlike a duplicate comment, a duplicate append is
// permanent catalog content that recall then serves twice.
func TestAppendToEntryOnPRIsIdempotent(t *testing.T) {
	s := &stubMR{}
	c := s.start(t)
	for i := range 3 {
		if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if commits := s.writes(); len(commits) != 1 {
		t.Fatalf("commits = %d, want 1 — a replayed delivery must not append the note again", len(commits))
	}
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the third note", "k2"); err != nil {
		t.Fatalf("a different note must still be appended: %v", err)
	}
	if commits := s.writes(); len(commits) != 2 {
		t.Fatalf("commits = %d, want 2 — a different key is a different note", len(commits))
	}
}

// TestAppendToEntryOnPRReportsSuccessWhenTheWriteLandedButTheResponseDidNot:
// c.do errors for every failed round trip, including a response lost after
// GitLab applied the commit. Reported as a failure, the caller files the note
// again as a comment and labels the write on the COMMENT route — the wrong
// answer to the one question that label exists to answer.
func TestAppendToEntryOnPRReportsSuccessWhenTheWriteLandedButTheResponseDidNot(t *testing.T) {
	s := &stubMR{commitLands: true}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err != nil {
		t.Fatalf("the commit landed; reporting failure sends the caller to double-write it as a comment: %v", err)
	}
	if !strings.Contains(s.entryContent(), "the second note") {
		t.Fatalf("test is not exercising the case — the write did not land:\n%s", s.entryContent())
	}
}

// TestAppendToEntryOnPRRefusesAnMRThatIsNoLongerOpen closes the window between
// the caller's open-check and this write, and NAMES the case so the caller opens
// a fresh entry rather than commenting onto a finished merge request.
func TestAppendToEntryOnPRRefusesAnMRThatIsNoLongerOpen(t *testing.T) {
	for _, state := range []string{"closed", "merged", "locked"} {
		t.Run("state="+state, func(t *testing.T) {
			s := &stubMR{state: state}
			c := s.start(t)
			err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1")
			if !errors.Is(err, providers.ErrPRNotOpen) {
				t.Fatalf("err = %v, want ErrPRNotOpen", err)
			}
			if commits := s.writes(); len(commits) != 0 {
				t.Errorf("committed onto a %s merge request: %+v", state, commits)
			}
		})
	}
}

// TestAppendToEntryOnPRRefusesAForkSourceBranch is the GitLab spelling of
// GitHub's fork guard: source_branch is a bare branch name, so a fork's "main"
// would be committed onto the configured project's own main. The two project ids
// settle it with no extra lookup.
func TestAppendToEntryOnPRRefusesAForkSourceBranch(t *testing.T) {
	s := &stubMR{srcProj: 99, branch: "main"}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
		t.Fatal("want a refusal rather than a commit onto the target project's branch")
	}
	if commits := s.writes(); len(commits) != 0 {
		t.Errorf("committed from a fork merge request: %+v", commits)
	}
}

// TestAppendToEntryOnPRNeutralizesImages keeps the appended block at the defusal
// level renderEntry gives a first draft. Defence in depth: thread.NoteBody has
// already rewritten "![" in the note TEXT, so what this actually covers is the
// identity fields interpolated around it (author, thread title, which go through
// noteField rather than the markdown defusals) and any future caller handing
// this a block NoteBody did not build.
func TestAppendToEntryOnPRNeutralizesImages(t *testing.T) {
	s := &stubMR{}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "look ![px](https://evil.example/px.gif)", "k1"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	body, _ := s.action(t, 0)["content"].(string)
	if strings.Contains(body, "![px](") || strings.Contains(body, "evil.example") {
		t.Errorf("image markdown reached the entry file:\n%s", body)
	}
	if !strings.Contains(body, "`[image: px]`") {
		t.Errorf("want the defused label renderEntry would have produced:\n%s", body)
	}
}

// TestAppendToEntryOnPRMarkerIsInvisibleAndSingular: exactly one HTML-comment
// marker per note, so it never renders in the entry the catalog serves.
func TestAppendToEntryOnPRMarkerIsInvisibleAndSingular(t *testing.T) {
	s := &stubMR{}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	body, _ := s.action(t, 0)["content"].(string)
	if n := strings.Count(body, okf.NoteMarker("k1")); n != 1 {
		t.Errorf("marker appears %d times, want 1:\n%s", n, body)
	}
}

// TestAppendToEntryOnPRRefusesWhenTheEntryIsAmbiguous: the next move is a commit
// into a merge request a human is reviewing, so an unclear target is an error,
// never a guess — the caller degrades to a comment on it.
func TestAppendToEntryOnPRRefusesWhenTheEntryIsAmbiguous(t *testing.T) {
	for name, changed := range map[string][]string{
		"two entries": {"concepts/a.md", "concepts/b.md"},
		"no entry":    {"index.md", "log.md"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &stubMR{changed: changed, entry: "concepts/a.md"}
			c := s.start(t)
			if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
				t.Fatal("want an error rather than a guess at which file to rewrite")
			}
			if commits := s.writes(); len(commits) != 0 {
				t.Errorf("committed %v despite an ambiguous entry", commits)
			}
		})
	}
}

// TestAppendToEntryOnPRRefusesWithoutASourceBranch: an MR payload with no
// source_branch must not become a commit onto the empty branch name, which
// GitLab would resolve to the project default.
//
// The payload is healthy in every OTHER respect — three changed paths naming one
// entry, matching changes_count, same-project ids — deliberately. An earlier
// version served no changes at all, so deleting the guard under test still
// produced an error (from okf.EntryFile, one line further down) and the test went
// on passing while pinning nothing.
func TestAppendToEntryOnPRRefusesWithoutASourceBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests/84/changes") {
			_, _ = w.Write([]byte(`{"state":"opened","source_project_id":7,"target_project_id":7,` +
				`"changes_count":"3","overflow":false,"changes":[{"new_path":"index.md"},` +
				`{"new_path":"log.md"},{"new_path":"concepts/oom-1.md"}]}`))
			return
		}
		t.Errorf("reached %s %s past the source-branch guard", r.Method, r.URL.EscapedPath())
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1")
	if err == nil {
		t.Fatal("want an error when the merge request names no source branch")
	}
	if !strings.Contains(err.Error(), "source branch") {
		t.Errorf("err = %v, want the source-branch refusal rather than whatever a later guard happened to catch", err)
	}
}

// TestAppendToEntryOnPRRefusesATruncatedChangesListing is the GitLab spelling of
// github's full-page refusal, and the guard whose absence made this client's
// "every guard the sibling has" claim false.
//
// `/changes` takes no page parameter, so there is no full page to notice. GitLab
// truncates on DIFF SIZE instead and says so in `overflow`; changes_count is the
// count it computed for itself, capped to the string "1000+" past the file
// limit. Each of those is a listing okf.EntryFile would resolve just as cleanly
// to the wrong file — the changed paths here are the healthy three, so nothing
// but the truncation signal can produce the refusal.
func TestAppendToEntryOnPRRefusesATruncatedChangesListing(t *testing.T) {
	for name, s := range map[string]*stubMR{
		"overflow":               {overflow: true},
		"count past the cap":     {changesCount: "1000+"},
		"count exceeds listing":  {changesCount: "40"},
		"count not yet computed": {changesCount: "-"},
	} {
		t.Run(name, func(t *testing.T) {
			c := s.start(t)
			if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
				t.Fatal("want a refusal: a truncated listing cannot be told from a complete one")
			}
			if commits := s.writes(); len(commits) != 0 {
				t.Errorf("committed from a truncated listing: %+v", commits)
			}
		})
	}
}

// TestAppendToEntryOnPRRefusesAnOversizedChangesListing is the other half of what
// github's full-page check refuses: a merge request this large is not a RunLore
// curation request (entry plus, at most, the reserved index.md and log.md),
// whatever GitLab reports about truncation.
//
// Reproduces the divergence directly: a hundred changed paths of which exactly
// one is a non-reserved .md is a listing okf.EntryFile resolves without
// complaint, so before this guard GitLab answered nil and committed into
// someone else's file inside a human's open merge request, while GitHub refused
// the identically shaped pull request.
func TestAppendToEntryOnPRRefusesAnOversizedChangesListing(t *testing.T) {
	changed := []string{"concepts/oom-1.md"}
	for i := range maxChangedPaths - 1 {
		changed = append(changed, fmt.Sprintf("chart/values-%d.yaml", i))
	}
	s := &stubMR{changed: changed}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
		t.Fatal("want a refusal: a merge request this size is not a curation merge request")
	}
	if commits := s.writes(); len(commits) != 0 {
		t.Errorf("committed from an oversized listing: %+v", commits)
	}
}

// TestAppendToEntryOnPRRefusesWithoutALastCommitID: the Commits API treats a
// missing last_commit_id as "overwrite unconditionally", so sending the action
// without it is not a weaker write, it is the read-modify-write this file exists
// to avoid — a reviewer's Web IDE edit reverted with nothing reporting it.
//
// GitHub's counterpart cannot reach that state: its contents API rejects a PUT
// over an existing file with no sha. GitLab's accepts it, so the refusal has to
// be spelled here.
func TestAppendToEntryOnPRRefusesWithoutALastCommitID(t *testing.T) {
	s := &stubMR{noLastCommitID: true}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err == nil {
		t.Fatal("want an error: an absent last_commit_id must not downgrade the write to an unconditional overwrite")
	}
	if commits := s.writes(); len(commits) != 0 {
		t.Fatalf("sent an unconditional overwrite: %+v", commits)
	}
}

// TestAppendToEntryOnPRRefusesAnMRWithoutProjectIDs: the fork guard compares two
// ids, so a response carrying NEITHER compares 0 against 0 and passes — the one
// input on which "equal ids" stops meaning "same-project merge request". GitHub's
// fork guard fails closed on its missing field (an empty full_name never equals
// the configured repo), so this one has to as well.
func TestAppendToEntryOnPRRefusesAnMRWithoutProjectIDs(t *testing.T) {
	s := &stubMR{noProjects: true, branch: "main"}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
		t.Fatal("want a refusal rather than reading two absent ids as a same-project merge request")
	}
	if commits := s.writes(); len(commits) != 0 {
		t.Errorf("committed onto %v without knowing which project the branch is in", commits)
	}
}
