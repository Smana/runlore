# Provider Pagination + Truncation Signalling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make four read-only providers paginate to a configured cap instead of returning one API page, and append an in-band "results truncated at N" marker when the cap binds — so the model never silently reasons over a capped window.

**Architecture:** Each provider follows its backend's pagination primitive (`aws-sdk-go-v2` paginators / GCP `NextPageToken` / VictoriaLogs `limit`+`offset`) up to its existing cap, over-collecting just enough to *detect* that the cap is binding, then appends a synthetic sentinel `LogLine`/`Change` carrying the marker (design Option C — zero interface churn; the marker rides the existing slice through `renderRows`). No changes to provider interfaces, fakes' signatures, the tool layer, or the already-correct `awsvpc.go`.

**Tech Stack:** Go 1.26, `aws-sdk-go-v2` (cloudtrail/eks/autoscaling paginators), `google.golang.org/api/logging/v2`, stdlib `net/http`+`net/http/httptest`, stdlib `testing` (no testify), table-driven.

**Spec:** `dev/superpowers/specs/2026-06-24-provider-pagination-design.md`

**Branch:** `feat/provider-pagination` (checked out; spec+plan committed first).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/providers/cloud/aws/cloudtrail.go` | CloudTrail change timeline | Paginate via `NewLookupEventsPaginator`; over-collect; sentinel `Change` |
| `internal/providers/cloud/aws/aws_test.go` | AWS provider tests | Multi-page fake CT; under-cap (no sentinel) + over-cap (sentinel) cases |
| `internal/providers/cloud/aws/resourcehealth.go` | Cloud resource health | Paginate `ListNodegroups` + `DescribeAutoScalingGroups`; truncation lines |
| `internal/providers/cloud/aws/resourcehealth_test.go` (**new**) | ResourceHealth tests | Multi-page eks/asg fakes; truncation-line assertions |
| `internal/network/gcpfirewall/gcpfirewall.go` | GCP firewall drops | `NextPageToken` loop; sentinel `LogLine` |
| `internal/network/gcpfirewall/gcpfirewall_test.go` | GCP firewall tests | Two-page httptest; sentinel + `pageToken` assertions |
| `internal/logs/victorialogs/victorialogs.go` | VictoriaLogs query | `limit`+`offset` paging to `maxLines`; sentinel `LogLine` |
| `internal/logs/victorialogs/victorialogs_test.go` | VictoriaLogs tests | Offset-keyed httptest; sentinel + `offset` assertions |

**Task order & independence:** Task 0 (spec+plan commit) first. Tasks 1–4 (one per provider) are mutually independent — each is committed as soon as its full gate is green (durability requirement). Task 5 is the final whole-tree gate.

**Per-commit gate (MUST be green before every commit):**
```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
```

---

### Task 0: Commit the spec + plan

- [ ] **Step 1: Commit** (so a session cut-off can't lose the design)
```bash
git add dev/superpowers/specs/2026-06-24-provider-pagination-design.md dev/superpowers/plans/2026-06-24-provider-pagination.md
git commit -m "docs(providers): pagination + truncation-signalling spec + plan"
```

---

### Task 1: CloudTrail — paginate + signal truncation (highest impact)

**Files:** `internal/providers/cloud/aws/cloudtrail.go`, `internal/providers/cloud/aws/aws_test.go`

- [ ] **Step 1: Extend the fake to serve multiple pages (failing tests first)**

In `aws_test.go`, make `fakeCT` page-aware: it holds `[]*cloudtrail.LookupEventsOutput` (each with optional `NextToken`) and returns them in order, recording each input's `NextToken`. Add two subtests:
- `under cap`: total events ≤ `maxEvents` across two pages → `len(changes)` equals the real total, **no** change has `Workload.Kind == "(truncated)"`.
- `over cap`: total events > `maxEvents` (set `maxEvents` small, e.g. 2) → exactly `maxEvents` real changes **plus** a trailing sentinel change with `Workload.Kind == "(truncated)"` and a `Name` containing `"truncated at 2"`. Assert the second call's input `NextToken` equals the first page's token (paginator threaded it).

Keep the existing `TestCloudChanges` passing (single page, ≤ cap → unchanged behaviour, no sentinel).

- [ ] **Step 2: Run tests — verify they fail**
`go test ./internal/providers/cloud/aws/ -run TestCloudChanges`
Expected: FAIL (sentinel not produced / only first page read).

- [ ] **Step 3: Implement pagination + sentinel in `cloudtrail.go`**

Replace the single `c.ct.LookupEvents(ctx, in)` call (`:41`) with a paginator loop:
```go
p := cloudtrail.NewLookupEventsPaginator(c.ct, in)
var changes []providers.Change
truncated := false
for p.HasMorePages() {
	out, err := p.NextPage(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloudtrail lookup: %w", err)
	}
	for i := range out.Events {
		changes = append(changes, eventToChange(out.Events[i]))
	}
	// Over-collect by one cap+1 so we can prove the cap is binding; once we
	// have more than maxEvents, further pages cannot change the kept top-N.
	if len(changes) > c.maxEvents {
		truncated = true
		break
	}
}
```
Keep the existing sort-most-recent-first, then:
```go
if len(changes) > c.maxEvents {
	truncated = true
	changes = changes[:c.maxEvents]
}
if truncated {
	changes = append(changes, truncatedChange(c.maxEvents))
}
```
Add the helper:
```go
// truncatedChange is the sentinel appended when CloudChanges stops at its cap
// with more events upstream, so the model knows the timeline is partial. It is
// not a real event: Kind "(truncated)" is the recognizable marker, and a zero
// When sorts it to the end where cloud_tools renders it as a trailing note.
func truncatedChange(cap int) providers.Change {
	return providers.Change{
		Engine: providers.EngineAWS,
		Type:   providers.ChangeCloudAPI,
		Workload: providers.Workload{
			Kind: "(truncated)",
			Name: fmt.Sprintf("results truncated at %d — more events matched; narrow the window or resource", cap),
		},
	}
}
```
Note: the sentinel is appended **after** the sort+slice so it always lands last regardless of `When`.

- [ ] **Step 4: Run tests — verify they pass** `go test ./internal/providers/cloud/aws/`
- [ ] **Step 5: Full gate**
```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
```
- [ ] **Step 6: Commit**
```bash
git add internal/providers/cloud/aws/cloudtrail.go internal/providers/cloud/aws/aws_test.go
git commit -m "feat(aws): paginate CloudTrail LookupEvents + signal truncation"
```

---

### Task 2: ResourceHealth — paginate nodegroups + ASGs

**Files:** `internal/providers/cloud/aws/resourcehealth.go`, new `internal/providers/cloud/aws/resourcehealth_test.go`

- [ ] **Step 1: Write failing tests in a new `resourcehealth_test.go`**

Add page-aware fakes implementing the existing `eksAPI` / `asgAPI` interfaces (the AWS test file already lives in `package aws`, so reuse `ptr`). Two cases:
- nodegroups under cap → every nodegroup described (one result line each), no `"truncated"` line.
- nodegroups over cap (set a small cap on the `Client`) → capped count described + a line containing `"EKS nodegroups truncated at"`. Same shape for ASGs (`"ASGs truncated at"`).

Use a `Client{eks: fakeEKS, asg: fakeASG, clusterName: "demo", maxEvents: 2}` and scan the returned `providers.LogResult` for the expected substrings.

- [ ] **Step 2: Run — verify fail** `go test ./internal/providers/cloud/aws/ -run TestResourceHealth`
- [ ] **Step 3: Implement pagination in `resourcehealth.go`**

Nodegroups (`:30-43`): replace `c.eks.ListNodegroups(...)` with `eks.NewListNodegroupsPaginator(c.eks, &eks.ListNodegroupsInput{ClusterName: ptr(c.clusterName)})`. Accumulate names; stop after `c.maxEvents` names with `truncated=true` if pages remain; describe each accumulated name as today. After the describe loop, `if truncated { add("… EKS nodegroups truncated at %d (more exist)", c.maxEvents) }`.

ASGs (`:46-62`): replace `c.asg.DescribeAutoScalingGroups(...)` with `autoscaling.NewDescribeAutoScalingGroupsPaginator(c.asg, &autoscaling.DescribeAutoScalingGroupsInput{})`. Iterate ASGs across pages; for each, apply the existing cluster-substring filter and render as today; count ASGs **examined** and stop at `c.maxEvents` (set `truncated`), then `add("… ASGs truncated at %d (more exist)", c.maxEvents)`. Keep the per-ASG `DescribeScalingActivities` call unchanged.

Reuse the AWS client's existing `maxEvents` field as the shared budget (no new knob).

- [ ] **Step 4: Run — verify pass** `go test ./internal/providers/cloud/aws/`
- [ ] **Step 5: Full gate** (same five-command gate)
- [ ] **Step 6: Commit**
```bash
git add internal/providers/cloud/aws/resourcehealth.go internal/providers/cloud/aws/resourcehealth_test.go
git commit -m "feat(aws): paginate ResourceHealth nodegroups + ASGs + signal truncation"
```

---

### Task 3: GCP firewall — follow NextPageToken

**Files:** `internal/network/gcpfirewall/gcpfirewall.go`, `internal/network/gcpfirewall/gcpfirewall_test.go`

- [ ] **Step 1: Write failing tests**

Extend the httptest handler to serve two responses: first carries `nextPageToken`, second does not; key on the `pageToken` query/body param so page 2 returns different entries. Cases:
- under cap (`maxEvents` ≥ total) → all entries from both pages, no sentinel.
- over cap (set `c.maxEvents` small via the constructor default override in the test, or a tiny `maxEvents`) → capped + a final `LogLine` whose `Message` contains `"results truncated at"`. Assert page 2's request carried the `pageToken`.

(The existing single-page `TestDrops` stays green — one page, no next token, no sentinel.)

- [ ] **Step 2: Run — verify fail** `go test ./internal/network/gcpfirewall/`
- [ ] **Step 3: Implement the NextPageToken loop in `gcpfirewall.go`**

Replace the single `.Do()` (`:108`) with a manual page loop mirroring `awsvpc.go`:
```go
out := make(providers.LogResult, 0, c.maxEvents)
truncated := false
token := ""
for {
	call := c.svc.Entries.List(req).Context(ctx)
	if token != "" {
		call = call.PageToken(token)
	}
	resp, err := call.Do()
	if err != nil {
		return nil, fmt.Errorf("list firewall log entries: %w", err)
	}
	for _, e := range resp.Entries {
		// existing parse/append …
		if int64(len(out)) >= c.maxEvents {
			truncated = resp.NextPageToken != "" // more pages OR more on this page
			break
		}
	}
	if int64(len(out)) >= c.maxEvents || resp.NextPageToken == "" {
		if int64(len(out)) >= c.maxEvents && resp.NextPageToken != "" {
			truncated = true
		}
		break
	}
	token = resp.NextPageToken
}
if truncated {
	out = append(out, truncationLine(int(c.maxEvents)))
}
```
(Refine the truncated-detection so it is true only when the cap stopped us with more available; keep it simple and correct.) Add a local `truncationLine(cap int) providers.LogLine` helper with the standard marker text. The `req.PageSize` stays `maxEvents`.

- [ ] **Step 4: Run — verify pass** `go test ./internal/network/gcpfirewall/`
- [ ] **Step 5: Full gate**
- [ ] **Step 6: Commit**
```bash
git add internal/network/gcpfirewall/gcpfirewall.go internal/network/gcpfirewall/gcpfirewall_test.go
git commit -m "feat(gcpfirewall): follow NextPageToken + signal truncation"
```

---

### Task 4: VictoriaLogs — limit+offset paging to a cap

**Files:** `internal/logs/victorialogs/victorialogs.go`, `internal/logs/victorialogs/victorialogs_test.go`

- [ ] **Step 1: Write failing tests**

Extend the httptest handler to read the `offset` form value and serve page 0 (a full `limit`-sized page) then page 1 (the remainder). Cases:
- under cap → all lines from both pages, no sentinel.
- over cap (small `maxLines` set on the test `Client`) → capped lines + a final `LogLine` containing `"results truncated at"`. Assert a request carried `offset=<page size>`.

Keep `TestQuery` green (single short page → no second request, no sentinel).

- [ ] **Step 2: Run — verify fail** `go test ./internal/logs/victorialogs/`
- [ ] **Step 3: Implement paging in `victorialogs.go`**

Add a `maxLines int` field (default e.g. 1000) alongside the existing per-page `limit` (100). In `Query`, loop:
```go
var out providers.LogResult
truncated := false
for offset := 0; ; offset += c.limit {
	page, err := c.queryPage(ctx, query, w, c.limit, offset) // factor the request/parse out
	if err != nil { return nil, err }
	out = append(out, page...)
	if len(out) >= c.maxLines {
		out = out[:c.maxLines]
		truncated = len(page) == c.limit // a full last page implies more may exist
		break
	}
	if len(page) < c.limit { // short page → stream exhausted
		break
	}
}
if truncated {
	out = append(out, truncationLine(c.maxLines))
}
```
Factor the existing request-build + `parseNDJSON` into `queryPage(ctx, query, w, limit, offset)` that sets `form` keys `query`, `limit`, `offset`, `start`, `end`. Add the local `truncationLine` helper. Comment the offset-stability caveat (spec §5).

- [ ] **Step 4: Run — verify pass** `go test ./internal/logs/victorialogs/`
- [ ] **Step 5: Full gate**
- [ ] **Step 6: Commit**
```bash
git add internal/logs/victorialogs/victorialogs.go internal/logs/victorialogs/victorialogs_test.go
git commit -m "feat(victorialogs): limit+offset pagination + signal truncation"
```

---

### Task 5: Whole-tree verification

- [ ] **Step 1: Full gate, clean tree**
```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
```
Expected: build clean; all tests PASS; `gofmt -l .` prints nothing; lint `0 issues`.
- [ ] **Step 2: Confirm no provider interface / fake signature changed** — `git diff main -- internal/providers/providers.go` is empty; `awsvpc.go` untouched.

No commit (verification only).

---

## Notes for the implementer

- **Do not change** `internal/providers/providers.go`, the tool layer (`internal/investigate/*_tools.go`), or `awsvpc.go`. The sentinel rides the existing return slices.
- **Over-collect to detect a binding cap**, then trim to the cap before appending the sentinel — a result of *exactly* the cap with no more upstream must NOT emit a sentinel (that is the false-positive to avoid; the under-cap test guards it).
- The narrow `cloudTrailAPI` / `eksAPI` / `asgAPI` interfaces already satisfy the SDK paginators' `*APIClient` interfaces (verified: single method, identical signature) — no interface change is needed.
- Caps are **not** re-tuned: CloudTrail 25, GCP 100, VictoriaLogs page 100 / total `maxLines`, ResourceHealth reuses `maxEvents`.
- Marker text stays consistent across providers: `"… results truncated at N (more matched — narrow the query or shorten the window)"` for `LogLine`s; the `Change` sentinel uses `Kind:"(truncated)"`.
