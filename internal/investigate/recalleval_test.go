// SPDX-License-Identifier: Apache-2.0

package investigate

// Real-threshold retrieval eval harness for instant recall.
//
// Every OTHER recall test in this package (recall_test.go) drops the trust gates
// to 0.001–0.01 with a fakeScored stub, so they prove the GATE logic but never
// measure whether the real BM25 retriever actually RANKS the correct KB entry for
// a realistic, label-derived alert. That failure mode is tested nowhere else —
// which is exactly how the live "recall never fires on KubePodNotReady" gap went
// unnoticed: a generic alertname + terse annotation is a ~2-token BM25 query
// against a differently-worded runbook, scoring ~0.096 live — far below the
// production solo_floor of 4.0 (config.applyDefaults).
//
// This harness closes that hole. It builds a REAL catalog.Catalog (real bleve
// BM25, no fake scorer) over a fixture KB modeled on the live runlore-kb, feeds it
// realistic raw alerts as they actually arrive from the Alertmanager adapter
// (label-derived: a generic alertname, a pod/namespace label, a terse-or-empty
// annotation), and measures TWO things at PRODUCTION thresholds:
//
//  1. pure retrieval quality — Recall@1/3/5 and MRR over SearchScored ranking,
//     BEFORE the structural + margin gates (does BM25 even surface the entry?);
//  2. the end-to-end short-circuit fire-rate + precision through r.lookup at the
//     real production gates (MinScore 1.0, MarginGap 1.0, SoloFloor 4.0).
//
// The fixtures deliberately word each runbook DIFFERENTLY from the alert that
// should match it (that is the vocabulary-mismatch problem, in miniature). The
// pinned numbers below are the honest baseline; fix(recall) query-enrichment moves
// them and updates the asserted constants, keeping the before→after visible.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// --- production thresholds (mirror config.applyDefaults InstantRecall defaults) ---
// Kept as named constants so the harness gates EXACTLY as production does — no
// test ever tunes these down to force a fire (the whole point is to measure the
// real gap).
const (
	prodMinScore  = 1.0
	prodMarginGap = 1.0
	prodSoloFloor = 4.0
)

// evalCase is one labeled incident: a raw alert as it arrives (labels/annotations,
// or a GitOps FailureEvent) paired with a LIST of acceptable target KB entry paths.
// A list, not a single ground truth: per the IBM RCA-benchmark finding, several
// entries can validly answer one incident (the terse HarborRegistryDown note AND
// the detailed IAM-quota incident both "explain" a harbor-registry pod alert), and
// single-ground-truth scoring under-counts a correct-but-different hit. An EMPTY
// targets list marks a NEGATIVE case: no entry is correct, so recall must NOT fire.
type evalCase struct {
	name    string
	regime  string // "label" (Alertmanager label-derived) or "gitops" (FailureEvent)
	labels  map[string]string
	annots  map[string]string
	fail    *providers.FailureEvent // set for regime=="gitops" instead of labels/annots
	targets []string                // acceptable KB paths; empty ⇒ negative (must not fire)
}

func (c evalCase) negative() bool { return len(c.targets) == 0 }

// request rebuilds the normalized investigation Request the way the real sources
// do, so the harness exercises the SAME Request the pipeline sees. The
// label-derived mapping mirrors source/alertmanager.Decode + workloadFromLabels
// (mirrored, not imported: alertmanager imports this package, so a white-box test
// here cannot import it back without a cycle). The GitOps path calls the real
// FromFailureEvent directly.
func (c evalCase) request() Request {
	if c.regime == "gitops" {
		return FromFailureEvent(*c.fail)
	}
	kind, name := evalWorkloadFromLabels(c.labels)
	msg := c.annots["description"]
	if msg == "" {
		msg = c.annots["summary"]
	}
	return Request{
		Source:      SourceAlert,
		Title:       c.labels["alertname"],
		Severity:    c.labels["severity"],
		Workload:    providers.Workload{Namespace: c.labels["namespace"], Kind: kind, Name: name},
		Reason:      c.labels["severity"],
		Message:     msg,
		Labels:      c.labels,
		Annotations: c.annots,
	}
}

// evalWorkloadFromLabels mirrors alertmanager.workloadFromLabels: prefer a stable
// controller label over the ephemeral pod name. Kept in sync by construction — the
// live gap this harness guards is precisely a pod-label alert (only `pod` set), so
// the pod fallback carrying the volatile hash is load-bearing here.
func evalWorkloadFromLabels(labels map[string]string) (kind, name string) {
	for _, c := range []struct{ label, kind string }{
		{"deployment", "Deployment"},
		{"statefulset", "StatefulSet"},
		{"daemonset", "DaemonSet"},
		{"replicaset", "ReplicaSet"},
		{"cronjob", "CronJob"},
		{"job", "Job"},
	} {
		if v := labels[c.label]; v != "" {
			return c.kind, v
		}
	}
	if v := labels["workload"]; v != "" {
		return labels["workload_type"], v
	}
	if v := labels["pod"]; v != "" {
		return "Pod", v
	}
	return "", ""
}

// evalCatalogEntries is the fixture KB: ~16 runbook entries modeled on the live
// github.com/Smana/runlore-kb. The invariant that makes this a real test: each
// entry is WORDED DIFFERENTLY from the alert that should match it. A
// KubePodNotReady pod alert must find "Harbor Registry Down due to IAM Access Key
// Quota Limit"; a replicas-mismatch alert must find an ExternalSecret SM-path
// incident. The discriminating token is the WORKLOAD identity (namespace + name),
// which on a label-derived alert lives in the labels — not the free-text symptom.
var evalCatalogEntries = map[string]string{
	// --- concrete-resource incidents (a namespace/name resource → structurally
	// matchable by a label-derived alert; the ONLY remaining barrier is the BM25
	// score, which is what this harness isolates) ---

	"harbor-registry-iam-quota.md": mdEntry(
		"Incident",
		"Harbor Registry Down due to IAM Access Key Quota Limit",
		"The Crossplane AccessKey/xplane-harbor hit an AWS IAM quota (AccessKeysPerUser: 2), so it cannot mint the credentials the Harbor registry needs.",
		"tooling/harbor-registry",
		[]string{"crossplane", "iam", "quota", "harbor"},
		`The Crossplane AccessKey resource xplane-harbor has reached the AWS IAM
AccessKeysPerUser quota of 2. Without a fresh key the Kubernetes Secret
xplane-harbor-access-key is missing its username field, so the registry container
fails with CreateContainerConfigError. Resolution: delete an unused IAM access key
so Crossplane can reconcile a new one.`),

	"harborregistrydown.md": mdEntry(
		"Incident",
		"HarborRegistryDown",
		"Harbor registry down: Crossplane cannot provision its access key (IAM quota), Secret lacks username, container CreateContainerConfigError.",
		"tooling/harbor-registry",
		[]string{"harbor", "crossplane", "secret"},
		`Root cause: the Crossplane-managed AccessKey cannot be created because the AWS
IAM AccessKeysPerUser quota is exhausted, leaving xplane-harbor-access-key without a
username key and the registry pod in CreateContainerConfigError.`),

	"airflow-externalsecret-smpath.md": mdEntry(
		"Incident",
		"Application airflow Degraded — ExternalSecret wrong AWS Secrets Manager path in dev",
		"ExternalSecret referenced a prod-only AWS Secrets Manager key absent from the dev account; a kustomize overlay patch fixed the key prefix.",
		"data-platform/airflow-scheduler",
		[]string{"externalsecrets", "kustomize", "airflow", "secretsmanager"},
		`The base ExternalSecret manifest for the database admin credentials pointed at a
prod-prefixed AWS Secrets Manager path that does not exist in the dev account, so
UpdateFailed with "Secret does not exist" and the app stayed Degraded even though the
Helm release was Synced. Fix: add a dev-overlay kustomize patch overriding the key.`),

	"semantic-router-deleted.md": mdEntry(
		"Incident",
		"semantic-router serving Deployment scaled to zero by a stale GitOps prune",
		"A removed HelmRelease value pruned the semantic-router Deployment to zero replicas; inference requests 503 until the value is restored.",
		"ai/semantic-router",
		[]string{"gitops", "prune", "replicas", "inference"},
		`A values change removed the replicaCount override, so Flux pruned the
semantic-router serving Deployment down to zero and no inference pods remained.
Restore the replica override and reconcile.`),

	"harbor-valkey-oom.md": mdEntry(
		"Incident",
		"Harbor Valkey cache OOMKilled after a maxmemory misconfiguration",
		"The Valkey (Redis) cache backing Harbor was OOMKilled repeatedly because maxmemory was set above the container memory limit with no eviction policy.",
		"tooling/harbor-valkey",
		[]string{"valkey", "redis", "oom", "memory"},
		`The Valkey StatefulSet backing Harbor kept getting OOMKilled: maxmemory exceeded
the pod memory limit and no maxmemory-policy was set, so the process grew until the
kernel killed it. Set maxmemory below the limit and an allkeys-lru policy.`),

	"payment-api-badimage.md": mdEntry(
		"Incident",
		"payment-api ImagePullBackOff after a registry credential rotation",
		"payment-api pods could not pull their image (ImagePullBackOff) because the rotated registry pull-secret was not propagated to the namespace.",
		"apps/payment-api",
		[]string{"imagepullbackoff", "registry", "pullsecret"},
		`After the container registry credentials were rotated, the imagePullSecret in the
apps namespace still held the old token, so every payment-api pod stuck in
ImagePullBackOff with "unauthorized: authentication required". Refresh the pull secret.`),

	"coredns-crashloop.md": mdEntry(
		"Incident",
		"CoreDNS CrashLoopBackOff after a Corefile plugin typo",
		"A malformed Corefile plugin line made CoreDNS crash on startup, breaking cluster DNS resolution until the ConfigMap was corrected.",
		"kube-system/coredns",
		[]string{"coredns", "dns", "corefile", "crashloop"},
		`A bad edit to the coredns ConfigMap introduced an unknown plugin directive, so
CoreDNS failed to load the Corefile and crash-looped on boot, taking cluster DNS with
it. Revert the Corefile change and let the deployment roll.`),

	"checkout-cpu-throttle.md": mdEntry(
		"Incident",
		"Checkout p99 latency spike from CPU throttling under an HPA replica cap",
		"Checkout latency spiked because the HPA hit its maxReplicas cap while CPU limits throttled every pod during a traffic surge.",
		"shop/checkout",
		[]string{"latency", "cpu", "throttling", "hpa"},
		`During a promotion the checkout service saw p99 latency climb: the HorizontalPodAutoscaler
was pinned at maxReplicas and the per-pod CPU limit caused heavy CFS throttling, so no
pod could keep up. Raise the HPA cap and revisit the CPU limit.`),

	"cert-manager-acme-timeout.md": mdEntry(
		"Incident",
		"Certificate renewal blocked by an ACME DNS-01 propagation timeout",
		"cert-manager could not renew the wildcard certificate: the ACME DNS-01 challenge timed out waiting for the TXT record to propagate.",
		"cert-manager/cert-manager",
		[]string{"certmanager", "acme", "dns01", "certificate"},
		`The wildcard certificate failed to renew because the ACME DNS-01 solver's TXT record
did not propagate before the challenge deadline, leaving the Certificate in a
renewal-pending state. Check the DNS provider credentials and propagation delay.`),

	"elasticsearch-disk-watermark.md": mdEntry(
		"Incident",
		"Elasticsearch indices went read-only after the flood-stage disk watermark",
		"Elasticsearch tripped the flood-stage disk watermark and flipped indices to read-only, so log ingestion stopped until disk was freed and the block cleared.",
		"logging/elasticsearch",
		[]string{"elasticsearch", "disk", "watermark", "readonly"},
		`Node disk usage crossed the flood-stage watermark, so Elasticsearch applied the
index.blocks.read_only_allow_delete block and rejected writes. Free disk (or expand the
PVC) and clear the read-only block on the affected indices.`),

	"kafka-underreplicated.md": mdEntry(
		"Incident",
		"Kafka under-replicated partitions after a broker restart storm",
		"A rolling broker restart left partitions under-replicated; producers saw NotEnoughReplicas until the ISR caught up.",
		"streaming/kafka",
		[]string{"kafka", "partitions", "isr", "broker"},
		`A too-fast rolling restart of the Kafka StatefulSet took brokers down before the
in-sync replica set recovered, so UnderReplicatedPartitions climbed and acks=all
producers failed with NotEnoughReplicas. Slow the restart and wait for ISR recovery.`),

	// --- playbooks (glob resource → structurally NON-matching for a label-derived
	// pod alert; they serve the GitOps regime and act as corpus distractors) ---

	"helmrelease-upgrade-failure.md": mdEntry(
		"Playbook",
		"HelmRelease upgrade/install failure",
		"Diagnose a Flux HelmRelease (or ArgoCD Application) stuck Ready=False shortly after a chart or values change.",
		"helmrelease://*",
		[]string{"flux", "helmrelease", "helm", "upgrade", "gitops"},
		`A HelmRelease reports Ready=False / Released=False after a chart or values bump.
Identify the revision delta, read the managed resources' status, and roll back to the
previous revision before fixing forward. Common causes: bad values, a stalled migration
job, a missing CRD, or a renamed valuesFrom key.`),

	"kustomization-reconciliation-failure.md": mdEntry(
		"Playbook",
		"Flux Kustomization reconciliation failure",
		"Diagnose a Flux Kustomization stuck Ready=False — build, dependency, or apply errors after a Git change.",
		"kustomization://*",
		[]string{"flux", "kustomization", "gitops", "reconcile", "dependson"},
		`A Flux Kustomization reports Ready=False. The status message names the phase:
build failed, dependency not ready, health check failed, or a server-side apply error
(immutable field, missing CRD, admission webhook denial). Read the Ready condition
verbatim and walk dependsOn for an upstream cascade.`),

	"karpenter-ami-not-found.md": mdEntry(
		"Playbook",
		"Karpenter EC2NodeClass not ready — AMI alias not found",
		"Karpenter provisions no nodes; EC2NodeClass stays Ready=Unknown (failed to discover any AMIs for alias) and pods sit Pending on Insufficient cpu.",
		"ec2nodeclass://*",
		[]string{"karpenter", "ec2nodeclass", "ami", "scaling", "pending"},
		`Pending pods never schedule (Insufficient cpu) and no new node appears because the
EC2NodeClass pins a Bottlerocket AMI alias that does not exist for the cluster's
Kubernetes version, so Karpenter discovers no AMI and never creates a NodeClaim.`),

	"node-disk-pressure.md": mdEntry(
		"Playbook",
		"Node DiskPressure eviction and image garbage collection",
		"A node under DiskPressure evicts pods and garbage-collects images; workloads reschedule elsewhere while the node's disk is reclaimed.",
		"", // resource-less playbook (scopeless tier only)
		[]string{"node", "diskpressure", "eviction", "kubelet"},
		`The kubelet reports DiskPressure and begins evicting pods and pruning images once
disk usage crosses the eviction threshold. Identify what filled the disk (logs, images,
emptyDir) and reclaim it; the taint clears when usage drops below the threshold.`),
}

// mdEntry renders one fixture KB markdown file with OKF frontmatter. Scalar values
// are YAML single-quoted (matching the live runlore-kb style) so a colon-space,
// em-dash, or glob in a title/description/resource can't break the frontmatter
// parse and silently drop the entry from the index.
func mdEntry(typ, title, desc, resource string, tags []string, body string) string {
	fm := "---\n" +
		"type: " + yq(typ) + "\n" +
		"title: " + yq(title) + "\n" +
		"description: " + yq(desc) + "\n"
	if resource != "" {
		fm += "resource: " + yq(resource) + "\n"
	}
	fm += "tags: ["
	for i, t := range tags {
		if i > 0 {
			fm += ", "
		}
		fm += yq(t)
	}
	fm += "]\n---\n\n" + body + "\n"
	return fm
}

// yq single-quotes a YAML scalar, doubling any embedded single quote.
func yq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// writeEvalCatalog writes the fixture KB to a temp dir and returns a REAL
// catalog.Catalog (real bleve BM25 index) over it.
func writeEvalCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	for name, content := range evalCatalogEntries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	cat, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("build fixture catalog: %v", err)
	}
	if cat.Len() != len(evalCatalogEntries) {
		t.Fatalf("indexed %d entries, want %d", cat.Len(), len(evalCatalogEntries))
	}
	return cat
}

// evalCases is the labeled incident set: realistic raw alerts (label-derived +
// GitOps) with acceptable target lists, including negatives. HARD cases carry an
// EMPTY description — the workload identity then lives ONLY in the pod/namespace
// label, exactly the live regime where the raw title+message query is ~1 token.
func evalCases() []evalCase {
	return []evalCase{
		// L1 — the live case: a pod-scoped KubePodNotReady with NO annotation. The
		// raw query is just the alertname; "harbor-registry" is only in the pod label.
		{
			name: "harbor_registry_notready", regime: "label",
			labels:  map[string]string{"alertname": "KubePodNotReady", "namespace": "tooling", "pod": "harbor-registry-59598dbd57-ltkzw", "severity": "warning"},
			targets: []string{"harbor-registry-iam-quota.md", "harborregistrydown.md"},
		},
		// L2 — same incident, this time the container-waiting reason is in the text.
		{
			name: "harbor_registry_configerror", regime: "label",
			labels:  map[string]string{"alertname": "KubeContainerWaiting", "namespace": "tooling", "pod": "harbor-registry-59598dbd57-abcde", "severity": "warning"},
			annots:  map[string]string{"description": "Pod tooling/harbor-registry container registry has been waiting: CreateContainerConfigError"},
			targets: []string{"harbor-registry-iam-quota.md", "harborregistrydown.md"},
		},
		// L3 — a Deployment replicas-mismatch alert vs an ExternalSecret SM-path incident.
		{
			name: "airflow_replicas_mismatch", regime: "label",
			labels:  map[string]string{"alertname": "KubeDeploymentReplicasMismatch", "namespace": "data-platform", "deployment": "airflow-scheduler", "severity": "warning"},
			annots:  map[string]string{"description": "Deployment data-platform/airflow-scheduler has not matched the expected number of replicas for longer than 15 minutes."},
			targets: []string{"airflow-externalsecret-smpath.md"},
		},
		// L4 — HARD: replicas-mismatch, no annotation; identity only in the deployment label.
		{
			name: "semantic_router_gone", regime: "label",
			labels:  map[string]string{"alertname": "KubeDeploymentReplicasMismatch", "namespace": "ai", "deployment": "semantic-router", "severity": "warning"},
			targets: []string{"semantic-router-deleted.md"},
		},
		// L5 — a StatefulSet crash-loop alert vs an OOM/maxmemory incident.
		{
			name: "valkey_crashloop", regime: "label",
			labels:  map[string]string{"alertname": "KubePodCrashLooping", "namespace": "tooling", "statefulset": "harbor-valkey", "pod": "harbor-valkey-0", "severity": "warning"},
			annots:  map[string]string{"description": "Pod tooling/harbor-valkey-0 (harbor-valkey) is in waiting state (CrashLoopBackOff)."},
			targets: []string{"harbor-valkey-oom.md"},
		},
		// L6 — HARD: pod-scoped KubePodNotReady, no annotation.
		{
			name: "payment_api_notready", regime: "label",
			labels:  map[string]string{"alertname": "KubePodNotReady", "namespace": "apps", "deployment": "payment-api", "pod": "payment-api-7d9f8c4b6-xk2lm", "severity": "warning"},
			targets: []string{"payment-api-badimage.md"},
		},
		// L7 — CrashLoop with a terse restart-count annotation.
		{
			name: "coredns_crashloop", regime: "label",
			labels:  map[string]string{"alertname": "KubePodCrashLooping", "namespace": "kube-system", "deployment": "coredns", "pod": "coredns-5d78c9b4c-abcde", "severity": "warning"},
			annots:  map[string]string{"description": "Pod kube-system/coredns is restarting frequently."},
			targets: []string{"coredns-crashloop.md"},
		},
		// L8 — CPUThrottlingHigh shares the token "throttling" with its entry (easier).
		{
			name: "checkout_cpu_throttle", regime: "label",
			labels:  map[string]string{"alertname": "CPUThrottlingHigh", "namespace": "shop", "deployment": "checkout", "pod": "checkout-6c8f7d-qwert", "severity": "warning"},
			annots:  map[string]string{"description": "Processes in shop/checkout experience high CPU throttling."},
			targets: []string{"checkout-cpu-throttle.md"},
		},
		// L9 — a namespace-scoped cert-expiry alert (no workload name).
		{
			name: "cert_expiry", regime: "label",
			labels:  map[string]string{"alertname": "CertManagerCertExpirySoon", "namespace": "cert-manager", "severity": "warning"},
			annots:  map[string]string{"description": "Certificate cert-manager/wildcard-tls will expire in 24 hours."},
			targets: []string{"cert-manager-acme-timeout.md"},
		},
		// L10 — HARD: StatefulSet pod not ready, no annotation.
		{
			name: "elasticsearch_notready", regime: "label",
			labels:  map[string]string{"alertname": "KubePodNotReady", "namespace": "logging", "statefulset": "elasticsearch", "pod": "elasticsearch-0", "severity": "warning"},
			targets: []string{"elasticsearch-disk-watermark.md"},
		},
		// L11 — StatefulSet replicas mismatch vs an under-replicated-partitions incident.
		{
			name: "kafka_replicas_mismatch", regime: "label",
			labels:  map[string]string{"alertname": "KubeStatefulSetReplicasMismatch", "namespace": "streaming", "statefulset": "kafka", "severity": "warning"},
			annots:  map[string]string{"description": "StatefulSet streaming/kafka has not matched the expected number of replicas."},
			targets: []string{"kafka-underreplicated.md"},
		},

		// --- GitOps regime: the Title already carries the ref; enrichment must not
		// hurt these. Their entries have glob resources so they do NOT fire through
		// the structural gate — here they are measured on RETRIEVAL ranking only. ---
		{
			name: "helmrelease_upgrade_gitops", regime: "gitops",
			fail: &providers.FailureEvent{
				Workload: providers.Workload{Kind: "HelmRelease", Name: "podinfo", Namespace: "apps"},
				Reason:   "UpgradeFailed",
				Message:  "Helm upgrade failed: timeout waiting for Deployment/podinfo to become ready",
			},
			targets: []string{"helmrelease-upgrade-failure.md"},
		},
		{
			name: "kustomization_build_gitops", regime: "gitops",
			fail: &providers.FailureEvent{
				Workload: providers.Workload{Kind: "Kustomization", Name: "infrastructure", Namespace: "flux-system"},
				Reason:   "BuildFailed",
				Message:  "kustomize build failed: accumulating resources: evalsymlink failure",
			},
			targets: []string{"kustomization-reconciliation-failure.md"},
		},

		// --- negatives: no entry is correct → recall must NOT fire ---
		{
			name: "watchdog", regime: "label",
			labels:  map[string]string{"alertname": "Watchdog", "severity": "none"},
			annots:  map[string]string{"description": "This is an alert meant to ensure that the entire alerting pipeline is functional."},
			targets: nil,
		},
		{
			name: "blackbox_probe", regime: "label",
			labels:  map[string]string{"alertname": "BlackboxProbeFailed", "namespace": "monitoring", "severity": "warning"},
			annots:  map[string]string{"description": "Probe of https://status.example.com failed."},
			targets: nil,
		},
	}
}

// --- retrieval metrics (pure SearchScored ranking, BEFORE the gates) ---

const retrievalWindow = 10 // deep enough to place Recall@5 and a meaningful MRR

type retrievalMetrics struct {
	positives  int
	h1, h3, h5 int // exact hit counts (a target ranked within the top 1/3/5)
	r1, r3, r5 float64
	mrr        float64
	ranks      map[string]int // case name → 1-based rank of first target (0 = miss)
}

// rankOfTarget returns the 1-based rank of the first hit whose Path is an
// acceptable target, or 0 if no target appears in the window.
func rankOfTarget(hits []catalog.ScoredEntry, targets []string) int {
	set := map[string]struct{}{}
	for _, p := range targets {
		set[p] = struct{}{}
	}
	for i, h := range hits {
		if _, ok := set[h.Entry.Path]; ok {
			return i + 1
		}
	}
	return 0
}

func computeRetrieval(t *testing.T, cat *catalog.Catalog, cases []evalCase) retrievalMetrics {
	t.Helper()
	m := retrievalMetrics{ranks: map[string]int{}}
	for _, c := range cases {
		if c.negative() {
			continue
		}
		m.positives++
		hits, err := cat.SearchScored(buildRecallQuery(c.request()), retrievalWindow)
		if err != nil {
			t.Fatalf("%s: SearchScored: %v", c.name, err)
		}
		rank := rankOfTarget(hits, c.targets)
		m.ranks[c.name] = rank
		switch {
		case rank == 0:
			// miss: no acceptable target in the top retrievalWindow
		case rank <= 1:
			m.h1, m.h3, m.h5 = m.h1+1, m.h3+1, m.h5+1
		case rank <= 3:
			m.h3, m.h5 = m.h3+1, m.h5+1
		case rank <= 5:
			m.h5++
		}
		if rank >= 1 {
			m.mrr += 1.0 / float64(rank)
		}
	}
	if m.positives > 0 {
		n := float64(m.positives)
		m.r1, m.r3, m.r5 = float64(m.h1)/n, float64(m.h3)/n, float64(m.h5)/n
		m.mrr /= n
	}
	return m
}

// --- fire-rate + precision through the full lookup at PRODUCTION thresholds ---

type fireMetrics struct {
	labelPositives int
	fired          int // fired on a label-derived positive
	firedCorrect   int // fired AND landed on an acceptable target
	negatives      int
	negFired       int // a negative that WRONGLY fired (precision leak)
}

func (f fireMetrics) fireRate() float64 {
	if f.labelPositives == 0 {
		return 0
	}
	return float64(f.fired) / float64(f.labelPositives)
}

func (f fireMetrics) precision() float64 {
	if f.fired == 0 {
		return 0
	}
	return float64(f.firedCorrect) / float64(f.fired)
}

func computeFire(t *testing.T, cat *catalog.Catalog, cases []evalCase) fireMetrics {
	t.Helper()
	r := &Recall{Catalog: cat, MinScore: prodMinScore, MarginGap: prodMarginGap, SoloFloor: prodSoloFloor}
	var f fireMetrics
	for _, c := range cases {
		// Fire-rate is a claim about the LABEL-DERIVED regime (the one under test);
		// GitOps entries have glob resources that never clear the structural gate, a
		// separate limitation, so they are excluded from the fire denominator.
		if c.regime != "label" {
			continue
		}
		entry, _ := r.lookup(context.Background(), c.request())
		if c.negative() {
			f.negatives++
			if entry != nil {
				f.negFired++
			}
			continue
		}
		f.labelPositives++
		if entry == nil {
			continue
		}
		f.fired++
		if rankOfTarget([]catalog.ScoredEntry{{Entry: *entry}}, c.targets) == 1 {
			f.firedCorrect++
		}
	}
	return f
}

func logRetrieval(t *testing.T, tag string, m retrievalMetrics) {
	t.Helper()
	t.Logf("[%s] retrieval over %d positive cases: Recall@1=%.2f Recall@3=%.2f Recall@5=%.2f MRR=%.3f",
		tag, m.positives, m.r1, m.r3, m.r5, m.mrr)
	names := make([]string, 0, len(m.ranks))
	for n := range m.ranks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Logf("    rank[%s] = %d", n, m.ranks[n])
	}
}

func logFire(t *testing.T, tag string, f fireMetrics) {
	t.Helper()
	t.Logf("[%s] production-threshold fire: fired=%d/%d (rate=%.2f) precision=%.2f | negatives fired=%d/%d",
		tag, f.fired, f.labelPositives, f.fireRate(), f.precision(), f.negFired, f.negatives)
}

// Measured metrics — the honest before→after, kept in the source so a reviewer
// sees the gap without running the suite. fix(recall) query-enrichment flips the
// two pinned constants below and updates this table; the fire-rate row stays 0.
//
//	                        raw title+message    + structured-entity enrichment
//	Recall@1 / @3 / @5       0.69 / 0.69 / 0.69    1.00 / 1.00 / 1.00
//	MRR                      0.692                 1.000
//	fire-rate @ prod gates   0/11 (0.00)           0/11 (0.00)   ← UNCHANGED
//
// Verdict: enrichment fully fixes RETRIEVAL — the 4 identity-in-the-label cases go
// from zero-hit to rank #1 — but the enriched BM25 top scores (~0.5–1.2) stay an
// order of magnitude below SoloFloor 4.0, so nothing short-circuits. Closing the
// end-to-end loop needs a reranker (calibrated scores) or a threshold-philosophy
// change; the harness now MEASURES exactly that, at real production thresholds.
const (
	// wantHardCaseRank is the ranking of the 4 hardLabelCases' target.
	// BASELINE 0 (miss — the raw query is a lone alertname token); enriched → 1.
	wantHardCaseRank = 0
	// wantRetrievalHitsAt1 pins Recall@1's hit count over the 13 positive cases.
	// BASELINE 9; enriched → 13.
	wantRetrievalHitsAt1 = 9
	// wantFireCount pins the production-threshold short-circuit count over the 11
	// label positives. It stays 0 in BOTH regimes — the pinned proof that query
	// enrichment alone never clears SoloFloor 4.0. A future reranker/threshold change
	// is what should move this (and this test then documents that step too).
	wantFireCount = 0
)

// hardLabelCases are the empty-annotation alerts whose workload identity lives ONLY
// in the pod/namespace label. Under the raw title+message query they reduce to a
// single alertname token → zero BM25 hits; the enrichment is what recovers them.
var hardLabelCases = []string{
	"harbor_registry_notready", "payment_api_notready",
	"semantic_router_gone", "elasticsearch_notready",
}

// TestRecallEvalRetrieval measures pure BM25 ranking quality BEFORE the gates —
// Recall@1/3/5 and MRR over SearchScored. It prints the metrics (t.Log) for CI.
func TestRecallEvalRetrieval(t *testing.T) {
	cat := writeEvalCatalog(t)
	m := computeRetrieval(t, cat, evalCases())
	logRetrieval(t, "retrieval", m)

	if m.positives != 13 {
		t.Fatalf("expected 13 positive cases, got %d", m.positives)
	}
	// The identity-in-the-label cases are the crux: BASELINE they MISS entirely,
	// which is the whole reason recall never fires. fix(recall) flips wantHardCaseRank
	// to 1 (each recovered to rank #1).
	for _, n := range hardLabelCases {
		if m.ranks[n] != wantHardCaseRank {
			t.Fatalf("hard case %q: rank = %d, want %d (see before→after table)", n, m.ranks[n], wantHardCaseRank)
		}
	}
	if m.h1 != wantRetrievalHitsAt1 {
		t.Fatalf("Recall@1 hit count = %d, want %d", m.h1, wantRetrievalHitsAt1)
	}
	// Corpus property (holds in both regimes): when BM25 surfaces a target at all it
	// ranks it #1 — a clean win or a clean miss, never buried mid-list. So Recall@3
	// and Recall@5 add nothing over Recall@1, and MRR collapses to the hit rate.
	if m.h3 != m.h1 || m.h5 != m.h1 {
		t.Fatalf("expected every found target at rank 1 (h1=%d h3=%d h5=%d)", m.h1, m.h3, m.h5)
	}
}

// TestRecallEvalProductionFireRate is the pinned baseline-gap regression: at
// PRODUCTION thresholds (SoloFloor 4.0 etc.) the generic label-derived alerts FAIL
// to fire — the gates are NOT tuned down to hide it. This assertion is the honest
// proof that query enrichment ALONE does not close the loop (fire count stays 0);
// a reranker or threshold change is what should ever move wantFireCount off 0.
func TestRecallEvalProductionFireRate(t *testing.T) {
	cat := writeEvalCatalog(t)
	f := computeFire(t, cat, evalCases())
	logFire(t, "fire", f)

	// A negative alert must never short-circuit an investigation, at any query.
	if f.negFired != 0 {
		t.Fatalf("negative case(s) wrongly fired recall: %d/%d", f.negFired, f.negatives)
	}
	if f.labelPositives != 11 {
		t.Fatalf("expected 11 label-derived positives, got %d", f.labelPositives)
	}
	if f.fired != wantFireCount {
		t.Fatalf("production-threshold fire count = %d, want %d (see before→after table)", f.fired, wantFireCount)
	}
}

// TestRecallEvalGitOpsRetrievable guards the "don't hurt the GitOps regime"
// invariant: a GitOps FailureEvent (whose Title already carries the ref) must keep
// its runbook in the top 3 of the ranking under whichever query builder is
// compiled — the enrichment must not push it away.
func TestRecallEvalGitOpsRetrievable(t *testing.T) {
	cat := writeEvalCatalog(t)
	for _, c := range evalCases() {
		if c.regime != "gitops" {
			continue
		}
		hits, err := cat.SearchScored(buildRecallQuery(c.request()), retrievalWindow)
		if err != nil {
			t.Fatalf("%s: SearchScored: %v", c.name, err)
		}
		rank := rankOfTarget(hits, c.targets)
		t.Logf("gitops rank[%s] = %d", c.name, rank)
		if rank < 1 || rank > 3 {
			t.Fatalf("%s: GitOps runbook must stay in the top 3 (got rank %d)", c.name, rank)
		}
	}
}
