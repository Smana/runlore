---
type: Incident
title: AppWizardUnavailable
description: 'The alert rule AppWizardUnavailable (in /etc/vmalert/rules-out/rules-src-0/rules.yaml) references a non-existent Deployment name due to a typo: the expression queries kube_deployment_status_replicas_available{namespace="apps", deployment="app-wizzard"} (double-z), while the actual Deployment is named app-wizard (single-z). Because no Deployment named app-wizzard exists, the time series is always empty, so the absent() wrapper always evaluates to 1 (true), making the alert fire permanently regardless of the real app-wizard Deployment''s health. The actual app-wizard Deployment is fully healthy: status.availableReplicas=1, condition Available=True (MinimumReplicasAvailable) since 2026-10-27T07:37:46Z, pod app-wizard-979c7dcfd-v4nj4 Running ready=6/6.'
resource: apps/app-wizard
tags:
    - runlore
    - incident
    - deployment
    - apps
timestamp: "2026-10-29T06:28:30Z"
fingerprint: 5f91671c4ddbc6af62360bf68ada6e1971999f60d254304300acbc6b30c08185
confidence: 0.95
---

## Decision

- **why keep:** The alert rule AppWizardUnavailable (in /etc/vmalert/rules-out/rules-src-0/rules.yaml) references a non-existent Deployment name due to a typo: the expression queries kube_deployment_status_replicas_available{namespace="apps", deployment="app-wizzard"} (double-z), while the actual Deployment is named app-wizard (single-z). Because no Deployment named app-wizzard exists, the time series is always empty, so the absent() wrapper always evaluates to 1 (true), making the alert fire permanently regardless of the real app-wizard Deployment's health. The actual app-wizard Deployment is fully healthy: status.availableReplicas=1, condition Available=True (MinimumReplicasAvailable) since 2026-10-27T07:37:46Z, pod app-wizard-979c7dcfd-v4nj4 Running ready=6/6.
- **confidence:** 95%

## Symptom

AppWizardUnavailable

Affected resource: Deployment apps/app-wizard

## Investigate

- alert_rule('AppWizardUnavailable') returned expr: absent(kube_deployment_status_replicas_available{namespace="apps", deployment="app-wizzard"} > 0) — note deployment="app-wizzard" with double-z, file=/etc/vmalert/rules-out/rules-src-0/rules.yaml
- list_resources(kind=Deployment, namespace=apps) returned 5 objects including apps/app-wizard (single-z) — no app-wizzard exists
- resource_spec(Deployment, app-wizard, apps) returned status.availableReplicas=1, status.readyReplicas=1, condition Available=True reason=MinimumReplicasAvailable lastTransitionTime=2026-10-27T07:37:46Z
- query_metrics('kube_deployment_status_replicas_available{namespace="apps", deployment="app-wizzard"}') returned 'no series matched' — the typo label matches nothing
- query_metrics('absent(kube_deployment_status_replicas_available{namespace="apps", deployment="app-wizzard"} > 0)') returned {} = 1 — the alert expression evaluates to true because the inner selector is empty
- query_metrics('kube_deployment_status_replicas_available{namespace="apps"}') returned deployment="app-wizard" = 1 (among other deployments) — the correct deployment label has 1 available replica
- pod_status(namespace=apps) showed app-wizard-979c7dcfd-v4nj4 Running ready=6/6 age=7h
- kube_events(namespace=apps, since_minutes=30) returned no Warning events

## Cause

1. **The alert rule AppWizardUnavailable (in /etc/vmalert/rules-out/rules-src-0/rules.yaml) references a non-existent Deployment name due to a typo: the expression queries kube_deployment_status_replicas_available{namespace="apps", deployment="app-wizzard"} (double-z), while the actual Deployment is named app-wizard (single-z). Because no Deployment named app-wizzard exists, the time series is always empty, so the absent() wrapper always evaluates to 1 (true), making the alert fire permanently regardless of the real app-wizard Deployment's health. The actual app-wizard Deployment is fully healthy: status.availableReplicas=1, condition Available=True (MinimumReplicasAvailable) since 2026-10-27T07:37:46Z, pod app-wizard-979c7dcfd-v4nj4 Running ready=6/6.** (95%)

## Resolution

- Fix the typo in the alert rule expression in /etc/vmalert/rules-out/rules-src-0/rules.yaml (sourced from Git): change deployment="app-wizzard" to deployment="app-wizard" so the selector matches the real Deployment. After the corrected rule is applied, absent() will return 0 when app-wizard has ≥1 available replica (which it currently does), and the alert will clear. Also check the alert's summary/description annotations for the same typo. (reversible=true)

