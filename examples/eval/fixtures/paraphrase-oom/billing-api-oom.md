---
type: Incident
title: billing-api OOMKilled exit 137 during invoice batch processing
description: >-
  billing-api containers hit their cgroup memory limit and exit with status 137
  (OOMKilled) whenever end-of-hour invoice PDF batches run concurrently.
resource: payments/billing-api
tags: [oom, oomkilled, memory, cgroup, billing-api]
timestamp: 2026-10-15T00:00:00Z
---

# Symptom

`billing-api` pods in namespace `payments` repeatedly restart with `OOMKilled` (exit code
`137`) and `Memory cgroup out of memory` kernel events.

# Root cause

Concurrent invoice PDF rendering spikes heap allocation above the container's `512Mi`
cgroup memory limit, triggering the Linux OOM killer.

# Resolution

Raise `payments/billing-api` memory limit to `1Gi` and cap `INVOICE_BATCH_WORKERS=2`.
