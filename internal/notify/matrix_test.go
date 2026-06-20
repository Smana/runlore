package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMatrixDeliver(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	defer srv.Close()

	err := NewMatrix(srv.URL, "!room:hs", "tok").Deliver(context.Background(), sampleInvestigation())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !strings.Contains(gotPath, "/_matrix/client/v3/rooms/") || !strings.Contains(gotPath, "/send/m.room.message/") {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if mt, _ := gotBody["msgtype"].(string); mt != "m.notice" {
		t.Fatalf("msgtype = %v", gotBody["msgtype"])
	}
	if body, _ := gotBody["body"].(string); !strings.Contains(body, "flux rollback hr/harbor") {
		t.Fatalf("body missing content: %v", gotBody["body"])
	}
}
