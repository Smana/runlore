// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateAndEditIssue(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(b))
		_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/o/r/issues/7"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))

	n, ref, err := c.CreateIssue(context.Background(), "Knowledge queue", "body", []string{"runlore-queue"})
	if err != nil || n != 7 || ref.URL != "https://github.com/o/r/issues/7" {
		t.Fatalf("CreateIssue: %d %+v %v", n, ref, err)
	}
	var sent struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !strings.HasPrefix(got[0], "POST /repos/o/r/issues ") {
		t.Fatalf("request: %s", got[0])
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got[0], "POST /repos/o/r/issues ")), &sent); err != nil ||
		sent.Title != "Knowledge queue" || sent.Body != "body" || len(sent.Labels) != 1 || sent.Labels[0] != "runlore-queue" {
		t.Fatalf("payload: %+v %v", sent, err)
	}

	if err := c.EditIssue(context.Background(), 7, "new body"); err != nil {
		t.Fatal(err)
	}
	if got[1] != `PATCH /repos/o/r/issues/7 {"body":"new body"}` {
		t.Fatalf("edit request: %s", got[1])
	}
}

func TestListMergedPRsByLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Fatalf("path %s", r.URL.Path)
		}
		q := r.URL.Query().Get("q")
		for _, want := range []string{"repo:o/r", "is:pr", "is:merged", "label:runlore", "merged:>=2026-11-02"} {
			if !strings.Contains(q, want) {
				t.Fatalf("query %q missing %q", q, want)
			}
		}
		_, _ = w.Write([]byte(`{"total_count":2,"items":[
		 {"number":3,"title":"KB: a","labels":[{"name":"runlore"}],"pull_request":{"merged_at":"2026-11-02T10:00:00Z"}},
		 {"number":4,"title":"plain","labels":[{"name":"runlore"}]}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	prs, err := c.ListMergedPRsByLabel(context.Background(), "runlore", time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	if err != nil || len(prs) != 1 || prs[0].Number != 3 {
		t.Fatalf("%+v %v", prs, err)
	}
}

func TestListMergedPRsByLabelWithoutSinceHasNoDateClause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("q"); strings.Contains(q, "merged:") {
			t.Fatalf("zero since means every merge: %q", q)
		}
		_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	if _, err := c.ListMergedPRsByLabel(context.Background(), "runlore", time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestListPRsByLabelCarriesCreatedAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"number":1,"title":"KB: a","created_at":"2026-11-01T08:30:00Z","labels":[],"pull_request":{}}]`))
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil || len(prs) != 1 || !prs[0].CreatedAt.Equal(time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", prs, err)
	}
}
