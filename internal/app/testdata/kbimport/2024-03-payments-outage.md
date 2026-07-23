---
title: Payments API outage
date: 2024-03-14
tags: [payments, sev1]
resource: payments/api
type: postmortem
---

## Symptom

5xx spike on payments/api starting 09:12 UTC; PaymentsHighErrorRate alert fired.

## Investigate

- deploy 4a1f2c rolled out at 09:10
- error logs: connection refused to payments-db

## Cause

1. deploy 4a1f2c dropped the DB connection-pool env var

## Resolution

- rollback to the previous release; re-add the env var before re-deploying
