<!--
Thanks for contributing to RunLore! Keep the change small and focused
(one concern per PR), and describe what changed and how you verified it.
-->

## Summary

<!-- What does this PR change, and why? -->

## Linked issue

<!-- e.g. Closes #123 — or "n/a" -->

## How was it verified?

<!-- Cite the gate results, and `hack/e2e-k3d.sh` if it touches a feature path. -->

## Checklist

<!-- The quality gate below is exactly what CI runs (.github/workflows/ci.yaml). -->

- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes
- [ ] `go test ./...` passes (`-race` where goroutines are involved)
- [ ] `gofmt -l .` prints nothing
- [ ] `golangci-lint run ./...` reports 0 issues
- [ ] Test added first (TDD) — the failing test came before the implementation
- [ ] Docs updated (README / `docs/`) if behavior or config changed
- [ ] Respects **read-only-first**: no new cluster writes; the only writes are to Git via reviewed PRs
