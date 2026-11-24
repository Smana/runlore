// SPDX-License-Identifier: Apache-2.0

package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestRevertForgeGitHub(t *testing.T) {
	var (
		gotBlobCount int
		gotTreeBody  map[string]any
		gotCommit    map[string]any
		gotRef       map[string]any
		gotPR        map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/gitops":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/gitops/git/ref/heads/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"sha": "headsha1234567890"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/gitops/commits/commitsha1234567":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha":     "commitsha1234567",
				"parents": []map[string]any{{"sha": "parentsha1234567"}},
				"commit":  map[string]any{"message": "bump app"},
				"files": []map[string]any{
					{"filename": "apps/web/deploy.yaml", "status": "modified"},
					{"filename": "apps/web/new.yaml", "status": "renamed", "previous_filename": "apps/web/old.yaml"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/gitops/compare/commitsha1234567...headsha1234567890":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{"filename": "apps/other/cm.yaml"}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/gitops/contents/apps/web/deploy.yaml":
			if r.URL.Query().Get("ref") != "parentsha1234567" {
				http.Error(w, "wrong ref", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString([]byte("replicas: 2\n")),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/gitops/contents/apps/web/missing.yaml":
			http.Error(w, "not found", http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/gitops/pulls":
			if r.URL.Query().Get("head") == "org:runlore/revert-existing" {
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"html_url": "https://github.com/org/gitops/pull/7", "number": 7},
				})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/org/gitops/git/commits/headsha1234567890":
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": map[string]any{"sha": "basetreesha"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/gitops/git/blobs":
			gotBlobCount++
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "blobsha1"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/gitops/git/trees":
			_ = json.NewDecoder(r.Body).Decode(&gotTreeBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "newtreesha"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/gitops/git/commits":
			_ = json.NewDecoder(r.Body).Decode(&gotCommit)
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "newcommitsha"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/gitops/git/refs":
			_ = json.NewDecoder(r.Body).Decode(&gotRef)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": gotRef["ref"]})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/org/gitops/pulls":
			_ = json.NewDecoder(r.Body).Decode(&gotPR)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"html_url": "https://github.com/org/gitops/pull/42",
				"number":   42,
			})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "", "", "", func(context.Context) (string, error) { return "tok", nil })
	ctx := context.Background()

	br, head, err := c.DefaultBranch(ctx, "org/gitops")
	if err != nil || br != "main" || head != "headsha1234567890" {
		t.Fatalf("DefaultBranch = (%q, %q, %v)", br, head, err)
	}

	detail, err := c.CommitDetail(ctx, "org/gitops", "commitsha1234567")
	if err != nil || len(detail.Parents) != 1 || len(detail.Files) != 2 || detail.Files[1].PreviousPath != "apps/web/old.yaml" {
		t.Fatalf("CommitDetail = (%+v, %v)", detail, err)
	}

	changed, err := c.CompareChangedFiles(ctx, "org/gitops", "commitsha1234567", "headsha1234567890")
	if err != nil || len(changed) != 1 || changed[0] != "apps/other/cm.yaml" {
		t.Fatalf("CompareChangedFiles = (%v, %v)", changed, err)
	}

	blob, found, err := c.FileBlob(ctx, "org/gitops", "parentsha1234567", "apps/web/deploy.yaml")
	if err != nil || !found || !bytes.Equal(blob, []byte("replicas: 2\n")) {
		t.Fatalf("FileBlob = (%q, %v, %v)", blob, found, err)
	}

	_, found, err = c.FileBlob(ctx, "org/gitops", "parentsha1234567", "apps/web/missing.yaml")
	if err != nil || found {
		t.Fatalf("missing FileBlob = (found=%v, err=%v)", found, err)
	}

	u, num, ok, err := c.FindOpenPRByHead(ctx, "org/gitops", "runlore/revert-existing")
	if err != nil || !ok || num != 7 || u != "https://github.com/org/gitops/pull/7" {
		t.Fatalf("FindOpenPRByHead(existing) = (%q, %d, %v, %v)", u, num, ok, err)
	}

	prURL, prNum, err := c.CreateRevertDraftPR(ctx, providers.RevertPRRequest{
		Repo:          "org/gitops",
		BaseBranch:    "main",
		BaseSHA:       "headsha1234567890",
		HeadBranch:    "runlore/revert-commitsha123",
		CommitMessage: `Revert "bump app"`,
		Title:         `Revert "bump app"`,
		Body:          "body",
		Files: []providers.RevertFileAction{
			{Path: "apps/web/deploy.yaml", Content: []byte("replicas: 2\n")},
			{Path: "apps/web/added.yaml", Delete: true},
		},
	})
	if err != nil || prNum != 42 || prURL != "https://github.com/org/gitops/pull/42" {
		t.Fatalf("CreateRevertDraftPR = (%q, %d, %v)", prURL, prNum, err)
	}
	if gotBlobCount != 1 {
		t.Fatalf("blob creations = %d, want 1", gotBlobCount)
	}
	if gotTreeBody["base_tree"] != "basetreesha" {
		t.Fatalf("base_tree = %v", gotTreeBody["base_tree"])
	}
	treeEntries, _ := gotTreeBody["tree"].([]any)
	if len(treeEntries) != 2 {
		t.Fatalf("tree entries = %+v, want 2", treeEntries)
	}
	// Verify deleted file carries explicit nil sha in JSON.
	delEntry, _ := treeEntries[1].(map[string]any)
	if delEntry["path"] != "apps/web/added.yaml" || delEntry["sha"] != nil {
		t.Fatalf("delete tree entry = %+v, want sha=nil", delEntry)
	}
	if gotPR["draft"] != true {
		t.Fatalf("PR draft = %v, want true", gotPR["draft"])
	}
	if !strings.HasPrefix(gotRef["ref"].(string), "refs/heads/runlore/revert-") {
		t.Fatalf("ref = %v", gotRef["ref"])
	}
}
