package alertmanager

import (
	"os"
	"testing"
)

func TestDecodeFiringProducesRequest(t *testing.T) {
	body, err := os.ReadFile("testdata/firing.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&Source{}).Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(res.Requests))
	}
	r := res.Requests[0]
	if r.Title != "HighMem" || r.Severity != "critical" || r.Workload.Namespace != "prod-web" || r.Workload.Name != "web" {
		t.Fatalf("bad request: %+v", r)
	}
	if r.Workload.Kind != "Deployment" {
		t.Fatalf("want Workload.Kind=Deployment, got %q", r.Workload.Kind)
	}
	if r.Environment != "prod" {
		t.Fatalf("want Environment=prod, got %q", r.Environment)
	}
	if r.GroupKey != "g1" {
		t.Fatalf("want GroupKey=g1, got %q", r.GroupKey)
	}
	if r.Fingerprint != "abc" || len(r.Fingerprints) != 1 || r.Fingerprints[0] != "abc" {
		t.Fatalf("want fingerprint abc (and Fingerprints=[abc]), got %q %v", r.Fingerprint, r.Fingerprints)
	}
	if len(res.Resolved) != 0 {
		t.Fatalf("firing must not resolve")
	}
}

// TestDecodeEnvFallback covers the "env" fallback when "environment" is absent,
// and that a missing fingerprint yields a nil Fingerprints slice.
func TestDecodeEnvFallback(t *testing.T) {
	body := []byte(`{"alerts":[{"status":"firing","labels":{"alertname":"X","env":"staging","namespace":"ns"}}]}`)
	res, err := (&Source{}).Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(res.Requests))
	}
	r := res.Requests[0]
	if r.Environment != "staging" {
		t.Fatalf("want Environment=staging (env fallback), got %q", r.Environment)
	}
	if r.Fingerprints != nil {
		t.Fatalf("want nil Fingerprints when fingerprint empty, got %v", r.Fingerprints)
	}
}

// TestDecodeGroupKeyThreaded covers groupKey propagation across multiple firing
// alerts in one webhook POST (moved from the retired trigger AM parse tests).
func TestDecodeGroupKeyThreaded(t *testing.T) {
	body := []byte(`{"groupKey":"{}:{alertname=\"X\"}","alerts":[
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"fp1"},
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"fp2"}]}`)
	res, err := (&Source{}).Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 2 {
		t.Fatalf("want 2 requests, got %d", len(res.Requests))
	}
	for _, r := range res.Requests {
		if r.GroupKey != `{}:{alertname="X"}` {
			t.Fatalf("GroupKey not threaded: %q", r.GroupKey)
		}
	}
}

// TestDecodeFiringResolvedSplit covers the firing-vs-resolved split and
// fingerprint extraction (moved from the retired trigger AM parse tests).
func TestDecodeFiringResolvedSplit(t *testing.T) {
	body := []byte(`{"alerts":[
		{"status":"firing","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"f1"},
		{"status":"resolved","labels":{"alertname":"X","namespace":"ns"},"fingerprint":"f1"}]}`)
	res, err := (&Source{}).Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 || res.Requests[0].Fingerprint != "f1" {
		t.Fatalf("want 1 firing request with fingerprint f1, got %+v", res.Requests)
	}
	if len(res.Resolved) != 1 || res.Resolved[0].Fingerprint != "f1" {
		t.Fatalf("want 1 resolved f1, got %+v", res.Resolved)
	}
}

func TestWorkloadFromLabels(t *testing.T) {
	cases := []struct {
		name             string
		labels           map[string]string
		wantKind, wantNm string
	}{
		{"deployment", map[string]string{"deployment": "payment-api"}, "Deployment", "payment-api"},
		{"pod only", map[string]string{"pod": "x-abc123"}, "Pod", "x-abc123"},
		{"controller beats pod", map[string]string{"deployment": "payment-api", "pod": "payment-api-abc"}, "Deployment", "payment-api"},
		{"workload with type", map[string]string{"workload": "w", "workload_type": "Rollout"}, "Rollout", "w"},
		{"workload no type -> empty kind", map[string]string{"workload": "w"}, "", "w"},
		{"none", map[string]string{"severity": "critical"}, "", ""},
	}
	for _, c := range cases {
		k, n := workloadFromLabels(c.labels)
		if k != c.wantKind || n != c.wantNm {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, k, n, c.wantKind, c.wantNm)
		}
	}
}

func TestDecodeResolvedProducesResolution(t *testing.T) {
	body, err := os.ReadFile("testdata/resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&Source{}).Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 0 {
		t.Fatalf("resolved must not enqueue")
	}
	if len(res.Resolved) != 1 || res.Resolved[0].Fingerprint != "abc" {
		t.Fatalf("want resolved abc, got %+v", res.Resolved)
	}
	if res.Resolved[0].At.IsZero() {
		t.Fatal("Resolution.At must not be zero (should be receipt time)")
	}
}
