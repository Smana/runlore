// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"cmp"
	"strings"
)

// arnFields is the field count of a well-formed AWS ARN:
// arn:partition:service:region:account-id:resource. The resource field is last and
// may itself contain both ":" and "/", so it is never split further here.
const arnFields = 6

// ResourceID is the identity two resource names are compared by. It exists because
// one cloud resource reaches RunLore under two different spellings: a CloudWatch
// alert identifies an RDS instance by its DBInstanceIdentifier dimension
// ("datagrok-aqemia-shared") on one firing and by its full ARN
// ("arn:aws:rds:us-east-1:142655614335:db:datagrok-aqemia-shared") on the next.
// Compared as strings those are two unrelated resources, so one recurring fault
// looked like two first sightings and a catalog entry filed under either spelling
// was invisible to the other.
//
// Name is the bare identifier both spellings share. Region and Account are the
// ARN's qualifiers, empty for a short name and for the ARN shapes that carry
// neither (an S3 bucket ARN is "arn:aws:s3:::my-bucket").
type ResourceID struct {
	Name    string // the bare resource identifier, pod-hash normalized
	Region  string // AWS region; "" when unqualified (a short name, or a global resource)
	Account string // AWS account id; "" when unqualified
}

// ParseResourceID resolves a resource name to the identity it should be matched by:
// an AWS ARN yields its resource identifier plus the region and account that
// qualify it, and any other value yields itself as the Name with no qualifiers.
//
// The Name is passed through NormalizeWorkloadName on BOTH paths, and that
// symmetry is load-bearing rather than incidental: if the pod-hash strip ran on
// short names only, a resource whose identifier happens to end in something shaped
// like a pod hash would normalize to two different Names depending on which
// spelling the alert used — reintroducing the very split this type exists to close.
func ParseResourceID(name string) ResourceID {
	id, region, account, ok := parseARN(name)
	if !ok {
		return ResourceID{Name: NormalizeWorkloadName(name)}
	}
	return ResourceID{Name: NormalizeWorkloadName(id), Region: region, Account: account}
}

// Agrees reports whether r and o name the same cloud resource.
//
// It is deliberately NOT equality of a canonical bare name. Two accounts (or two
// regions) can host an instance under the same name and those are genuinely
// different databases, so a qualifier is compared whenever BOTH sides carry it. A
// missing qualifier is treated as "unknown, therefore compatible": that is what
// lets the unqualified short form agree with the ARN it was extracted from, which
// is the whole point of the type.
//
// The relation is symmetric and reflexive but NOT transitive — a short name agrees
// with two ARNs that do not agree with each other. That is why callers needing a
// map key cannot use it: a key is compared by equality, so it reads the account off
// Workload.Account instead (see curator.IncidentKey).
//
// REACHABILITY. The qualifier check used to be vacuous for alert traffic, and is no
// longer. Ingestion rewrites an ARN to its bare identifier before a Workload is
// built, so a name derived from an ALERT carries no qualifiers of its own; what
// makes the check bite is that ingestion now also LIFTS the account and region onto
// the workload (ResolveWorkloadIdentity), and Workload.ResourceID feeds them back in
// here. An alert from one AWS account therefore no longer agrees with a catalog
// entry filed under a full ARN in another.
//
// What remains, by design: an alert that carries NO account — every Kubernetes
// workload, and any stack whose alert rules omit the account label — is unqualified
// and still agrees with an ARN in any account. That is required, not tolerated: it
// is how the live ARN-spelled catalog entries stay reachable at all, and treating
// unknown as different would silently un-index them.
func (r ResourceID) Agrees(o ResourceID) bool {
	return r.Name == o.Name && qualifierAgrees(r.Region, o.Region) && qualifierAgrees(r.Account, o.Account)
}

// qualifierAgrees compares one ARN qualifier: equal values agree, and an absent
// value on either side is unknown rather than different.
func qualifierAgrees(a, b string) bool { return a == "" || b == "" || a == b }

// cloudAccountLabels are the alert-label spellings an AWS account id arrives under,
// in precedence order: `account_id` is what yace (the CloudWatch exporter this was
// written against) stamps on every series it emits, and `aws_account_id` is the
// cloudwatch_exporter / relabel spelling. A bare `account` is deliberately NOT read:
// it is a plausible APPLICATION label (a customer account, a billing account) and
// reading it would qualify a resource by something that is not a cloud scope.
var cloudAccountLabels = []string{"account_id", "aws_account_id"}

// cloudRegionLabels are the alert-label spellings an AWS region arrives under.
var cloudRegionLabels = []string{"region", "aws_region"}

// ResolveWorkloadIdentity is the ingestion-side chokepoint that turns a workload as
// an alert SPELLED it into the identity RunLore keys and matches by: the name is
// reduced to its bare resource identifier (an ARN loses its scaffolding), and the
// cloud scope that identifier lives in is recorded on Account/Region.
//
// WHY THE SCOPE HAS TO BE LIFTED OFF THE NAME. Reducing an ARN to its identifier is
// what makes one resource have ONE spelling downstream, and that is load-bearing: a
// CloudWatch-derived rule templates whichever dimension it has to hand, so one RDS
// instance arrives as "datagrok" on one firing and as
// "arn:aws:rds:us-east-1:111111111111:db:datagrok" on the next. But the reduction
// also DELETED the account, and the surrounding key fields do not put it back — a
// CloudWatch rule authors `namespace` as the exporter's, one Prometheus stamps one
// `cluster` label across every account it scrapes, and Kind is empty for a cloud
// resource. So "datagrok" in two accounts became one identity. Recording the account
// as its own field is what separates them WITHOUT re-splitting the two spellings,
// because the field is filled on both paths.
//
// RECONCILIATION. The account and region are taken from the ARN when the name is one
// and from the alert labels otherwise, so a value present on EITHER side survives:
// an ARN names its account even where the rule drops the label, and a short-spelled
// firing has only the label. Where both are present and disagree, the ARN wins — it
// names the resource itself, while a label describes the series that observed it. A
// value already set on w is never overwritten.
//
// KUBERNETES IS EXCLUDED, and that exclusion is the point of the Kind check rather
// than an optimisation: a Kubernetes object's identity is namespace/name and nothing
// else, so a cluster whose Prometheus stamps `account_id` as an external label must
// not acquire a qualifier and must keep byte-identical keys. An ARN-spelled name
// overrides the check — a name that IS an ARN is a cloud resource whatever label
// carried it.
//
// KNOWN RESIDUAL: a workload that arrives on the `workload` label with no
// `workload_type` has no Kind, so a Kubernetes object spelled that way on a stack
// that stamps account labels globally WOULD be qualified. It is the one shape this
// cannot tell apart — there is no signal left — and the cost is one restarted
// recurrence count, not a wrong answer.
func ResolveWorkloadIdentity(w Workload, labels map[string]string) Workload {
	if w.Name == "" {
		return w // nothing to qualify: there is no resource
	}
	bare, region, account, isARN := parseARN(w.Name)
	if isARN {
		w.Name = bare
	} else if w.Kind != "" {
		return w // a Kubernetes object: namespace/name is the whole identity
	}
	w.Region = cmp.Or(w.Region, region, labelValue(labels, cloudRegionLabels))
	w.Account = cmp.Or(w.Account, account, labelValue(labels, cloudAccountLabels))
	return w
}

// labelValue returns the first non-empty label among keys.
func labelValue(labels map[string]string, keys []string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(labels[k]); v != "" {
			return v
		}
	}
	return ""
}

// ResourceID is the identity this workload should be COMPARED by: its name resolved
// as a cloud identifier, qualified by the scope the name carries or, failing that,
// by the scope stamped on the workload at ingestion.
//
// It unions the two sources on purpose, and that is why it is not what the identity
// KEYS use. A key must be spelling-invariant — an ARN-spelled and a short-spelled
// firing of one resource must produce the same bytes — and only the Account field
// is, because ingestion fills it on both paths while an ARN inside a name is present
// on one. Comparison has no such constraint: ResourceID.Agrees treats an absent
// qualifier as compatible rather than different, so reading MORE evidence can only
// split resources that really are distinct, never fuse two that are not. That
// asymmetry is what lets a bare alert be told apart from a catalog entry still
// filed under a full ARN in another account.
func (w Workload) ResourceID() ResourceID {
	id := ParseResourceID(w.Name)
	return ResourceID{
		Name:    id.Name,
		Region:  cmp.Or(id.Region, w.Region),
		Account: cmp.Or(id.Account, w.Account),
	}
}

// ARNResourceName reduces an AWS ARN to the resource identifier that the matching
// CloudWatch dimension carries, and returns any other value byte-for-byte. It is the
// name half of the ingestion-side canonicalisation, so only one spelling of a
// resource is ever stored in a notification, a curated entry's `resource:`
// frontmatter, or a ledger key.
//
// Ingestion itself goes through ResolveWorkloadIdentity, which applies this AND
// keeps the cloud scope the ARN named; call this directly only where there is no
// workload to qualify.
//
// It deliberately does NOT normalize the pod-template hash. Ingestion records the
// workload an alert actually fired on — a pod-scoped alert must keep its full pod
// name, which downstream normalizes only when comparing — so this narrows the
// contract to the one rewrite that is pure gain: dropping ARN scaffolding that
// names nothing the short form does not already name.
//
// Nothing is lost by rewriting it: the full ARN still travels verbatim on the
// request's Labels and reaches the seed prompt, so the model can still see the
// account and region it names.
func ARNResourceName(name string) string {
	if id, _, _, ok := parseARN(name); ok {
		return id
	}
	return name
}

// NormalizeResourceName is the canonical single-string form of a resource NAME: the
// ARN scaffolding removed, then the pod-hash suffix stripped. It is the name half of
// the identity keys — curator.IncidentKey (hence the recurrence ledger's TriggerKey)
// and DupFingerprint — because a map key can only be compared by equality, and a
// predicate like ResourceID.Agrees cannot be one.
//
// It deliberately yields the bare identifier and NOTHING ELSE, dropping the account
// and region an ARN carried. That is forced: canon(ARN in account A) == canon(short)
// == canon(ARN in account B) is exactly what fuses the two spellings of one resource,
// and it equally forces the two accounts equal. No single canonical string can do
// both.
//
// The account is therefore not recovered from the name — it travels BESIDE it, on
// Workload.Account, stamped at ingestion from the alert labels and from an ARN alike
// (ResolveWorkloadIdentity) so that it is present under either spelling. IncidentKey
// appends it as a segment of its own, which is what splits two accounts without
// re-splitting the two spellings. Read Workload's doc for why the field, and not the
// name, is the only thing a key can honestly qualify on.
func NormalizeResourceName(name string) string {
	return ParseResourceID(name).Name
}

// parseARN splits an AWS ARN into its resource identifier, region and account,
// reporting false for anything that is not one (leaving the value untouched).
//
// The identifier is the resource field with its leading resource-TYPE segment
// removed, the type being everything up to the first ":" or "/" — the two
// separators AWS uses interchangeably ("…:db:NAME" for RDS, "…:instance/NAME" for
// EC2). What remains is exactly what the CloudWatch dimension of that resource
// carries, including multi-segment paths: an ALB's LoadBalancer dimension really is
// "app/my-lb/50dc6c4951", so the segments after the type are kept rather than
// reduced to the last one.
//
// Validation is strict on purpose. A value only parses as an ARN with all six
// fields present and a non-empty partition, service and resource, so ordinary names
// that merely start with or contain "arn" ("arnica-exporter") and truncated
// fragments ("arn:aws:rds") fall through unchanged.
//
// aws-sdk-go-v2's aws/arn.Parse splits identically and is already an indirect
// dependency, and is deliberately NOT used: it would make the AWS SDK a direct
// import of internal/providers, which is cloud-SDK-free today (only
// internal/providers/cloud/aws touches it). It also supplies neither the strictness
// above nor the resource-TYPE strip below, so the part that would be reused is the
// one line this shares with it.
func parseARN(s string) (id, region, account string, ok bool) {
	if !strings.HasPrefix(s, "arn:") {
		return "", "", "", false
	}
	f := strings.SplitN(s, ":", arnFields)
	if len(f) != arnFields {
		return "", "", "", false
	}
	partition, service, resource := f[1], f[2], f[5]
	if partition == "" || service == "" || resource == "" {
		return "", "", "", false
	}
	region, account = f[3], f[4]
	if !typelessResourceServices[service] {
		if i := strings.IndexAny(resource, ":/"); i >= 0 {
			resource = resource[i+1:]
		}
	}
	if resource == "" {
		return "", "", "", false
	}
	return resource, region, account, true
}

// typelessResourceServices are the services whose ARN resource field is the resource
// ITSELF, with no leading resource-type segment — so a "/" inside it separates two
// parts of ONE identifier instead of a type from a name.
//
// S3 is the case that matters and the reason this exists. "arn:aws:s3:::my-bucket"
// has no separator and needs no exception, but "arn:aws:s3:::prod-logs/exports"
// names the "exports" prefix IN "prod-logs": stripping its first segment would
// reduce prod-logs/exports and staging-logs/exports to the same "exports", and an S3
// ARN carries NEITHER region nor account, so ResourceID.Agrees has no qualifier left
// to tell the two buckets apart and would report two different buckets as one
// resource. That is a false agreement — the failure this whole type exists to
// prevent — as opposed to a missed one, which merely costs an investigation.
//
// It is a per-service exception rather than a general rule because the ARN grammar
// genuinely cannot distinguish "type/name" from "name/subpath": all three forms
// (resource, resource-type/resource, resource-type:resource) are legal and the
// service is the only thing that says which is in use. Typeless-but-separator-free
// services (SNS topics, SQS queues) need no entry — there is nothing to strip.
var typelessResourceServices = map[string]bool{"s3": true}
