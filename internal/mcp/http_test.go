// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoServer is testServer: NewServer plus one "echo" tool.
func echoServer(t *testing.T) *Server {
	t.Helper()
	return testServer()
}

// pingOf is a valid ping request padded so its body is longer than n bytes.
func pingOf(n int) string {
	return `{"jsonrpc":"2.0","id":9,"method":"ping","params":{"pad":"` + strings.Repeat("x", n) + `"}}`
}

var testClient = &http.Client{}

func call(t *testing.T, method, url, body string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
}

func TestServeHTTP(t *testing.T) {
	s := echoServer(t)
	srv := httptest.NewServer(s)
	defer srv.Close()
	for _, c := range []struct {
		method, body string
		code         int
		has          string
	}{
		{"POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, 200, `"protocolVersion":"2025-06-18"`},
		{"POST", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"q":1}}}`, 200, `"isError":false`},
		{"POST", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"kb_write"}}`, 200, `"isError":true`},
		{"POST", `{"jsonrpc":"2.0","id":4,"method":"resources/subscribe"}`, 200, `"code":-32601`},
		{"POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202, ``},
		{"POST", `{`, 400, `"code":-32700`},
		{"POST", pingOf(1 << 20), 413, ``},
		{"GET", ``, 405, ``}, {"DELETE", ``, 405, ``},
	} {
		if code, ctype, body := call(t, c.method, srv.URL, c.body); code != c.code || !strings.Contains(body, c.has) || (code == 200 && ctype != "application/json") {
			t.Errorf("%s %.40s: %d %q %s", c.method, c.body, code, ctype, body)
		}
	}

	// The tools/call answer equals the stdio Serve answer to the same line.
	line := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"q":1}}}`
	_, _, got := call(t, "POST", srv.URL, line)
	stdio := run(t, s, line)
	if len(stdio) != 1 {
		t.Fatalf("stdio answers: %d", len(stdio))
	}
	var gotM map[string]any
	if err := jsonUnmarshal(got, &gotM); err != nil {
		t.Fatal(err)
	}
	if a, b := mustJSON(t, gotM), mustJSON(t, stdio[0]); a != b {
		t.Errorf("http answer %s != stdio answer %s", a, b)
	}

	// 405 carries Allow: POST; the 400 answer has a null id.
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("Allow") != "POST" {
		t.Errorf("Allow = %q", resp.Header.Get("Allow"))
	}
	if _, _, b := call(t, "POST", srv.URL, `{`); !strings.Contains(b, `"id":null`) {
		t.Errorf("parse error id not null: %s", b)
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
