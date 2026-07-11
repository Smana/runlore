# Plan — Action-Policy Namespace Gate at `Review` (R11, test-only)

Spec: `dev/superpowers/specs/2026-06-24-action-policy-namespace-test-design.md`

## Steps

1. **CHALLENGE (done).** Confirmed `namespaceViolation` is reached only via
   `auto.go`; `policy_test.go::TestReviewEnvelope` uses `Op==""` suggestions, so
   the namespace branches are never hit at `Review`. No reason strings asserted.
   Gap real.

2. **Spec + plan** (this commit).

3. **Add tests** to `internal/action/policy_test.go`:
   - `TestReviewNamespaceGate` — table-driven, executable actions through `Review`,
     covering allowed / denied / built-in-protected (flux-system + kube-system) /
     operator-protected / empty-namespace / empty-allowlist. Assert keep/withhold
     count + exact reason substring.
   - `TestReviewExecutableNeedsTargetKind` — executable action, no kind → withheld
     with the kind reason.

4. **Gate green** before commit: `go build/vet/test ./...`, `gofmt -l .`,
   `golangci-lint run ./...` (0 issues), `go test -race ./internal/action/`.

5. **Commit** tests. No co-author / attribution trailers.

## Bug-handling

If a row reveals the gate behaves differently at `Review` vs `auto` (e.g. a
namespace branch is mis-scoped), fix `policy.go` minimally, document in the spec,
and add the regression assertion. Expectation: none, since `auto` calls the same
`violation`.
