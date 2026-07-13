---
type: Incident
title: Harbor Registry Down due to IAM Access Key Quota Limit
description: 'The Crossplane resource `AccessKey/xplane-harbor` has hit an AWS IAM quota limit (`AccessKeysPerUser: 2`), preventing it from creating the credentials required by the Harbor registry.'
resource: tooling/harbor-registry
tags:
    - runlore
    - incident
timestamp: "2026-06-28T13:07:56Z"
fingerprint: 2d6bd8279304b3e17a5d5e35a55fb0c115ffbeabde820af8cdd2494a4141a60b
---

## Decision

- **why keep:** The Crossplane resource `AccessKey/xplane-harbor` has hit an AWS IAM quota limit (`AccessKeysPerUser: 2`), preventing it from creating the credentials required by the Harbor registry.
- **confidence:** 95%

## Symptom

Harbor Registry Down due to IAM Access Key Quota Limit

## Investigate

- pod_status shows harbor-registry pod failing with 'CreateContainerConfigError: couldn\'t find key username in Secret tooling/xplane-harbor-access-key'
- kube_events for the 'tooling' namespace shows a persistent Warning on the 'AccessKey/xplane-harbor' resource: 'LimitExceeded: Cannot exceed quota for AccessKeysPerUser: 2'
- The failed Crossplane 'AccessKey' resource is responsible for creating the Secret used by the 'harbor-registry' pod.
- The knowledge base article 'HarborRegistryDown' describes this exact failure scenario.

## Cause

1. **The Crossplane resource `AccessKey/xplane-harbor` has hit an AWS IAM quota limit (`AccessKeysPerUser: 2`), preventing it from creating the credentials required by the Harbor registry.** (95%)

## Resolution

- An administrator with AWS access should navigate to the IAM user associated with the Crossplane provider and delete an old or unused access key. This will allow Crossplane to create the new key required by Harbor. After the key is deleted, the `AccessKey/xplane-harbor` resource should be reconciled. (reversible=false)

## Unresolved

- The name of the specific IAM user that has reached its access key quota. This needs to be identified in the AWS console.
- Which of the two existing access keys is safe to delete.

