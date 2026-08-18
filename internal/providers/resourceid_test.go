// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestResourceIDAgreement pins the identity rule that closes the split observed on
// 2026-08-17/18: one RDS instance fired twice and the two firings were treated as
// unrelated resources because the alert spelled it as the bare DBInstanceIdentifier
// once and as the full ARN the next time.
//
// Agreement is deliberately NOT plain equality of a canonical bare name: an ARN
// carries an account and a region, and two accounts hosting the same instance name
// are two different databases. A qualifier is compared only when BOTH sides carry
// it — the short form is unqualified and therefore compatible with either account,
// which is the whole point.
func TestResourceIDAgreement(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"rds arn vs its DBInstanceIdentifier dimension",
			"arn:aws:rds:us-east-1:142655614335:db:datagrok-aqemia-shared", "datagrok-aqemia-shared", true},
		{"slash-style arn vs its identifier",
			"arn:aws:ec2:us-east-1:142655614335:instance/i-0abc123def4567890", "i-0abc123def4567890", true},
		{"slash-style arn keeps the full multi-segment resource path (the CloudWatch dimension)",
			"arn:aws:elasticloadbalancing:us-east-1:1:loadbalancer/app/my-lb/50dc6c4951", "app/my-lb/50dc6c4951", true},
		{"same instance name in two different accounts is two different databases",
			"arn:aws:rds:us-east-1:111111111111:db:datagrok", "arn:aws:rds:us-east-1:222222222222:db:datagrok", false},
		{"same instance name in two different regions is two different databases",
			"arn:aws:rds:us-east-1:111111111111:db:datagrok", "arn:aws:rds:eu-west-1:111111111111:db:datagrok", false},
		{"an arn agrees with itself",
			"arn:aws:rds:us-east-1:111111111111:db:datagrok", "arn:aws:rds:us-east-1:111111111111:db:datagrok", true},
		{"a kubernetes workload name is untouched",
			"harbor-registry", "harbor-registry", true},
		{"two distinct kubernetes names stay distinct",
			"harbor-registry", "harbor-core", false},
		{"a name that merely contains the substring arn is not an arn",
			"arnica-exporter", "exporter", false},
		{"a truncated arn-looking value is not an arn either",
			"arn:aws:rds", "rds", false},
		{"empty agrees with empty",
			"", "", true},
		{"empty never agrees with a named resource",
			"", "datagrok-aqemia-shared", false},
		{"pod-template-hash forgiveness still holds",
			"harbor-registry-59598dbd57-ltkzw", "harbor-registry", true},
		{"the hash strip runs on BOTH sides of an arn so the two spellings cannot diverge",
			"arn:aws:rds:us-east-1:1:db:web-7d9c8b6f5-abcde", "web", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := providers.ParseResourceID(c.a), providers.ParseResourceID(c.b)
			if got := a.Agrees(b); got != c.want {
				t.Errorf("ParseResourceID(%q).Agrees(ParseResourceID(%q)) = %v, want %v", c.a, c.b, got, c.want)
			}
			// Agreement is symmetric: the gate compares an alert against a catalog
			// entry and neither side is privileged.
			if got := b.Agrees(a); got != c.want {
				t.Errorf("agreement is not symmetric: %q vs %q = %v, want %v", c.b, c.a, got, c.want)
			}
		})
	}
}

// TestARNResourceName pins the ingestion-side canonicalisation: an ARN reduces to
// the resource identifier its CloudWatch dimension carries, and every other value is
// returned byte-for-byte. It must NOT strip a pod-template hash — ingestion stores
// the workload name a Kubernetes alert fired on, and that name is normalized only at
// comparison time.
func TestARNResourceName(t *testing.T) {
	cases := map[string]string{
		"arn:aws:rds:us-east-1:142655614335:db:datagrok-aqemia-shared":               "datagrok-aqemia-shared",
		"arn:aws:ec2:us-east-1:142655614335:instance/i-0abc123def4567890":            "i-0abc123def4567890",
		"arn:aws:elasticloadbalancing:us-east-1:1:loadbalancer/app/my-lb/50dc6c4951": "app/my-lb/50dc6c4951",
		"arn:aws:s3:::my-bucket":                  "my-bucket", // no region, no account
		"arn:aws-cn:rds:cn-north-1:1:db:datagrok": "datagrok",  // non-default partition
		"harbor-registry-59598dbd57-ltkzw":        "harbor-registry-59598dbd57-ltkzw",
		"observability/datagrok-aqemia-shared":    "observability/datagrok-aqemia-shared",
		"arnica-exporter":                         "arnica-exporter",
		"arn:aws:rds":                             "arn:aws:rds",
		"arn:::::":                                "arn:::::",
		"":                                        "",
	}
	for in, want := range cases {
		if got := providers.ARNResourceName(in); got != want {
			t.Errorf("ARNResourceName(%q) = %q, want %q", in, got, want)
		}
		if got := providers.ARNResourceName(want); got != want {
			t.Errorf("ARNResourceName is not idempotent for %q: %q", want, got)
		}
	}
}

// TestNormalizeResourceNameIsTheCanonicalKeyForm pins the single-string form used
// where identity has to BE a map key (curator.IncidentKey → the recurrence ledger's
// TriggerKey, and DupFingerprint): the two spellings of one resource must collapse
// to the same string, because a key can only ever be compared by equality.
func TestNormalizeResourceNameIsTheCanonicalKeyForm(t *testing.T) {
	const short = "datagrok-aqemia-shared"
	arn := "arn:aws:rds:us-east-1:142655614335:db:" + short
	if got, want := providers.NormalizeResourceName(arn), short; got != want {
		t.Fatalf("NormalizeResourceName(%q) = %q, want %q", arn, got, want)
	}
	if got := providers.NormalizeResourceName(short); got != short {
		t.Fatalf("NormalizeResourceName(%q) = %q, want it unchanged", short, got)
	}
	// The pod-hash forgiveness the key form already had must survive.
	if got, want := providers.NormalizeResourceName("harbor-registry-59598dbd57-ltkzw"), "harbor-registry"; got != want {
		t.Fatalf("NormalizeResourceName lost the pod-hash strip: got %q, want %q", got, want)
	}
}
