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
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	// Metrics (Prometheus API) + logs (VictoriaLogs LogsQL).
	mux.HandleFunc("GET /api/v1/query", logJSON("METRICS",
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up","job":"harbor"},"value":[1700000000,"0"]}]}}`))
	mux.HandleFunc("POST /select/logsql/query", logJSON("LOGS",
		`{"_time":"2026-06-20T10:00:00Z","_msg":"db connection refused","kubernetes.pod_name":"harbor-db-0"}`))
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
	go serveHubble(":9998")
	log.Printf("MOCK backends listening on %s", addr)
	// #nosec G114 — test double, no timeouts needed.
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// serveHubble runs a gRPC Hubble observer that streams a canned DROPPED flow.
func serveHubble(addr string) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("MOCK hubble listen: %v", err)
		return
	}
	srv := grpc.NewServer()
	observerpb.RegisterObserverServer(srv, mockObserver{})
	log.Printf("MOCK hubble (gRPC) on %s", addr)
	if err := srv.Serve(lis); err != nil {
		log.Printf("MOCK hubble serve: %v", err)
	}
}

type mockObserver struct {
	observerpb.UnimplementedObserverServer
}

func (mockObserver) GetFlows(_ *observerpb.GetFlowsRequest, stream observerpb.Observer_GetFlowsServer) error {
	log.Printf("MOCK HUBBLE GetFlows")
	flow := &flowpb.Flow{
		Time:           timestamppb.New(time.Unix(1700000000, 0)),
		Verdict:        flowpb.Verdict_DROPPED,
		DropReasonDesc: flowpb.DropReason_POLICY_DENIED,
		Source:         &flowpb.Endpoint{Namespace: "apps", PodName: "harbor-core-1"},
		Destination:    &flowpb.Endpoint{Namespace: "db", PodName: "postgres-0"},
	}
	return stream.Send(&observerpb.GetFlowsResponse{ResponseTypes: &observerpb.GetFlowsResponse_Flow{Flow: flow}})
}

// chatCompletions scripts the ReAct loop: call what_changed, then kb_search, then
// submit_findings — driven by how many tool results the request already carries.
// The adversarial verify pass is a separate, tool-result-free request that offers
// ONLY the submit_verdicts tool; detect it by that tool and keep the cause so a
// curatable (Verified) finding survives to the curator.
func chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	for _, t := range req.Tools {
		if t.Function.Name == "submit_verdicts" {
			// Verify pass: keep the (single) root cause so the finding is Verified.
			log.Printf("MOCK chat/completions: verify -> submit_verdicts (keep)")
			writeToolCallSSE(w, "v0", "submit_verdicts",
				`{"verdicts":[{"index":0,"verdict":"keep","confidence":0.9,"reason":"mock: evidence supports the cause"}]}`)
			return
		}
	}
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
	case 2:
		name, args = "query_metrics", `{"query":"up"}`
	case 3:
		name, args = "query_logs", `{"query":"error","since_minutes":30}`
	case 4:
		name, args = "network_drops", `{"namespace":"apps"}`
	default:
		name, args = "submit_findings", `{"confidence":0.9,"root_causes":[{"summary":"mock: chart bump broke harbor-db","confidence":0.9,"evidence":["pg_up=0"],"suggested_action":"flux rollback hr/harbor","reversible":true}],"unresolved":["mock unresolved"],"actions":[{"description":"suspend the failing Kustomization to stop the reconcile loop","op":"suspend","reversible":true,"blast_radius":1,"target":{"kind":"Kustomization","name":"broken-app","namespace":"apps"}}]}`
	}
	log.Printf("MOCK chat/completions: toolResults=%d -> %s", toolResults, name)
	writeToolCallSSE(w, fmt.Sprintf("c%d", toolResults), name, args)
}

// writeToolCallSSE emits one tool call as an OpenAI chat/completions SSE stream:
// a delta chunk carrying the whole tool_call (the client reassembles per index, so
// sending id+name+arguments in one fragment is valid), a finish_reason chunk, a
// trailing usage-only chunk (the client requests stream_options.include_usage), and
// the [DONE] sentinel. The streaming client rejects a non-streamed JSON body, so the
// mock must speak SSE to exercise the real accumulate path.
func writeToolCallSSE(w http.ResponseWriter, id, name, args string) {
	delta := map[string]any{"choices": []any{map[string]any{
		"index": 0,
		"delta": map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"index": 0, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		},
	}}}
	finish := map[string]any{"choices": []any{map[string]any{
		"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls",
	}}}
	usage := map[string]any{
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
	}
	writeSSE(w, delta, finish, usage)
}

// writeSSE marshals each chunk as a `data:` SSE event, flushing per event, then
// writes the terminal `data: [DONE]`. Content-Type marks the stream so the client's
// SSE reader engages.
func writeSSE(w http.ResponseWriter, chunks ...any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for _, c := range chunks {
		b, err := json.Marshal(c)
		if err != nil {
			log.Printf("MOCK sse marshal: %v", err)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
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
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
