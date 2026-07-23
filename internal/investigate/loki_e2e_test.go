// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/logs/loki"
)

// TestLokiToolsEndToEnd wires the three log tools to the real Loki client
// against a fake Loki serving realistic fixtures — the parity proof that
// query_logs, logs_error_summary, and discover_log_fields all work end-to-end
// on a Loki backend (client normalization + dialect query building + renderer).
func TestLokiToolsEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch {
		case r.URL.Path == "/loki/api/v1/query_range" && strings.HasPrefix(q, "sum by"):
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[
			  {"metric":{"detected_level":"error"},"values":[[1704103200,"3"],[1704103500,"412"]]}]}}`)
		case r.URL.Path == "/loki/api/v1/query_range":
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[
			  {"stream":{"namespace":"apps","pod":"harbor-db-0","container":"db"},
			   "values":[["1750413600000000000","db connection refused"]]}]}}`)
		case r.URL.Path == "/loki/api/v1/labels":
			_, _ = io.WriteString(w, `{"status":"success","data":["namespace","pod","container"]}`)
		case r.URL.Path == "/loki/api/v1/detected_fields":
			_, _ = io.WriteString(w, `{"fields":[{"label":"level","type":"string","cardinality":4,"parsers":["logfmt"]}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	lg := loki.New(srv.URL)
	flds := LogFields{Dialect: DialectLogQL}

	out, err := QueryLogsTool{Logs: lg, Fields: flds}.Call(context.Background(),
		`{"namespace":"apps","level":"error"}`)
	if err != nil || !strings.Contains(out, "db connection refused") {
		t.Fatalf("query_logs: %v\n%s", err, out)
	}
	// The renderer must find pod/container under the Loki stream-label names.
	if !strings.Contains(out, "harbor-db-0/db") {
		t.Fatalf("stream identity not derived from loki labels:\n%s", out)
	}

	out, err = LogsErrorSummaryTool{Logs: lg, Fields: flds}.Call(context.Background(),
		`{"namespace":"apps"}`)
	if err != nil || !strings.Contains(out, "412") || !strings.Contains(out, "top messages") {
		t.Fatalf("logs_error_summary: %v\n%s", err, out)
	}

	out, err = DiscoverLogFieldsTool{Logs: lg, Fields: flds}.Call(context.Background(),
		`{"namespace":"apps"}`)
	if err != nil || !strings.Contains(out, "namespace") || !strings.Contains(out, "level (×4)") {
		t.Fatalf("discover_log_fields: %v\n%s", err, out)
	}
}
