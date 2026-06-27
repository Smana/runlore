# Provider Pagination + Truncation Signalling — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-24 |
| **Scope** | Make four read-only providers **paginate to a configured cap** instead of returning one API page, and — when the cap is hit — surface a **"results truncated at N"** marker in the tool output so the model knows it did not see everything. Providers: AWS CloudTrail `CloudChanges`, AWS `ResourceHealth` (EKS `ListNodegroups` + ASG `DescribeAutoScalingGroups`), GCP firewall `Drops`, VictoriaLogs `Query`. **Re-tuning the caps themselves and IP→workload resolution are out of scope.** |
| **Author** | Smana (designed with Claude) |
| **Related** | Item R8; `internal/providers/cloud/aws/cloudtrail.go`, `…/resourcehealth.go`, `internal/network/gcpfirewall/gcpfirewall.go`, `internal/logs/victorialogs/victorialogs.go`; the **already-correct** prior art `internal/network/awsvpc/awsvpc.go` (paginates to `maxEvents` via `NextToken`) and the tool-layer `renderRows` cap in `internal/investigate/query_tools.go` |

---

## 1. Why this exists

Several backends **silently truncate**: a bounded result returns successfully with **no "truncated" signal**, so the agent reasons over a capped window believing it is complete. There are two distinct failure modes, and the fix addresses both:

1. **Sub-page truncation (no pagination at all).** The provider makes a *single* API call and returns only the first page, even when the backend has more results in the window.
   - `cloudtrail.go:41` — `LookupEvents` is one unpaginated call; a CloudTrail page is **≤50 events**, then the code sorts and caps to `maxEvents=25`. If the window has >50 mutating events, pages 2+ are never fetched — **highest impact**, this is the cloud "what changed" timeline.
   - `resourcehealth.go:31` (`ListNodegroups`) and `:46` (`DescribeAutoScalingGroups`) — single calls; a cluster with more nodegroups/ASGs than one page silently shows a subset.
   - `gcpfirewall.go:108` — one `Entries.List(...).Do()`, ignores `NextPageToken`; capped at one `PageSize`.
   - `victorialogs.go:36` — hard `limit=100`, no `offset`; the 101st matching line is invisible.

2. **Cap reached without a signal.** Even where a cap is intentional (CloudTrail `maxEvents`, GCP `maxEvents`, the tool-layer `renderRows` cap of 50), the model gets a successful, complete-looking result with no indication that the cap was *binding* — i.e. that more matched upstream.

The prior-art `awsvpc.go` already does (1) correctly (it follows `NextToken` until `maxEvents`), but it does **not** do (2) — when it returns exactly `maxEvents` because the backend had more, the caller cannot tell. So truncation-signalling is genuinely new work; pagination is new for three of the four providers and already-present (but unsignalled) for CloudTrail's cap.

## 2. CHALLENGE — finding vs current code

Per-provider, both sides argued, with a verdict and `file:line`.

### 2.1 CloudTrail `CloudChanges` — **Confirmed (highest impact)** — `cloudtrail.go:41-55`
- **For the finding:** Exactly one `LookupEvents` call (`:41`). A page is ≤50 events; the SDK exposes `NextToken`/`MaxResults` and a `LookupEventsPaginator`, neither used. With >50 mutating events in the window, pages 2+ are silently dropped *before* the `maxEvents=25` cap even applies. The cap at `:52-54` then drops more, again with no signal. This is the cloud change-timeline the agent leans on when no Git change explains an incident — the worst place to under-report.
- **Against:** `LookupEvents` returns most-recent-first, and the code sorts most-recent-first before capping to 25 (`:49-54`), so for the common case (few events) the top-25 are correct. One could argue the single page is "good enough".
- **Verdict:** **Confirmed.** "Good enough" assumes <50 events and trusts undocumented page ordering across pages. The fix paginates to a cap and signals truncation. Not overstated.

### 2.2 `ResourceHealth` — `ListNodegroups` + `DescribeAutoScalingGroups` — **Partial** — `resourcehealth.go:31`, `:46`
- **For the finding:** Both are single unpaginated calls. `ListNodegroups` pages at ≤100; `DescribeAutoScalingGroups` at ≤100 (default `MaxRecords`). A large cluster legitimately exceeds these, and the per-nodegroup / per-ASG describe loop then silently covers only page 1.
- **Against:** Realistic EKS clusters have a handful of nodegroups and a modest number of ASGs; both default page sizes (100) are rarely exceeded in practice, so impact is lower than CloudTrail. `ResourceHealth` is also explicitly **best-effort** (a failing sub-query adds a line, not a hard error), so partial visibility is already the contract.
- **Verdict:** **Partial — implement, lower priority.** Correctness still matters (a 100+-ASG account is plausible), and pagination via the SDK paginators is cheap and consistent with CloudTrail. We paginate both with the *same* `maxEvents` cap budget and emit a truncation line through the same best-effort `add(...)` channel. We do **not** add a separate cap knob.

### 2.3 GCP firewall `Drops` — **Confirmed** — `gcpfirewall.go:108`
- **For the finding:** One `Entries.List(req).Do()`; `NextPageToken` on the response is ignored. `PageSize` is set to `maxEvents`, but the Cloud Logging API does **not** guarantee it returns `PageSize` entries per page — it may return fewer and a `NextPageToken`, so even *reaching* `maxEvents` requires following pages. Today a busy firewall-log window can return well under `maxEvents` purely because page 1 was short.
- **Against:** The `break` at `:125` already stops at `maxEvents`, so within a single fat page the cap is honoured. If the first page happens to be full, behaviour is acceptable.
- **Verdict:** **Confirmed.** Relying on a single page to be full is exactly the silent-truncation bug. Fix: follow `NextPageToken` (via `EntriesListCall.Pages`) until `maxEvents` collected or pages exhausted; signal truncation when the cap binds.

### 2.4 VictoriaLogs `Query` — **Confirmed** — `victorialogs.go:36`
- **For the finding:** `limit=100` hard-coded at construction (`:29`), sent as the only bound (`:36`); no `offset`, so line 101 is unreachable. VictoriaLogs' `/select/logsql/query` documents both `limit` and `offset` for pagination (`offset=M` skips the M entries with the largest `_time`). A high-volume error window is silently capped at 100.
- **Against:** 100 lines is often enough to spot a pattern, and the model can narrow the LogsQL query or shrink the window to see more. One could argue pagination just moves the cap.
- **Verdict:** **Confirmed.** "The model can narrow it" only works if the model *knows* it was truncated — which is precisely the missing signal. Fix: page with `limit`+`offset` up to a `maxLines` cap and emit a truncation marker when the cap binds. (Design note in §5 on offset-pagination stability.)

**Net:** all four are real; CloudTrail is highest impact, `ResourceHealth` is Partial/lower. None are SKIP.

## 3. Design

### 3.1 The truncation signal — design fork (locked)

The provider must tell the tool layer "I stopped at my cap and there was more". Options considered:

| Option | Verdict |
|---|---|
| **A. Change return types** to carry a `truncated bool` (e.g. `(LogResult, bool, error)` / `([]Change, bool, error)`). | **Rejected.** Touches every `providers.*Provider` interface + every fake + the prior-art `awsvpc`; large blast radius for a marker. |
| **B. New `Truncated` struct field** on `LogResult`/a result wrapper. | **Rejected.** `LogResult` is `[]LogLine`; wrapping it ripples through all consumers. |
| **C. In-band sentinel row** appended by the provider — a final `LogLine`/`Change` whose rendered text is the marker. | **Chosen.** Zero interface churn, the marker rides the existing slice into the existing `renderRows`, the model sees it verbatim. The provider owns the "I hit my cap" knowledge (the tool layer cannot distinguish a binding cap from an exactly-full result). |

**Locked = Option C.** Concretely:

- **Logs/network (`LogResult`)** — when a query collects `cap` lines *and the backend signals more* (another page/token exists, or a probe `limit=cap+1` returns `cap+1`), the provider appends one final synthetic `LogLine`:
  `{Message: "… results truncated at <cap> (more matched; narrow the query or window)"}` with no `Time`/`Fields`. It renders through `renderRows` like any other line.
- **`[]Change` (CloudTrail)** — same idea via a synthetic `Change`. To keep it readable through `cloud_tools.go`'s `Fprintf("%s %s %s/%s", When, ManagedBy, Kind, Name)`, the marker is carried in `Workload.Name` with a recognizable sentinel `Kind`, e.g. `Change{Workload: {Kind: "(truncated)", Name: "results truncated at N — more events matched; narrow the window/resource"}}`. It sorts to the end (zero `When` → oldest) — acceptable, and we document it.
- **`ResourceHealth` (best-effort lines)** — it already appends free-form lines via `add(...)`; on a binding page-cap it adds `"… EKS nodegroups truncated at N (more exist)"` / `"… ASGs truncated at N (more exist)"`. No sentinel struct needed.

A small shared helper keeps the marker text consistent:

```go
// truncationLine is the sentinel LogLine appended when a provider stops at its
// cap with more results upstream, so the model knows the view is partial.
func truncationLine(cap int) providers.LogLine {
	return providers.LogLine{Message: fmt.Sprintf("… results truncated at %d (more matched — narrow the query or shorten the window)", cap)}
}
```

Each provider keeps its own helper local (no new shared package) — the text is the only thing worth keeping consistent, and these providers don't otherwise share code.

### 3.2 CloudTrail — paginate to `maxEvents`, signal truncation (`cloudtrail.go`)

- Replace the single `LookupEvents` with `cloudtrail.NewLookupEventsPaginator(c.ct, in)` (the narrow `cloudTrailAPI` interface already satisfies `LookupEventsAPIClient` — one method, identical signature). Loop `HasMorePages()` / `NextPage(ctx)`, accumulating events.
- Stop once we've accumulated **more than `maxEvents`** events *or* pages are exhausted. We must over-collect by enough to know truncation occurred: keep paginating until either (a) pages exhausted, or (b) we have `> maxEvents` events. Then sort most-recent-first (unchanged), and if `len > maxEvents` set `truncated=true` and slice to `maxEvents`.
- Append the sentinel `Change` when `truncated`. Preserve the existing `LookupAttributes` (ReadOnly=false + optional ResourceName) — the paginator reuses the same input.
- Set `in.MaxResults` is unnecessary (default 50 page is fine); the paginator handles `NextToken` threading.

### 3.3 `ResourceHealth` — paginate nodegroups + ASGs (`resourcehealth.go`)

- `ListNodegroups`: use `eks.NewListNodegroupsPaginator(c.eks, &eks.ListNodegroupsInput{ClusterName: …})`; accumulate names up to a cap (`maxEvents`, reusing the client's existing budget), describe each as today. If pages remain when the cap is hit, `add("… EKS nodegroups truncated at %d (more exist)", cap)`.
- `DescribeAutoScalingGroups`: use `autoscaling.NewDescribeAutoScalingGroupsPaginator(c.asg, &autoscaling.DescribeAutoScalingGroupsInput{})`; iterate ASGs across pages, applying the existing cluster-substring filter, up to the cap; emit the ASG truncation line when the cap binds. The per-ASG `DescribeScalingActivities` (already `MaxRecords`-bounded to 5) is unchanged.
- The narrow `eksAPI` / `asgAPI` interfaces already satisfy the paginators' `*APIClient` interfaces (single-method, identical signatures), so no interface change.

### 3.4 GCP firewall — follow `NextPageToken` (`gcpfirewall.go`)

- Replace the single `.Do()` with `c.svc.Entries.List(req).Pages(ctx, func(resp) error {...})`, appending parsed lines and **returning a sentinel error to stop** once `len(out) >= maxEvents` (the standard `Pages` early-stop idiom), or letting it run to exhaustion. Because `Pages` may hand us a page that pushes us past the cap, we detect truncation as "we hit the cap AND there was at least one more entry we didn't take / another page existed".
- Simpler and equally correct: keep the manual loop — `req.PageToken(token)` + `.Do()` — accumulate until `len(out) >= maxEvents` or `NextPageToken == ""`. Set `truncated` when we stopped due to the cap with more available. **Chosen: manual `PageToken` loop** — it makes the cap/truncation logic explicit and mirrors `awsvpc.go`'s `NextToken` loop (house style), rather than threading state through a `Pages` callback.
- Append the sentinel `LogLine` when truncated.

### 3.5 VictoriaLogs — `limit`+`offset` paging to a cap (`victorialogs.go`)

- Add a `maxLines` cap field (the page `limit` stays 100; `maxLines` is the total budget). Page with `offset = 0, 100, 200, …`, each request `limit=100`, until a page returns `< limit` lines (exhausted) or we've collected `maxLines`.
- **Truncation probe:** request one extra — keep paging while `len(out) < maxLines`; if the page that *reaches/exceeds* `maxLines` was itself full (so more likely exist), trim to `maxLines` and append the sentinel line. Equivalent and simpler: after assembling, if we stopped because `len >= maxLines` (not because a short page ended the stream), mark truncated.
- `start`/`end` window params unchanged. The per-request `limit`/`offset` go in the same form body.

### 3.6 No change to the tool layer or interfaces

`renderRows` (cap 50) is unchanged — the sentinel line/Change flows through it. If a provider returns more than 50 rows *and* a sentinel, `renderRows` will itself note "… (N more)" and the sentinel may be the row that gets cut; to avoid the marker being hidden, providers cap at a value `≤` what the tool will show, OR we keep provider caps modest. **Decision:** keep provider caps at their current small values (CloudTrail 25, GCP 100) but note in §5 that when a provider cap exceeds `maxToolRows=50`, the sentinel can be elided by the tool-layer cap — acceptable because in that case `renderRows` *already* prints its own "… (N more)" truncation note, so the model is still told the view is partial. The two truncation signals are complementary, not redundant.

## 4. Components / seams

| Change | Location |
|---|---|
| Paginate `LookupEvents`; over-collect to detect cap; append sentinel `Change` | `internal/providers/cloud/aws/cloudtrail.go` |
| Paginate `ListNodegroups` + `DescribeAutoScalingGroups`; emit truncation lines | `internal/providers/cloud/aws/resourcehealth.go` |
| `NextPageToken` loop; append sentinel `LogLine` | `internal/network/gcpfirewall/gcpfirewall.go` |
| `limit`+`offset` paging to `maxLines`; append sentinel `LogLine` | `internal/logs/victorialogs/victorialogs.go` |
| Multi-page fakes + truncation-marker assertions | each provider's `*_test.go` |

No changes to `internal/providers/providers.go`, the tool layer, fakes' signatures, or `awsvpc.go`.

## 5. Trade-offs accepted in v1

- **In-band sentinel (Option C), not a typed signal.** A sentinel row is slightly hacky (a `LogLine`/`Change` that is not a real result) but avoids churning every provider interface and fake for a marker. The marker text is unambiguous and the row carries no `Time`/`Fields`, so it cannot be mistaken for data.
- **Caps are not re-tuned.** CloudTrail stays 25, GCP 100, VictoriaLogs 100/page; `ResourceHealth` reuses the AWS client's `maxEvents`. Re-fitting caps from live volume is a follow-up.
- **CloudTrail sentinel sorts to the end** (zero `When`). Acceptable: the marker is meant to be read as a trailing note, and `cloud_tools.go` renders it last.
- **Tool-layer cap (50) can elide the provider sentinel** when a provider cap > 50. Accepted because `renderRows` then prints its own "… (N more)" note — the model is still told the view is partial (§3.6). Today no provider cap exceeds 50 except GCP/VictoriaLogs at 100; for those the sentinel rides at index 100 and `renderRows` will already have noted truncation at 50, so the partial-view signal is preserved.
- **VictoriaLogs offset stability:** offset pagination orders by largest `_time`; concurrent ingestion during paging could shift the boundary. Accepted for v1 (incidents query a closed past window where ingestion of that range is effectively settled); documented in the code comment.
- **`ResourceHealth` ASG pagination still post-filters by cluster substring**, so the cap counts ASGs *examined*, not matched — the truncation line means "stopped scanning ASGs at N", which is the honest statement.

## 6. Testing

Per provider, table-driven, stdlib `testing`, no testify:

- **CloudTrail** — a fake `cloudTrailAPI` returning **two pages** via `NextToken`: (a) total ≤ `maxEvents` → all events returned, **no** sentinel; (b) total > `maxEvents` → exactly `maxEvents` real changes + a trailing sentinel `Change` whose `Workload.Kind == "(truncated)"`. Assert the paginator followed `NextToken` (second call carried the token).
- **`ResourceHealth`** — fake `eksAPI`/`asgAPI` returning two pages of nodegroups / ASGs: under cap → all described, no truncation line; over cap → capped + a `"… EKS nodegroups truncated"` / `"… ASGs truncated"` line present in the result.
- **GCP firewall** — `httptest` server returning a first page with a `nextPageToken` then a second page: under cap → all entries, no sentinel; over cap (`maxEvents` small in the test) → capped + sentinel `LogLine`. Assert the second request carried `pageToken`.
- **VictoriaLogs** — `httptest` server keyed on the `offset` form value: serves page 0 (100 lines) then page 1; with `maxLines` small in the test, assert all pages up to the cap are returned and the sentinel line appears when the cap binds; assert the request carried `offset`.
- **Whole tree:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run` all green before each commit.

## 7. Out of scope (follow-up)

- Re-tuning the caps / making them configurable per deployment.
- IP→workload (namespace/pod) resolution for GCP firewall / VPC flow logs.
- A typed truncation signal on the provider interfaces (only worth it if more consumers need to branch on it programmatically).
- Backfilling the same sentinel into `awsvpc.go` (it paginates correctly but does not signal a binding cap) — a natural, separate follow-up that should reuse this slice's marker text.
