// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

const (
	queueIssueLabel = "runlore-queue"
	queueIssueTitle = "Knowledge queue"
	queueMarkerHead = "<!-- runlore:knowledge-queue at="
	nothingNewLine  = "Nothing new since the last rewrite."
	auditLine       = "Nothing merged by automation to sample since the last rewrite."
)

// queueMarker is the hidden first line of the issue body: the instant of the rewrite. The command,
// the CronJob and the sweeper agree on "the last rewrite" by reading it back, with no store.
func queueMarker(at time.Time) string {
	return queueMarkerHead + at.UTC().Format(time.RFC3339) + " -->"
}

// parseQueueMarker reads the first marker of an issue body. RunLore writes it on the first line,
// and every untrusted segment below it has its markup defused, so it cannot be forged from there.
func parseQueueMarker(body string) (time.Time, bool) {
	i := strings.Index(body, queueMarkerHead)
	if i < 0 {
		return time.Time{}, false
	}
	rest := body[i+len(queueMarkerHead):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, rest[:j])
	return at, err == nil
}

// QueueIssue rewrites the one Knowledge queue issue of the KB repository: Decide (the open entry PRs a
// human must look at, riskiest first), Audit and Measures. Run follows the schedule; RunNow ignores it.
type QueueIssue struct {
	Forge    QueueIssueForge
	Evidence Evidence // nil: every PR reads as unverified
	// ScreenEntries runs the threat check on each row's entry, under factory.curation.defer only. A
	// field of its own: without defer, a ledger still sets Evidence for contested and verified.
	ScreenEntries bool
	Stats         interface {
		Measures(time.Time) (outcome.Measures, error)
	} // nil: Measures says the ledger is not configured
	// Reconcile, when set, is the Queue pass, run before the refresh, so a 👎 since the last sweep
	// withdraws the label and the PR reaches Decide. The agent leaves it nil: its Queue just ran.
	Reconcile Pass
	// Refresh, when set, rebuilds conflicting entry PRs before each rewrite: a rebuilt, verified PR is
	// ready-to-merge and absent from Decide, and a refused one is a row with risk conflict.
	Refresh *RefreshEntries
	Line    providers.LineNotifier // nil: no Slack line
	KBRepo  string
	Weekday time.Weekday
	Hour    int
	Minute  int
	// Unscheduled means curate.queue_issue.schedule is empty: Run writes nothing. A field of its own
	// because Sunday 00:00 is a valid schedule that the zero values would also spell.
	Unscheduled bool
	Now         func() time.Time
	Log         *slog.Logger
}

func (q QueueIssue) now() time.Time {
	if q.Now != nil {
		return q.Now().UTC()
	}
	return time.Now().UTC()
}

// scheduled is the latest scheduled instant at or before now.
func (q QueueIssue) scheduled(now time.Time) time.Time {
	back := (int(now.Weekday()) - int(q.Weekday) + 7) % 7
	at := time.Date(now.Year(), now.Month(), now.Day()-back, q.Hour, q.Minute, 0, 0, time.UTC)
	if at.After(now) {
		at = at.AddDate(0, 0, -7)
	}
	return at
}

// Run rewrites the issue when the last scheduled instant has passed with no rewrite since: the
// marker is older than that instant, or absent.
func (q QueueIssue) Run(ctx context.Context) error {
	if q.Unscheduled {
		return nil
	}
	now := q.now()
	issue, err := q.find(ctx)
	if err != nil {
		return err
	}
	if issue != nil {
		if at, ok := parseQueueMarker(issue.Body); ok && !at.Before(q.scheduled(now)) {
			return nil
		}
	}
	return q.rewrite(ctx, now, issue)
}

// RunNow rewrites the issue at once, whatever the schedule.
func (q QueueIssue) RunNow(ctx context.Context) error {
	issue, err := q.find(ctx)
	if err != nil {
		return err
	}
	return q.rewrite(ctx, q.now(), issue)
}

// find returns the open queue issue RunLore wrote, nil when there is none. With two, the lowest number wins.
func (q QueueIssue) find(ctx context.Context) (*providers.CuratedIssue, error) {
	issues, err := q.Forge.ListIssuesByLabel(ctx, queueIssueLabel)
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		return nil, nil
	}
	// Only an issue RunLore wrote, which carries the marker, is ours to rewrite: a human's issue
	// that happens to wear the label is left alone and a new one is filed.
	issues = slices.DeleteFunc(slices.Clone(issues), func(is providers.CuratedIssue) bool {
		_, ok := parseQueueMarker(is.Body)
		return !ok
	})
	if len(issues) == 0 {
		return nil, nil
	}
	best := slices.MinFunc(issues, func(a, b providers.CuratedIssue) int { return a.Number - b.Number })
	if len(issues) > 1 {
		q.Log.Warn("queue-issue: several open issues carry the queue label and marker; rewriting the lowest number", "issue", best.Number, "count", len(issues))
	}
	return &best, nil
}

func (q QueueIssue) rewrite(ctx context.Context, now time.Time, issue *providers.CuratedIssue) error {
	var last time.Time
	if issue != nil {
		last, _ = parseQueueMarker(issue.Body)
	}
	// In this order: the Queue may label a PR the refresh must then refuse, and it is the refresh's
	// refusal that takes the label off and puts the PR in Decide.
	if q.Reconcile != nil {
		if err := q.Reconcile.Run(ctx); err != nil {
			return err
		}
	}
	refused, err := refreshFirst(ctx, q.Refresh)
	if err != nil {
		return err
	}
	rows, err := q.decide(ctx, refused)
	if err != nil {
		return err
	}
	merged, err := q.merged(ctx, last)
	if err != nil {
		return err
	}
	body := q.render(now, last, rows, merged)
	var url string
	if issue == nil {
		_, ref, err := q.Forge.CreateIssue(ctx, queueIssueTitle, body, []string{queueIssueLabel})
		if err != nil {
			return err
		}
		url = ref.URL
	} else {
		if err := q.Forge.EditIssue(ctx, issue.Number, body); err != nil {
			return err
		}
		url = issue.URL
	}
	q.Log.Info("queue-issue: rewritten", "to_decide", len(rows), "merged", merged, "url", url)
	if q.Line != nil && url != "" {
		line := fmt.Sprintf("Knowledge queue, as of %s UTC: %d to decide, %d merged. %s", now.Format("2006-01-02 15:04"), len(rows), merged, url)
		if err := q.Line.DeliverLine(ctx, line); err != nil {
			q.Log.Warn("queue-issue: the Slack line was not delivered", "err", err)
		}
	}
	return nil
}

// decideRow is one open entry PR that waits for a human.
type decideRow struct {
	pr      providers.CuratedIssue
	risk    string
	rank    int
	created time.Time
	// withheld drops the PR's title and evidence from the row: either can hold what the check matched.
	withheld bool
}

// decide lists the open entry PRs not labelled ready-to-merge, riskiest first and oldest first within
// a risk. Retire, revalidate and resolve PRs are entry edits, never rows. refused holds the PRs the
// refresh would not rebuild. With ScreenEntries, each row's entry goes through the threat check.
func (q QueueIssue) decide(ctx context.Context, refused map[int]string) ([]decideRow, error) {
	prs, err := q.Forge.ListPRsByLabel(ctx, runloreLabel)
	if err != nil {
		return nil, err
	}
	var contested map[int]bool
	var verified map[string]bool
	if q.Evidence != nil {
		if contested, err = contestedPRs(q.Evidence, q.KBRepo); err != nil {
			return nil, err
		}
		verified = q.Evidence.Verifications()
	}
	var rows []decideRow
	for _, pr := range prs {
		if isEntryEditProposal(pr.Labels) || slices.Contains(pr.Labels, readyLabel) {
			continue
		}
		row := decideRow{pr: pr, risk: "verified", rank: math.MaxInt, created: pr.CreatedAt}
		var threats []string
		if q.ScreenEntries {
			var readErr error
			// An unread entry is withheld like a hit: a row is no label, and it may hold anything. It
			// says so, since accepted is no answer to a failure that clears by itself.
			if threats, readErr = entryThreats(ctx, q.Forge, q.Log, pr); readErr != nil {
				threats = []string{ruleEntryUnreadable + " (read failed, retrying)"}
			}
		}
		risks := Risks(pr, contested, verified, threats)
		if _, ok := refused[pr.Number]; ok {
			risks = append(risks, RiskConflict)
			slices.Sort(risks)
		}
		if len(risks) > 0 {
			row.risk, row.rank = risks[0].String(), int(risks[0])
		}
		if len(threats) > 0 {
			row.risk, row.withheld = RiskThreat.String()+": "+strings.Join(threats, ", "), true
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if !a.created.Equal(b.created) {
			return a.created.Before(b.created)
		}
		return a.pr.Number < b.pr.Number
	})
	return rows, nil
}

// merged counts the entries merged since the last rewrite. The forge's date filter has day
// granularity, so the exact instant is applied here from each PR's merge time.
func (q QueueIssue) merged(ctx context.Context, since time.Time) (int, error) {
	prs, err := q.Forge.ListMergedPRsByLabel(ctx, runloreLabel, since)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pr := range prs {
		if !isEntryEditProposal(pr.Labels) && (since.IsZero() || pr.MergedAt.IsZero() || !pr.MergedAt.Before(since)) {
			n++
		}
	}
	return n, nil
}

func (q QueueIssue) render(now, last time.Time, rows []decideRow, merged int) string {
	var b strings.Builder
	b.WriteString(queueMarker(now) + "\n\n")
	fmt.Fprintf(&b, "Rewritten %s UTC by `lore curate`.\n\n## Decide\n\n", now.Format("2006-01-02 15:04"))
	if !last.IsZero() && !slices.ContainsFunc(rows, func(r decideRow) bool { return r.pr.CreatedAt.After(last) }) {
		b.WriteString(nothingNewLine + "\n\n")
	}
	if len(rows) == 0 {
		b.WriteString("Nothing waits for a decision.\n")
	} else {
		b.WriteString("| Risk | Entry | Verdict | Confidence | Evidence |\n|---|---|---|---|---|\n")
		for _, r := range rows {
			verdict, conf := "—", "—"
			if v, c, ok := providers.ParseCardMarker(r.pr.Body); ok {
				verdict, conf = v, fmt.Sprintf("%d%%", int(math.Round(c*100)))
			}
			evidence := "—"
			if ev := providers.ParseCardEvidence(r.pr.Body); ev != "" && !r.withheld {
				evidence = cell(truncateRunes(ev, 200))
			}
			text := fmt.Sprintf("#%d", r.pr.Number)
			if !r.withheld {
				text = cell(truncateRunes(text+" "+r.pr.Title, 160))
			}
			if r.pr.URL != "" {
				text = "[" + text + "](" + r.pr.URL + ")"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.risk, text, cell(verdict), conf, evidence)
		}
	}
	b.WriteString("\n## Audit\n\n" + auditLine + "\n\n## Measures\n\n")
	b.WriteString("| Measure | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Merged entries | %d |\n| Queue age | %s |\n", merged, queueAge(rows, now))
	b.WriteString(q.ledgerMeasures(last))
	return b.String()
}

// ledgerMeasures renders the rows the outcome ledger answers, and a line saying why when it cannot.
func (q QueueIssue) ledgerMeasures(since time.Time) string {
	if q.Stats == nil {
		return "\nIncidents, instant recalls and tokens are not measured: outcome ledger not configured.\n"
	}
	m, err := q.Stats.Measures(since)
	if err != nil {
		q.Log.Warn("queue-issue: measures unavailable", "err", err)
		return "\nIncidents, instant recalls and tokens are not measured: outcome ledger unavailable.\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| Incidents | %d |\n", m.Incidents)
	recalls := strconv.Itoa(m.InstantRecalls)
	if m.InstantRecalls > 0 {
		recalls += fmt.Sprintf(", %d%% resolved", int(math.Round(100*float64(m.RecallsResolved)/float64(m.InstantRecalls))))
	}
	fmt.Fprintf(&b, "| Instant recalls | %s |\n", recalls)
	tokens := "—"
	if m.Incidents > 0 {
		tokens = strconv.Itoa(m.TokensPerIncident)
		if m.TokensPerIncident >= 1000 {
			tokens = strconv.Itoa((m.TokensPerIncident+500)/1000) + "k"
		}
	}
	fmt.Fprintf(&b, "| Tokens per incident | %s |\n", tokens)
	if m.Partial {
		b.WriteString("\nThe outcome ledger was compacted: incidents, instant recalls and tokens cover its retained events only, and undercount.\n")
	}
	return b.String()
}

// queueAge is the median and the oldest age, in whole days, of the rows' PRs.
func queueAge(rows []decideRow, now time.Time) string {
	var ages []int
	for _, r := range rows {
		if !r.created.IsZero() {
			ages = append(ages, int(now.Sub(r.created).Hours()/24))
		}
	}
	if len(ages) == 0 {
		return "—"
	}
	slices.Sort(ages)
	return fmt.Sprintf("median %dd, oldest %dd", ages[(len(ages)-1)/2], ages[len(ages)-1])
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// cell makes untrusted text safe in one table cell: one line, no image or markup a renderer would
// act on, no pipe that ends the cell, and no bracket that closes a link. A backtick of the text is
// replaced first, so the only code spans left are the ones the image neutralizer writes, which keep
// their brackets.
func cell(s string) string {
	s = strings.NewReplacer("`", "'", "\\", "/").Replace(strings.Join(strings.Fields(s), " "))
	s = okf.DefuseMarkup(s)
	segs := strings.Split(s, "`")
	for i, seg := range segs {
		seg = strings.ReplaceAll(seg, "|", `\|`)
		if i%2 == 0 {
			// An @ notifies a user or team, and a scheme or www. makes a link: both as entities.
			seg = strings.NewReplacer("@", "&#64;", "://", ":&#47;&#47;", "www.", "www&#46;", "WWW.", "WWW&#46;",
				"[", `\[`, "]", `\]`).Replace(seg)
		}
		segs[i] = seg
	}
	return strings.Join(segs, "`")
}
