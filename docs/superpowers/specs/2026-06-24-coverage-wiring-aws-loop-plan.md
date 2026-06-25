# R25 — Implementation plan

Test-first, commit incrementally. Gate green before each commit:
`go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./... --enable gosec`.

## Step 0 — spec + plan (this commit)
Write design + plan docs capturing the CHALLENGE (measured baseline vs reported).

## Step 1 — AWS resource-health adapter tests
- Extend `resourcehealth_test.go` fakes so they can return errors and serve EC2 instance status.
  - Add `descErr` to `fakeEKS` (DescribeNodegroup fails) and a health-issue describe variant.
  - Add `listErr`/`descErr` to `fakeASG` (DescribeAutoScalingGroups fails).
  - Add a `fakeEC2` implementing `ec2API` (DescribeInstanceStatus, plus the unused DescribeInstances).
- Tests:
  - `TestResourceHealthEC2InstanceStatus`: selector `i-…` → renders state/system/instance; covers
    `instanceState`/`summaryStatus` incl. nil arms.
  - `TestResourceHealthErrorLines`: nodegroup describe fail, ASG describe fail, EC2 status fail → error
    line present, `err == nil` (best-effort).
  - `TestResourceHealthNodegroupHealthIssues`: a nodegroup with health issues → `health=[…]` rendered.
- Commit: `test(aws): cover EC2 instance-status + resource-health error paths`.

## Step 2 — investigate loop error paths
- `loop_test.go`: add an `errModel` (Complete returns a plain error). `TestInvestigateModelError`
  asserts `Investigate` returns a non-nil error wrapping `model:` (and is NOT swallowed like the deadline path).
- `loop_test.go`: `TestLoopRecoversFromMalformedFindings` — script a malformed `submit_findings` then a valid
  one; assert the loop feeds the parse error back and recovers, delivering the valid finding.
- Commit: `test(investigate): cover model-error wrap + malformed-findings recovery`.

## Step 3 — parseFindings malformed JSON + tolerance
- `tools_test.go`: `TestParseFindingsMalformed` (bad JSON → `parse findings:` error).
- Implement defensive pre-clean in `parseFindings` (fence strip + one-level string unwrap, fallback only).
- `tools_test.go`: `TestParseFindingsTolerant` (fenced + double-encoded inputs parse; still-bad input errors).
- Commit: `feat(investigate): tolerate fenced/double-encoded findings args + tests`.

## Step 4 — cmd/lore wiring smoke test
- `main_test.go` (or `wiring_test.go`): `TestBuildModelAndToolsSmoke` — minimal config, `KUBECONFIG` set to a
  nonexistent path, assert non-nil model + non-panicking tool slice.
- Commit: `test(lore): wiring smoke test for buildModelAndTools`.

## Step 5 — measure after, finalize
Re-run `go test -cover` on the three packages; record before/after in the report.
