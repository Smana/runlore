// SPDX-License-Identifier: Apache-2.0

package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestRevertForgeGitLab(t *testing.T) {
	var (
		gotCommit map[string]any
		gotMR     map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.RawPath
		if p == "" {
			p = r.URL.Path
		}
		switch {
		case r.Method == http.MethodGet && p == "/api/v4/projects/group%2Fsub%2Fgitops":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
		case r.Method == http.MethodGet && p == "/api/v4/projects/group%2Fsub%2Fgitops/repository/branches/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"id": "headsha1234567890"}})
		case r.Method == http.MethodGet && p == "/api/v4/projects/group%2Fsub%2Fgitops/repository/commits/commitsha1234567":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "commitsha1234567",
				"parent_ids": []string{"parentsha1234567"},
				"message":    "bump app\n\ndetails",
			})
		case r.Method == http.MethodGet && p == "/api/v4/projects/group%2Fsub%2Fgitops/repository/commits/commitsha1234567/diff":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"old_path": "apps/web/deploy.yaml", "new_path": "apps/web/deploy.yaml"},
				{"old_path": "apps/web/added.yaml", "new_path": "apps/web/added.yaml", "new_file": true},
				{"old_path": "apps/web/removed.yaml", "new_path": "apps/web/removed.yaml", "deleted_file": true},
				{"old_path": "apps/web/old.yaml", "new_path": "apps/web/new.yaml", "renamed_file": true},
			})
		case r.Method == http.MethodGet && p == "/api/v4/projects/group%2Fsub%2Fgitops/repository/compare":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"diffs": []map[string]any{{"old_path": "apps/other/cm.yaml", "new_path": "apps/other/cm.yaml"}},
			})
		case r.Method == http.MethodGet && p == "/api/v4/projects/group%2Fsub%2Fgitops/repository/files/apps%2Fweb%2Fdeploy.yaml/raw":
			_, _ = w.Write([]byte("replicas: 2\n"))
		case r.Method == http.MethodGet && p == "/api/v4/projects/group%2Fsub%2Fgitops/repository/files/apps%2Fweb%2Fremoved.yaml/raw":
			if r.URL.Query().Get("ref") == "headsha1234567890" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte("old: true\n"))
		case r.Method == http.MethodGet && p == "/api/v4/projects/group%2Fsub%2Fgitops/merge_requests":
			if r.URL.Query().Get("source_branch") == "runlore/revert-existing" {
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"web_url": "https://gitlab.example.com/group/sub/gitops/-/merge_requests/9", "iid": 9},
				})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && p == "/api/v4/projects/group%2Fsub%2Fgitops/repository/commits":
			_ = json.NewDecoder(r.Body).Decode(&gotCommit)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "newcommitsha"})
		case r.Method == http.MethodPost && p == "/api/v4/projects/group%2Fsub%2Fgitops/merge_requests":
			_ = json.NewDecoder(r.Body).Decode(&gotMR)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"web_url": "https://gitlab.example.com/group/sub/gitops/-/merge_requests/42",
				"iid":     42,
			})
		default:
			http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", func(context.Context) (string, error) { return "tok", nil })
	ctx := context.Background()
	repo := "group/sub/gitops"

	br, head, err := c.DefaultBranch(ctx, repo)
	if err != nil || br != "main" || head != "headsha1234567890" {
		t.Fatalf("DefaultBranch = (%q, %q, %v)", br, head, err)
	}

	detail, err := c.CommitDetail(ctx, repo, "commitsha1234567")
	if err != nil || len(detail.Parents) != 1 || len(detail.Files) != 4 {
		t.Fatalf("CommitDetail = (%+v, %v)", detail, err)
	}
	if detail.Files[0].Status != "modified" || detail.Files[1].Status != "added" ||
		detail.Files[2].Status != "removed" || detail.Files[3].Status != "renamed" ||
		detail.Files[3].PreviousPath != "apps/web/old.yaml" {
		t.Fatalf("unexpected statuses: %+v", detail.Files)
	}

	changed, err := c.CompareChangedFiles(ctx, repo, "commitsha1234567", "headsha1234567890")
	if err != nil || len(changed) != 1 || changed[0] != "apps/other/cm.yaml" {
		t.Fatalf("CompareChangedFiles = (%v, %v)", changed, err)
	}

	blob, found, err := c.FileBlob(ctx, repo, "parentsha1234567", "apps/web/deploy.yaml")
	if err != nil || !found || !bytes.Equal(blob, []byte("replicas: 2\n")) {
		t.Fatalf("FileBlob = (%q, %v, %v)", blob, found, err)
	}

	u, num, ok, err := c.FindOpenPRByHead(ctx, repo, "runlore/revert-existing")
	if err != nil || !ok || num != 9 || u != "https://gitlab.example.com/group/sub/gitops/-/merge_requests/9" {
		t.Fatalf("FindOpenPRByHead = (%q, %d, %v, %v)", u, num, ok, err)
	}

	mrURL, mrNum, err := c.CreateRevertDraftPR(ctx, providers.RevertPRRequest{
		Repo:          repo,
		BaseBranch:    "main",
		BaseSHA:       "headsha1234567890",
		HeadBranch:    "runlore/revert-commitsha123",
		CommitMessage: `Revert "bump app"`,
		Title:         `Revert "bump app"`,
		Body:          "body",
		Files: []providers.RevertFileAction{
			{Path: "apps/web/deploy.yaml", Content: []byte("replicas: 2\n")},
			{Path: "apps/web/removed.yaml", Content: []byte("old: true\n")},
			{Path: "apps/web/added.yaml", Delete: true},
		},
	})
	if err != nil || mrNum != 42 || mrURL != "https://gitlab.example.com/group/sub/gitops/-/merge_requests/42" {
		t.Fatalf("CreateRevertDraftPR = (%q, %d, %v)", mrURL, mrNum, err)
	}
	if gotCommit["start_branch"] != "main" || gotCommit["branch"] != "runlore/revert-commitsha123" {
		t.Fatalf("commit payload = %+v", gotCommit)
	}
	actions, _ := gotCommit["actions"].([]any)
	if len(actions) != 3 {
		t.Fatalf("actions = %+v, want 3", actions)
	}
	if actions[0].(map[string]any)["action"] != "update" ||
		actions[1].(map[string]any)["action"] != "create" ||
		actions[2].(map[string]any)["action"] != "delete" {
		t.Fatalf("unexpected action kinds: %+v", actions)
	}
	title, _ := gotMR["title"].(string)
	if !strings.HasPrefix(title, "Draft: ") {
		t.Fatalf("MR title = %q, want Draft: prefix", title)
	}
}
