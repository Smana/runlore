# Helm probe timing — implementation plan (R21)

Design: `2026-06-24-helm-probe-timing-design.md`

## Steps

1. **Spec + plan** (this + the design doc). Commit. ✅
2. **values.yaml** — add a `probes:` block (startup/liveness/readiness) with the
   defaults from the design. Document the rationale inline (startupProbe owns the
   cold window; readiness stays tight for leader-handoff detection; liveness not
   loosened). Commit.
3. **deployment.yaml** — render a `startupProbe` (guarded by
   `.Values.probes.startup.enabled`) on `/healthz`, and switch the liveness +
   readiness probes to read their cadence from `.Values.probes.*` with `default`
   fallbacks. Drop `initialDelaySeconds` (startupProbe owns the warmup). Commit.
4. **Validate** — `helm lint`; `helm template` across the value combos in the
   design; pipe each render through `yq` to assert valid YAML and inspect the
   probe block. Commit nothing if clean (validation is read-only) — record output
   in the report.

## Done when

- `helm lint deploy/helm/runlore` passes.
- `helm template` shows the startupProbe (default) and a sane readiness probe;
  `probes.startup.enabled=false` drops only the startupProbe; all renders are
  valid YAML.
- Liveness probe still trips on a real hang (cadence unchanged, only made
  explicit + configurable).
