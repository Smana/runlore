// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDetect mirrors the metrics flavor probe's contract
// (internal/metrics/prometheus DetectFlavor): best-effort, one shot, FAIL SAFE
// to the shipped default. Only a 200 buildinfo JSON with a version identifies
// Loki; a 404 (VictoriaLogs), an unreachable backend, or a 200 that is not
// buildinfo JSON (a proxy's HTML error page) all resolve to victorialogs.
func TestDetect(t *testing.T) {
	lokiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/status/buildinfo" {
			t.Errorf("path=%q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"version":"3.1.0","revision":"935aee77ed","branch":"HEAD","goVersion":"go1.22"}`)
	}))
	defer lokiSrv.Close()
	if got := Detect(context.Background(), lokiSrv.URL, "", nil); got != ProviderLoki {
		t.Fatalf("buildinfo 200 must detect loki, got %q", got)
	}

	vlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // VictoriaLogs serves no /loki/api/v1/status/buildinfo and no ES-shaped root
	}))
	defer vlSrv.Close()
	if got := Detect(context.Background(), vlSrv.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("404 must fail safe to victorialogs, got %q", got)
	}

	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html>welcome</html>`) // 200 but not buildinfo/ES root — VictoriaLogs' real root page
	}))
	defer htmlSrv.Close()
	if got := Detect(context.Background(), htmlSrv.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("non-JSON 200 must fail safe to victorialogs, got %q", got)
	}

	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()
	if got := Detect(context.Background(), down.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("unreachable must fail safe to victorialogs, got %q", got)
	}
}

// TestDetectSendsAuth: a Loki behind auth must still be detectable.
func TestDetectSendsAuth(t *testing.T) {
	t.Setenv("RUNLORE_TEST_DETECT_TOKEN", "s3cr3t")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"version":"3.1.0"}`)
	}))
	defer srv.Close()
	if got := Detect(context.Background(), srv.URL, "RUNLORE_TEST_DETECT_TOKEN", map[string]string{"X-Scope-OrgID": "t"}); got != ProviderLoki {
		t.Fatalf("got %q", got)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("probe must carry auth, got %q", gotAuth)
	}
}

// TestDetectElasticsearch: a plain `version.number` with no `distribution`
// field (real Elasticsearch's root response shape) identifies Elasticsearch.
func TestDetectElasticsearch(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/status/buildinfo" {
			http.NotFound(w, r)
			return
		}
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{
			"name": "es01",
			"cluster_name": "docker-cluster",
			"version": {
				"number": "8.11.3",
				"build_flavor": "default",
				"build_type": "docker",
				"lucene_version": "9.7.0"
			},
			"tagline": "You Know, for Search"
		}`)
	}))
	defer srv.Close()
	if got := Detect(context.Background(), srv.URL, "", nil); got != ProviderElasticsearch {
		t.Fatalf("ES root response must detect elasticsearch, got %q", got)
	}
	if gotPath != "/" {
		t.Fatalf("ES probe must hit root, got path %q", gotPath)
	}
}

// TestDetectOpenSearch: `version.distribution: opensearch` (the field
// OpenSearch adds precisely so clients can tell the two apart) identifies
// OpenSearch rather than Elasticsearch.
func TestDetectOpenSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/status/buildinfo" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{
			"name": "os01",
			"cluster_name": "opensearch-cluster",
			"version": {
				"distribution": "opensearch",
				"number": "2.11.0",
				"build_type": "tar",
				"lucene_version": "9.7.0"
			},
			"tagline": "The OpenSearch Project: https://opensearch.org/"
		}`)
	}))
	defer srv.Close()
	if got := Detect(context.Background(), srv.URL, "", nil); got != ProviderOpenSearch {
		t.Fatalf("OpenSearch root response must detect opensearch, got %q", got)
	}
}

// TestDetectElasticAuth: the ES/OpenSearch probe carries the same auth as the
// Loki probe — a backend behind auth must still be detectable.
func TestDetectElasticAuth(t *testing.T) {
	t.Setenv("RUNLORE_TEST_DETECT_ES_TOKEN", "es-s3cr3t")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/status/buildinfo" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"version":{"number":"8.11.3"}}`)
	}))
	defer srv.Close()
	if got := Detect(context.Background(), srv.URL, "RUNLORE_TEST_DETECT_ES_TOKEN", nil); got != ProviderElasticsearch {
		t.Fatalf("got %q", got)
	}
	if gotAuth != "Bearer es-s3cr3t" {
		t.Fatalf("ES probe must carry auth, got %q", gotAuth)
	}
}

// TestDetectElasticFailSafe: a non-200 and an unrecognised 200 payload on the
// ES/OpenSearch root probe must still fail safe to victorialogs — the SAME
// guarantee TestDetect already pins for the Loki probe, verified explicitly
// against the second probe so neither one can regress it independently.
func TestDetectElasticFailSafe(t *testing.T) {
	// Non-200 on both probes.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer errSrv.Close()
	if got := Detect(context.Background(), errSrv.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("503 on both probes must fail safe to victorialogs, got %q", got)
	}

	// 200 but no version.number at all (an unrelated JSON API, or VictoriaLogs'
	// own /select endpoints answering something JSON-shaped at root).
	unrecognisedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/status/buildinfo" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer unrecognisedSrv.Close()
	if got := Detect(context.Background(), unrecognisedSrv.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("unrecognised 200 payload must fail safe to victorialogs, got %q", got)
	}

	// Unreachable.
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()
	if got := Detect(context.Background(), down.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("unreachable must fail safe to victorialogs, got %q", got)
	}
}
