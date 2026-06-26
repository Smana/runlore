# Changelog

All notable changes to RunLore are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

RunLore is **pre-1.0 and under active development** — there are no tagged releases
yet, so everything currently lives under `[Unreleased]`.

## [Unreleased]

### Added

- **React → Investigate → Learn loop.** A read-only-first SRE agent that triggers on
  Alertmanager alerts and GitOps-failure events, runs a ReAct investigation
  (`what_changed` → `kb_search` → `submit_findings`), and posts a confidence-scored
  root cause with an evidence trail and suggested next steps.
- **What-changed-first RCA.** Git revision diffing surfaces the exact change behind an
  incident, via the configured GitOps engine.
- **GitOps providers.** Flux (informer-backed) and Argo CD, behind an
  engine-agnostic provider contract.
- **Metrics-agnostic signals.** VictoriaMetrics and Prometheus, with pluggable logs
  (VictoriaLogs) and CNI-agnostic network flows (Hubble).
- **Pluggable models.** Anthropic, Gemini, and any OpenAI-compatible endpoint, in or
  out of cluster.
- **The learning loop.** Every investigation drafts a knowledge-base entry as a GitHub
  pull request into a repo you own (OKF-compatible markdown); merged entries are
  indexed (bleve) so the same incident gets instant recall next time. Curation is
  confidence-routed.
- **Honest-about-uncertainty RCA.** `unresolved` is a first-class answer, and an
  adversarial *verify* pass can only ever lower a finding's confidence.
- **Notifications.** Slack and Matrix notifiers with fan-out.
- **Delivery.** A single Go binary deployed via Helm; `lore serve` (in-cluster,
  webhook-driven) and `lore investigate` (one-shot CLI). Leader election for HA.
- **Eval harness.** `lore eval` replays recorded incident cases and reports the
  root-cause-identification rate; a nightly CI eval guards against RCA regressions.

[Unreleased]: https://github.com/Smana/runlore/commits/main
