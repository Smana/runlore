# Action-Policy Namespace Gate at the `Review` Boundary — Test Design Spec

- **Date:** 2026-06-24
- **Status:** Draft (awaiting review)
- **Type:** Test-only (no behaviour change intended)
- **Author:** RunLore maintainers
- **Scope item:** R11 — the action-policy namespace allow/deny gate is untested at the `Review` boundary

## 1. Problem

The action autonomy ladder's safety logic lives in `internal/action/policy.go`.
The load-bearing namespace allow/deny gate (`namespaceViolation`, ~line 88) is the
single point that decides whether an *executable* action may reach a cluster
namespace. It enforces, in order:

1. `ns == ""` → `"target namespace required"`
2. `ns` in `builtinProtectedNamespaces` (`flux-system`, `kube-system`) **or** in
   `cfg.Allow.ProtectedNamespaces` → `"namespace <ns> is protected (never an action target)"`
3. `ns` not in `cfg.Allow.Namespaces` (allowlist; **empty allowlist permits nothing**)
   → `"namespace <ns> not in the action allowlist"`
4. otherwise compliant (`""`).

### Confirmed gap (CHALLENGE result)

- **`namespaceViolation` is never exercised through `Review` with executable actions.**
  `policy_test.go::TestReviewEnvelope` builds actions with **no `Op`** set
  (`Op==""`), so `violation` (policy.go:59-79) sees `executable == false` and
  returns `""` at line 73 **before** reaching `namespaceViolation` (line 79).
  The namespace branches are reached only indirectly via `auto.go:118`
  (`p.violation(act)` at the exec boundary), and `auto_test.go` covers **only**
  the built-in protected case (`TestAutoDeniesProtectedNamespace`).
- **No reason string is ever asserted.** `policy_test.go:40` asserts the *count*
  of withheld actions (`len(withheld) != 3`); `auto_test.go` asserts the auto
  annotation wrapper substring (`"denied"`, `"irreversible"`, …), not the
  policy's specific reason string. A bug producing the wrong reason — or
  mis-scoping the namespace gate at `Review` — passes today.

This is the only execution-authorization decision above the read-only rung, so a
silent regression here is a direct safety failure.

## 2. Goals

- Drive `namespaceViolation` through the **public `Review` API** (not by calling
  the unexported `violation`/`namespaceViolation` directly), using **executable**
  actions (`Op` set to a real registry op, `Target.Kind` and namespace set) so the
  test mirrors production data flow and survives refactors of the private helpers.
- Cover the four namespace outcomes: **allowed**, **denied (not in allowlist)**,
  **built-in protected** (`flux-system` and `kube-system`), **operator-configured
  protected**, **empty namespace**, and **empty allowlist (permits nothing)**.
- Assert **both** the keep/withhold decision **and** the exact reason string
  embedded in the `withheld` entry.

## 3. Non-goals / out of scope

- No production-code behaviour change unless a real bug is found while writing the
  tests (the gate is expected to behave identically at `Review` and `auto`,
  because `auto` calls the same `violation`).
- No new mocks: the gate is pure (`config.ActionPolicy` in, decision out), so the
  tests use real `Policy.Review` end to end.

## 4. Test design

Table-driven `TestReviewNamespaceGate` in `internal/action/policy_test.go`.

Each row: an executable action (`Op: "suspend"`, `Target.Kind: "Kustomization"`,
varying `Namespace`) + a `config.ActionAllow` (allowlist / protected list) →
expected `kept` length + expected `withheld` reason substring.

`Review` wraps the reason as `"<label> (<reason>)"` (policy.go:46), so assertions
use `strings.Contains(withheld[0], wantReason)` on the wrapped string.

Cases:

| namespace     | Allow.Namespaces        | Allow.ProtectedNamespaces | expect      | reason substring |
|---------------|-------------------------|---------------------------|-------------|------------------|
| `apps`        | `[apps]`                | —                         | kept        | —                |
| `restricted`  | `[apps]`                | —                         | withheld    | `not in the action allowlist` |
| `flux-system` | `[flux-system]`*        | —                         | withheld    | `is protected`   |
| `kube-system` | `[kube-system]`*        | —                         | withheld    | `is protected`   |
| `security`    | `[security]`            | `[security]`              | withheld    | `is protected`   |
| `""` (empty)  | `[apps]`                | —                         | withheld    | `target namespace required` |
| `apps`        | `[]` (empty allowlist)  | —                         | withheld    | `not in the action allowlist` |

\* allowlisting `flux-system`/`kube-system` proves the **built-in protected deny
takes precedence over the operator allowlist** (defense in depth).

A second focused test (`TestReviewExecutableNeedsTargetKind`) confirms an
executable action with a namespace but **no kind** is withheld with
`"executable action needs a target kind"` — closing the adjacent untested branch
(policy.go:76-78) that guards the namespace gate.

## 5. Verification

`go build/vet/test ./...`, `gofmt -l .`, `golangci-lint run ./...` (0 issues),
`go test -race ./internal/action/`.
