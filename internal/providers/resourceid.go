// SPDX-License-Identifier: Apache-2.0

package providers

import "strings"

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
// map key use NormalizeResourceName instead (see its doc for the tradeoff), and why
// this is a predicate rather than a comparable canonical value.
func (r ResourceID) Agrees(o ResourceID) bool {
	return r.Name == o.Name && qualifierAgrees(r.Region, o.Region) && qualifierAgrees(r.Account, o.Account)
}

// qualifierAgrees compares one ARN qualifier: equal values agree, and an absent
// value on either side is unknown rather than different.
func qualifierAgrees(a, b string) bool { return a == "" || b == "" || a == b }

// ARNResourceName reduces an AWS ARN to the resource identifier that the matching
// CloudWatch dimension carries, and returns any other value byte-for-byte. It is
// the ingestion-side canonicalisation: applied where a Workload name is derived
// from alert labels, so only one spelling of a resource is ever stored in a
// notification, a curated entry's `resource:` frontmatter, or a ledger key.
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

// NormalizeResourceName is the canonical single-string form of a resource name: the
// ARN scaffolding removed, then the pod-hash suffix stripped. It is what the
// identity keys use — curator.IncidentKey (hence the recurrence ledger's TriggerKey)
// and DupFingerprint — because a map key can only be compared by equality, and a
// predicate like ResourceID.Agrees cannot be one.
//
// KNOWN CONSEQUENCE, and the reason Agrees exists beside it: collapsing to the bare
// identifier drops the account and region, so one instance name in two AWS accounts
// produces ONE key. That is accepted here and nowhere else. Every caller folds this
// name into a key that is already qualified by the surrounding facts — IncidentKey
// adds alertname, namespace, kind and cluster; DupFingerprint adds the namespace and
// the cause — so two accounts collide only when the same alert fires on the same
// resource name, in the same namespace, on the same cluster. The alternative is
// strictly worse: without the collapse the SAME database recurring under two
// spellings gets two keys, which is the bug being fixed and is guaranteed to happen
// on every ARN-shaped alert rather than in a contrived naming coincidence. The
// structural matching path, where a wrong collapse would apply one database's
// runbook to another, uses Agrees and keeps the two accounts apart.
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
	if i := strings.IndexAny(resource, ":/"); i >= 0 {
		resource = resource[i+1:]
	}
	if resource == "" {
		return "", "", "", false
	}
	return resource, region, account, true
}
