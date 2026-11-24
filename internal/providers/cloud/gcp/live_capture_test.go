// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"
)

var (
	gcpEmailRE  = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
	gcpIPv4RE   = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\b`)
	gcpBearerRE = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+`)
	// gcpTokenFieldRE scrubs JSON string values for token/secret/authorization keys.
	gcpTokenFieldRE = regexp.MustCompile(`"(?i)(access_token|id_token|refresh_token|authorization|token|secret)"\s*:\s*"[^"]*"`)
)

// redactGCPFixture deterministically scrubs sensitive GCP identifiers (project ID,
// project number, cluster name, email addresses, IPv4 addresses, and bearer/token
// strings) from a raw JSON API payload while preserving valid JSON structure.
func redactGCPFixture(raw []byte, projectID, projectNum, cluster string) []byte {
	s := string(raw)
	if projectID != "" {
		s = strings.ReplaceAll(s, projectID, "proj-fixture")
	}
	if projectNum != "" {
		s = strings.ReplaceAll(s, projectNum, "123456789012")
	}
	if cluster != "" {
		s = strings.ReplaceAll(s, cluster, "gke-fixture")
	}
	s = gcpTokenFieldRE.ReplaceAllString(s, `"$1":"REDACTED"`)
	s = gcpBearerRE.ReplaceAllString(s, "Bearer REDACTED")
	s = gcpEmailRE.ReplaceAllString(s, "user@example.com")
	s = gcpIPv4RE.ReplaceAllString(s, "198.51.100.1")

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(s), "", "  "); err == nil {
		pretty.WriteByte('\n')
		return pretty.Bytes()
	}
	return []byte(s)
}

func TestRedactGCPFixture(t *testing.T) {
	raw := []byte(`{
  "entries": [
    {
      "logName": "projects/acme-prod-42/logs/cloudaudit.googleapis.com%2Factivity",
      "protoPayload": {
        "serviceName": "container.googleapis.com",
        "methodName": "google.container.v1.ClusterManager.UpdateCluster",
        "resourceName": "projects/987654321098/locations/europe-west1/clusters/prod-eu-1",
        "authenticationInfo": {
          "principalEmail": "alice.sre@acme.example.org"
        },
        "requestMetadata": {
          "callerIp": "203.0.113.77"
        },
        "status": {
          "code": 7,
          "message": "Permission denied for Bearer ya29.a0AfH6SMBxSecretToken123"
        }
      }
    }
  ],
  "cluster": {
    "name": "prod-eu-1",
    "endpoint": "34.76.12.88",
    "access_token": "ya29.raw-secret"
  }
}`)

	out := redactGCPFixture(raw, "acme-prod-42", "987654321098", "prod-eu-1")
	if !json.Valid(out) {
		t.Fatalf("redacted output is not valid JSON:\n%s", out)
	}
	got := string(out)

	for _, forbidden := range []string{
		"acme-prod-42",
		"987654321098",
		"prod-eu-1",
		"alice.sre@acme.example.org",
		"203.0.113.77",
		"34.76.12.88",
		"ya29.a0AfH6SMBxSecretToken123",
		"ya29.raw-secret",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("redacted output still contains sensitive string %q:\n%s", forbidden, got)
		}
	}

	for _, want := range []string{
		"proj-fixture",
		"123456789012",
		"gke-fixture",
		"user@example.com",
		"198.51.100.1",
		"Bearer REDACTED",
		`"access_token": "REDACTED"`,
		`"methodName": "google.container.v1.ClusterManager.UpdateCluster"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted output missing expected replacement/field %q:\n%s", want, got)
		}
	}

	// Deterministic: running twice produces identical output.
	out2 := redactGCPFixture(raw, "acme-prod-42", "987654321098", "prod-eu-1")
	if !bytes.Equal(out, out2) {
		t.Fatal("redactGCPFixture is not deterministic")
	}
}

// TestGCPLiveCapture captures real, read-only GCP API payloads from a live GKE
// cluster using Application Default Credentials, redacts sensitive identifiers,
// and writes them into testdata/captured_*.json.
//
// Gated behind RUNLORE_GCP_LIVE_CAPTURE=1 so normal `go test ./...` skips cleanly.
func TestGCPLiveCapture(t *testing.T) {
	if os.Getenv("RUNLORE_GCP_LIVE_CAPTURE") != "1" {
		t.Skip("set RUNLORE_GCP_LIVE_CAPTURE=1 to capture live GKE fixtures")
	}

	project := strings.TrimSpace(os.Getenv("RUNLORE_GCP_PROJECT"))
	location := strings.TrimSpace(os.Getenv("RUNLORE_GCP_LOCATION"))
	cluster := strings.TrimSpace(os.Getenv("RUNLORE_GCP_CLUSTER"))
	projectNum := strings.TrimSpace(os.Getenv("RUNLORE_GCP_PROJECT_NUMBER"))
	if project == "" || location == "" || cluster == "" {
		t.Fatal("RUNLORE_GCP_PROJECT, RUNLORE_GCP_LOCATION, and RUNLORE_GCP_CLUSTER are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	hc, _, err := htransport.NewClient(ctx, option.WithScopes(
		"https://www.googleapis.com/auth/cloud-platform.read-only",
	))
	if err != nil {
		t.Fatalf("ADC HTTP client: %v", err)
	}

	outDir := "testdata"
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}

	writeFixture := func(name string, raw []byte) {
		t.Helper()
		redacted := redactGCPFixture(raw, project, projectNum, cluster)
		if !json.Valid(redacted) {
			t.Fatalf("%s: redacted payload is not valid JSON", name)
		}
		if err := os.WriteFile(filepath.Join(outDir, name), redacted, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	since := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)

	// 1. Cloud Logging entries:list (normal activity + system_event)
	normalFilter := fmt.Sprintf(`logName=("projects/%s/logs/%s" OR "projects/%s/logs/%s") AND timestamp>=%q`,
		project, activityLog, project, systemEventLog, since)
	rawAudit := capturePostJSON(ctx, t, hc, "https://logging.googleapis.com/v2/entries:list", map[string]any{
		"resourceNames": []string{"projects/" + project},
		"filter":        normalFilter,
		"orderBy":       "timestamp desc",
		"pageSize":      20,
	})
	writeFixture("captured_auditlog.json", rawAudit)

	// 2. Cloud Logging entries:list (failed-calls-only)
	failedFilter := normalFilter + ` AND protoPayload.status.code!=0`
	rawFailed := capturePostJSON(ctx, t, hc, "https://logging.googleapis.com/v2/entries:list", map[string]any{
		"resourceNames": []string{"projects/" + project},
		"filter":        failedFilter,
		"orderBy":       "timestamp desc",
		"pageSize":      20,
	})
	writeFixture("captured_auditlog_failed.json", rawFailed)

	// 3. Container API cluster + node pools
	clusterURL := fmt.Sprintf("https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
		project, location, cluster)
	rawCluster := captureGetJSON(ctx, t, hc, clusterURL)
	writeFixture("captured_container.json", rawCluster)

	// 4. Compute API MIG listErrors + instances (best-effort from nodePools[].instanceGroupUrls)
	var clResp struct {
		NodePools []struct {
			InstanceGroupURLs []string `json:"instanceGroupUrls"`
		} `json:"nodePools"`
	}
	if err := json.Unmarshal(rawCluster, &clResp); err != nil {
		t.Fatalf("parse cluster JSON: %v", err)
	}
	computeCapture := map[string]json.RawMessage{}
	for _, np := range clResp.NodePools {
		if len(np.InstanceGroupURLs) == 0 {
			continue
		}
		igURL := strings.TrimSuffix(np.InstanceGroupURLs[0], "/")
		// Convert instanceGroupManagers URL or instanceGroups URL to MIG listErrors + listInstances.
		migURL := strings.Replace(igURL, "/instanceGroups/", "/instanceGroupManagers/", 1)
		if rawErrs, err := tryGetJSON(ctx, hc, migURL+"/listErrors"); err == nil {
			computeCapture["listErrors"] = json.RawMessage(rawErrs)
		}
		if rawInst, err := tryPostJSON(ctx, hc, migURL+"/listManagedInstances", nil); err == nil {
			computeCapture["listManagedInstances"] = json.RawMessage(rawInst)
		}
		break
	}
	rawCompute, err := json.Marshal(computeCapture)
	if err != nil {
		t.Fatalf("marshal compute capture: %v", err)
	}
	writeFixture("captured_compute.json", rawCompute)

	// 5. Verify negative-path Preflight 403 gcloud command renders with valid shell syntax.
	c := preflightClient(t, Identity{
		Project:           "proj-fixture",
		ProjectNumber:     "123456789012",
		PodNamespace:      "runlore",
		PodServiceAccount: "runlore",
	}, http.StatusForbidden, `{"error":{"code":403,"message":"Permission 'logging.logEntries.list' denied"}}`)
	perr := c.Preflight(ctx)
	var denied *DeniedError
	if !errors.As(perr, &denied) {
		t.Fatalf("expected *DeniedError, got %v", perr)
	}
	// Extract the gcloud command lines (before the trailing explanatory sentence) and check sh -n.
	cmdPart, _, _ := strings.Cut(denied.Command, "\n\n")
	if out, err := exec.CommandContext(ctx, "sh", "-n", "-c", cmdPart).CombinedOutput(); err != nil {
		t.Fatalf("preflight gcloud command failed shell syntax check: %v\n%s\ncmd:\n%s", err, out, cmdPart)
	}
}

func capturePostJSON(ctx context.Context, t *testing.T, hc *http.Client, endpoint string, payload any) []byte {
	t.Helper()
	b, err := tryPostJSON(ctx, hc, endpoint, payload)
	if err != nil {
		t.Fatalf("POST %s: %v", endpoint, err)
	}
	return b
}

func tryPostJSON(ctx context.Context, hc *http.Client, endpoint string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

func captureGetJSON(ctx context.Context, t *testing.T, hc *http.Client, endpoint string) []byte {
	t.Helper()
	b, err := tryGetJSON(ctx, hc, endpoint)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	return b
}

func tryGetJSON(ctx context.Context, hc *http.Client, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}
