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
		http.NotFound(w, r) // VictoriaLogs serves no /loki/api/v1/status/buildinfo
	}))
	defer vlSrv.Close()
	if got := Detect(context.Background(), vlSrv.URL, "", nil); got != ProviderVictoriaLogs {
		t.Fatalf("404 must fail safe to victorialogs, got %q", got)
	}

	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `<html>welcome</html>`) // 200 but not buildinfo
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
