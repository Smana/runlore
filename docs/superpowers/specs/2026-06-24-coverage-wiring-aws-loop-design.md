# R25 — Coverage on blind spots + loop error-path tests

## Problem (as reviewed)

The review flagged low coverage in load-bearing-but-untested seams:

- `internal/providers/cloud/aws` (reported ~31%): adapters for CloudTrail / EKS / ASG / resource
  health untested.
- `cmd/lore` (reported ~1%): the `serve`/wiring path essentially untested.
- `internal/investigate` loop error paths: the model-error wrap (`loop.go`, `fmt.Errorf("model: %w", err)`)
  and `parseFindings` malformed-JSON path (`tools.go`) lacked dedicated tests.

## CHALLENGE — measured baseline (this tree)

Aggregate `go test -cover` on the actual tree differs sharply from the review's figures:

| Package                              | Reported | Measured (before) |
|--------------------------------------|----------|-------------------|
| `internal/providers/cloud/aws`       | ~31%     | **72.5%**         |
| `cmd/lore`                           | ~1%      | **5.0%**          |
| `internal/investigate`               | —        | **79.7%**         |

So the package-level numbers are stale (likely written against an earlier tree that already grew
`cloudtrail_*`/`resourcehealth_*` tests). But the *specific seams* the review names are genuinely
uncovered — per-function `go tool cover` confirms:

- AWS: `resourcehealth.go` `instanceState` **0%**, `summaryStatus` **0%** (the EC2 `i-…` selector branch
  is never exercised), `nodegroupHealth` **33%** (health-issues rendering untested), and the
  describe-failure error lines (nodegroup describe fail, ASG describe fail, EC2 status query fail) have
  no test.
- `internal/investigate`: the non-deadline model-error wrap (`return fmt.Errorf("model: %w", err)`) and the
  `parseFindings` malformed-JSON path (both the unit error and the loop's "feed the error back to the
  model as a tool result" handling) have no dedicated test.
- `cmd/lore`: `buildModelAndTools` (the shared wiring seam) is never built in a test — no smoke test.

**Verdict:** real, narrow gaps inside otherwise-covered packages. Close the named seams; do not chase the
stale aggregate numbers.

## Approach (test-first, stdlib `testing`, table-driven, no testify)

1. **AWS resource-health adapters** — extend the existing fakes (narrow interface injection already in
   place) to cover:
   - EC2 instance-status path: selector `i-…` → `DescribeInstanceStatus` → renders
     `EC2 … state=… system=… instance=…` (exercises `instanceState` + `summaryStatus`, incl. the nil arms).
   - Error lines (best-effort degradation, no hard failure): nodegroup `DescribeNodegroup` failure,
     ASG `DescribeAutoScalingGroups` failure, EC2 `DescribeInstanceStatus` failure — each adds an error
     line and `ResourceHealth` still returns `nil` error.
   - `nodegroupHealth` rendering when the nodegroup carries health issues.

2. **`internal/investigate` loop error paths**
   - Model error (non-deadline): a model whose `Complete` returns a plain error → `Investigate` returns a
     wrapped `model: …` error (and metrics record `result=error`), distinct from the deadline path which
     delivers a synthetic result and returns nil.
   - `parseFindings` malformed JSON: direct unit test that a non-JSON / truncated args string returns a
     `parse findings:` error; plus a loop-level test that a `submit_findings` call with malformed args is
     fed back to the model as a `tool` error message and the loop continues (recovers on the next turn).

3. **`parseFindings` tolerance (low-risk enhancement)** — some OpenAI-compatible backends double-encode or
   code-fence the tool-call arguments (a JSON string wrapping the real object, or a ```json fenced block).
   Add a small, defensive pre-clean: strip a leading/trailing ```json fence, and if the payload parses as a
   JSON *string*, unwrap one level and retry. Only applied as a fallback *after* a direct parse fails, so
   well-formed payloads are untouched. Covered by tests for fenced + double-encoded inputs and a still-bad
   input that must still error.

4. **`cmd/lore` wiring smoke test** — call `buildModelAndTools` with a representative minimal config
   (a model provider set, no gitops, no cluster/network/cloud) and assert it returns a non-nil model and a
   tool slice without panicking. `KUBECONFIG` pointed at a nonexistent path so the kube probe fails fast and
   deterministically (returns nil, tools just omit the cluster set). This is a focused wiring/smoke test, not
   a `serve`-loop test.

## Out of scope

- The full `serve` loop, leader election, HTTP wiring (already partially covered by `main_test.go`).
- Chasing aggregate package coverage numbers beyond the named seams.

## Gate (before each commit)

`go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./... --enable gosec`
