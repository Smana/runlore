# GCP CloudProvider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `providers.CloudProvider` for GCP — Cloud Audit Logs behind `cloud_what_changed`, GKE/MIG/Compute state behind `cloud_resource_health` — so a GKE cluster gets the cloud lens with `cloud: {provider: gcp}` and nothing else.

**Architecture:** A new `internal/providers/cloud/gcp` package mirroring `cloud/aws` file-for-file. Changes come from Cloud Logging (`entries.list` over the `activity` + `system_event` audit streams); state comes from native `container/v1` and `compute/v1` describes. Identity (project/location/cluster) resolves through three tiers — config, GKE metadata server, Kubernetes node `providerID`. Auth is Application Default Credentials against a GKE Workload Identity **direct principal binding**, so there is no service-account key, no GSA and no KSA annotation.

**Tech Stack:** Go 1.25 · `google.golang.org/api/{logging/v2,container/v1,compute/v1}` · `cloud.google.com/go/compute/metadata` · stdlib `net/http/httptest` for all provider tests.

**Spec:** [`docs/superpowers/specs/2026-08-24-gcp-cloud-provider-design.md`](../specs/2026-08-24-gcp-cloud-provider-design.md)

**Branch:** `feat/gcp-cloud-provider` (already exists, spec committed). Push branches only — **do not open pull requests**; the maintainer does that.

---

## Conventions this repo already uses — read before Task 1

You will be dropped into a mature Go codebase. These are not suggestions; matching them is most of the review bar.

- **No Makefile.** Tests are `go test -race ./...` (see `.github/workflows/ci.yaml:44`). Lint is `golangci-lint run` against `.golangci.yml`.
- **Every file starts with** `// SPDX-License-Identifier: Apache-2.0` followed by a blank line.
- **Commits are Conventional Commits** (`feat:`, `fix:`, `chore(docs):`). release-please parses them. **Never add a `Co-Authored-By` trailer** — the maintainer's standing rule.
- **Comments explain *why*, at length, and cite real incidents.** Read `internal/investigate/cloud_tools.go:60-80` for the house style. A comment that restates the code will be flagged.
- **Test naming is a full sentence describing the invariant**, e.g. `TestResolveWorkloadIdentityReconcilesTheCloudScope`. Table-driven with a `name` field written as prose.
- **Providers declare conformance at package level**: `var _ providers.CloudProvider = (*Client)(nil)` (see `internal/providers/cloud/aws/aws.go:75`).
- **Capping providers emit `providers.TruncationLine(limit)`** (`providers.go:1293`) rather than inventing their own sentinel text. The one exception already in the tree is `CloudChanges`, which returns `[]Change` not `LogResult` and so uses a `Change`-shaped sentinel (`cloudtrail.go:96`).

## File structure

| File | Responsibility |
|---|---|
| `internal/providers/providers.go` *(modify)* | `EngineGCP`; `CloudDescriber` + `CloudVocabulary`; `AWSCloudVocabulary()`; widened doc comments |
| `internal/investigate/cloud_tools.go` *(modify)* | Tool text composed from the vocabulary instead of hardcoded AWS literals |
| `internal/config/config.go` *(modify)* | `CloudAWS`/`CloudGCP` constants; nested `GCPCloudCfg` |
| `internal/providers/cloud/gcp/gcp.go` *(create)* | `Client`, `New`, narrow API interfaces, `CloudVocabulary()`, preflight |
| `internal/providers/cloud/gcp/identity.go` *(create)* | Three-tier project/location/cluster resolution |
| `internal/providers/cloud/gcp/auditlog.go` *(create)* | `CloudChanges` |
| `internal/providers/cloud/gcp/resourcehealth.go` *(create)* | `ResourceHealth` |
| `internal/app/investigate.go` *(modify)* | `switch cfg.Cloud.Provider` wiring |
| `deploy/helm/runlore/templates/_helpers.tpl` *(modify)* | `POD_SERVICE_ACCOUNT` downward-API env |
| `website/content/docs/integrations/data-sources/gcp-cloud.md` *(create)* | Integration page |

**Split rationale:** `auditlog.go` and `resourcehealth.go` share only the `Client`. They have disjoint APIs, disjoint failure modes and disjoint tests, exactly as `cloudtrail.go` and `resourcehealth.go` are split on the AWS side. `identity.go` is separate because Task 5's tier 3 is explicitly provisional and must delete cleanly (see Task 5).

---

## Task 1: `EngineGCP`, `CloudDescriber` and the AWS vocabulary

**Why first:** every later task depends on these types. Doing it first also proves the central promise of the spec — that AWS tool text does not change — as an executable test rather than a claim.

**Files:**
- Modify: `internal/providers/providers.go:29-49` (engine + change-type comments), and append the new types
- Test: `internal/providers/cloudvocabulary_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/providers/cloudvocabulary_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package providers

import "testing"

// shippedChangeDescription is the cloud_what_changed description EXACTLY as it
// shipped before the vocabulary existed, pasted verbatim from
// internal/investigate/cloud_tools.go. The spec promised that wiring GCP would not
// change one byte of what an AWS deployment's model is told; this constant is what
// makes that promise testable rather than aspirational. If a refactor of the
// skeleton changes AWS's rendered text, this test fails and the promise is kept.
const shippedChangeDescription = "List recent MUTATING AWS control-plane events (CloudTrail) — ASG/EC2/EKS/RDS/SG changes, " +
	"manual actions, and other infra changes invisible to GitOps. Use when no Git change explains " +
	"the incident. Optional resource is an EXACT CloudTrail ResourceName — a full ARN, instance-id, " +
	"ASG name, or a resource's full path (e.g. a Secrets Manager secret's \"apps/team/name\") — never a " +
	"service name or substring; OMIT it to see every mutating event, which is the right move when you do " +
	"not know the exact identifier. since_minutes default 90 (CloudTrail lags ~15m)."

const shippedHealthDescription = "Describe AWS-side health for the cluster's nodes/capacity: EKS nodegroup status + health " +
	"issues, ASG scaling activities (launch/capacity failures), and — when given an EC2 instance-id " +
	"(i-…) — its instance/system status checks. Use to confirm a node/infra/capacity cause. " +
	"Optional since_minutes scopes the scaling-activity lookback to the incident window " +
	"(default: recent activities)."

func TestAWSCloudVocabularyReproducesTheShippedToolText(t *testing.T) {
	v := AWSCloudVocabulary()
	if got := v.ChangeDescription(); got != shippedChangeDescription {
		t.Errorf("ChangeDescription() drifted from the shipped AWS text\n got: %q\nwant: %q", got, shippedChangeDescription)
	}
	if got := v.HealthDescription(); got != shippedHealthDescription {
		t.Errorf("HealthDescription() drifted from the shipped AWS text\n got: %q\nwant: %q", got, shippedHealthDescription)
	}
}

// TestEngineGCPIsDistinctFromEngineAWS guards the one way a new engine constant can
// go wrong silently: colliding with an existing value would fuse two clouds' changes
// into one bucket everywhere Engine is used as a key.
func TestEngineGCPIsDistinctFromEngineAWS(t *testing.T) {
	if EngineGCP == EngineAWS {
		t.Fatal("EngineGCP must not equal EngineAWS")
	}
	if EngineGCP != "gcp" {
		t.Errorf("EngineGCP = %q, want %q", EngineGCP, "gcp")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/ -run 'TestAWSCloudVocabulary|TestEngineGCP' -v`
Expected: FAIL to compile — `undefined: AWSCloudVocabulary`, `undefined: EngineGCP`.

- [ ] **Step 3: Add `EngineGCP` and widen two comments**

In `internal/providers/providers.go`, replace the engine block at lines 31-37:

```go
// Supported GitOps engines. EngineAWS and EngineGCP mark a non-GitOps change from a
// cloud control plane (CloudTrail / Cloud Audit Logs), so cloud events join the same
// "what changed" model.
const (
	EngineFlux   Engine = "flux"
	EngineArgoCD Engine = "argocd"
	EngineAWS    Engine = "aws"
	EngineGCP    Engine = "gcp"
)
```

At line 48, widen the change-type comment:

```go
	ChangeCloudAPI  ChangeType = "cloud-api"  // a mutating cloud control-plane call (CloudTrail / Cloud Audit Logs)
```

At lines 99-100, widen the `Workload` field comments — the fields themselves do NOT change:

```go
	Region    string // AWS region / GCP location; "" for a Kubernetes object and for an unqualified resource
	Account   string // AWS account id / GCP project id; "" for a Kubernetes object and for an unqualified resource
```

- [ ] **Step 4: Add `CloudVocabulary`, `CloudDescriber` and `AWSCloudVocabulary`**

Append to `internal/providers/providers.go`, immediately after the `CloudProvider` interface (which ends around line 973):

```go
// CloudVocabulary is the cloud-specific vocabulary the cloud tools render their
// model-facing descriptions from. It exists because those descriptions were written
// as AWS literals ("MUTATING AWS control-plane events (CloudTrail)", "EC2 instance-id
// (i-…)"), and a GCP deployment reading them would be told it is querying CloudTrail
// while it queries Cloud Audit Logs.
//
// That is not cosmetic. cloud_tools.go documents a real investigation that dead-ended
// because the model held a wrong belief about how the `resource` argument MATCHES.
// Tool text is the only place that belief comes from, so it has to be true per cloud.
//
// The struct is fragments, not two finished strings, deliberately: the skeleton in
// ChangeDescription/HealthDescription is shared, so the two clouds cannot drift into
// describing structurally different contracts for the same tool.
type CloudVocabulary struct {
	Cloud          string // "AWS" | "GCP"
	ChangeLog      string // "CloudTrail" | "Cloud Audit Logs"
	ChangeExamples string // the kinds of change this log carries
	ScopeGuidance  string // how the `resource` argument matches, and when to omit it
	WidenedBanner  string // format string with ONE %q (the resource), shown when a scoped lookup is widened
	LagNote        string // ingestion lag, e.g. "CloudTrail lags ~15m"
	HealthSurface  string // what cloud_resource_health actually describes
	InstanceArg    string // schema description for the instance argument
}

// ChangeDescription renders the cloud_what_changed description for this cloud.
func (v CloudVocabulary) ChangeDescription() string {
	return fmt.Sprintf(
		"List recent MUTATING %s control-plane events (%s) — %s. Use when no Git change explains "+
			"the incident. %s since_minutes default 90 (%s).",
		v.Cloud, v.ChangeLog, v.ChangeExamples, v.ScopeGuidance, v.LagNote,
	)
}

// HealthDescription renders the cloud_resource_health description for this cloud.
func (v CloudVocabulary) HealthDescription() string {
	return fmt.Sprintf(
		"Describe %s-side health for the cluster's nodes/capacity: %s Use to confirm a "+
			"node/infra/capacity cause. Optional since_minutes scopes the scaling-activity lookback "+
			"to the incident window (default: recent activities).",
		v.Cloud, v.HealthSurface,
	)
}

// CloudDescriber is an OPTIONAL CloudProvider extension naming the cloud's own
// vocabulary. Same shape as every other optional capability here (OwnerWalker,
// EventWindower, GitOpsEngineReporter, ProgressNotifier): the tools type-assert for
// it and degrade gracefully.
//
// Degrading means AWSCloudVocabulary, which reproduces the shipped AWS text byte for
// byte — so a provider that does not implement this interface produces exactly the
// tool descriptions it produced before this interface existed.
type CloudDescriber interface {
	CloudVocabulary() CloudVocabulary
}

// AWSCloudVocabulary is the AWS wording, and the fallback for any CloudProvider that
// does not implement CloudDescriber. Its fragments are pinned by
// TestAWSCloudVocabularyReproducesTheShippedToolText against the literals that
// shipped before the vocabulary existed.
func AWSCloudVocabulary() CloudVocabulary {
	return CloudVocabulary{
		Cloud:          "AWS",
		ChangeLog:      "CloudTrail",
		ChangeExamples: "ASG/EC2/EKS/RDS/SG changes, manual actions, and other infra changes invisible to GitOps",
		ScopeGuidance: "Optional resource is an EXACT CloudTrail ResourceName — a full ARN, instance-id, " +
			"ASG name, or a resource's full path (e.g. a Secrets Manager secret's \"apps/team/name\") — never a " +
			"service name or substring; OMIT it to see every mutating event, which is the right move when you do " +
			"not know the exact identifier.",
		WidenedBanner: "resource %q matched no CloudTrail events — ResourceName is an exact match on the " +
			"full AWS resource name or ARN (e.g. a secret's full path \"apps/team/name\"), not a service or " +
			"substring. Showing ALL mutating events in the window instead:\n",
		LagNote: "CloudTrail lags ~15m",
		HealthSurface: "EKS nodegroup status + health issues, ASG scaling activities (launch/capacity " +
			"failures), and — when given an EC2 instance-id (i-…) — its instance/system status checks.",
		InstanceArg: "optional EC2 instance id (i-…)",
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/providers/ -run 'TestAWSCloudVocabulary|TestEngineGCP' -v`
Expected: PASS, both tests.

If `ChangeDescription` mismatches, diff the two strings character by character — the likeliest cause is a missing or doubled space where a fragment meets the skeleton, or a `.` that lives in the fragment *and* the skeleton.

- [ ] **Step 6: Run the full provider suite for regressions**

Run: `go test -race ./internal/providers/...`
Expected: PASS (`fmt` is already imported in `providers.go`; if the build complains it is not, add it).

- [ ] **Step 7: Commit**

```bash
git add internal/providers/providers.go internal/providers/cloudvocabulary_test.go
git commit -m "feat(providers): add EngineGCP and the CloudDescriber capability

Cloud tool descriptions were AWS literals, so a GCP deployment would be
told it queries CloudTrail while it queries Cloud Audit Logs. Tool text is
the only source of the model's belief about how the resource argument
matches, and cloud_tools.go records an investigation that dead-ended on
exactly that belief being wrong.

AWSCloudVocabulary reproduces the shipped text byte for byte, pinned by a
test, so a provider that does not implement CloudDescriber is unaffected."
```

---

## Task 2: Render cloud tool text from the vocabulary

**Files:**
- Modify: `internal/investigate/cloud_tools.go` (whole file)
- Test: `internal/investigate/cloud_tools_test.go` (extend)

- [ ] **Step 1: Write the failing test**

Append to `internal/investigate/cloud_tools_test.go`:

```go
// fakeDescribingCloud is a CloudProvider that ALSO implements CloudDescriber, so the
// tools must prefer its vocabulary over the AWS fallback.
type fakeDescribingCloud struct {
	changes []providers.Change
	health  providers.LogResult
	vocab   providers.CloudVocabulary
}

func (f fakeDescribingCloud) CloudChanges(context.Context, providers.Selector, providers.TimeWindow) ([]providers.Change, error) {
	return f.changes, nil
}

func (f fakeDescribingCloud) ResourceHealth(context.Context, providers.Selector, providers.TimeWindow) (providers.LogResult, error) {
	return f.health, nil
}

func (f fakeDescribingCloud) CloudVocabulary() providers.CloudVocabulary { return f.vocab }

func gcpish() providers.CloudVocabulary {
	return providers.CloudVocabulary{
		Cloud:          "GCP",
		ChangeLog:      "Cloud Audit Logs",
		ChangeExamples: "GKE/Compute/IAM changes",
		ScopeGuidance:  "Optional resource is a SUBSTRING match on protoPayload.resourceName.",
		WidenedBanner:  "resource %q matched no Cloud Audit Log entries. Showing ALL:\n",
		LagNote:        "Cloud Audit Logs lag well under a minute",
		HealthSurface:  "GKE cluster and node-pool conditions.",
		InstanceArg:    "optional Compute Engine instance name",
	}
}

// TestCloudToolsDescribeTheCloudTheyActuallyQuery is the whole point of
// CloudDescriber: a GCP-backed tool must never tell the model it is reading
// CloudTrail.
func TestCloudToolsDescribeTheCloudTheyActuallyQuery(t *testing.T) {
	c := fakeDescribingCloud{vocab: gcpish()}
	changeDesc := CloudWhatChangedTool{Cloud: c}.Description()
	if !strings.Contains(changeDesc, "Cloud Audit Logs") {
		t.Errorf("description does not name Cloud Audit Logs: %q", changeDesc)
	}
	if strings.Contains(changeDesc, "CloudTrail") {
		t.Errorf("description still names CloudTrail on a GCP provider: %q", changeDesc)
	}
	healthDesc := CloudResourceHealthTool{Cloud: c}.Description()
	if strings.Contains(healthDesc, "EKS") || strings.Contains(healthDesc, "EC2") {
		t.Errorf("health description still names AWS services on a GCP provider: %q", healthDesc)
	}
	schema := CloudResourceHealthTool{Cloud: c}.Schema()
	if !strings.Contains(schema, "Compute Engine instance name") {
		t.Errorf("schema does not carry the GCP instance arg: %q", schema)
	}
}

// TestCloudToolsFallBackToAWSWordingWithoutADescriber pins the compatibility half of
// the promise: a provider that does not implement CloudDescriber (which is every
// provider that existed before it) keeps its exact prior text.
func TestCloudToolsFallBackToAWSWordingWithoutADescriber(t *testing.T) {
	c := fakeCloud{} // the pre-existing fake; does NOT implement CloudDescriber
	got := CloudWhatChangedTool{Cloud: c}.Description()
	want := providers.AWSCloudVocabulary().ChangeDescription()
	if got != want {
		t.Errorf("fallback description = %q, want %q", got, want)
	}
}
```

**Note:** `fakeCloud` already exists in `cloud_tools_test.go`. Read that file first and reuse it; if its name differs, use the actual name. Ensure `strings` is imported.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/investigate/ -run TestCloudTools -v`
Expected: FAIL — the descriptions are still hardcoded AWS literals.

- [ ] **Step 3: Rewrite the tool text to come from the vocabulary**

In `internal/investigate/cloud_tools.go`, add the resolver just below the imports:

```go
// vocabularyFor returns the cloud's own vocabulary, or the AWS wording when the
// provider does not describe itself. The fallback is what keeps an AWS deployment's
// tool text byte-identical to what it was before CloudDescriber existed.
func vocabularyFor(c providers.CloudProvider) providers.CloudVocabulary {
	if d, ok := c.(providers.CloudDescriber); ok {
		return d.CloudVocabulary()
	}
	return providers.AWSCloudVocabulary()
}
```

Replace `CloudWhatChangedTool.Description` in full:

```go
// Description returns the tool description, worded for the cloud actually wired.
func (t CloudWhatChangedTool) Description() string {
	return vocabularyFor(t.Cloud).ChangeDescription()
}
```

Replace `CloudResourceHealthTool.Description` in full:

```go
// Description returns the tool description, worded for the cloud actually wired.
func (t CloudResourceHealthTool) Description() string {
	return vocabularyFor(t.Cloud).HealthDescription()
}
```

Replace `CloudResourceHealthTool.Schema` in full — the instance argument's description is cloud-specific, so the schema is now built rather than a constant:

```go
// Schema returns the JSON schema for the arguments. The instance argument's
// description is cloud-specific ("i-…" means nothing on GCP), so it is rendered from
// the vocabulary rather than fixed.
func (t CloudResourceHealthTool) Schema() string {
	b, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"instance_id":   map[string]any{"type": "string", "description": vocabularyFor(t.Cloud).InstanceArg},
			"since_minutes": map[string]any{"type": "integer", "description": "scope scaling-activity lookback to the last N minutes"},
		},
		"required": []string{},
	})
	if err != nil {
		// Unreachable: the map above is a literal of JSON-encodable types. Falling
		// back to the pre-vocabulary schema keeps the tool callable rather than
		// registering an empty schema the model cannot use.
		return `{"type":"object","properties":{"instance_id":{"type":"string"},"since_minutes":{"type":"integer"}},"required":[]}`
	}
	return string(b)
}
```

Finally, in `CloudWhatChangedTool.Call`, replace the hardcoded widening banner. Find the block beginning `if widened {` and replace the `fmt.Fprintf` with:

```go
	if widened {
		fmt.Fprintf(&b, vocabularyFor(t.Cloud).WidenedBanner, in.Resource)
	}
```

Keep the long `// CloudTrail's ResourceName lookup is an EXACT match...` comment above the widening logic — it documents the real incident that motivated the fallback. Add one sentence at its end:

```go
	// The banner text itself now comes from the provider's vocabulary, because this
	// paragraph is an AWS fact: GCP's filter language has a substring operator, so a
	// scoped miss there means the resource genuinely did not appear, and telling the
	// model it probably used the wrong match semantics would be false.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/investigate/ -run TestCloudTools -v`
Expected: PASS.

- [ ] **Step 5: Run the full investigate suite**

Run: `go test -race ./internal/investigate/...`
Expected: PASS. If a golden or eval test pins the old schema string, the byte-for-byte fallback means the *description* is unchanged; only the health **schema** now has descriptions where it had none. If a test pins that schema literally, update it and note the addition in the commit body.

- [ ] **Step 6: Commit**

```bash
git add internal/investigate/cloud_tools.go internal/investigate/cloud_tools_test.go
git commit -m "feat(investigate): render cloud tool text from the provider's vocabulary

cloud_what_changed and cloud_resource_health described AWS unconditionally.
Both now render from CloudDescriber when the provider offers one and fall
back to the AWS wording otherwise, so AWS text is unchanged and a GCP
provider can state the API it actually queries."
```

---

## Task 3: Config — provider constants and the nested `gcp` block

**Files:**
- Modify: `internal/config/config.go:276-284` (the `Cloud` struct)
- Test: `internal/config/cloud_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/config/cloud_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCloudGCPBlockIsFullyOptional pins the zero-configuration promise: on GKE every
// GCP field is autodetected, so `cloud: {provider: gcp}` alone must parse and leave
// the block empty for the provider to fill in. A required field here would break the
// project's standing constraint that no integration adds mandatory config.
func TestCloudGCPBlockIsFullyOptional(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("cloud:\n  provider: gcp\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Cloud.Provider != CloudGCP {
		t.Errorf("provider = %q, want %q", c.Cloud.Provider, CloudGCP)
	}
	if c.Cloud.GCP.Project != "" || c.Cloud.GCP.Location != "" || c.Cloud.GCP.ClusterName != "" {
		t.Errorf("expected an empty GCP block, got %+v", c.Cloud.GCP)
	}
}

// TestCloudGCPBlockRoundTrips covers the escape hatch — the off-GKE or cross-project
// operator who must state all three explicitly.
func TestCloudGCPBlockRoundTrips(t *testing.T) {
	in := "cloud:\n  provider: gcp\n  gcp:\n    project: my-proj\n    location: europe-west1\n    cluster_name: prod\n"
	var c Config
	if err := yaml.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Cloud.GCP.Project != "my-proj" {
		t.Errorf("project = %q, want my-proj", c.Cloud.GCP.Project)
	}
	if c.Cloud.GCP.Location != "europe-west1" {
		t.Errorf("location = %q, want europe-west1", c.Cloud.GCP.Location)
	}
	if c.Cloud.GCP.ClusterName != "prod" {
		t.Errorf("cluster_name = %q, want prod", c.Cloud.GCP.ClusterName)
	}
}

// TestCloudAWSFlatFieldsStillParse is the non-regression half: adding a nested block
// must not disturb the flat AWS spelling that every existing deployment uses.
func TestCloudAWSFlatFieldsStillParse(t *testing.T) {
	in := "cloud:\n  provider: aws\n  region: eu-west-3\n  cluster_name: prod\n"
	var c Config
	if err := yaml.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Cloud.Provider != CloudAWS || c.Cloud.Region != "eu-west-3" || c.Cloud.ClusterName != "prod" {
		t.Errorf("flat AWS fields did not round-trip: %+v", c.Cloud)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestCloud -v`
Expected: FAIL to compile — `undefined: CloudGCP`, `c.Cloud.GCP undefined`.

- [ ] **Step 3: Extend the config types**

In `internal/config/config.go`, replace the `Cloud` struct block (currently lines 276-284) with:

```go
// Cloud provider identifiers for config.cloud.provider. Cloud context is opt-in;
// an empty provider disables the cloud tools entirely.
const (
	CloudAWS = "aws" // CloudTrail what-changed + EC2/ASG/EKS health; auth is EKS Pod Identity / IRSA
	CloudGCP = "gcp" // Cloud Audit Logs what-changed + GKE/MIG/Compute health; auth is GKE Workload Identity
)

// Cloud configures the cloud context provider. Auth is always in-cluster identity —
// EKS Pod Identity / IRSA on AWS, a GKE Workload Identity direct principal binding on
// GCP — never a static key. Empty Provider disables the cloud tools (default).
//
// The AWS fields are flat for back-compatibility with every deployment that predates
// a second cloud; GCP is nested, matching the per-provider blocks Network already
// uses in this file. New clouds nest.
type Cloud struct {
	Provider    string `yaml:"provider"`     // "" (disabled) | "aws" | "gcp"
	Region      string `yaml:"region"`       // AWS only: e.g. eu-west-3 (default: AWS_REGION / IMDS)
	ClusterName string `yaml:"cluster_name"` // AWS only: EKS cluster name, scopes nodegroup/ASG queries

	GCP GCPCloudCfg `yaml:"gcp"` // when provider=gcp
}

// GCPCloudCfg configures the GCP cloud context provider. EVERY field is optional: on
// GKE all three are resolved from the metadata server, falling back to the cluster's
// own node objects, so `cloud: {provider: gcp}` is a complete configuration.
//
// Set them only to override that resolution — a cluster whose metadata server does
// not expose the cluster attributes, or a deployment reading a project other than the
// one it runs in.
type GCPCloudCfg struct {
	Project     string `yaml:"project"`      // GCP project id (default: metadata server)
	Location    string `yaml:"location"`     // cluster region OR zone (default: metadata server)
	ClusterName string `yaml:"cluster_name"` // GKE cluster name (default: metadata server)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -run TestCloud -v`
Expected: PASS, all three tests.

- [ ] **Step 5: Run the full config suite**

Run: `go test -race ./internal/config/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/cloud_test.go
git commit -m "feat(config): add cloud provider constants and the nested gcp block

Every GCP field is optional — the provider resolves project, location and
cluster from the GKE metadata server — so cloud: {provider: gcp} is a
complete configuration. AWS keeps its flat region/cluster_name spelling."
```

---

## Task 4: The `gcp` package skeleton and its vocabulary

**Files:**
- Create: `internal/providers/cloud/gcp/gcp.go`
- Test: `internal/providers/cloud/gcp/gcp_test.go`

- [ ] **Step 1: Add the dependencies**

```bash
go get google.golang.org/api/container/v1@v0.293.0
go get google.golang.org/api/compute/v1@v0.293.0
go get cloud.google.com/go/compute/metadata
go mod tidy
```

Expected: `go.mod` gains `cloud.google.com/go/compute/metadata` as a **direct** requirement (it was indirect at line 36). `google.golang.org/api` stays at v0.293.0.

- [ ] **Step 2: Write the failing test**

Create `internal/providers/cloud/gcp/gcp_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestVocabularyNamesGCPAndNeverAWS is the contract Task 1 built CloudDescriber for:
// every model-facing string this provider produces must describe Cloud Audit Logs and
// GKE, and must not contain an AWS noun anywhere.
func TestVocabularyNamesGCPAndNeverAWS(t *testing.T) {
	v := Client{}.CloudVocabulary()
	if v.Cloud != "GCP" {
		t.Errorf("Cloud = %q, want GCP", v.Cloud)
	}
	if v.ChangeLog != "Cloud Audit Logs" {
		t.Errorf("ChangeLog = %q, want Cloud Audit Logs", v.ChangeLog)
	}
	rendered := v.ChangeDescription() + " " + v.HealthDescription() + " " + v.WidenedBanner + " " + v.InstanceArg
	for _, banned := range []string{"CloudTrail", "EKS", "EC2", "ASG", "AWS", "i-…", "ARN"} {
		if strings.Contains(rendered, banned) {
			t.Errorf("GCP vocabulary leaks the AWS noun %q: %s", banned, rendered)
		}
	}
}

// TestWidenedBannerTakesExactlyOneVerb guards a crash: the banner is used as a
// Printf format with one argument, so a stray or missing verb renders %!(EXTRA…) or
// %!q(MISSING) into text the model reads as evidence.
func TestWidenedBannerTakesExactlyOneVerb(t *testing.T) {
	if n := strings.Count(Client{}.CloudVocabulary().WidenedBanner, "%"); n != 1 {
		t.Errorf("WidenedBanner has %d %% verbs, want exactly 1", n)
	}
}

// Compile-time conformance, mirroring aws.go:75.
var (
	_ providers.CloudProvider = (*Client)(nil)
	_ providers.CloudDescriber = (*Client)(nil)
)
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/providers/cloud/gcp/ -v`
Expected: FAIL to compile — no package `gcp`.

- [ ] **Step 4: Write `gcp.go`**

Create `internal/providers/cloud/gcp/gcp.go`:

```go
// SPDX-License-Identifier: Apache-2.0

// Package gcp implements providers.CloudProvider against GCP using the Google API
// Go clients and in-cluster identity (GKE Workload Identity, resolved through
// Application Default Credentials). All calls are read-only: Cloud Logging
// entries.list over the audit streams (the GCP "what changed" lens), and
// container/compute describes (resource health).
//
// Auth is a Workload Identity DIRECT PRINCIPAL BINDING — the Kubernetes
// ServiceAccount is bound straight to the IAM roles, with no intermediate Google
// service account and no iam.gke.io/gcp-service-account annotation. ADC resolves it
// with no code here; what this package adds is diagnosis, because a missing binding
// otherwise surfaces as a bare 403 in the middle of an investigation.
package gcp

import (
	"context"
	"fmt"

	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/providers"
)

// defaultMaxEvents bounds how many entries a single lens returns. Same budget as the
// AWS client (aws.go:52) so neither cloud floods the model where the other would not.
const defaultMaxEvents = 25

// Narrow API surfaces (just the calls we use) so tests can inject fakes and the real
// generated services satisfy them. Unlike the AWS SDK these are structs rather than
// interfaces, so the seams are the *List/*Get call sites; tests drive them through an
// httptest endpoint, exactly as internal/network/gcpfirewall does.
type entriesAPI interface {
	List(ctx context.Context, req *logging.ListLogEntriesRequest) (*logging.ListLogEntriesResponse, error)
}

// Client is the GCP cloud provider.
type Client struct {
	entries   entriesAPI
	container *container.Service
	compute   *compute.Service

	project     string // GCP project id
	location    string // cluster region or zone
	clusterName string // GKE cluster name
	projectNum  string // numeric project id, for the principal:// binding hint
	identitySrc string // which tier resolved the triple, for the startup log line

	maxEvents int64
}

// loggingEntries adapts the generated Logging service to entriesAPI.
type loggingEntries struct{ svc *logging.Service }

func (l loggingEntries) List(ctx context.Context, req *logging.ListLogEntriesRequest) (*logging.ListLogEntriesResponse, error) {
	return l.svc.Entries.List(req).Context(ctx).Do()
}

// New builds a Client from Application Default Credentials. id carries the resolved
// project/location/cluster (see identity.go); extra ClientOptions are passed to every
// service constructor, which is how tests inject option.WithHTTPClient +
// option.WithEndpoint + option.WithoutAuthentication.
func New(ctx context.Context, id Identity, opts ...option.ClientOption) (*Client, error) {
	if id.Project == "" {
		return nil, fmt.Errorf("gcp: project is required (autodetection failed; set cloud.gcp.project)")
	}
	lsvc, err := logging.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new logging service: %w", err)
	}
	csvc, err := container.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new container service: %w", err)
	}
	msvc, err := compute.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new compute service: %w", err)
	}
	return &Client{
		entries:     loggingEntries{svc: lsvc},
		container:   csvc,
		compute:     msvc,
		project:     id.Project,
		location:    id.Location,
		clusterName: id.ClusterName,
		projectNum:  id.ProjectNumber,
		identitySrc: id.Source,
		maxEvents:   defaultMaxEvents,
	}, nil
}

var (
	_ providers.CloudProvider  = (*Client)(nil)
	_ providers.CloudDescriber = (*Client)(nil)
)

// CloudVocabulary describes Cloud Audit Logs and GKE, so the cloud tools never tell a
// GCP deployment's model that it is reading CloudTrail.
//
// Two fragments differ from AWS in substance, not just nouns. ScopeGuidance says
// SUBSTRING because GCP's filter language has a ':' operator, where CloudTrail's
// ResourceName is an exact match — the difference that dead-ended the investigation
// recorded in cloud_tools.go. And LagNote says under a minute, where CloudTrail lags
// ~15m, so a GCP investigation need not over-widen its window to see a recent change.
func (Client) CloudVocabulary() providers.CloudVocabulary {
	return providers.CloudVocabulary{
		Cloud:     "GCP",
		ChangeLog: "Cloud Audit Logs",
		ChangeExamples: "GKE/Compute/IAM/network changes, manual actions, and Google-initiated host " +
			"events (host error, live migration, preemption) invisible to GitOps",
		ScopeGuidance: "Optional resource is a SUBSTRING match on the audit entry's " +
			"protoPayload.resourceName — a bare name like \"my-nodepool\" matches, and so does a full " +
			"\"projects/p/zones/z/instances/i\" path; OMIT it to see every mutating event, which is the " +
			"right move when you do not know the identifier.",
		WidenedBanner: "resource %q matched no Cloud Audit Log entries in the window — the filter is a " +
			"SUBSTRING match on protoPayload.resourceName, so this means the resource genuinely did not " +
			"appear, not that the name was spelled wrong. Showing ALL mutating events in the window instead:\n",
		LagNote: "Cloud Audit Logs lag well under a minute",
		HealthSurface: "GKE cluster and node-pool status + conditions, managed-instance-group errors " +
			"(stockouts, quota and IP exhaustion), and — when given a Compute Engine instance name — its " +
			"instance status.",
		InstanceArg: "optional Compute Engine instance name",
	}
}

// deref safely dereferences an optional string.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
```

**Note:** this file references `Identity` (Task 5) and does not yet define `CloudChanges`/`ResourceHealth` (Tasks 6-8), so it will not compile until Task 5. That is expected; Step 5 below only checks the dependency fetch.

- [ ] **Step 5: Verify the dependencies resolve**

Run: `go build ./... 2>&1 | head -20`
Expected: errors naming `Identity`, `CloudChanges` and `ResourceHealth` as undefined — and **no** errors about missing modules. If a module is missing, re-run Step 1.

- [ ] **Step 6: Commit (compiles after Task 6; commit the dependency change now)**

```bash
git add go.mod go.sum
git commit -m "chore(deps): add container/v1, compute/v1 and compute/metadata

compute/v1 is 15M of generated source but links to +88 KB measured with
-trimpath -ldflags='-s -w', so the single-binary property is unaffected and
the idiomatic generated client is the right choice over hand-rolled REST."
```

---

## Task 5: Identity resolution (`identity.go`)

**Files:**
- Create: `internal/providers/cloud/gcp/identity.go`
- Test: `internal/providers/cloud/gcp/identity_test.go`

**Tier 3 is provisional.** It exists only because it is not established that the GKE metadata server exposes `instance/attributes/cluster-location` to Pods across all GKE versions and modes. Step 3 of the live runbook settles it, and if tier 2 holds, tier 3 is deleted. **Write it so that deletion is removing `nodeLookup` and one `case` — nothing else.**

- [ ] **Step 1: Write the failing test**

Create `internal/providers/cloud/gcp/identity_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"testing"
)

// stubSources lets a test choose which tiers answer, so precedence is observable
// without a metadata server or a cluster.
type stubSources struct {
	meta map[string]string
	node *NodeIdentity
}

func (s stubSources) metadataGet(_ context.Context, key string) (string, error) {
	if v, ok := s.meta[key]; ok && v != "" {
		return v, nil
	}
	return "", errors.New("not found")
}

func (s stubSources) nodeLookup(context.Context) (*NodeIdentity, error) {
	if s.node == nil {
		return nil, errors.New("no cluster access")
	}
	return s.node, nil
}

func TestResolveIdentityPrefersConfigThenMetadataThenNode(t *testing.T) {
	fullMeta := map[string]string{
		metaProjectID:    "meta-proj",
		metaProjectNum:   "123456789012",
		metaClusterName:  "meta-cluster",
		metaClusterLoc:   "europe-west1",
	}
	node := &NodeIdentity{Project: "node-proj", Location: "us-central1-a"}

	cases := []struct {
		name       string
		cfg        Identity
		src        stubSources
		wantProj   string
		wantLoc    string
		wantClus   string
		wantSource string
	}{
		{
			name:       "explicit config wins outright — it is the operator overriding detection",
			cfg:        Identity{Project: "cfg-proj", Location: "asia-east1", ClusterName: "cfg-cluster"},
			src:        stubSources{meta: fullMeta, node: node},
			wantProj:   "cfg-proj",
			wantLoc:    "asia-east1",
			wantClus:   "cfg-cluster",
			wantSource: sourceConfig,
		},
		{
			name:       "no config: the metadata server answers all three",
			src:        stubSources{meta: fullMeta, node: node},
			wantProj:   "meta-proj",
			wantLoc:    "europe-west1",
			wantClus:   "meta-cluster",
			wantSource: sourceMetadata,
		},
		{
			name:       "metadata lacks the cluster attributes: the node object supplies what it can",
			src:        stubSources{meta: map[string]string{metaProjectID: "meta-proj", metaProjectNum: "123456789012"}, node: node},
			wantProj:   "meta-proj",
			wantLoc:    "us-central1-a",
			wantClus:   "",
			wantSource: sourceNode,
		},
		{
			name:       "config fills only what it states; the rest still falls through",
			cfg:        Identity{ClusterName: "cfg-cluster"},
			src:        stubSources{meta: fullMeta, node: node},
			wantProj:   "meta-proj",
			wantLoc:    "europe-west1",
			wantClus:   "cfg-cluster",
			wantSource: sourceMetadata,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveIdentity(context.Background(), c.cfg, c.src)
			if got.Project != c.wantProj {
				t.Errorf("Project = %q, want %q", got.Project, c.wantProj)
			}
			if got.Location != c.wantLoc {
				t.Errorf("Location = %q, want %q", got.Location, c.wantLoc)
			}
			if got.ClusterName != c.wantClus {
				t.Errorf("ClusterName = %q, want %q", got.ClusterName, c.wantClus)
			}
			if got.Source != c.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, c.wantSource)
			}
		})
	}
}

// TestResolveIdentityReportsNothingWhenNoTierAnswers pins the failure path: New must
// get an empty Project so it can refuse with an actionable message rather than
// querying "projects/".
func TestResolveIdentityReportsNothingWhenNoTierAnswers(t *testing.T) {
	got := resolveIdentity(context.Background(), Identity{}, stubSources{})
	if got.Project != "" {
		t.Errorf("Project = %q, want empty", got.Project)
	}
	if got.Source != sourceNone {
		t.Errorf("Source = %q, want %q", got.Source, sourceNone)
	}
}

// TestParseProviderIDExtractsProjectAndZone pins the tier-3 parse. A GKE node's
// providerID is gce://PROJECT/ZONE/INSTANCE; anything else must be rejected rather
// than yielding a plausible-looking wrong project.
func TestParseProviderIDExtractsProjectAndZone(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		project string
		zone    string
		ok      bool
	}{
		{"a GKE node", "gce://my-proj/us-central1-a/gke-prod-pool-abc", "my-proj", "us-central1-a", true},
		{"an AWS node is not ours", "aws:///eu-west-1a/i-0abc", "", "", false},
		{"a truncated id yields nothing", "gce://my-proj", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, z, ok := parseProviderID(c.in)
			if ok != c.ok || p != c.project || z != c.zone {
				t.Errorf("parseProviderID(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, p, z, ok, c.project, c.zone, c.ok)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/cloud/gcp/ -run TestResolveIdentity -v`
Expected: FAIL to compile — `undefined: resolveIdentity`, `undefined: Identity`.

- [ ] **Step 3: Write `identity.go`**

Create `internal/providers/cloud/gcp/identity.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"strings"

	"cloud.google.com/go/compute/metadata"
)

// Metadata server keys. The first two are available on any GCE instance; the cluster
// attributes are stamped by GKE and proxied to Pods by the GKE metadata server.
const (
	metaProjectID   = "project/project-id"
	metaProjectNum  = "project/numeric-project-id"
	metaClusterName = "instance/attributes/cluster-name"
	metaClusterLoc  = "instance/attributes/cluster-location"
)

// Which tier resolved the identity. Logged at startup, and load-bearing: it is how
// the live-validation run answers whether tier 3 is needed at all.
const (
	sourceConfig   = "config"
	sourceMetadata = "metadata-server"
	sourceNode     = "node-provider-id"
	sourceNone     = "unresolved"
)

// Identity is the project/location/cluster triple the GCP provider scopes every query
// with, plus provenance.
type Identity struct {
	Project       string
	Location      string
	ClusterName   string
	ProjectNumber string // numeric project id — required by the principal:// binding string
	Source        string // which tier resolved it
}

// NodeIdentity is what tier 3 recovers from a Kubernetes node object. Exported
// because ResolveIdentity takes a lookup returning it, and the caller that supplies
// that lookup lives in internal/app.
type NodeIdentity struct {
	Project  string
	Location string
}

// identitySources are the two lookups resolveIdentity fans out to, injected so the
// precedence table can be tested without a metadata server or a cluster.
type identitySources interface {
	metadataGet(ctx context.Context, key string) (string, error)
	nodeLookup(ctx context.Context) (*NodeIdentity, error)
}

// resolveIdentity fills the triple from the first tier that answers each field:
// explicit config, then the GKE metadata server, then a cluster node's providerID.
//
// Per-FIELD rather than per-tier, deliberately: an operator who sets only
// cloud.gcp.cluster_name should not thereby have to restate the project the metadata
// server already knows. Source names the highest tier that supplied anything, which
// is what the startup log line reports.
//
// TIER 3 IS PROVISIONAL. It exists because it is not established that the GKE
// metadata server proxies instance/attributes/cluster-location to Pods across every
// GKE version and mode. If the live-validation run shows tier 2 is reliable, delete
// nodeLookup, the NodeIdentity type and the sourceNode branch — nothing else depends
// on them.
func resolveIdentity(ctx context.Context, cfg Identity, src identitySources) Identity {
	out := cfg
	source := ""
	if cfg.Project != "" || cfg.Location != "" || cfg.ClusterName != "" {
		source = sourceConfig
	}

	get := func(key string) string {
		v, err := src.metadataGet(ctx, key)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}

	// Tier 2: the metadata server.
	if out.Project == "" {
		if v := get(metaProjectID); v != "" {
			out.Project, source = v, sourceMetadata
		}
	}
	if out.ClusterName == "" {
		if v := get(metaClusterName); v != "" {
			out.ClusterName, source = v, sourceMetadata
		}
	}
	if out.Location == "" {
		if v := get(metaClusterLoc); v != "" {
			out.Location, source = v, sourceMetadata
		}
	}
	// The project NUMBER is never configurable: it is only ever used to render the
	// principal:// binding hint, and an operator-supplied one would be a new way to
	// print a subtly wrong command.
	if out.ProjectNumber == "" {
		out.ProjectNumber = get(metaProjectNum)
	}

	// Tier 3: a node object's providerID and topology labels.
	if out.Project == "" || out.Location == "" {
		if n, err := src.nodeLookup(ctx); err == nil && n != nil {
			if out.Project == "" && n.Project != "" {
				out.Project, source = n.Project, sourceNode
			}
			if out.Location == "" && n.Location != "" {
				out.Location, source = n.Location, sourceNode
			}
		}
	}

	if source == "" {
		source = sourceNone
	}
	out.Source = source
	return out
}

// parseProviderID extracts (project, zone) from a GKE node's spec.providerID, which
// is "gce://PROJECT/ZONE/INSTANCE". It returns ok=false for any other shape — an AWS
// or bare-metal providerID, or a truncated one — because a partial parse here would
// scope every subsequent query to a plausible-looking wrong project.
func parseProviderID(id string) (project, zone string, ok bool) {
	const prefix = "gce://"
	if !strings.HasPrefix(id, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(id, prefix), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// liveSources is the production identitySources: the real metadata server, and a node
// lookup the caller supplies (nil when no cluster is reachable, which simply makes
// tier 3 unavailable rather than an error).
type liveSources struct {
	nodes func(ctx context.Context) (*NodeIdentity, error)
}

func (liveSources) metadataGet(ctx context.Context, key string) (string, error) {
	return metadata.GetWithContext(ctx, key)
}

func (l liveSources) nodeLookup(ctx context.Context) (*NodeIdentity, error) {
	if l.nodes == nil {
		return nil, errNoNodeSource
	}
	return l.nodes(ctx)
}

// errNoNodeSource marks tier 3 as unavailable (no cluster reader was wired). It is a
// sentinel rather than a nil return so resolveIdentity's error branch stays uniform.
var errNoNodeSource = errors.New("gcp: no node source wired")

// ResolveIdentity is the package entry point: it resolves the triple from config,
// the metadata server, and optionally a node lookup.
func ResolveIdentity(ctx context.Context, cfg Identity, nodes func(context.Context) (*NodeIdentity, error)) Identity {
	return resolveIdentity(ctx, cfg, liveSources{nodes: nodes})
}
```

Add `"errors"` to the import block.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/providers/cloud/gcp/ -run 'TestResolveIdentity|TestParseProviderID' -v`
Expected: PASS, all subtests. (`gcp.go` still references undefined `CloudChanges`/`ResourceHealth`; if the package will not build, temporarily add no-op stubs returning `nil, nil` and delete them in Tasks 6 and 8.)

- [ ] **Step 5: Commit**

```bash
git add internal/providers/cloud/gcp/identity.go internal/providers/cloud/gcp/identity_test.go
git commit -m "feat(gcp): resolve project, location and cluster in three tiers

Config, then the GKE metadata server, then a node's providerID. Per-field
rather than per-tier so setting only cluster_name does not force restating
the project. Tier 3 is provisional pending live validation and is written
to delete cleanly."
```

---

## Task 6: `CloudChanges` (`auditlog.go`)

**Files:**
- Create: `internal/providers/cloud/gcp/auditlog.go`
- Test: `internal/providers/cloud/gcp/auditlog_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/providers/cloud/gcp/auditlog_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/providers"
)

// auditEntry builds a LogEntry carrying an AuditLog protoPayload, the shape both the
// activity and system_event streams use.
func auditEntry(t *testing.T, ts, service, method, principal, resourceName, resType string, code int64, msg string) *logging.LogEntry {
	t.Helper()
	payload := map[string]any{
		"@type":         "type.googleapis.com/google.cloud.audit.AuditLog",
		"serviceName":   service,
		"methodName":    method,
		"resourceName":  resourceName,
		"authenticationInfo": map[string]any{"principalEmail": principal},
	}
	if code != 0 {
		payload["status"] = map[string]any{"code": code, "message": msg}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &logging.LogEntry{
		Timestamp:    ts,
		InsertId:     "ins-" + method,
		ProtoPayload: b,
		Resource:     &logging.MonitoredResource{Type: resType, Labels: map[string]string{"location": "europe-west1"}},
	}
}

// newTestClient serves one canned entries.list response to every request.
func newTestClient(t *testing.T, resp any) (*Client, func()) {
	t.Helper()
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	c, err := New(context.Background(),
		Identity{Project: "my-proj", Location: "europe-west1", ClusterName: "prod", ProjectNumber: "123456789012"},
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		srv.Close()
		t.Fatalf("New: %v", err)
	}
	return c, srv.Close
}

// TestCloudChangesMapsAnAuditEntryOntoTheChangeModel pins every field the engine-
// agnostic timeline reads, so a GCP change joins the same view as a Flux diff.
func TestCloudChangesMapsAnAuditEntryOntoTheChangeModel(t *testing.T) {
	ts := "2026-08-24T10:00:00Z"
	resp := logging.ListLogEntriesResponse{Entries: []*logging.LogEntry{
		auditEntry(t, ts, "container.googleapis.com", "google.container.v1.ClusterManager.SetNodePoolSize",
			"alice@example.com", "projects/my-proj/locations/europe-west1/clusters/prod/nodePools/default",
			"gke_nodepool", 0, ""),
	}}
	c, done := newTestClient(t, resp)
	defer done()

	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1", len(got))
	}
	ch := got[0]
	if ch.Engine != providers.EngineGCP {
		t.Errorf("Engine = %q, want %q", ch.Engine, providers.EngineGCP)
	}
	if ch.Type != providers.ChangeCloudAPI {
		t.Errorf("Type = %q, want %q", ch.Type, providers.ChangeCloudAPI)
	}
	if ch.ManagedBy != "container.googleapis.com" {
		t.Errorf("ManagedBy = %q", ch.ManagedBy)
	}
	if ch.ToRev == "" {
		t.Error("ToRev is empty; the model needs a stable change_ref handle")
	}
	if ch.Workload.Kind != "gke_nodepool" {
		t.Errorf("Workload.Kind = %q, want gke_nodepool", ch.Workload.Kind)
	}
	if ch.Workload.Account != "my-proj" {
		t.Errorf("Workload.Account = %q, want my-proj", ch.Workload.Account)
	}
	if ch.Workload.Region != "europe-west1" {
		t.Errorf("Workload.Region = %q, want europe-west1", ch.Workload.Region)
	}
	want, _ := time.Parse(time.RFC3339, ts)
	if !ch.When.Equal(want) {
		t.Errorf("When = %v, want %v", ch.When, want)
	}
	if !strings.Contains(ch.Source.Path, "alice@example.com") {
		t.Errorf("Source.Path %q does not name the principal", ch.Source.Path)
	}
}

// TestCloudChangesMarksFailedCalls is the highest-value mapping: a denied or
// quota-exhausted call rendered as a success would have the model conclude the change
// took effect. Mirrors the AWS errorCode handling in cloudtrail.go.
func TestCloudChangesMarksFailedCalls(t *testing.T) {
	resp := logging.ListLogEntriesResponse{Entries: []*logging.LogEntry{
		auditEntry(t, "2026-08-24T10:00:00Z", "compute.googleapis.com", "v1.compute.instances.insert",
			"svc@my-proj.iam.gserviceaccount.com", "projects/my-proj/zones/europe-west1-b/instances/node-1",
			"gce_instance", 8, "Quota 'CPUS' exceeded. Limit: 24.0"),
	}}
	c, done := newTestClient(t, resp)
	defer done()

	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1", len(got))
	}
	path := got[0].Source.Path
	if !strings.Contains(path, "FAILED") {
		t.Errorf("Source.Path %q does not mark the call as failed", path)
	}
	if !strings.Contains(path, "RESOURCE_EXHAUSTED") {
		t.Errorf("Source.Path %q does not name the rpc code (8 = RESOURCE_EXHAUSTED)", path)
	}
	if !strings.Contains(path, "Quota 'CPUS' exceeded") {
		t.Errorf("Source.Path %q drops the status message", path)
	}
}

// TestCloudChangesFiltersBothAuditStreams pins that system_event is queried too — it
// carries host errors, live migration and preemption, which have no AWS equivalent
// and are invisible if the filter names only the activity log.
func TestCloudChangesFiltersBothAuditStreams(t *testing.T) {
	var gotFilter string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req logging.ListLogEntriesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotFilter = req.Filter
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	c, err := New(context.Background(), Identity{Project: "my-proj"},
		option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	if _, err := c.CloudChanges(context.Background(), providers.Selector{Name: "my-nodepool"},
		providers.TimeWindow{Start: start, End: start.Add(time.Hour)}); err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	for _, want := range []string{
		"cloudaudit.googleapis.com%2Factivity",
		"cloudaudit.googleapis.com%2Fsystem_event",
		`protoPayload.resourceName:"my-nodepool"`,
		`timestamp>="2026-08-24T09:00:00Z"`,
	} {
		if !strings.Contains(gotFilter, want) {
			t.Errorf("filter %q does not contain %q", gotFilter, want)
		}
	}
}

// TestCloudChangesAppendsTheTruncationSentinelLast pins ordering: a zero When would
// sort the sentinel among real events, so it must be appended after the sort and cap.
func TestCloudChangesAppendsTheTruncationSentinelLast(t *testing.T) {
	var entries []*logging.LogEntry
	for i := 0; i < defaultMaxEvents+5; i++ {
		entries = append(entries, auditEntry(t,
			time.Date(2026, 8, 24, 10, 0, i, 0, time.UTC).Format(time.RFC3339),
			"compute.googleapis.com", "v1.compute.instances.insert", "alice@example.com",
			"projects/my-proj/zones/z/instances/n", "gce_instance", 0, ""))
	}
	c, done := newTestClient(t, logging.ListLogEntriesResponse{Entries: entries})
	defer done()

	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(got) != defaultMaxEvents+1 {
		t.Fatalf("got %d changes, want %d (cap + sentinel)", len(got), defaultMaxEvents+1)
	}
	last := got[len(got)-1]
	if last.Workload.Kind != "(truncated)" {
		t.Errorf("last change is not the sentinel: %+v", last.Workload)
	}
	if !got[0].When.After(got[1].When) {
		t.Error("changes are not newest-first")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/cloud/gcp/ -run TestCloudChanges -v`
Expected: FAIL to compile — `c.CloudChanges undefined` (or the stub returns nil).

- [ ] **Step 3: Write `auditlog.go`**

Create `internal/providers/cloud/gcp/auditlog.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	logging "google.golang.org/api/logging/v2"

	"github.com/Smana/runlore/internal/providers"
)

// The two audit streams this lens reads. Both are always-on and free, and both are
// mutating by construction, which is why there is no read-only filter here — the AWS
// side needs one because CloudTrail carries reads in the same stream.
//
// data_access is deliberately NOT read: it is off by default, dominated by reads, and
// would require roles/logging.privateLogViewer — a materially wider grant for a
// stream that mostly answers "who looked at this".
const (
	activityLog    = "cloudaudit.googleapis.com%2Factivity"
	systemEventLog = "cloudaudit.googleapis.com%2Fsystem_event"
)

// auditPayload is the subset of the AuditLog proto this lens surfaces.
type auditPayload struct {
	ServiceName        string `json:"serviceName"`
	MethodName         string `json:"methodName"`
	ResourceName       string `json:"resourceName"`
	AuthenticationInfo struct {
		PrincipalEmail string `json:"principalEmail"`
	} `json:"authenticationInfo"`
	Status struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

// rpcCodeName maps the google.rpc.Code numbers that actually appear on a failed audit
// entry to their names. A number alone ("status code 8") means nothing to the model;
// RESOURCE_EXHAUSTED is a diagnosis.
var rpcCodeName = map[int64]string{
	1: "CANCELLED", 2: "UNKNOWN", 3: "INVALID_ARGUMENT", 4: "DEADLINE_EXCEEDED",
	5: "NOT_FOUND", 6: "ALREADY_EXISTS", 7: "PERMISSION_DENIED", 8: "RESOURCE_EXHAUSTED",
	9: "FAILED_PRECONDITION", 10: "ABORTED", 11: "OUT_OF_RANGE", 12: "UNIMPLEMENTED",
	13: "INTERNAL", 14: "UNAVAILABLE", 15: "DATA_LOSS", 16: "UNAUTHENTICATED",
}

// CloudChanges returns recent MUTATING GCP control-plane events in the window,
// normalized to the engine-agnostic Change model so they join the same "what changed"
// timeline as GitOps diffs. When the selector carries a Name it scopes the lookup by
// a SUBSTRING match on protoPayload.resourceName.
//
// The substring operator is the material difference from the AWS lens, whose
// ResourceName is an exact match — see the incident recorded in cloud_tools.go. A
// scoped miss here therefore means the resource genuinely did not appear.
func (c *Client) CloudChanges(ctx context.Context, sel providers.Selector, w providers.TimeWindow) ([]providers.Change, error) {
	filter := fmt.Sprintf(`logName=("projects/%s/logs/%s" OR "projects/%s/logs/%s")`,
		c.project, activityLog, c.project, systemEventLog)
	if !w.Start.IsZero() {
		filter += fmt.Sprintf(` AND timestamp>="%s"`, w.Start.Format(time.RFC3339))
	}
	if !w.End.IsZero() {
		filter += fmt.Sprintf(` AND timestamp<="%s"`, w.End.Format(time.RFC3339))
	}
	if sel.Name != "" {
		filter += fmt.Sprintf(` AND protoPayload.resourceName:%q`, sel.Name)
	}

	var changes []providers.Change
	truncated := false
	token := ""
	// Page until the cap binds or the stream is exhausted. A single List returns one
	// page, so without paging a busy window is silently cut to whatever fit in it.
	for {
		resp, err := c.entries.List(ctx, &logging.ListLogEntriesRequest{
			ResourceNames: []string{"projects/" + c.project},
			Filter:        filter,
			OrderBy:       "timestamp desc",
			PageSize:      c.maxEvents,
			PageToken:     token,
		})
		if err != nil {
			return nil, fmt.Errorf("list audit log entries: %w", err)
		}
		for _, e := range resp.Entries {
			if e == nil {
				continue
			}
			ch, ok := c.entryToChange(e)
			if !ok {
				continue // not an AuditLog payload; skip rather than fail the query
			}
			changes = append(changes, ch)
		}
		if int64(len(changes)) > c.maxEvents {
			truncated = true
			break
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}

	// Sort newest-first BEFORE capping so the cap keeps the newest events regardless
	// of what order the API returned.
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].When.After(changes[j].When) })
	if int64(len(changes)) > c.maxEvents {
		truncated = true
		changes = changes[:c.maxEvents]
	}
	// Appended AFTER the sort and cap: the sentinel has a zero When, which would
	// otherwise sort it among real events.
	if truncated {
		changes = append(changes, truncatedChange(c.maxEvents))
	}
	return changes, nil
}

// truncatedChange is the sentinel marking a partial timeline. Same shape as the AWS
// one (cloudtrail.go:96) so the tool renders both clouds' partial views identically.
func truncatedChange(limit int64) providers.Change {
	return providers.Change{
		Engine: providers.EngineGCP,
		Type:   providers.ChangeCloudAPI,
		Workload: providers.Workload{
			Kind: "(truncated)",
			Name: fmt.Sprintf("results truncated at %d — more events matched; narrow the window or resource", limit),
		},
	}
}

// entryToChange maps one audit LogEntry onto an engine-agnostic Change. ok=false
// means the entry carried no AuditLog payload and should be skipped.
func (c *Client) entryToChange(e *logging.LogEntry) (providers.Change, bool) {
	if len(e.ProtoPayload) == 0 {
		return providers.Change{}, false
	}
	var p auditPayload
	if err := json.Unmarshal(e.ProtoPayload, &p); err != nil {
		return providers.Change{}, false
	}
	ch := providers.Change{
		Engine:    providers.EngineGCP,
		Type:      providers.ChangeCloudAPI,
		ManagedBy: p.ServiceName,
		ToRev:     e.InsertId, // stable handle for the model's change_ref
	}
	if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
		ch.When = t
	}
	ch.Workload = providers.Workload{
		Name:    p.ResourceName,
		Account: c.project,
	}
	if e.Resource != nil {
		ch.Workload.Kind = e.Resource.Type
		// GCP spells the scope label differently per resource type; location covers
		// regional resources and zone covers zonal ones.
		if v := e.Resource.Labels["location"]; v != "" {
			ch.Workload.Region = v
		} else if v := e.Resource.Labels["zone"]; v != "" {
			ch.Workload.Region = v
		}
	}
	// Source.Path carries "methodName by principalEmail", plus a FAILED suffix when
	// the call did not succeed — so the model does not read a denied or
	// quota-exhausted call as a change that took effect.
	path := p.MethodName + " by " + p.AuthenticationInfo.PrincipalEmail
	if p.Status.Code != 0 {
		name, ok := rpcCodeName[p.Status.Code]
		if !ok {
			name = fmt.Sprintf("code %d", p.Status.Code)
		}
		path += " — FAILED: " + name
		if p.Status.Message != "" {
			path += " (" + p.Status.Message + ")"
		}
	}
	ch.Source = providers.SourceRef{Path: path}
	return ch, true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/providers/cloud/gcp/ -run TestCloudChanges -v`
Expected: PASS, all four tests.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/cloud/gcp/auditlog.go internal/providers/cloud/gcp/auditlog_test.go
git commit -m "feat(gcp): read Cloud Audit Logs as the what-changed lens

Queries the activity and system_event streams — both always-on, free and
mutating by construction, so no read-only filter is needed. system_event
carries host errors, live migration and preemption, which the AWS lens has
no equivalent for. Failed calls render their google.rpc.Code by name so a
quota-exhausted call is not read as a change that took effect."
```

---

## Task 7: `ResourceHealth` — GKE cluster, node pools, Autopilot

**Files:**
- Create: `internal/providers/cloud/gcp/resourcehealth.go`
- Test: `internal/providers/cloud/gcp/resourcehealth_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/providers/cloud/gcp/resourcehealth_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	container "google.golang.org/api/container/v1"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/providers"
)

// routedClient serves a different canned body per URL path substring, so one test can
// drive the container and compute calls independently.
func routedClient(t *testing.T, routes map[string]any) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for frag, body := range routes {
			if strings.Contains(r.URL.Path, frag) {
				b, _ := json.Marshal(body)
				_, _ = w.Write(b)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`))
	}))
	c, err := New(context.Background(),
		Identity{Project: "my-proj", Location: "europe-west1", ClusterName: "prod", ProjectNumber: "123456789012"},
		option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		srv.Close()
		t.Fatalf("New: %v", err)
	}
	return c, srv.Close
}

func joined(lines providers.LogResult) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestResourceHealthReportsDegradedNodePools covers the primary Standard-cluster path:
// cluster status, per-pool status and conditions, and version skew.
func TestResourceHealthReportsDegradedNodePools(t *testing.T) {
	cluster := container.Cluster{
		Name:           "prod",
		Status:         "DEGRADED",
		StatusMessage:  "one or more node pools are degraded",
		CurrentMasterVersion: "1.30.4-gke.100",
		NodePools: []*container.NodePool{{
			Name:          "default",
			Status:        "ERROR",
			StatusMessage: "Instance group is unhealthy",
			Version:       "1.28.9-gke.100",
			InitialNodeCount: 3,
			Autoscaling:   &container.NodePoolAutoscaling{Enabled: true, MinNodeCount: 1, MaxNodeCount: 10},
			InstanceGroupUrls: []string{
				"https://www.googleapis.com/compute/v1/projects/my-proj/zones/europe-west1-b/instanceGroupManagers/gke-prod-default-grp",
			},
		}},
	}
	c, done := routedClient(t, map[string]any{
		"/clusters/": cluster,
		"listErrors": map[string]any{"items": []any{}},
		"listManagedInstances": map[string]any{"managedInstances": []any{}},
	})
	defer done()

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)
	for _, want := range []string{"DEGRADED", "default", "ERROR", "Instance group is unhealthy", "1.28.9-gke.100", "1.30.4-gke.100"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// TestResourceHealthOnAutopilotSaysTheNodeLayerIsManaged is the degradation contract.
// Running the MIG and instance sub-queries on Autopilot returns nothing, which the
// model would reasonably read as "capacity is fine" — a false negative on exactly the
// question this tool exists to answer. So they are skipped and the reason is stated.
func TestResourceHealthOnAutopilotSaysTheNodeLayerIsManaged(t *testing.T) {
	cluster := container.Cluster{
		Name:      "prod",
		Status:    "RUNNING",
		Autopilot: &container.Autopilot{Enabled: true},
		CurrentMasterVersion: "1.30.4-gke.100",
	}
	c, done := routedClient(t, map[string]any{"/clusters/": cluster})
	defer done()

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)
	if !strings.Contains(out, "Autopilot") {
		t.Errorf("output does not mention Autopilot:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "google-managed") {
		t.Errorf("output does not say the node layer is Google-managed:\n%s", out)
	}
}

// TestResourceHealthDegradesWhenTheClusterLookupFails pins the best-effort contract
// shared with the AWS provider (resourcehealth.go:25): a failing sub-query contributes
// a line, it does not fail the call.
func TestResourceHealthDegradesWhenTheClusterLookupFails(t *testing.T) {
	c, done := routedClient(t, map[string]any{}) // every route 404s
	defer done()

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth must not hard-fail on a sub-query error, got: %v", err)
	}
	if !strings.Contains(joined(got), "gke") {
		t.Errorf("expected an error line naming the failed gke lookup:\n%s", joined(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/cloud/gcp/ -run TestResourceHealth -v`
Expected: FAIL — `c.ResourceHealth undefined` (or the stub returns nil).

- [ ] **Step 3: Write `resourcehealth.go` (cluster half)**

Create `internal/providers/cloud/gcp/resourcehealth.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// ResourceHealth returns cloud-side state for the resources backing the selector: GKE
// cluster and node-pool status, managed-instance-group errors, and — when the selector
// names an instance — that instance's status.
//
// Best-effort, matching the AWS contract (resourcehealth.go:25): a failing sub-query
// contributes an error line, not a hard failure, so partial cloud visibility still
// helps an investigation.
func (c *Client) ResourceHealth(ctx context.Context, sel providers.Selector, w providers.TimeWindow) (providers.LogResult, error) {
	var lines providers.LogResult
	add := func(format string, a ...any) {
		lines = append(lines, providers.LogLine{Message: fmt.Sprintf(format, a...)})
	}

	migs, autopilot := c.describeCluster(ctx, add)

	// On Autopilot the node layer is Google-managed: there are no node pools the
	// operator controls, and the MIGs backing them are not in their project's view.
	// Running the sub-queries anyway returns empty results, which the model reads as
	// "capacity is fine" — a false negative on the exact question this tool answers.
	// So they are skipped and the reason is stated.
	if autopilot {
		add("NOTE: this is a GKE Autopilot cluster — the node layer is Google-managed, so " +
			"node-pool, instance-group and instance health are not visible here. Node-level causes " +
			"must be inferred from Kubernetes node conditions and events instead.")
		return lines, nil
	}

	c.describeMIGs(ctx, migs, w, add)
	c.describeInstance(ctx, sel, add)
	return lines, nil
}

// describeCluster reports cluster and node-pool state and returns the MIG references
// the pools name, so describeMIGs never has to guess which groups belong to this
// cluster — where the AWS side must tag-match (resourcehealth.go, asgInCluster).
func (c *Client) describeCluster(ctx context.Context, add func(string, ...any)) (migs []string, autopilot bool) {
	if c.clusterName == "" || c.location == "" {
		add("gke: cluster name/location unresolved — set cloud.gcp.cluster_name and cloud.gcp.location")
		return nil, false
	}
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", c.project, c.location, c.clusterName)
	cl, err := c.container.Projects.Locations.Clusters.Get(name).Context(ctx).Do()
	if err != nil {
		// 403 and 404 mean different things and must not be collapsed: a 403 is a
		// missing container.clusterViewer binding, a 404 is a wrong cluster name or
		// location — an identity-resolution bug. Reporting both as "denied" sends the
		// operator debugging IAM for a metadata problem.
		add("gke cluster %s: %s", c.clusterName, describeAPIError(err,
			"missing roles/container.clusterViewer on the RunLore principal",
			fmt.Sprintf("no cluster %q at location %q — check cloud.gcp.cluster_name/location (resolved from %s)",
				c.clusterName, c.location, c.identitySrc)))
		return nil, false
	}
	autopilot = cl.Autopilot != nil && cl.Autopilot.Enabled
	add("GKE cluster %s: status=%s%s version=%s%s", cl.Name, cl.Status,
		optional(" (%s)", cl.StatusMessage), cl.CurrentMasterVersion,
		map[bool]string{true: " mode=Autopilot", false: ""}[autopilot])
	for _, cond := range cl.Conditions {
		add("  cluster condition: %s %s", cond.CanonicalCode, cond.Message)
	}
	for _, np := range cl.NodePools {
		if np == nil {
			continue
		}
		scale := ""
		if np.Autoscaling != nil && np.Autoscaling.Enabled {
			scale = fmt.Sprintf(" autoscaling=%d..%d", np.Autoscaling.MinNodeCount, np.Autoscaling.MaxNodeCount)
		}
		// Version skew between a pool and the control plane is a standing cause of
		// scheduling and API-compat failures, and it is invisible from inside the
		// cluster once the nodes are Ready.
		skew := ""
		if np.Version != "" && cl.CurrentMasterVersion != "" && np.Version != cl.CurrentMasterVersion {
			skew = fmt.Sprintf(" SKEW: pool=%s control-plane=%s", np.Version, cl.CurrentMasterVersion)
		}
		add("  node pool %s: status=%s%s nodes=%d%s%s", np.Name, np.Status,
			optional(" (%s)", np.StatusMessage), np.InitialNodeCount, scale, skew)
		for _, cond := range np.Conditions {
			add("    condition: %s %s", cond.CanonicalCode, cond.Message)
		}
		migs = append(migs, np.InstanceGroupUrls...)
	}
	return migs, autopilot
}

// optional renders format with v only when v is non-empty, so an absent status
// message does not print an empty "()" the model might read as a truncated field.
func optional(format, v string) string {
	if v == "" {
		return ""
	}
	return fmt.Sprintf(format, v)
}

// describeAPIError turns a Google API error into an actionable line, distinguishing
// the permission case from the not-found case.
func describeAPIError(err error, onForbidden, onNotFound string) string {
	switch {
	case isStatus(err, 403):
		return "permission denied — " + onForbidden
	case isStatus(err, 404):
		return "not found — " + onNotFound
	default:
		return fmt.Sprintf("lookup failed: %v", err)
	}
}

// isStatus reports whether err is a googleapi.Error with the given HTTP code.
func isStatus(err error, code int) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == code
}

// migName extracts the zone/region and group name from a MIG self-link, which is a
// full compute URL. Returns ok=false for anything unparseable so a malformed link is
// skipped rather than turned into a lookup against a nonsense name.
func migName(selfLink string) (scope, name string, zonal, ok bool) {
	parts := strings.Split(selfLink, "/")
	for i := 0; i+1 < len(parts); i++ {
		switch parts[i] {
		case "zones":
			scope, zonal = parts[i+1], true
		case "regions":
			scope, zonal = parts[i+1], false
		case "instanceGroupManagers":
			name = parts[i+1]
		}
	}
	return scope, name, zonal, scope != "" && name != ""
}
```

Add `"errors"` and `"google.golang.org/api/googleapi"` to the imports.

- [ ] **Step 4: Add temporary no-op stubs so the package compiles**

Append to `resourcehealth.go` — Task 8 replaces both:

```go
// describeMIGs is implemented in Task 8.
func (c *Client) describeMIGs(context.Context, []string, providers.TimeWindow, func(string, ...any)) {}

// describeInstance is implemented in Task 8.
func (c *Client) describeInstance(context.Context, providers.Selector, func(string, ...any)) {}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/providers/cloud/gcp/ -run TestResourceHealth -v`
Expected: PASS, all three tests.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/cloud/gcp/resourcehealth.go internal/providers/cloud/gcp/resourcehealth_test.go
git commit -m "feat(gcp): report GKE cluster and node-pool health

Node pools hand over their instanceGroupUrls, so the MIG lookup is exact
where the AWS side must tag-match. Autopilot skips the node-layer
sub-queries and says why: running them returns empty results the model
would read as 'capacity is fine'. 403 and 404 get different messages —
a 404 is an identity-resolution bug, not a permissions one."
```

---

## Task 8: `ResourceHealth` — MIG errors and instance status

**Files:**
- Modify: `internal/providers/cloud/gcp/resourcehealth.go` (replace the two stubs)
- Test: `internal/providers/cloud/gcp/resourcehealth_test.go` (extend)

- [ ] **Step 1: Write the failing test**

Append to `internal/providers/cloud/gcp/resourcehealth_test.go`:

```go
// TestResourceHealthSurfacesMIGStockouts is the highest-value line this provider
// emits: ZONE_RESOURCE_POOL_EXHAUSTED and quota errors are why a pool silently stops
// scaling, and nothing inside the cluster can see them.
func TestResourceHealthSurfacesMIGStockouts(t *testing.T) {
	cluster := container.Cluster{
		Name: "prod", Status: "RUNNING", CurrentMasterVersion: "1.30.4-gke.100",
		NodePools: []*container.NodePool{{
			Name: "default", Status: "RUNNING", Version: "1.30.4-gke.100",
			InstanceGroupUrls: []string{
				"https://www.googleapis.com/compute/v1/projects/my-proj/zones/europe-west1-b/instanceGroupManagers/gke-prod-default-grp",
			},
		}},
	}
	errs := map[string]any{"items": []map[string]any{{
		"timestamp": "2026-08-24T10:00:00Z",
		"error": map[string]any{
			"code":    "ZONE_RESOURCE_POOL_EXHAUSTED",
			"message": "The zone 'europe-west1-b' does not have enough resources available.",
		},
	}}}
	c, done := routedClient(t, map[string]any{
		"/clusters/":           cluster,
		"listErrors":           errs,
		"listManagedInstances": map[string]any{"managedInstances": []any{}},
	})
	defer done()

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)
	for _, want := range []string{"gke-prod-default-grp", "ZONE_RESOURCE_POOL_EXHAUSTED", "does not have enough resources"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// TestMIGNameParsesZonalAndRegionalSelfLinks pins the parse that decides which compute
// API to call. Getting it wrong sends a zonal group to the regional endpoint, which
// 404s and looks like "no errors" — a silent false negative.
func TestMIGNameParsesZonalAndRegionalSelfLinks(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		scope string
		mig   string
		zonal bool
		ok    bool
	}{
		{"zonal", "https://www.googleapis.com/compute/v1/projects/p/zones/europe-west1-b/instanceGroupManagers/g", "europe-west1-b", "g", true, true},
		{"regional", "https://www.googleapis.com/compute/v1/projects/p/regions/europe-west1/instanceGroupManagers/g", "europe-west1", "g", false, true},
		{"unparseable", "https://example.com/nope", "", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scope, mig, zonal, ok := migName(c.in)
			if ok != c.ok || scope != c.scope || mig != c.mig || (ok && zonal != c.zonal) {
				t.Errorf("migName(%q) = (%q,%q,%v,%v), want (%q,%q,%v,%v)", c.in, scope, mig, zonal, ok, c.scope, c.mig, c.zonal, c.ok)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/cloud/gcp/ -run 'TestResourceHealthSurfacesMIG|TestMIGName' -v`
Expected: FAIL — the stub emits nothing, so no line mentions the group.

- [ ] **Step 3: Replace the two stubs**

In `internal/providers/cloud/gcp/resourcehealth.go`, delete both stubs and add:

```go
// maxMIGErrors bounds the errors reported per group, so one thrashing pool cannot
// crowd out every other signal in the lens.
const maxMIGErrors = 5

// describeMIGs reports errors from the managed instance groups backing the cluster's
// node pools. This is where GCP records the capacity failures that stop a pool
// scaling — ZONE_RESOURCE_POOL_EXHAUSTED, QUOTA_EXCEEDED, IP_SPACE_EXHAUSTED — and
// none of them are visible from inside the cluster: the nodes simply never arrive.
//
// It is the analog of the AWS side's DescribeScalingActivities, and window-scoped the
// same way, so a stale error from last week does not read as a live cause.
func (c *Client) describeMIGs(ctx context.Context, migs []string, w providers.TimeWindow, add func(string, ...any)) {
	for _, link := range dedup(migs) {
		scope, name, zonal, ok := migName(link)
		if !ok {
			continue // malformed self-link: skip rather than query a nonsense name
		}
		var items []*compute.InstanceManagedByIgmError
		var err error
		if zonal {
			var resp *compute.InstanceGroupManagersListErrorsResponse
			resp, err = c.compute.InstanceGroupManagers.ListErrors(c.project, scope, name).Context(ctx).Do()
			if resp != nil {
				items = resp.Items
			}
		} else {
			var resp *compute.RegionInstanceGroupManagersListErrorsResponse
			resp, err = c.compute.RegionInstanceGroupManagers.ListErrors(c.project, scope, name).Context(ctx).Do()
			if resp != nil {
				items = resp.Items
			}
		}
		if err != nil {
			add("mig %s: %s", name, describeAPIError(err,
				"missing roles/compute.viewer on the RunLore principal",
				fmt.Sprintf("no instance group %q in %q", name, scope)))
			continue
		}
		shown := 0
		for _, e := range items {
			if e == nil || e.Error == nil {
				continue
			}
			if !inWindow(e.Timestamp, w) {
				continue
			}
			if shown >= maxMIGErrors {
				add("  … more errors on %s (truncated at %d)", name, maxMIGErrors)
				break
			}
			add("MIG %s error: %s — %s (%s)", name, e.Error.Code, e.Error.Message, e.Timestamp)
			shown++
		}
		if shown == 0 {
			add("MIG %s: no errors in the window", name)
		}
	}
}

// describeInstance reports a single Compute Engine instance's state, when the selector
// names one. The AWS analog takes an i-… id; here it is the instance NAME, which is
// what a GKE node object's name already is.
func (c *Client) describeInstance(ctx context.Context, sel providers.Selector, add func(string, ...any)) {
	if sel.Name == "" {
		return
	}
	// The zone is required and is not derivable from the name, so an aggregated
	// lookup is the only shape that works from a bare name.
	agg, err := c.compute.Instances.AggregatedList(c.project).
		Filter(fmt.Sprintf("name=%s", sel.Name)).Context(ctx).Do()
	if err != nil {
		add("instance %s: %s", sel.Name, describeAPIError(err,
			"missing roles/compute.viewer on the RunLore principal",
			fmt.Sprintf("no instance named %q in project %s", sel.Name, c.project)))
		return
	}
	found := false
	for _, scoped := range agg.Items {
		for _, inst := range scoped.Instances {
			if inst == nil || inst.Name != sel.Name {
				continue
			}
			found = true
			add("instance %s: status=%s%s last-start=%s", inst.Name, inst.Status,
				optional(" (%s)", inst.StatusMessage), inst.LastStartTimestamp)
		}
	}
	if !found {
		add("instance %s: not found in project %s", sel.Name, c.project)
	}
}

// inWindow reports whether an RFC3339 timestamp falls inside w. A zero window accepts
// everything (the documented "recent, unscoped" behaviour), and an unparseable
// timestamp is kept rather than dropped — losing a real capacity error to a format
// quirk is the worse failure.
func inWindow(ts string, w providers.TimeWindow) bool {
	if w.Start.IsZero() && w.End.IsZero() {
		return true
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return true
	}
	if !w.Start.IsZero() && t.Before(w.Start) {
		return false
	}
	if !w.End.IsZero() && t.After(w.End) {
		return false
	}
	return true
}

// dedup removes repeated self-links: several node pools can reference the same group.
func dedup(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
```

Add `"time"` and `compute "google.golang.org/api/compute/v1"` to the imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/providers/cloud/gcp/ -run 'TestResourceHealth|TestMIGName' -v`
Expected: PASS, all five tests.

- [ ] **Step 5: Run the whole package with the race detector**

Run: `go test -race ./internal/providers/cloud/gcp/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/cloud/gcp/resourcehealth.go internal/providers/cloud/gcp/resourcehealth_test.go
git commit -m "feat(gcp): surface MIG capacity errors and instance status

ZONE_RESOURCE_POOL_EXHAUSTED, QUOTA_EXCEEDED and IP_SPACE_EXHAUSTED are why
a node pool silently stops scaling, and nothing inside the cluster can see
them — the nodes simply never arrive. Zonal and regional groups use
different endpoints; sending one to the other 404s and reads as 'no
errors', so the self-link parse is pinned by its own test."
```

---

## Task 9: Workload Identity preflight

**Files:**
- Modify: `internal/providers/cloud/gcp/gcp.go` (add `Preflight`)
- Test: `internal/providers/cloud/gcp/preflight_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `internal/providers/cloud/gcp/preflight_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"
)

// TestPreflightPrintsAPastableBindingOnDenial is the whole reason preflight exists.
// A missing Workload Identity binding otherwise surfaces as a bare 403 partway
// through an investigation. The message must carry the project NUMBER (not the id),
// the namespace and the KSA — the three parts operators most often get wrong.
func TestPreflightPrintsAPastableBindingOnDenial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Permission 'logging.logEntries.list' denied"}}`))
	}))
	defer srv.Close()

	c, err := New(context.Background(),
		Identity{Project: "my-proj", ProjectNumber: "123456789012"},
		option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Setenv("POD_NAMESPACE", "runlore")
	t.Setenv("POD_SERVICE_ACCOUNT", "runlore")

	perr := c.Preflight(context.Background())
	if perr == nil {
		t.Fatal("Preflight must fail when Cloud Logging denies the read")
	}
	msg := perr.Error()
	for _, want := range []string{
		"roles/logging.viewer",
		"principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/my-proj.svc.id.goog/subject/ns/runlore/sa/runlore",
		"add-iam-policy-binding",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("preflight error does not contain %q:\n%s", want, msg)
		}
	}
	// The project ID must not appear where the NUMBER belongs — the classic footgun
	// this message exists to prevent.
	if strings.Contains(msg, "projects/my-proj/locations/global") {
		t.Errorf("binding uses the project ID where the number belongs:\n%s", msg)
	}
}

// TestPreflightPassesWhenLoggingAnswers pins the happy path: a readable log means the
// binding works, and the cloud tools register.
func TestPreflightPassesWhenLoggingAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	defer srv.Close()

	c, err := New(context.Background(), Identity{Project: "my-proj", ProjectNumber: "123456789012"},
		option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/providers/cloud/gcp/ -run TestPreflight -v`
Expected: FAIL to compile — `c.Preflight undefined`.

- [ ] **Step 3: Add `Preflight` to `gcp.go`**

Append to `internal/providers/cloud/gcp/gcp.go`:

```go
// Preflight makes one cheap authoritative read to confirm the Workload Identity
// binding actually grants what the cloud lens needs, so a misconfiguration surfaces
// at startup with a fix rather than as a bare 403 partway through an investigation.
//
// It probes ONLY Cloud Logging — the changes lens, which is the core of the provider.
// The health APIs degrade per-sub-query at call time (see resourcehealth.go), so
// probing them here would trade three extra startup calls for a diagnosis those calls
// already produce in place.
func (c *Client) Preflight(ctx context.Context) error {
	_, err := c.entries.List(ctx, &logging.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + c.project},
		Filter:        fmt.Sprintf(`logName="projects/%s/logs/%s"`, c.project, activityLog),
		PageSize:      1,
	})
	if err == nil {
		return nil
	}
	if !isStatus(err, 403) && !isStatus(err, 401) {
		return fmt.Errorf("cloud logging read failed on project %s: %w", c.project, err)
	}
	return fmt.Errorf("Cloud Logging read denied on project %s.\n"+
		"RunLore authenticated as ServiceAccount %s/%s, which no GCP role is bound to.\n\n"+
		"  gcloud projects add-iam-policy-binding %s \\\n"+
		"    --role=roles/logging.viewer \\\n"+
		"    --member=%q\n\n"+
		"Repeat for roles/container.clusterViewer and roles/compute.viewer. "+
		"Note the project NUMBER in the member string — the project ID does not work there.",
		c.project, podNamespace(), podServiceAccount(),
		c.project, c.principal())
}

// principal renders the Workload Identity direct-binding member string for this
// pod's Kubernetes ServiceAccount.
//
// It uses the numeric project id, which is NOT interchangeable with the project id
// here: a member string built with the id is accepted by gcloud and silently never
// matches. That is the single most common way a direct binding is set up wrong, which
// is why this is generated rather than left to the docs.
func (c *Client) principal() string {
	return fmt.Sprintf(
		"principal://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s.svc.id.goog/subject/ns/%s/sa/%s",
		c.projectNum, c.project, podNamespace(), podServiceAccount())
}

// podNamespace and podServiceAccount read the downward-API values the Helm chart
// injects. They fall back to a placeholder rather than an empty string so the printed
// command is obviously a template when RunLore runs outside a pod, instead of
// rendering a subtly wrong binding that looks complete.
func podNamespace() string {
	if v := os.Getenv("POD_NAMESPACE"); v != "" {
		return v
	}
	return "<namespace>"
}

func podServiceAccount() string {
	if v := os.Getenv("POD_SERVICE_ACCOUNT"); v != "" {
		return v
	}
	return "<serviceaccount>"
}
```

Add `"os"` to the imports. `isStatus` comes from `resourcehealth.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/providers/cloud/gcp/ -run TestPreflight -v`
Expected: PASS, both tests.

- [ ] **Step 5: Run the full package and lint**

Run: `go test -race ./internal/providers/cloud/gcp/... && golangci-lint run ./internal/providers/cloud/gcp/...`
Expected: PASS, no lint findings.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/cloud/gcp/gcp.go internal/providers/cloud/gcp/preflight_test.go
git commit -m "feat(gcp): preflight the Workload Identity binding at startup

ADC resolves a direct principal binding with no code, so what was missing
was diagnosis: a wrong binding surfaced as a bare 403 mid-investigation.
Preflight makes one cheap logging read and, on denial, prints the exact
add-iam-policy-binding command with the project NUMBER substituted — using
the project ID there is accepted by gcloud and silently never matches."
```

---

## Task 10: Wire the provider into the app

**Files:**
- Modify: `internal/app/investigate.go:25-42` (imports), `:281-291` (cloud wiring)
- Test: `internal/app/investigate_test.go` (extend)

- [ ] **Step 1: Write the failing test**

Append to `internal/app/investigate_test.go`:

```go
// TestUnknownCloudProviderIsRefusedLoudly pins the failure mode a typo'd provider
// should have: today an unrecognised value silently disables the cloud tools with no
// diagnostic, so `provider: gpc` looks exactly like `provider: ""`. The network
// switch already warns on an unknown provider (investigate.go:256); cloud must too.
func TestUnknownCloudProviderIsRefusedLoudly(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := config.Config{Cloud: config.Cloud{Provider: "gpc"}}

	_, _, _ = BuildModelAndTools(context.Background(), cfg, nil, log)

	if !strings.Contains(buf.String(), "unknown cloud provider") {
		t.Errorf("expected a warning naming the unknown provider, got:\n%s", buf.String())
	}
}
```

**Note:** read `investigate_test.go` first — `BuildModelAndTools`'s exact signature and the existing logger/config helpers may differ. Reuse whatever the neighbouring tests already do rather than inventing a new harness.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestUnknownCloudProvider -v`
Expected: FAIL — no warning is logged.

- [ ] **Step 3: Replace the cloud wiring**

In `internal/app/investigate.go`, add the import beside `awscloud`:

```go
	gcpcloud "github.com/Smana/runlore/internal/providers/cloud/gcp"
```

Replace the block at lines 281-291 with:

```go
	// Cloud context: the control-plane "what changed" lens plus node/capacity health.
	// Opt-in and pluggable, same shape as the network switch above — the provider must
	// match where the cluster actually runs.
	var cloudProvider providers.CloudProvider
	switch cfg.Cloud.Provider {
	case config.CloudAWS:
		if cl, err := awscloud.New(ctx, cfg.Cloud.Region, cfg.Cloud.ClusterName); err != nil {
			log.Warn("aws cloud provider unavailable; cloud tools disabled", "err", err)
		} else {
			cloudProvider = cl
			tools = append(tools, investigate.CloudWhatChangedTool{Cloud: cl}, investigate.CloudResourceHealthTool{Cloud: cl})
			log.Info("cloud provider enabled", "provider", config.CloudAWS, "region", cfg.Cloud.Region)
		}
	case config.CloudGCP:
		// Identity resolves from config, then the GKE metadata server, then a node's
		// providerID — so `cloud: {provider: gcp}` alone is a complete configuration
		// on GKE. Tier 3 is wired only when a cluster reader exists; without one it
		// simply does not contribute.
		id := gcpcloud.ResolveIdentity(ctx, gcpcloud.Identity{
			Project:     cfg.Cloud.GCP.Project,
			Location:    cfg.Cloud.GCP.Location,
			ClusterName: cfg.Cloud.GCP.ClusterName,
		}, nil)
		if cfg.Cloud.Region != "" {
			log.Warn("config.cloud.region is AWS-only and is ignored on GCP; set config.cloud.gcp.location")
		}
		if cl, err := gcpcloud.New(ctx, id); err != nil {
			log.Warn("gcp cloud provider unavailable; cloud tools disabled", "err", err)
		} else if perr := cl.Preflight(ctx); perr != nil {
			// Non-fatal, matching every other provider here: a cloud lens that cannot
			// read must not stop an investigation that still has cluster, GitOps,
			// metrics and logs. The message carries the exact binding command.
			log.Warn("gcp cloud provider preflight failed; cloud tools disabled", "err", perr)
		} else {
			cloudProvider = cl
			tools = append(tools, investigate.CloudWhatChangedTool{Cloud: cl}, investigate.CloudResourceHealthTool{Cloud: cl})
			log.Info("cloud provider enabled", "provider", config.CloudGCP,
				"project", id.Project, "location", id.Location, "cluster", id.ClusterName,
				// identity_source is load-bearing, not decoration: it is how the live
				// validation run answers whether the metadata server exposes the
				// cluster attributes, and therefore whether tier 3 is needed at all.
				"identity_source", id.Source)
		}
	case "":
		// cloud context disabled (default)
	default:
		log.Warn("unknown cloud provider; cloud tools disabled", "provider", cfg.Cloud.Provider)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/app/ -run TestUnknownCloudProvider -v`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./... 2>&1 | tail -30`
Expected: PASS across all packages.

- [ ] **Step 6: Lint and build**

Run: `golangci-lint run && go build ./...`
Expected: no findings, clean build.

- [ ] **Step 7: Commit**

```bash
git add internal/app/investigate.go internal/app/investigate_test.go
git commit -m "feat(app): wire the GCP cloud provider

The cloud wiring becomes a switch, matching the network provider above,
and an unknown provider now warns instead of silently disabling the tools.
The startup line logs which tier resolved the identity — that field is what
the live-validation run reads to decide whether tier 3 is needed."
```

---

## Task 11: Helm — `POD_SERVICE_ACCOUNT` and values documentation

**Files:**
- Modify: `deploy/helm/runlore/templates/_helpers.tpl` (the `env:` block, around line 118-136)
- Modify: `deploy/helm/runlore/values.yaml` (around line 371, beside the network example)

- [ ] **Step 1: Add the downward-API variable**

In `deploy/helm/runlore/templates/_helpers.tpl`, immediately after the `POD_NAMESPACE` entry:

```yaml
        # The GCP cloud provider renders the Workload Identity binding command from
        # its own principal — namespace + ServiceAccount — when a preflight read is
        # denied. spec.serviceAccountName is a downward-API field, so this needs no
        # RBAC; without it the printed command carries a <serviceaccount> placeholder
        # the operator has to fill in by hand, which is where the wrong name gets used.
        - name: POD_SERVICE_ACCOUNT
          valueFrom:
            fieldRef:
              fieldPath: spec.serviceAccountName
```

- [ ] **Step 2: Verify the chart still renders**

Run: `helm template runlore deploy/helm/runlore | grep -A3 POD_SERVICE_ACCOUNT`
Expected: the env entry appears with `fieldPath: spec.serviceAccountName`.

If `helm` is unavailable, run `helm lint deploy/helm/runlore` or skip with a note — CI renders the chart.

- [ ] **Step 3: Document the cloud block in values.yaml**

In `deploy/helm/runlore/values.yaml`, near the commented `network:` example around line 371, add:

```yaml
  # Cloud context — the control-plane "what changed" lens plus node/capacity health.
  # Opt-in; no provider means no cloud tools.
  #
  # cloud:
  #   provider: aws                        # CloudTrail + EC2/ASG/EKS; auth is Pod Identity / IRSA
  #   region: eu-west-3
  #   cluster_name: my-eks-cluster
  #
  # cloud:
  #   provider: gcp                        # Cloud Audit Logs + GKE/MIG/Compute
  #   # Nothing else is needed on GKE: project, location and cluster name are read
  #   # from the metadata server. Set cloud.gcp.{project,location,cluster_name} only
  #   # to override that.
  #
  # GCP auth is a Workload Identity DIRECT PRINCIPAL BINDING. Do NOT annotate the
  # ServiceAccount with iam.gke.io/gcp-service-account — there is no Google service
  # account in this setup, and a leftover annotation silently redirects RunLore to a
  # GSA holding none of the roles below, which presents as a permissions failure whose
  # printed fix does not resolve it. Bind the KSA directly:
  #
  #   PN=$(gcloud projects describe MY_PROJECT --format='value(projectNumber)')
  #   for ROLE in roles/logging.viewer roles/container.clusterViewer roles/compute.viewer; do
  #     gcloud projects add-iam-policy-binding MY_PROJECT --role="$ROLE" \
  #       --member="principal://iam.googleapis.com/projects/$PN/locations/global/workloadIdentityPools/MY_PROJECT.svc.id.goog/subject/ns/runlore/sa/runlore"
  #   done
```

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/runlore/templates/_helpers.tpl deploy/helm/runlore/values.yaml
git commit -m "feat(helm): expose POD_SERVICE_ACCOUNT and document the GCP cloud block

The GCP provider renders its Workload Identity binding command from its own
principal, which needs the KSA name. spec.serviceAccountName is a
downward-API field, so no RBAC is involved.

The values comment says explicitly NOT to annotate the ServiceAccount:
direct principal binding uses no GSA, and a leftover annotation redirects
to one holding none of the roles."
```

---

## Task 12: Documentation

**Files:**
- Create: `website/content/docs/integrations/data-sources/gcp-cloud.md`
- Modify: `website/content/docs/concepts/data-sources.md`, `website/content/docs/concepts/design.md:310`, `website/content/docs/integrations/_index.md`, `README.md`

- [ ] **Step 1: Read the AWS page as the template**

Run: `cat website/content/docs/integrations/data-sources/aws-cloud.md`

Match its front matter shape exactly — `integration: {kind: cloud, id: ...}` — and its section order. The site's integration index is generated from that front matter, so a mismatch drops the page from the listing.

- [ ] **Step 2: Write `gcp-cloud.md`**

Create the page covering, in this order:

1. **What it gives you** — `cloud_what_changed` from Cloud Audit Logs (activity + system_event), `cloud_resource_health` from GKE/MIG/Compute.
2. **Config** — `cloud: {provider: gcp}` and the statement that nothing else is needed on GKE.
3. **IAM** — the three roles and the `principal://` loop from Task 11 Step 3, verbatim, plus the explicit "do not annotate the ServiceAccount" warning and the project-NUMBER note.
4. **Verify it** — `kubectl -n runlore logs deploy/runlore | grep 'cloud provider enabled'`, and that the line reports `identity_source`.
5. **Autopilot** — node-layer health is unavailable and the tool says so.
6. **Limitations** — single project; Data Access logs are not read; Cloud Monitoring is not a cloud-provider concern (point the Prometheus provider at Google Managed Prometheus instead).

- [ ] **Step 3: Update the four cross-reference points**

- `website/content/docs/concepts/data-sources.md` — add GCP to the Cloud row and a `### gcp-cloud` subsection beside the existing `### gcp-firewall-logs`.
- `website/content/docs/concepts/design.md:310` — the Cloud row currently reads `**AWS** (CloudTrail what-changed + EC2/ASG/EKS health)` with `GCP, Azure via native SDKs` as future. Move GCP into the shipped column, leaving Azure future.
- `website/content/docs/integrations/_index.md` — add a `hextra/feature-card` beside the GCP Firewall Logs one.
- `README.md` — the capability table around line 195: add a Cloud row, or extend it if one exists.

- [ ] **Step 4: Verify the docs build**

Run: `cd website && hugo --minify 2>&1 | tail -20`
Expected: build succeeds, no broken `relref`. A bad `relref` is a hard error, so a typo'd cross-reference fails here rather than shipping.

- [ ] **Step 5: Commit and push**

```bash
git add website/ README.md
git commit -m "docs: document the GCP cloud provider

Covers the direct principal binding, the three roles, and the deliberate
non-annotation of the ServiceAccount. Marks the provider as not yet
validated on a live cluster, per the project-status posture."
git push -u origin feat/gcp-cloud-provider
```

**Do not open a pull request** — the maintainer does that.

---

## Post-merge: live validation on a real GKE cluster

Not a code task. Until this runs, the provider is documented as functional but **not** verified on a live cluster, matching the honesty posture of `README.md`'s project-status table.

- [ ] **1.** Create a Standard cluster with Workload Identity: `gcloud container clusters create runlore-test --workload-pool=PROJECT.svc.id.goog --num-nodes=1 --zone=europe-west1-b`
- [ ] **2.** Apply the three `add-iam-policy-binding` commands from the values comment.
- [ ] **3.** Deploy with `cloud: {provider: gcp}` and **nothing else**. Confirm the startup line reports project, location, cluster **and `identity_source`**.
      **This is the decisive step.** `identity_source=metadata-server` means tier 3 is dead weight → delete `nodeLookup`, `NodeIdentity`, `parseProviderID` and the `sourceNode` branch. `identity_source=node-provider-id` means the metadata server does not expose the cluster attributes and tier 3 is load-bearing → wire it to the real cluster reader, which Task 10 currently passes as `nil`.
- [ ] **4.** Confirm `cloud_what_changed` returns the cluster-creation audit events.
- [ ] **5.** Force a stockout: request a rare machine type in a node pool. Confirm `cloud_resource_health` surfaces `ZONE_RESOURCE_POOL_EXHAUSTED`.
- [ ] **6.** **Negative test:** remove the `roles/logging.viewer` binding, restart, and confirm the printed command works verbatim when pasted back.
- [ ] **7.** Capture the real API responses and replace the hand-written fixtures in `auditlog_test.go` and `resourcehealth_test.go`.
- [ ] **8.** Update `README.md`'s project-status table to reflect live validation.

---

## Self-review notes

**Spec coverage.** Every spec section maps to a task: model changes → 1; `CloudDescriber` → 1, 2; config → 3; package skeleton → 4; identity → 5; `CloudChanges` → 6; `ResourceHealth` → 7, 8; Workload Identity + error tiers → 9; wiring → 10; Helm → 11; docs → 12; runbook → post-merge.

**One deliberate refinement of the spec.** `CloudVocabulary` gained `WidenedBanner`, `ChangeExamples` and `LagNote`, and `ChangeScopeArg` was renamed `ScopeGuidance`. The spec's five-field sketch could not render the widening banner — which is the one string that most needs to differ per cloud, since it is where the AWS text makes a claim about match semantics that is false on GCP. **Update the spec's struct to match before merging** so the two documents do not disagree.

**Two things this plan pins that the spec only asserted.** That AWS tool text does not change is now `TestAWSCloudVocabularyReproducesTheShippedToolText` rather than a promise. And `identity_source` is logged specifically so step 3 of the runbook has an observable answer, rather than the tier-3 question being settled by opinion.

**Known ordering constraint.** `gcp.go` (Task 4) references types defined in Task 5 and methods defined in Tasks 6–8, so the package does not compile until Task 6. Tasks 4, 5 and 7 each say where a temporary stub is needed. Do not reorder these three.

**Google API symbols verified against `google.golang.org/api@v0.293.0`** — the version already in `go.mod`. Confirmed present with these exact names and shapes, so the code in this plan is not written from recall:

| Symbol | Confirmed |
|---|---|
| `container.NodePool` | `.Name .Status .StatusMessage .Version .InitialNodeCount .InstanceGroupUrls .Autoscaling .Conditions` |
| `container.NodePoolAutoscaling` | `.Enabled .MinNodeCount .MaxNodeCount` (both counts `int64`) |
| `container.Autopilot` | `.Enabled` |
| `container.StatusCondition` | `.CanonicalCode .Message` |
| `container.Cluster` | `.CurrentMasterVersion` |
| `compute.InstanceGroupManagersService.ListErrors` | `(project, zone, instanceGroupManager string)` |
| `compute.RegionInstanceGroupManagersService.ListErrors` | `(project, region, instanceGroupManager string)` |
| `compute.InstanceManagedByIgmError` | `.Error *InstanceManagedByIgmErrorManagedInstanceError` · `.Timestamp string` |
| `compute.InstanceManagedByIgmErrorManagedInstanceError` | `.Code .Message` (both `string`) |
| `compute.InstancesService.AggregatedList` | `(project string)` |
| `logging.LogEntry` | `.InsertId .ProtoPayload googleapi.RawMessage .Resource *MonitoredResource .Timestamp` |
| `logging.MonitoredResource` | `.Labels map[string]string` · `.Type string` |
| `metadata.GetWithContext` | `(ctx, suffix) (string, error)` |

Note that `Cluster` *also* carries deprecated `InitialNodeCount` and `InstanceGroupUrls` fields. Task 7 reads them from `NodePool`, which is the correct one; reading the `Cluster` copies would return empty on any multi-pool cluster.

**One API-design fix applied during review.** `NodeIdentity` was originally unexported while `ResolveIdentity` — which takes a lookup function returning it — was exported. That combination compiles but makes tier 3 unwireable from `internal/app`, which is exactly what runbook step 3 asks for if the metadata server turns out not to expose the cluster attributes. Exported.
