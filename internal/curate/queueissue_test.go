// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

const qiRepo = queueRepo

type fakeQueueForge struct {
	open     []providers.CuratedIssue
	merged   int
	mergedBy []providers.CuratedIssue // wins over merged when set
	issues   []providers.CuratedIssue // existing queue issues

	created, edited int
	title, body     string
	labels          []string
	url             string
	mergedSince     time.Time
	listErr         error
	entries         map[int]string
	readErr         error
	reads           int
}

func (f *fakeQueueForge) PREntryText(_ context.Context, n int) ([]byte, error) {
	f.reads++
	return []byte(f.entries[n]), f.readErr
}

func (f *fakeQueueForge) ListPRsByLabel(_ context.Context, label string) ([]providers.CuratedIssue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if label != "runlore" {
		return nil, nil
	}
	return slices.Clone(f.open), nil
}

func (f *fakeQueueForge) ListMergedPRsByLabel(_ context.Context, _ string, since time.Time) ([]providers.CuratedIssue, error) {
	f.mergedSince = since
	if f.mergedBy != nil {
		return f.mergedBy, nil
	}
	out := make([]providers.CuratedIssue, f.merged)
	for i := range out {
		out[i] = providers.CuratedIssue{Number: 100 + i, Labels: []string{"runlore"}}
	}
	return out, nil
}

func (f *fakeQueueForge) ListIssuesByLabel(_ context.Context, label string) ([]providers.CuratedIssue, error) {
	if label != "runlore-queue" {
		return nil, nil
	}
	if f.issues != nil {
		return f.issues, nil
	}
	if f.created == 0 {
		return nil, nil
	}
	return []providers.CuratedIssue{{Number: 7, URL: f.url, Title: f.title, Body: f.body, Labels: f.labels}}, nil
}

func (f *fakeQueueForge) CreateIssue(_ context.Context, title, body string, labels []string) (int, providers.Ref, error) {
	f.created++
	f.title, f.body, f.labels, f.url = title, body, labels, "https://github.com/Smana/runlore-kb/issues/7"
	return 7, providers.Ref{URL: f.url}, nil
}

func (f *fakeQueueForge) EditIssue(_ context.Context, _ int, body string) error {
	f.edited++
	f.body = body
	return nil
}

type qiEvidence struct {
	contestedPR int
	verified    map[string]bool
}

func (e qiEvidence) Verifications() map[string]bool { return e.verified }
func (e qiEvidence) ContestedTriggers() []outcome.ContestedTrigger {
	if e.contestedPR == 0 {
		return nil
	}
	return []outcome.ContestedTrigger{{TriggerKey: "k", CuratedURL: prURL(e.contestedPR), Downs: 1}}
}

type qiStats struct {
	m   outcome.Measures
	err error
}

func (s qiStats) Measures(time.Time) (outcome.Measures, error) { return s.m, s.err }

type qiLines struct {
	got []string
	err error
}

func (l *qiLines) DeliverLine(_ context.Context, text string) error {
	l.got = append(l.got, text)
	return l.err
}

func qiPR(n int, title, fp string, created time.Time, extra ...string) providers.CuratedIssue {
	body := "Drafted by RunLore\n\n" + providers.FingerprintMarker(fp)
	return providers.CuratedIssue{Number: n, Title: title, Body: body, CreatedAt: created,
		URL: prURL(n), Labels: append([]string{"runlore"}, extra...)}
}

func inOrder(s string, parts ...string) bool {
	for _, p := range parts {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return true
}

func qiMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newQI(f *fakeQueueForge, now time.Time) QueueIssue {
	return QueueIssue{Forge: f, KBRepo: qiRepo, Weekday: time.Monday, Hour: 8, Now: func() time.Time { return now }, Log: discardLog()}
}

func TestQueueIssue(t *testing.T) {
	ctx := context.Background()
	mon := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	f := &fakeQueueForge{open: []providers.CuratedIssue{
		qiPR(12, "KB: old unverified", "fpU", mon.AddDate(0, 0, -92)), qiPR(14, "KB: contested", "fpC", mon.AddDate(0, 0, -3)),
		qiPR(15, "KB: newer unverified", "fpN", mon.AddDate(0, 0, -1))}, merged: 3}
	line := &qiLines{}
	q := QueueIssue{Forge: f, Evidence: qiEvidence{contestedPR: 14}, Stats: qiStats{m: outcome.Measures{Incidents: 4, InstantRecalls: 2, RecallsResolved: 1, TokensPerIncident: 41000}},
		Line: line, KBRepo: qiRepo, Weekday: time.Monday, Hour: 8, Now: func() time.Time { return mon.Add(time.Hour) }, Log: discardLog()}
	qiMust(t, q.Run(ctx))
	b := f.body
	if f.created != 1 || f.title != "Knowledge queue" || !slices.Equal(f.labels, []string{"runlore-queue"}) ||
		!inOrder(b, "## Decide", "#14", "#12", "#15", "## Audit", "## Measures") { // contested first, then the oldest unverified
		t.Fatalf("one issue, sections in order: %+v", f)
	}
	for _, s := range []string{"| Merged entries | 3 |", "median 3d, oldest 92d", "| Instant recalls | 2, 50% resolved |", "| Tokens per incident | 41k |", "at=2026-11-02T09:00:00Z"} {
		if !strings.Contains(b, s) {
			t.Errorf("missing %q in\n%s", s, b)
		}
	}
	if strings.Contains(b, "Nothing new since the last rewrite.") {
		t.Fatal("the first rewrite has no marker to compare with")
	}
	if qiMust(t, q.Run(ctx)); f.created != 1 || f.edited != 0 || len(line.got) != 1 || !strings.Contains(line.got[0], f.url) {
		t.Fatal("the Slack line links it; the same scheduled instant is not rewritten")
	}
	if want := "Knowledge queue, as of 2026-11-02 09:00 UTC: 3 to decide, 3 merged. " + f.url; line.got[0] != want {
		t.Fatalf("line %q, want %q", line.got[0], want)
	}
	q.Now = func() time.Time { return mon.Add(2 * time.Hour) }
	if qiMust(t, q.RunNow(ctx)); f.created != 1 || f.edited != 1 || !strings.Contains(f.body, "#14") || !strings.Contains(f.body, "Nothing new since the last rewrite.") || len(line.got) != 2 {
		t.Fatal("RunNow: the same issue rewritten at once, whatever the schedule; an unchanged queue says nothing is new, and still lists its rows")
	}
	f.open = append(f.open, qiPR(16, "KB: new", "fpZ", mon.Add(150*time.Minute))) // after the 10:00 marker
	q.Now = func() time.Time { return mon.Add(3 * time.Hour) }
	if qiMust(t, q.RunNow(ctx)); !strings.Contains(f.body, "#16") || strings.Contains(f.body, "Nothing new since the last rewrite.") {
		t.Fatal("a PR created after the marker is new")
	}
	f.open = nil
	q.Now = func() time.Time { return mon.Add(4 * time.Hour) }
	if qiMust(t, q.RunNow(ctx)); !strings.Contains(f.body, "Nothing new since the last rewrite.") {
		t.Fatal("an empty queue says so too")
	}
	if !f.mergedSince.Equal(mon.Add(3 * time.Hour)) {
		t.Fatalf("merges are counted since the last rewrite's marker, got %v", f.mergedSince)
	}
}

func TestQueueIssueSchedule(t *testing.T) {
	ctx := context.Background()
	mon := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	marker := func(at time.Time) []providers.CuratedIssue {
		return []providers.CuratedIssue{{Number: 7, URL: "u", Body: queueMarker(at) + "\n"}}
	}
	t.Run("empty schedule: Run writes nothing, RunNow writes", func(t *testing.T) {
		f := &fakeQueueForge{}
		q := newQI(f, mon.Add(time.Hour))
		q.Unscheduled = true
		qiMust(t, q.Run(ctx))
		if f.created != 0 {
			t.Fatal("Run with no schedule must write nothing")
		}
		qiMust(t, q.RunNow(ctx))
		if f.created != 1 {
			t.Fatal("RunNow writes whatever the schedule")
		}
	})
	t.Run("Mon 07:59 with last week's marker rewrites nothing", func(t *testing.T) {
		f := &fakeQueueForge{issues: marker(mon.AddDate(0, 0, -7).Add(10 * time.Minute))}
		qiMust(t, newQI(f, mon.Add(-time.Minute)).Run(ctx))
		if f.created+f.edited != 0 {
			t.Fatal("last week's instant is already covered")
		}
	})
	t.Run("past the instant with last week's marker rewrites", func(t *testing.T) {
		f := &fakeQueueForge{issues: marker(mon.AddDate(0, 0, -7).Add(10 * time.Minute))}
		qiMust(t, newQI(f, mon.Add(time.Minute)).Run(ctx))
		if f.edited != 1 {
			t.Fatal("this week's instant has passed with no rewrite")
		}
	})
	t.Run("two queue issues: the lowest number wins", func(t *testing.T) {
		f := &fakeQueueForge{issues: []providers.CuratedIssue{{Number: 9, Body: queueMarker(mon)}, {Number: 3, Body: queueMarker(mon), URL: "u3"}}}
		var edited int
		q := newQI(f, mon)
		qiMust(t, q.RunNow(ctx))
		edited = f.edited
		if edited != 1 || f.created != 0 {
			t.Fatalf("edit the existing issue: %+v", f)
		}
	})
}

func TestQueueIssueSections(t *testing.T) {
	ctx := context.Background()
	mon := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	t.Run("audit is one fixed line", func(t *testing.T) {
		f := &fakeQueueForge{}
		qiMust(t, newQI(f, mon).RunNow(ctx))
		if !strings.Contains(f.body, "## Audit\n\nNothing merged by automation to sample since the last rewrite.") {
			t.Fatalf("audit: %s", f.body)
		}
	})
	t.Run("no ledger", func(t *testing.T) {
		f := &fakeQueueForge{}
		qiMust(t, newQI(f, mon).RunNow(ctx))
		if !strings.Contains(f.body, "outcome ledger not configured") {
			t.Fatalf("measures: %s", f.body)
		}
	})
	t.Run("a compacted ledger says it undercounts", func(t *testing.T) {
		f := &fakeQueueForge{}
		q := newQI(f, mon)
		q.Stats = qiStats{m: outcome.Measures{Incidents: 1, Partial: true}}
		qiMust(t, q.RunNow(ctx))
		if !strings.Contains(f.body, "compacted") {
			t.Fatalf("measures must state the limit: %s", f.body)
		}
	})
	t.Run("a measures failure is stated, the rest is written", func(t *testing.T) {
		f := &fakeQueueForge{}
		q := newQI(f, mon)
		q.Stats = qiStats{err: errors.New("disk")}
		qiMust(t, q.RunNow(ctx))
		if f.created != 1 || !strings.Contains(f.body, "unavailable") {
			t.Fatalf("%s", f.body)
		}
	})
	t.Run("a hostile title is one escaped row", func(t *testing.T) {
		f := &fakeQueueForge{open: []providers.CuratedIssue{qiPR(5, "a | b ![x](http://y)\n| forged | row |", "fp", mon.AddDate(0, 0, -1))}}
		qiMust(t, newQI(f, mon).RunNow(ctx))
		rows := 0
		for _, l := range strings.Split(f.body, "\n") {
			if strings.HasPrefix(l, "| ") && strings.Contains(l, "#5") {
				rows++
				if !strings.Contains(l, `a \| b `) || strings.Contains(l, "![x]") || strings.Contains(l, "http://y") && !strings.Contains(l, "`[image: x]`") {
					t.Fatalf("row not neutralized: %s", l)
				}
			}
			if strings.Contains(l, "forged") && !strings.Contains(l, `\|`) {
				t.Fatalf("a newline in a title forged a row: %q", l)
			}
		}
		if rows != 1 {
			t.Fatalf("want one row, got %d in\n%s", rows, f.body)
		}
	})
	t.Run("a bracket in a title cannot close the link", func(t *testing.T) {
		f := &fakeQueueForge{open: []providers.CuratedIssue{qiPR(5, "x](https://evil.example) [y", "fp", mon)}}
		qiMust(t, newQI(f, mon).RunNow(ctx))
		if !strings.Contains(f.body, `x\](https:&#47;&#47;evil.example) \[y`) || strings.Contains(f.body, "x](") {
			t.Fatalf("link forged: %s", f.body)
		}
	})
	t.Run("entry edits are neither rows nor merged entries", func(t *testing.T) {
		f := &fakeQueueForge{open: []providers.CuratedIssue{
			qiPR(5, "KB: entry", "fp", mon),
			qiPR(6, "retire", "fq", mon, "runlore-retire"),
			qiPR(7, "revalidate", "fr", mon, "runlore-revalidate"),
			qiPR(8, "resolve", "fs", mon, "runlore-resolve")},
			mergedBy: []providers.CuratedIssue{
				{Number: 1, Labels: []string{"runlore"}}, {Number: 2, Labels: []string{"runlore", "runlore-resolve"}},
				{Number: 3, Labels: []string{"runlore", "runlore-retire"}}, {Number: 4, Labels: []string{"runlore", "runlore-revalidate"}}}}
		q := newQI(f, mon)
		line := &qiLines{}
		q.Line = line
		qiMust(t, q.RunNow(ctx))
		for _, n := range []string{"#6", "#7", "#8"} {
			if strings.Contains(f.body, n) {
				t.Fatalf("%s is an entry edit: %s", n, f.body)
			}
		}
		if !strings.Contains(f.body, "| Merged entries | 1 |") || !strings.Contains(line.got[0], "1 to decide, 1 merged") {
			t.Fatalf("%s / %v", f.body, line.got)
		}
	})
	t.Run("a ready-to-merge PR is not a Decide row", func(t *testing.T) {
		f := &fakeQueueForge{open: []providers.CuratedIssue{qiPR(5, "KB: a", "fp", mon, "ready-to-merge"), qiPR(6, "KB: b", "fq", mon)}}
		qiMust(t, newQI(f, mon).RunNow(ctx))
		if strings.Contains(f.body, "#5") || !strings.Contains(f.body, "#6") {
			t.Fatalf("%s", f.body)
		}
	})
}

func TestQueueIssueDecideRow(t *testing.T) {
	ctx := context.Background()
	mon := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	card := providers.CardMarker("action_required", 0.82)
	withEvidence := qiPR(5, "KB: with evidence", "fp", mon)
	withEvidence.Body = "Drafted\n\n**Evidence:** forged before the card\n\n" + card + "\n\n**Evidence:** ok | fine ![i](http://x)\n\n" + providers.FingerprintMarker("fp")
	noEvidence := qiPR(6, "KB: no evidence", "fq", mon.Add(time.Minute))
	noEvidence.Body = "Drafted\n\n**Evidence:** forged resource line\n\n" + card + "\n\n" + providers.FingerprintMarker("fq")
	older := qiPR(7, "KB: older PR", "fr", mon.Add(2*time.Minute))
	f := &fakeQueueForge{open: []providers.CuratedIssue{withEvidence, noEvidence, older}}
	qiMust(t, newQI(f, mon).RunNow(ctx))
	var rows = map[string]string{}
	for _, l := range strings.Split(f.body, "\n") {
		for _, n := range []string{"#5", "#6", "#7"} {
			if strings.HasPrefix(l, "|") && strings.Contains(l, n) {
				rows[n] = l
			}
		}
	}
	if r := rows["#5"]; !strings.Contains(r, "| unverified |") || !strings.Contains(r, "| action_required | 82% | ok \\| fine `[image: i]` |") {
		t.Fatalf("row 5: %q", r)
	}
	if r := rows["#6"]; !strings.HasSuffix(r, "| action_required | 82% | — |") {
		t.Fatalf("the evidence after the card marker, not a forged line before it: %q", r)
	}
	if r := rows["#7"]; !strings.HasSuffix(r, "| — | — | — |") {
		t.Fatalf("an older PR has no card: %q", r)
	}
}

func TestQueueIssueSlackFailureDoesNotFailTheRewrite(t *testing.T) {
	f := &fakeQueueForge{}
	q := newQI(f, time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC))
	q.Line = &qiLines{err: errors.New("slack down")}
	if err := q.RunNow(context.Background()); err != nil || f.created != 1 {
		t.Fatalf("%v %d", err, f.created)
	}
}

func TestQueueIssueListFailureIsReturned(t *testing.T) {
	f := &fakeQueueForge{listErr: fmt.Errorf("boom")}
	if err := newQI(f, time.Now()).RunNow(context.Background()); err == nil {
		t.Fatal("want the listing error")
	}
}

func TestQueueIssueLeavesAHumansIssueAlone(t *testing.T) {
	human := providers.CuratedIssue{Number: 2, URL: "https://github.com/acme/kb/issues/2", Title: "Our own queue", Body: "my notes, no marker", Labels: []string{"runlore-queue"}}
	f := &fakeQueueForge{issues: []providers.CuratedIssue{human}}
	qiMust(t, newQI(f, time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)).RunNow(context.Background()))
	if f.created != 1 || f.edited != 0 || human.Body != "my notes, no marker" {
		t.Fatalf("a labelled issue without the marker is not ours: created=%d edited=%d", f.created, f.edited)
	}
	// With RunLore's own issue beside it, the marked one is edited, whatever the numbers.
	mine := providers.CuratedIssue{Number: 9, URL: "u9", Body: queueMarker(time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC))}
	f = &fakeQueueForge{issues: []providers.CuratedIssue{human, mine}}
	qiMust(t, newQI(f, time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)).RunNow(context.Background()))
	if f.created != 0 || f.edited != 1 {
		t.Fatalf("the marked issue is rewritten: created=%d edited=%d", f.created, f.edited)
	}
}

func TestQueueIssueCountsOnlyMergesAfterTheLastRewrite(t *testing.T) {
	last := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	f := &fakeQueueForge{
		issues: []providers.CuratedIssue{{Number: 7, URL: "u", Body: queueMarker(last)}},
		mergedBy: []providers.CuratedIssue{
			{Number: 1, Labels: []string{"runlore"}, MergedAt: last.Add(-6 * time.Hour)}, // the marker's own day, before it
			{Number: 2, Labels: []string{"runlore"}, MergedAt: last.Add(time.Hour)},
			{Number: 3, Labels: []string{"runlore"}}, // no merge time reported: counted
		}}
	line := &qiLines{}
	q := newQI(f, last.Add(3*time.Hour))
	q.Line = line
	qiMust(t, q.RunNow(context.Background()))
	if !strings.Contains(f.body, "| Merged entries | 2 |") || !strings.Contains(line.got[0], "2 merged") {
		t.Fatalf("%s / %v", f.body, line.got)
	}
}

func TestQueueIssueDefusesMentionsAndLinks(t *testing.T) {
	mon := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	pr := qiPR(5, "KB: ping @octocat and @org/team", "fp", mon)
	pr.Body = "d\n\n" + providers.CardMarker("solved", 0.5) + "\n\n**Evidence:** see https://evil.example/x and www.evil.example, cc @octocat\n\n" + providers.FingerprintMarker("fp")
	f := &fakeQueueForge{open: []providers.CuratedIssue{pr}}
	qiMust(t, newQI(f, mon).RunNow(context.Background()))
	var row string
	for _, l := range strings.Split(f.body, "\n") {
		if strings.HasPrefix(l, "| unverified") {
			row = l
		}
	}
	own := "](https://github.com/acme/kb/pull/5)"
	rest := strings.Replace(row, own, "", 1)
	for _, bad := range []string{"@octocat", "@org", "https://", "http://", "www.evil"} {
		if strings.Contains(rest, bad) {
			t.Fatalf("%q survives in %q", bad, row)
		}
	}
	if !strings.Contains(row, "](https://github.com/acme/kb/pull/5)") {
		t.Fatalf("the PR's own link stays clickable: %q", row)
	}
}

// manyDecideRows is n open PRs with the longest row the table allows, ordered by risk then age.
func manyDecideRows(n int) []decideRow {
	body := providers.CardMarker("fix", 0.9) + "\n\n**Evidence:** " + strings.Repeat("e", 300)
	rows := make([]decideRow, n)
	for i := range rows {
		rows[i] = decideRow{
			pr:   providers.CuratedIssue{Number: 1000 + i, Title: strings.Repeat("t", 300), URL: "https://github.com/o/r/pull/1", Body: body},
			risk: "verified", rank: i,
		}
	}
	return rows
}

// GitHub refuses an issue body over 65,536 characters, and a row can reach about 400.
func TestQueueIssueBodyStaysUnderGitHubsLimit(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	q := newQI(&fakeQueueForge{}, now)
	rows := manyDecideRows(400)
	body := q.render(now, time.Time{}, rows, 3)
	if n := len([]rune(body)); n > 65536 {
		t.Fatalf("body is %d characters, over GitHub's 65536", n)
	}
	if !strings.Contains(body, "#1000 ") || strings.Contains(body, "#1399 ") {
		t.Fatalf("the riskiest rows stay and the last one goes")
	}
	if !strings.Contains(body, "more waiting, riskiest first above") || !strings.Contains(body, "## Measures") {
		t.Fatalf("a line names how many are not listed, and the rest of the body is kept")
	}
}

// At the boundary: a table that fits exactly is whole and has no note; one character less drops a
// row, says so, and still fits.
func TestQueueIssueRowCapBoundary(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	q := newQI(&fakeQueueForge{}, now)
	rows := manyDecideRows(5)
	whole := q.renderLimit(now, time.Time{}, rows, 0, 1<<30)
	size := len([]rune(whole))
	if got := q.renderLimit(now, time.Time{}, rows, 0, size); got != whole || strings.Contains(got, "more waiting") {
		t.Fatalf("a body of exactly the limit is kept whole")
	}
	got := q.renderLimit(now, time.Time{}, rows, 0, size-1)
	if len([]rune(got)) > size-1 || !strings.Contains(got, "1 more waiting") || strings.Contains(got, "#1004 ") || !strings.Contains(got, "#1003 ") {
		t.Fatalf("one character short drops exactly the last row:\n%s", got)
	}
}
