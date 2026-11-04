// SPDX-License-Identifier: Apache-2.0

package curate_test

import (
	"context"
	"crypto/sha1" //nolint:gosec // a fake git blob id, as git itself computes it
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/providers"
)

// fakeCommit is a whole snapshot of the tree with its parent, which is all a merge check and a diff need.
type fakeCommit struct {
	sha, parent, author string
	files               map[string]string
}

// activityRow is one row of a ref's activity: what Maku's pushedBy reads.
type activityRow struct{ Type, Actor, Before, After string }

type fakePR struct {
	number                     int
	title, body, branch, state string
	headRepo                   string
	merged                     bool
	labels                     []string
}

// fakeRepo is an in-memory GitHub over httptest. Every write is made as actor, runlore[bot] unless
// a test pushes as somebody else directly.
type fakeRepo struct {
	t   *testing.T
	srv *httptest.Server
	mux *http.ServeMux

	mu      sync.Mutex
	seq     int
	commits map[string]*fakeCommit
	refs    map[string]string
	rows    map[string][]activityRow
	prs     map[int]*fakePR
	nextPR  int
	nWrites int
	events  []string // every write, in order
	patches []map[string]any

	before func(r *http.Request) int // a non-zero status answers the request with it
	after  func(r *http.Request)     // runs once the request was served, before the client reads the answer
}

func newFakeRepo(t *testing.T, files map[string]string) *fakeRepo {
	t.Helper()
	f := &fakeRepo{t: t, mux: http.NewServeMux(), commits: map[string]*fakeCommit{}, refs: map[string]string{},
		rows: map[string][]activityRow{}, prs: map[int]*fakePR{}, nextPR: 1}
	f.refs["main"] = f.commit("", curate.BotLogin, files)
	f.routes()
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRepo) client() *github.Client {
	return github.New(f.srv.URL, "o", "r", "main", func(context.Context) (string, error) { return "t", nil })
}

func (f *fakeRepo) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.mu.Lock()
		f.nWrites++
		f.events = append(f.events, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
	}
	if f.before != nil {
		if status := f.before(r); status != 0 {
			http.Error(w, "injected", status)
			return
		}
	}
	f.mux.ServeHTTP(w, r)
	if f.after != nil {
		f.after(r)
	}
}

// commit stores a commit and returns its sha. Call it with mu held, or before the server starts.
func (f *fakeRepo) commit(parent, author string, files map[string]string) string {
	f.seq++
	sha := fmt.Sprintf("%040x", f.seq)
	f.commits[sha] = &fakeCommit{sha: sha, parent: parent, author: author, files: files}
	return sha
}

func blobSHA(content string) string {
	h := sha1.Sum([]byte(content)) //nolint:gosec // a fake git blob id
	return hex.EncodeToString(h[:])
}

func (f *fakeRepo) isAncestor(old, of string) bool {
	for c := f.commits[of]; c != nil; c = f.commits[c.parent] {
		if c.sha == old {
			return true
		}
	}
	return false
}

// ownCommits are the commits of head that the base does not have, oldest first. Like GitHub's, the
// list starts at the merge base: a head the base has moved past still lists only its own.
func (f *fakeRepo) ownCommits(head string) []*fakeCommit {
	var out []*fakeCommit
	for c := f.commits[head]; c != nil && !f.isAncestor(c.sha, f.refs["main"]); c = f.commits[c.parent] {
		out = append(out, c)
	}
	slices.Reverse(out)
	return out
}

// mergeBaseFiles is the tree the PR's own diff is computed against.
func (f *fakeRepo) mergeBaseFiles(head string) map[string]string {
	for c := f.commits[head]; c != nil; c = f.commits[c.parent] {
		if f.isAncestor(c.sha, f.refs["main"]) {
			return c.files
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeRepo) routes() {
	const root = "/repos/o/r"
	handle := func(pattern string, h func(w http.ResponseWriter, r *http.Request)) {
		f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		})
	}
	handle("GET "+root+"/git/ref/heads/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		sha, ok := f.refs[r.PathValue("ref")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"object": map[string]string{"sha": sha}})
	})
	handle("POST "+root+"/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Ref, SHA string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		name := strings.TrimPrefix(in.Ref, "refs/heads/")
		if _, dup := f.refs[name]; dup || f.commits[in.SHA] == nil {
			http.Error(w, "exists", http.StatusUnprocessableEntity)
			return
		}
		f.refs[name] = in.SHA
		f.rows[name] = append([]activityRow{{"branch_creation", curate.BotLogin, "", in.SHA}}, f.rows[name]...)
		writeJSON(w, 201, map[string]any{})
	})
	handle("PATCH "+root+"/git/refs/heads/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("ref")
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.patches = append(f.patches, in)
		old, ok := f.refs[name]
		sha, _ := in["sha"].(string)
		if !ok || f.commits[sha] == nil {
			http.Error(w, "unknown", http.StatusUnprocessableEntity)
			return
		}
		kind := "push"
		if !f.isAncestor(old, sha) {
			if in["force"] != true {
				http.Error(w, "not a fast forward", http.StatusUnprocessableEntity)
				return
			}
			kind = "force_push"
		}
		f.refs[name] = sha
		f.rows[name] = append([]activityRow{{kind, curate.BotLogin, old, sha}}, f.rows[name]...)
		writeJSON(w, 200, map[string]any{})
	})
	handle("DELETE "+root+"/git/refs/heads/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.refs[r.PathValue("ref")]; !ok {
			http.Error(w, "unknown", http.StatusUnprocessableEntity)
			return
		}
		delete(f.refs, r.PathValue("ref"))
		w.WriteHeader(204)
	})
	handle("GET "+root+"/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		c := f.commits[f.refs[r.URL.Query().Get("ref")]]
		if c == nil {
			http.NotFound(w, r)
			return
		}
		content, ok := c.files[r.PathValue("path")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]string{"sha": blobSHA(content), "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(content))})
	})
	handle("PUT "+root+"/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Message, Content, Branch, SHA string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		head, ok := f.refs[in.Branch]
		if !ok {
			http.NotFound(w, r)
			return
		}
		path := r.PathValue("path")
		files := maps.Clone(f.commits[head].files)
		cur, exists := files[path]
		if exists && in.SHA != blobSHA(cur) || !exists && in.SHA != "" {
			http.Error(w, "sha", http.StatusConflict)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(in.Content)
		if err != nil {
			http.Error(w, "content", http.StatusUnprocessableEntity)
			return
		}
		files[path] = string(raw)
		sha := f.commit(head, curate.BotLogin, files)
		f.refs[in.Branch] = sha
		f.rows[in.Branch] = append([]activityRow{{"push", curate.BotLogin, head, sha}}, f.rows[in.Branch]...)
		writeJSON(w, 200, map[string]any{})
	})
	handle("POST "+root+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Title, Head, Body string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		n := f.nextPR
		f.nextPR++
		f.prs[n] = &fakePR{number: n, title: in.Title, body: in.Body, branch: in.Head, state: "open", headRepo: "o/r"}
		writeJSON(w, 201, map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/o/r/pull/%d", n)})
	})
	handle("GET "+root+"/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		pr := f.pr(w, r)
		if pr == nil {
			return
		}
		head := f.refs[pr.branch]
		var repo any
		if pr.headRepo != "" {
			repo = map[string]string{"full_name": pr.headRepo}
		}
		var mergedAt any
		if pr.merged {
			mergedAt = time.Now().UTC().Format(time.RFC3339)
		}
		labels := []map[string]string{}
		for _, l := range pr.labels {
			labels = append(labels, map[string]string{"name": l})
		}
		writeJSON(w, 200, map[string]any{"state": pr.state, "merged_at": mergedAt, "labels": labels,
			"mergeable": f.isAncestor(f.refs["main"], head),
			"head":      map[string]any{"ref": pr.branch, "sha": head, "repo": repo}})
	})
	handle("GET "+root+"/pulls/{n}/files", func(w http.ResponseWriter, r *http.Request) {
		pr := f.pr(w, r)
		if pr == nil {
			return
		}
		out := []map[string]string{}
		for _, p := range diffOf(f.mergeBaseFiles(f.refs[pr.branch]), f.commits[f.refs[pr.branch]].files).Paths() {
			out = append(out, map[string]string{"filename": p})
		}
		writeJSON(w, 200, out)
	})
	handle("GET "+root+"/pulls/{n}/commits", func(w http.ResponseWriter, r *http.Request) {
		pr := f.pr(w, r)
		if pr == nil {
			return
		}
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		own := f.ownCommits(f.refs[pr.branch])
		if perPage > 0 && len(own) > perPage {
			own = own[:perPage]
		}
		out := []map[string]any{}
		for _, c := range own {
			var author any
			if c.author != "" {
				author = map[string]string{"login": c.author}
			}
			out = append(out, map[string]any{"sha": c.sha, "author": author})
		}
		writeJSON(w, 200, out)
	})
	handle("POST "+root+"/issues/{n}/labels", func(w http.ResponseWriter, r *http.Request) {
		pr := f.pr(w, r)
		if pr == nil {
			return
		}
		var in struct{ Labels []string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		pr.labels = append(pr.labels, in.Labels...)
		writeJSON(w, 200, []any{})
	})
	handle("DELETE "+root+"/issues/{n}/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		pr := f.pr(w, r)
		if pr == nil {
			return
		}
		if !slices.Contains(pr.labels, r.PathValue("label")) {
			http.NotFound(w, r)
			return
		}
		pr.labels = slices.DeleteFunc(pr.labels, func(l string) bool { return l == r.PathValue("label") })
		writeJSON(w, 200, []any{})
	})
	handle("GET "+root+"/issues", func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		for _, n := range slices.Sorted(maps.Keys(f.prs)) {
			pr := f.prs[n]
			if pr.state != r.URL.Query().Get("state") || !slices.Contains(pr.labels, r.URL.Query().Get("labels")) {
				continue
			}
			labels := []map[string]string{}
			for _, l := range pr.labels {
				labels = append(labels, map[string]string{"name": l})
			}
			out = append(out, map[string]any{"number": n, "title": pr.title, "body": pr.body, "labels": labels,
				"pull_request": map[string]any{"merged_at": nil}})
		}
		writeJSON(w, 200, out)
	})
}

func (f *fakeRepo) pr(w http.ResponseWriter, r *http.Request) *fakePR {
	n, _ := strconv.Atoi(r.PathValue("n"))
	pr := f.prs[n]
	if pr == nil {
		http.NotFound(w, r)
	}
	return pr
}

// number reads the PR number out of the URL OpenPR returned.
func (f *fakeRepo) number(ref providers.Ref) int {
	f.t.Helper()
	n, err := strconv.Atoi(ref.URL[strings.LastIndexByte(ref.URL, '/')+1:])
	if err != nil {
		f.t.Fatalf("ref %q: %v", ref.URL, err)
	}
	return n
}

// merge merges a PR the way a merge button does: only when it is mergeable, and the base takes its tree.
func (f *fakeRepo) merge(n int) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := f.prs[n]
	head := f.refs[pr.branch]
	if !f.isAncestor(f.refs["main"], head) {
		f.t.Fatalf("PR %d conflicts", n)
	}
	f.refs["main"] = f.commit(f.refs["main"], "", maps.Clone(f.commits[head].files))
	f.commits[f.refs["main"]].parent = head
	pr.state, pr.merged = "closed", true
}

func (f *fakeRepo) file(branch, path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commits[f.refs[branch]].files[path]
}

// push adds a commit to a branch as author, a human's or nobody's.
func (f *fakeRepo) push(branch, author, path, content string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	head := f.refs[branch]
	files := maps.Clone(f.commits[head].files)
	files[path] = content
	sha := f.commit(head, author, files)
	f.refs[branch] = sha
	f.rows[branch] = append([]activityRow{{"push", author, head, sha}}, f.rows[branch]...)
	return sha
}

func (f *fakeRepo) activity(branch string) []activityRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rows[branch])
}

func (f *fakeRepo) head(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refs[f.prs[n].branch]
}

func (f *fakeRepo) ref(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refs[name]
}

func (f *fakeRepo) commitAuthors(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.ownCommits(f.refs[f.prs[n].branch]) {
		out = append(out, c.author)
	}
	return out
}

func (f *fakeRepo) hasRefWithPrefix(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(slices.Collect(maps.Keys(f.refs)), func(r string) bool { return strings.HasPrefix(r, prefix) })
}

func (f *fakeRepo) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nWrites
}

func (f *fakeRepo) writeEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

// fakeDiff is what GitHub would show for a PR against the current base.
type fakeDiff struct{ base, head map[string]string }

func diffOf(base, head map[string]string) fakeDiff { return fakeDiff{base, head} }

func (f *fakeRepo) diffAgainstBase(n int) fakeDiff {
	f.mu.Lock()
	defer f.mu.Unlock()
	return diffOf(f.commits[f.refs["main"]].files, f.commits[f.refs[f.prs[n].branch]].files)
}

func (d fakeDiff) Paths() []string {
	var out []string
	for p, c := range d.head {
		if b, ok := d.base[p]; !ok || b != c {
			out = append(out, p)
		}
	}
	for p := range d.base {
		if _, ok := d.head[p]; !ok {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

func (d fakeDiff) Added(path string) bool {
	_, onBase := d.base[path]
	_, onHead := d.head[path]
	return onHead && !onBase
}

// lines splits a file into lines the way a line diff sees them: a final newline ends the last line
// and adds none.
func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// common is the length of the longest common subsequence of two files' lines: what a line diff keeps.
func common(a, b []string) int {
	prev := make([]int, len(b)+1)
	for _, x := range a {
		cur := make([]int, len(b)+1)
		for j, y := range b {
			if x == y {
				cur[j+1] = prev[j] + 1
			} else {
				cur[j+1] = max(prev[j+1], cur[j])
			}
		}
		prev = cur
	}
	return prev[len(b)]
}

func (d fakeDiff) Deletions(path string) int {
	a, b := lines(d.base[path]), lines(d.head[path])
	return len(a) - common(a, b)
}

func (d fakeDiff) Additions(path string) int {
	a, b := lines(d.base[path]), lines(d.head[path])
	return len(b) - common(a, b)
}

func kbEntry(title, fingerprint string) providers.KBEntry {
	return providers.KBEntry{Type: "Incident", Title: title, Description: title + " happened", Body: "## Cause\n\nIt broke.",
		Fingerprint: fingerprint, Confidence: 0.9, Origin: "runlore"}
}

const secondPath = "incidents/second-incident-bbbbbbbb.md"

// conflicting opens two entry PRs and merges the first: the second now conflicts, as KP19 says.
func conflicting(t *testing.T) (*fakeRepo, *github.Client, int) {
	t.Helper()
	ctx := context.Background()
	repo := newFakeRepo(t, map[string]string{"index.md": "# Index\n\n## Incidents\n", "log.md": "# Change log\n"})
	c := repo.client()
	a, err := c.OpenPR(ctx, kbEntry("First incident", "aaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.OpenPR(ctx, kbEntry("Second incident", "bbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	repo.merge(repo.number(a))
	return repo, c, repo.number(b)
}

func TestRefreshAfterAMerge(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepo(t, map[string]string{"index.md": "# Index\n\n## Incidents\n", "log.md": "# Change log\n"})
	c := repo.client() // github.New over the fake, every write made as runlore[bot]
	a, _ := c.OpenPR(ctx, kbEntry("First incident", "aaaaaaaa"))
	b, _ := c.OpenPR(ctx, kbEntry("Second incident", "bbbbbbbb"))
	nb, path := repo.number(b), "incidents/second-incident-bbbbbbbb.md"
	repo.merge(repo.number(a))

	st, err := c.EntryPRState(ctx, nb)
	if err != nil || st.Mergeable == nil || *st.Mergeable {
		t.Fatalf("merging one makes the other conflict: %+v %v", st, err)
	}
	before := repo.file(st.Branch, path)
	r := curate.RefreshEntries{Forge: c, Log: slog.Default()}
	if res, err := r.Once(ctx); err != nil || !slices.Equal(res.Rebuilt, []int{nb}) || len(res.Refused) != 0 {
		t.Fatalf("%+v %v", res, err)
	}

	d := repo.diffAgainstBase(nb) // Maku's added-entry: one added file, additions-only companions
	if !slices.Equal(d.Paths(), []string{path, "index.md", "log.md"}) || !d.Added(path) || d.Deletions("index.md") != 0 || d.Deletions("log.md") != 0 {
		t.Fatalf("the shape changed: %+v\nlog.md was %q, is %q", d.Paths(), d.base["log.md"], d.head["log.md"])
	}
	if add := d.Additions("index.md") + d.Additions("log.md"); add == 0 || add > 10 {
		t.Fatalf("the companions add at most 10 lines, got %d", add)
	}
	if repo.file(st.Branch, path) != before {
		t.Fatal("the entry file is re-put unchanged")
	}
	if last := repo.activity(st.Branch)[0]; last.Type != "force_push" || last.Actor != curate.BotLogin || last.Before != st.HeadSHA || last.After != repo.head(nb) {
		t.Fatalf("the chain Maku's pushedBy reads holds: %+v", last)
	}
	if got := repo.commitAuthors(nb); len(got) == 0 || slices.ContainsFunc(got, func(a string) bool { return a != curate.BotLogin }) {
		t.Fatalf("every commit is the bot's: %v", got)
	}
	if repo.hasRefWithPrefix("runlore/rebuild-") {
		t.Fatal("the temporary branch is gone")
	}

	writes := repo.writes()
	if res, err := r.Once(ctx); err != nil || len(res.Rebuilt) != 0 || repo.writes() != writes {
		t.Fatalf("a second call is a no-op: %+v %v", res, err)
	}
}

// R9, over HTTP: the label is off before anything is pushed, and the Queue puts it back on the same evidence.
func TestRefreshRemovesTheLabelBeforeThePush(t *testing.T) {
	ctx := context.Background()
	repo, c, n := conflicting(t)
	if err := c.ReplaceLabel(ctx, n, "", "ready-to-merge"); err != nil {
		t.Fatal(err)
	}
	mark := len(repo.writeEvents())
	if _, err := (curate.RefreshEntries{Forge: c, Log: slog.Default()}).Once(ctx); err != nil {
		t.Fatal(err)
	}
	events := repo.writeEvents()[mark:]
	if !strings.HasPrefix(events[0], "DELETE /repos/o/r/issues/") || !strings.HasSuffix(events[0], "/labels/ready-to-merge") {
		t.Fatalf("the label comes off first: %q", events)
	}
	if !slices.ContainsFunc(events, func(e string) bool { return strings.HasPrefix(e, "PATCH /repos/o/r/git/refs/heads/runlore/kb-") }) {
		t.Fatalf("and the ref moves after it: %q", events)
	}
}

func TestRefreshDryRunWritesNothingOverHTTP(t *testing.T) {
	ctx := context.Background()
	repo, c, n := conflicting(t)
	head, writes := repo.head(n), repo.writes()
	g := curate.Guard{Inner: c, DryRun: true, Log: slog.Default()}
	res, err := curate.RefreshEntries{Forge: g, Log: slog.Default()}.Once(ctx)
	if err != nil || !slices.Equal(res.Rebuilt, []int{n}) {
		t.Fatalf("%+v %v", res, err)
	}
	if repo.writes() != writes || repo.head(n) != head {
		t.Fatalf("a dry-run wrote: %q", repo.writeEvents())
	}
}

func TestEntryPRState(t *testing.T) {
	ctx := context.Background()

	t.Run("a mergeable PR", func(t *testing.T) {
		repo := newFakeRepo(t, map[string]string{"log.md": "# Change log\n"})
		c := repo.client()
		ref, err := c.OpenPR(ctx, kbEntry("First incident", "aaaaaaaa"))
		if err != nil {
			t.Fatal(err)
		}
		st, err := c.EntryPRState(ctx, repo.number(ref))
		if err != nil || st.Mergeable == nil || !*st.Mergeable || st.Fork || st.Truncated {
			t.Fatalf("%+v %v", st, err)
		}
		if !strings.HasPrefix(st.Branch, "runlore/kb-first-incident-") || st.HeadSHA != repo.head(repo.number(ref)) {
			t.Fatalf("%+v", st)
		}
		if !slices.Equal(st.Files, []string{"incidents/first-incident-aaaaaaaa.md", "log.md"}) {
			t.Fatalf("%+v", st.Files)
		}
	})

	t.Run("who wrote each commit, no linked account being empty", func(t *testing.T) {
		repo, c, n := conflicting(t)
		st, _ := c.EntryPRState(ctx, n)
		repo.push(st.Branch, "Smana", "note.md", "x")
		repo.push(st.Branch, "", "note2.md", "y")
		st, err := c.EntryPRState(ctx, n)
		if err != nil || !slices.Contains(st.CommitAuthors, "Smana") || !slices.Contains(st.CommitAuthors, "") || !slices.Contains(st.CommitAuthors, curate.BotLogin) {
			t.Fatalf("%+v %v", st, err)
		}
		if res, err := (curate.RefreshEntries{Forge: c, Log: slog.Default()}).Once(ctx); err != nil || res.Refused[n] != "conflict" || len(res.Rebuilt) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("a full page of commits is truncated", func(t *testing.T) {
		repo, c, n := conflicting(t)
		st, _ := c.EntryPRState(ctx, n)
		for i := range 100 {
			repo.push(st.Branch, curate.BotLogin, "incidents/second-incident-bbbbbbbb.md", strconv.Itoa(i))
		}
		st, err := c.EntryPRState(ctx, n)
		if err != nil || !st.Truncated {
			t.Fatalf("%+v %v", st, err)
		}
	})

	t.Run("a head in another repository, or in none, is a fork", func(t *testing.T) {
		for _, headRepo := range []string{"someone/r", ""} {
			repo, c, n := conflicting(t)
			repo.mu.Lock()
			repo.prs[n].headRepo = headRepo
			repo.mu.Unlock()
			st, err := c.EntryPRState(ctx, n)
			if err != nil || !st.Fork {
				t.Fatalf("%q: %+v %v", headRepo, st, err)
			}
		}
	})
}

func TestRebuildEntryPR(t *testing.T) {
	ctx := context.Background()
	// Hooks are installed after OpenPR ran, so a write to log.md here is the rebuild's.
	isLogPut := func(r *http.Request) bool {
		return r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/contents/log.md")
	}
	cases := []struct {
		name    string
		arrange func(t *testing.T, repo *fakeRepo, n int) (head string)
		want    error  // errors.Is; nil means any error
		wantMsg string // when no sentinel names the failure
		wantRef func(repo *fakeRepo, n int, before string) string
	}{
		{
			name: "a stale head",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				return repo.head(n) + "stale"
			},
			want: providers.ErrHeadMoved,
		},
		{
			name: "a push landing after the bundle is written, before the PATCH",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.after = func(r *http.Request) {
					if isLogPut(r) {
						repo.after = nil
						repo.mu.Lock()
						branch := repo.prs[n].branch
						repo.mu.Unlock()
						repo.push(branch, "Smana", "note.md", "mine")
					}
				}
				return repo.head(n)
			},
			want:    providers.ErrHeadMoved,
			wantRef: func(repo *fakeRepo, n int, _ string) string { return repo.activity(repo.prs[n].branch)[0].After },
		},
		{
			name: "the entry already on the base",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.push("main", curate.BotLogin, secondPath, "---\ntype: Incident\n---\nother")
				return repo.head(n)
			},
			want: providers.ErrEntryOnBase,
		},
		{
			name: "a failing index read",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.before = func(r *http.Request) int {
					if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/contents/index.md") {
						return 500
					}
					return 0
				}
				return repo.head(n)
			},
			wantMsg: "read index.md",
		},
		{
			name: "a failing log write: a missing companion would change the shape",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.before = func(r *http.Request) int {
					if isLogPut(r) {
						return 500
					}
					return 0
				}
				return repo.head(n)
			},
			wantMsg: "contents/log.md",
		},
		{
			name: "a failing entry write",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.before = func(r *http.Request) int {
					if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, secondPath) {
						return 500
					}
					return 0
				}
				return repo.head(n)
			},
			wantMsg: secondPath,
		},
		{
			name: "a failing PATCH",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.before = func(r *http.Request) int {
					if r.Method == http.MethodPatch {
						return 500
					}
					return 0
				}
				return repo.head(n)
			},
			wantMsg: "status 500",
		},
		{
			name: "a merged PR",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				head := repo.head(n)
				repo.mu.Lock()
				repo.prs[n].state, repo.prs[n].merged = "closed", true
				repo.mu.Unlock()
				return head
			},
			want: providers.ErrPRNotOpen,
		},
		{
			name: "a fork, whose branch name would land in this repository",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.mu.Lock()
				repo.prs[n].headRepo = "someone/r"
				repo.mu.Unlock()
				return repo.head(n)
			},
			wantMsg: "fork",
		},
		{
			name: "a branch that is not an entry branch",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.mu.Lock()
				defer repo.mu.Unlock()
				old := repo.prs[n].branch
				repo.refs["fix/mine"] = repo.refs[old]
				repo.prs[n].branch = "fix/mine"
				return repo.refs["fix/mine"]
			},
			want: providers.ErrNotRunLorePR,
		},
		{
			name: "a file that does not read as an entry",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.mu.Lock()
				branch := repo.prs[n].branch
				repo.mu.Unlock()
				return repo.push(branch, curate.BotLogin, secondPath, "no frontmatter")
			},
			wantMsg: "does not read as a catalog entry",
		},
		{
			name: "an entry missing the fields its index line needs",
			arrange: func(_ *testing.T, repo *fakeRepo, n int) string {
				repo.mu.Lock()
				branch := repo.prs[n].branch
				repo.mu.Unlock()
				return repo.push(branch, curate.BotLogin, secondPath, "---\ndescription: x\n---\nbody\n")
			},
			wantMsg: "type or title",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, c, n := conflicting(t)
			head := tc.arrange(t, repo, n)
			before := repo.head(n)
			repo.mu.Lock()
			branch := repo.prs[n].branch
			repo.mu.Unlock()
			err := c.RebuildEntryPR(ctx, n, head)
			switch {
			case err == nil:
				t.Fatal("want an error")
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("err = %v, want %v", err, tc.want)
			case !strings.Contains(err.Error(), tc.wantMsg):
				t.Fatalf("err = %v, want it to name %q", err, tc.wantMsg)
			}
			want := before
			if tc.wantRef != nil {
				want = tc.wantRef(repo, n, before)
			}
			if got := repo.ref(branch); got != want {
				t.Fatalf("the ref moved: %s, want %s", got, want)
			}
			if rows := repo.activity(branch); len(rows) > 0 && rows[0].Type == "force_push" {
				t.Fatalf("a force push happened: %+v", rows[0])
			}
			if repo.hasRefWithPrefix("runlore/rebuild-") {
				t.Fatalf("the temporary branch is left behind: %q", repo.writeEvents())
			}
		})
	}

	t.Run("the PATCH carries force", func(t *testing.T) {
		repo, c, n := conflicting(t)
		if err := c.RebuildEntryPR(ctx, n, repo.head(n)); err != nil {
			t.Fatal(err)
		}
		if len(repo.patches) != 1 || repo.patches[0]["force"] != true || repo.patches[0]["sha"] != repo.head(n) {
			t.Fatalf("%+v", repo.patches)
		}
	})

	t.Run("a cancelled context still removes the temporary branch", func(t *testing.T) {
		repo, c, n := conflicting(t)
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		repo.after = func(r *http.Request) {
			if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, secondPath) {
				cancel()
			}
		}
		if err := c.RebuildEntryPR(cctx, n, repo.head(n)); err == nil {
			t.Fatal("want an error")
		}
		if repo.hasRefWithPrefix("runlore/rebuild-") {
			t.Fatal("the temporary branch is left behind")
		}
	})
}
