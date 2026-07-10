// SPDX-License-Identifier: Apache-2.0

package clientcore

import "testing"

// TestNewBase asserts the shared constructor normalization every provider relies
// on: base-URL fallback and trailing-slash trimming (providers join "/v1/..."
// paths verbatim, so a trailing slash would produce "//v1/...") and the
// maxTokens fallback (a zero config value must never reach the wire as
// max_tokens: 0).
func TestNewBase(t *testing.T) {
	cases := []struct {
		name           string
		baseURL        string
		defaultBaseURL string
		maxTokens      int
		wantURL        string
		wantMaxTokens  int
	}{
		{"empty baseURL falls back to the provider default", "", "https://api.example.com", 100, "https://api.example.com", 100},
		{"explicit baseURL wins over the default", "https://proxy.internal", "https://api.example.com", 100, "https://proxy.internal", 100},
		{"trailing slashes are trimmed", "https://api.example.com///", "", 100, "https://api.example.com", 100},
		{"the default is trimmed too", "", "https://api.example.com/", 100, "https://api.example.com", 100},
		{"no baseURL and no default stays empty (openai has no public default)", "", "", 100, "", 100},
		{"zero maxTokens falls back to DefaultMaxTokens", "https://u", "", 0, "https://u", DefaultMaxTokens},
		{"negative maxTokens falls back to DefaultMaxTokens", "https://u", "", -5, "https://u", DefaultMaxTokens},
		{"positive maxTokens is preserved", "https://u", "", 42, "https://u", 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBase(tc.baseURL, tc.defaultBaseURL, "model-x", "key-y", tc.maxTokens)
			if b.BaseURL != tc.wantURL {
				t.Errorf("BaseURL = %q, want %q", b.BaseURL, tc.wantURL)
			}
			if b.MaxTokens != tc.wantMaxTokens {
				t.Errorf("MaxTokens = %d, want %d", b.MaxTokens, tc.wantMaxTokens)
			}
			if b.Model != "model-x" || b.APIKey != "key-y" {
				t.Errorf("Model/APIKey not passed through: got %q/%q", b.Model, b.APIKey)
			}
		})
	}
}

// TestNewBaseHTTPClient asserts the constructed client is the hardened streaming
// client: no flat Timeout (a flat deadline would kill a legitimate long
// completion mid-stream — ctx + the idle reader bound the request instead) and
// the redirect guard wired (SSRF defense).
func TestNewBaseHTTPClient(t *testing.T) {
	b := NewBase("", "", "m", "k", 1)
	if b.HTTP == nil {
		t.Fatal("HTTP client must be set")
	}
	if b.HTTP.Timeout != 0 {
		t.Errorf("streaming client must not have a flat Timeout, got %v", b.HTTP.Timeout)
	}
	if b.HTTP.CheckRedirect == nil {
		t.Error("streaming client must carry the redirect guard")
	}
}
