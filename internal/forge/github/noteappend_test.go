// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

const noteEntry = "---\ntype: Concept\ntitle: Operator note: OOM\n---\n\n### 📝 Operator note\n\nthe first note\n"

// recordedPut is one contents-API PUT the client made.
type recordedPut struct{ path, branch, sha, content string }

// stubPR is a configurable GitHub stub serving ONE note pull request. Every
// field a guard in AppendToEntryOnPR reads is a knob, so each test breaks
// exactly one thing and the rest stays valid — the alternative, a per-test
// hand-rolled mux, hides which guard a test is actually exercising.
//
// Defaults describe the healthy case: an open PR whose head branch is in the
// configured repo, three changed files, and a well-formed entry.
type stubPR struct {
	state    string // "open" unless set
	branch   string // head ref; "runlore/kb-oom-1" unless set
	headRepo string // head.repo.full_name; "o/r" unless set. "-" omits head.repo entirely
	changed  []string
	entry    string // the path the contents API serves
	encoding string // "base64" unless set

	// putStatus, when non-zero, is the HTTP status the PUT answers with — 409 is
	// the sha-conflict a racing writer causes.
	putStatus int
	// putLands models the response being lost AFTER GitHub applied the write: the
	// content is updated, then the call fails. It is the only way to reach
	// appendLanded, and the case a fake that simply errors cannot express.
	putLands bool

	mu      sync.Mutex
	content string
	sha     string
	puts    []recordedPut
}

func (s *stubPR) or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// emptyEntry asks the stub to serve a ZERO-BYTE file. "" cannot say that: an
// unset content field means "serve the default entry", so the two need
// different spellings.
const emptyEntry = "\x00"

func (s *stubPR) start(t *testing.T) *Client {
	t.Helper()
	if s.content == emptyEntry {
		s.content = ""
	} else {
		s.content = s.or(s.content, noteEntry)
	}
	s.sha = "entrysha0"
	if s.changed == nil {
		s.changed = []string{"index.md", "log.md", "concepts/oom-1.md"}
	}
	s.entry = s.or(s.entry, "concepts/oom-1.md")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/84", func(w http.ResponseWriter, _ *http.Request) {
		head := `"head":{"ref":"` + s.or(s.branch, "runlore/kb-oom-1") + `"`
		if repo := s.or(s.headRepo, "o/r"); repo != "-" {
			head += `,"repo":{"full_name":"` + repo + `"}`
		}
		_, _ = w.Write([]byte(`{"state":"` + s.or(s.state, "open") + `",` + head + `}}`))
	})
	mux.HandleFunc("GET /repos/o/r/pulls/84/files", func(w http.ResponseWriter, _ *http.Request) {
		out := make([]map[string]string, 0, len(s.changed))
		for _, f := range s.changed {
			out = append(out, map[string]string{"filename": f})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.PathValue("path") != s.entry || r.URL.Query().Get("ref") != s.or(s.branch, "runlore/kb-oom-1") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		enc := s.or(s.encoding, "base64")
		body := map[string]string{"sha": s.sha, "encoding": enc, "content": base64.StdEncoding.EncodeToString([]byte(s.content))}
		if enc != "base64" {
			// What GitHub actually answers for a blob over 1 MB: 200, a real sha,
			// and NO content.
			body["content"] = ""
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Content, SHA, Branch string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := base64.StdEncoding.DecodeString(body.Content)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.puts = append(s.puts, recordedPut{path: r.PathValue("path"), branch: body.Branch, sha: body.SHA, content: string(raw)})
		if s.putLands {
			s.content, s.sha = string(raw), s.sha+"x"
		}
		if s.putStatus != 0 || s.putLands {
			status := s.putStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			w.WriteHeader(status)
			return
		}
		s.content, s.sha = string(raw), s.sha+"x"
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, "o", "r", "main", staticToken("tok"))
}

func (s *stubPR) writes() []recordedPut {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedPut(nil), s.puts...)
}

func (s *stubPR) entryContent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.content
}

// TestAppendToEntryOnPRCommitsOntoThePRsOwnBranch is the forge half of issue
// #493: the note has to end up in the ENTRY FILE the pull request carries, on
// that pull request's OWN branch, keeping everything already there.
//
// Four separate facts, because getting any one of them wrong loses or corrupts
// a human's note: the right file (not index.md or log.md), the right branch
// (base has no such file until merge), the blob sha (so a racing writer loses
// the PUT instead of silently clobbering), and the original content preserved
// ahead of the new block.
func TestAppendToEntryOnPRCommitsOntoThePRsOwnBranch(t *testing.T) {
	s := &stubPR{}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "### 📝 Operator note\n\nthe second note", "k1"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	puts := s.writes()
	if len(puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(puts))
	}
	if puts[0].path != "concepts/oom-1.md" {
		t.Errorf("committed to %q, want the entry file — index.md and log.md are bundle upkeep, not the entry", puts[0].path)
	}
	if puts[0].branch != "runlore/kb-oom-1" {
		t.Errorf("committed onto %q, want the pull request's own head branch", puts[0].branch)
	}
	if puts[0].sha != "entrysha0" {
		t.Errorf("sha = %q, want the blob sha just read — without it a racing writer is silently overwritten", puts[0].sha)
	}
	if !strings.Contains(puts[0].content, "the first note") || !strings.Contains(puts[0].content, "the second note") {
		t.Errorf("the entry must keep every note, got:\n%s", puts[0].content)
	}
	if strings.Index(puts[0].content, "the first note") > strings.Index(puts[0].content, "the second note") {
		t.Errorf("notes must accumulate in order, got:\n%s", puts[0].content)
	}
}

// TestAppendToEntryOnPRRefusesAnUnreadableEntry is the worst defect this file
// guards, and the one an earlier version shipped.
//
// GitHub answers the contents API for a blob over 1 MB with 200, a real blob
// sha, `"content": ""` and `"encoding": "none"`. base64-decoding that empty
// string SUCCEEDS, so a client that does not check the encoding gets zero bytes
// back and calls them the file. okf.AppendBlock returns the block ALONE when
// what it appends to is empty, and the PUT carries the correct sha — so the
// entry, its frontmatter and every earlier note are replaced by the newest note,
// the forge accepts it, and the human is told it worked. Only git history has
// the entry after that.
//
// An entry that grows by one rendered note per thread message can genuinely
// reach 1 MB: config.Validate rejects only a NEGATIVE max_note_bytes, so a
// configured 1 MiB crosses it on the second note, and even at defaults twenty
// notes at the worst-case rendered size land near it.
func TestAppendToEntryOnPRRefusesAnUnreadableEntry(t *testing.T) {
	s := &stubPR{encoding: "none"}
	c := s.start(t)
	err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1")
	if err == nil {
		t.Fatal("want an error: a read that returns nothing must never be treated as an empty entry")
	}
	// Name the ENCODING, not merely "something refused it". The frontmatter guard
	// one layer up rejects this same input, so without this the encoding check can
	// be deleted with the whole suite green — the two layers quietly stop being
	// independent, and nobody finds out until an unreadable read arrives that does
	// carry frontmatter.
	if !strings.Contains(err.Error(), "encoding") {
		t.Errorf("error = %v; want the ENCODING check to be what refused this. If the "+
			"frontmatter guard fired instead, the first layer is no longer pinned", err)
	}
	if puts := s.writes(); len(puts) != 0 {
		t.Fatalf("the entry was rewritten from an unreadable read: %+v", puts)
	}
	if got := s.entryContent(); got != noteEntry {
		t.Errorf("the entry changed:\n%s", got)
	}
}

// TestAppendToEntryOnPRRefusesAFileThatIsNotAnEntry is the independent half of
// the same guard, one layer up from the encoding check: whatever the read
// yielded, it is only written back if it reads as an OKF entry. It also covers
// what okf.EntryFile cannot promise — that the one non-reserved .md in a pull
// request is the catalog entry rather than some other markdown a reviewer
// touched.
func TestAppendToEntryOnPRRefusesAFileThatIsNotAnEntry(t *testing.T) {
	for name, content := range map[string]string{
		"empty file":      emptyEntry,
		"no frontmatter":  "# Notes\n\nsome markdown a reviewer added\n",
		"html leading in": "<!-- draft -->\n---\ntype: Concept\n---\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := &stubPR{content: content}
			c := s.start(t)
			if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
				t.Fatal("want an error rather than a rewrite of a file that is not an entry")
			}
			if puts := s.writes(); len(puts) != 0 {
				t.Fatalf("rewrote a non-entry: %+v", puts)
			}
		})
	}
}

// TestAppendToEntryOnPRIsIdempotent: the deliveries above this layer replay (a
// bounded per-process dedup set, wiped wholesale, that no restart survives), and
// a replayed append is permanent duplicate catalog content — unlike a replayed
// comment, which is visibly duplicated and dies at merge. The second call must
// therefore write nothing and still report success, because the note IS in the
// entry.
func TestAppendToEntryOnPRIsIdempotent(t *testing.T) {
	s := &stubPR{}
	c := s.start(t)
	for i := range 3 {
		if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if puts := s.writes(); len(puts) != 1 {
		t.Fatalf("puts = %d, want 1 — a replayed delivery must not append the note again", len(puts))
	}
	if got := strings.Count(s.entryContent(), "the second note"); got != 1 {
		t.Errorf("the note appears %d times in the entry, want 1:\n%s", got, s.entryContent())
	}
	// A DIFFERENT note must still land: the guard is per-note, not a lock on the
	// entry.
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the third note", "k2"); err != nil {
		t.Fatalf("a different note must still be appended: %v", err)
	}
	if puts := s.writes(); len(puts) != 2 {
		t.Fatalf("puts = %d, want 2 — a different key is a different note", len(puts))
	}
}

// TestAppendToEntryOnPRReportsSuccessWhenTheWriteLandedButTheResponseDidNot is
// the case a fake that merely returns an error cannot express.
//
// c.do reports an error for every failed round trip, which includes every way a
// RESPONSE can be lost after GitHub already applied the commit. Reported as a
// failure, the caller files the same note again as a comment — so the note
// exists twice — and, worse, reports the write on the COMMENT route, which is
// the label an operator reads to tell whether a note became knowledge or will be
// discarded at merge. It would say the wrong one on exactly the case it exists
// for. One re-read on the error path settles it.
func TestAppendToEntryOnPRReportsSuccessWhenTheWriteLandedButTheResponseDidNot(t *testing.T) {
	s := &stubPR{putLands: true}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err != nil {
		t.Fatalf("the commit landed; reporting failure sends the caller to double-write it as a comment: %v", err)
	}
	if !strings.Contains(s.entryContent(), "the second note") {
		t.Fatalf("test is not exercising the case — the write did not land:\n%s", s.entryContent())
	}
}

// TestAppendToEntryOnPRPropagatesAFailureThatDidNotLand is the other direction
// of the same check: when the write genuinely did not land, the error must
// survive so the caller degrades to a comment and the human's words are kept. A
// re-read that finds no marker must not be read as success.
func TestAppendToEntryOnPRPropagatesAFailureThatDidNotLand(t *testing.T) {
	s := &stubPR{putStatus: http.StatusConflict}
	c := s.start(t)
	err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1")
	if err == nil {
		t.Fatal("a 409 means a racing writer won and the note is NOT in the entry; want an error so the caller falls back")
	}
	if strings.Contains(s.entryContent(), "the second note") {
		t.Errorf("the entry must be untouched after a conflict:\n%s", s.entryContent())
	}
}

// TestAppendToEntryOnPRRefusesAPRThatIsNoLongerOpen closes the window between
// the caller's open-check and this write. Both outcomes past a merge are silent
// losses — a commit onto a merged PR's branch never reaches base, and the
// caller's comment fallback lands on a PR the catalog never indexes — so the
// case is NAMED, with providers.ErrPRNotOpen, and the caller opens a fresh entry
// instead.
func TestAppendToEntryOnPRRefusesAPRThatIsNoLongerOpen(t *testing.T) {
	for _, state := range []string{"closed", "merged"} {
		t.Run("state="+state, func(t *testing.T) {
			s := &stubPR{state: state}
			c := s.start(t)
			err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1")
			if !errors.Is(err, providers.ErrPRNotOpen) {
				t.Fatalf("err = %v, want ErrPRNotOpen — the caller must not degrade to commenting on a finished PR", err)
			}
			if puts := s.writes(); len(puts) != 0 {
				t.Errorf("committed onto a %s PR: %+v", s.state, puts)
			}
		})
	}
}

// TestAppendToEntryOnPRRefusesAHeadOutsideTheConfiguredRepo: head.ref is a bare
// branch name and every write here is addressed to the configured owner/repo, so
// a fork PR's head — commonly "main" — would be committed onto the BASE
// repository's main. Nothing reachable today opens such a PR on this path; this
// is the only guard between a note branch and main, and it must not depend on
// that staying true two packages away.
func TestAppendToEntryOnPRRefusesAHeadOutsideTheConfiguredRepo(t *testing.T) {
	for name, repo := range map[string]string{
		"fork":            "attacker/r",
		"deleted head":    "-",
		"same name, else": "o2/r",
	} {
		t.Run(name, func(t *testing.T) {
			s := &stubPR{headRepo: repo, branch: "main"}
			c := s.start(t)
			if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
				t.Fatal("want a refusal rather than a commit onto the base repository's branch")
			}
			if puts := s.writes(); len(puts) != 0 {
				t.Errorf("committed onto %q in the configured repo: %+v", puts[0].branch, puts)
			}
		})
	}
}

// TestAppendToEntryOnPRRefusesAPossiblyTruncatedFileListing: the listing asks
// for one page. A FULL page means either this is not a RunLore curation PR (they
// change at most three files) or the listing was cut and okf.EntryFile is
// choosing among a fraction of the changed paths with no way to know. "The one
// .md on page 1" is not a fact about the pull request, so it is not a file to
// rewrite.
func TestAppendToEntryOnPRRefusesAPossiblyTruncatedFileListing(t *testing.T) {
	changed := make([]string, 0, prFilesPerPage)
	changed = append(changed, "concepts/oom-1.md")
	for i := range prFilesPerPage - 1 {
		changed = append(changed, fmt.Sprintf("chart/values-%d.yaml", i))
	}
	s := &stubPR{changed: changed}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
		t.Fatal("want a refusal: a full page cannot be told from a truncated one")
	}
	if puts := s.writes(); len(puts) != 0 {
		t.Errorf("committed from a possibly truncated listing: %+v", puts)
	}
}

// TestAppendToEntryOnPRNeutralizesImagesInTheBlock keeps the appended block at
// the same defusal level renderEntry gives a first draft — this client's own
// neutralizeImages, which rewrites `![alt](url)` to a labelled code span.
//
// It is DEFENCE IN DEPTH rather than the only guard, and saying so precisely
// matters: both routes render through thread.NoteBody, which has already
// rewritten "![" in the note text, so a note cannot reach here carrying live
// image markdown. What this covers is the rest of the block — the identity
// fields NoteBody interpolates AROUND the note (author, thread title), which go
// through noteField, not through the markdown defusals — and any future caller
// that hands this method a block NoteBody did not build.
func TestAppendToEntryOnPRNeutralizesImagesInTheBlock(t *testing.T) {
	s := &stubPR{}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "look ![px](https://evil.example/px.gif)", "k1"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	got := s.writes()[0].content
	if strings.Contains(got, "![px](") || strings.Contains(got, "evil.example") {
		t.Errorf("image markdown reached the entry file:\n%s", got)
	}
	if !strings.Contains(got, "`[image: px]`") {
		t.Errorf("want the defused label renderEntry would have produced:\n%s", got)
	}
}

// TestAppendToEntryOnPRMarkerIsInvisibleAndSingular: the idempotency marker is
// an HTML comment, so it never renders in the entry a human or the catalog
// reads, and exactly one is written per note.
func TestAppendToEntryOnPRMarkerIsInvisibleAndSingular(t *testing.T) {
	s := &stubPR{}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "the second note", "k1"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	got := s.writes()[0].content
	if n := strings.Count(got, okf.NoteMarker("k1")); n != 1 {
		t.Errorf("marker appears %d times, want 1:\n%s", n, got)
	}
	if !strings.HasPrefix(strings.TrimSpace(okf.NoteMarker("k1")), "<!--") {
		t.Error("the marker must be an HTML comment so it never renders in the entry")
	}
}

// TestAppendToEntryOnPRRefusesWhenTheEntryIsAmbiguous: the next move is a commit
// into a pull request a human is reviewing, so an unclear target is an error,
// never a guess. The caller degrades to a comment on this error (see
// thread.Responder.addToPR); a wrong write would have no such recovery.
func TestAppendToEntryOnPRRefusesWhenTheEntryIsAmbiguous(t *testing.T) {
	for name, changed := range map[string][]string{
		"two entries": {"concepts/a.md", "concepts/b.md"},
		"no entry":    {"index.md", "log.md"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &stubPR{changed: changed, entry: "concepts/a.md"}
			c := s.start(t)
			if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
				t.Fatal("want an error rather than a guess at which file to rewrite")
			}
			if puts := s.writes(); len(puts) != 0 {
				t.Errorf("committed to %q despite an ambiguous entry", puts[0].path)
			}
		})
	}
}

// TestAppendToEntryOnPRRefusesWithoutAHeadBranch: a PR payload carrying no head
// ref must not become a commit onto the empty branch name — which the contents
// API resolves to the repository's DEFAULT branch, i.e. straight onto main.
//
// Hand-built rather than driven through stubPR, because the shape under test is
// a field that is absent from the payload entirely; the catch-all handler is the
// assertion that nothing past the guard is ever reached.
func TestAppendToEntryOnPRRefusesWithoutAHeadBranch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/84", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"open","head":{"repo":{"full_name":"o/r"}}}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("reached %s %s past the head-branch guard", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	if err := c.AppendToEntryOnPR(context.Background(), 84, "note", "k1"); err == nil {
		t.Fatal("want an error when the pull request names no head branch")
	}
}
