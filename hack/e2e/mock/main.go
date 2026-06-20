//go:build e2e

// Command mock is an e2e test double for RunLore's external backends: an
// OpenAI-compatible chat endpoint (scripted tool calls), a Slack webhook, a
// Matrix send endpoint, and a minimal GitHub API (App token + issue + PR). It is
// excluded from normal builds by the `e2e` build tag. Every request is logged to
// stdout so the e2e script can assert on behaviour.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	addr := ":9999"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", chatCompletions)
	mux.HandleFunc("POST /slack", logOK("SLACK"))
	mux.HandleFunc("PUT /_matrix/client/v3/rooms/{room}/send/m.room.message/{txn}", matrixSend)
	// Minimal GitHub API (for the curator).
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", githubToken)
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues", githubIssue)
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/heads/{branch}", githubBaseRef)
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/refs", logJSON("GH-CREATE-REF", `{}`))
	mux.HandleFunc("PUT /repos/{owner}/{repo}/contents/{path...}", logJSON("GH-PUT-CONTENTS", `{}`))
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", githubPR)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("MOCK unhandled %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	log.Printf("MOCK backends listening on %s", addr)
	// #nosec G114 — test double, no timeouts needed.
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// chatCompletions scripts the ReAct loop: call what_changed, then kb_search, then
// submit_findings — driven by how many tool results the request already carries.
func chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	toolResults := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	var name, args string
	switch toolResults {
	case 0:
		name, args = "what_changed", `{"namespace":"apps"}`
	case 1:
		name, args = "kb_search", `{"query":"harbor helmrelease upgrade"}`
	default:
		name, args = "submit_findings", `{"confidence":0.9,"root_causes":[{"summary":"mock: chart bump broke harbor-db","confidence":0.9,"evidence":["pg_up=0"],"suggested_action":"flux rollback hr/harbor","reversible":true}],"unresolved":["mock unresolved"]}`
	}
	log.Printf("MOCK chat/completions: toolResults=%d -> %s", toolResults, name)
	writeJSON(w, fmt.Sprintf(
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c%d","type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
		toolResults, name, args))
}

func matrixSend(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	log.Printf("MOCK MATRIX room=%s body=%s", r.PathValue("room"), truncate(string(body)))
	writeJSON(w, `{"event_id":"$mock"}`)
}

func githubToken(w http.ResponseWriter, _ *http.Request) {
	log.Printf("MOCK GH-TOKEN exchange")
	writeJSON(w, `{"token":"mock-installation-token","expires_at":"2099-01-01T00:00:00Z"}`)
}

func githubIssue(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	log.Printf("MOCK GH-ISSUE %s/%s body=%s", r.PathValue("owner"), r.PathValue("repo"), truncate(string(body)))
	writeJSON(w, `{"html_url":"https://github.com/mock/repo/issues/1"}`)
}

func githubBaseRef(w http.ResponseWriter, _ *http.Request) {
	log.Printf("MOCK GH-BASE-REF")
	writeJSON(w, `{"object":{"sha":"mockbasesha"}}`)
}

func githubPR(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	log.Printf("MOCK GH-PR %s/%s body=%s", r.PathValue("owner"), r.PathValue("repo"), truncate(string(body)))
	writeJSON(w, `{"html_url":"https://github.com/mock/repo/pull/2"}`)
}

func logOK(tag string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.Printf("MOCK %s body=%s", tag, truncate(string(body)))
		w.WriteHeader(http.StatusOK)
	}
}

func logJSON(tag, resp string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		log.Printf("MOCK %s body=%s", tag, truncate(string(body)))
		writeJSON(w, resp)
	}
}

func writeJSON(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, s)
}

func truncate(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
