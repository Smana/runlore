---
type: Incident
title: CoreDNS CrashLoopBackOff after malformed Corefile plugin directive
description: >-
  CoreDNS pods enter CrashLoopBackOff when the coredns ConfigMap contains an unknown or
  misspelled Corefile plugin directive, breaking in-cluster DNS resolution.
resource: kube-system/coredns
tags: [coredns, dns, corefile, crashloop, configmap]
timestamp: 2026-10-18T00:00:00Z
---

# Symptom

`coredns` pods in `kube-system` enter `CrashLoopBackOff` with `Error during parsing:
Unknown directive` in container logs.

# Root cause

A syntax error or typo in the `coredns` Corefile ConfigMap prevents CoreDNS from booting.

# Resolution

Revert or fix the offending directive in `kube-system/coredns` ConfigMap and verify the
CoreDNS rollout returns to Ready.
