package pagerduty

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// signatureHeader is the header PagerDuty V3 signs each delivery with.
const signatureHeader = "X-PagerDuty-Signature"

// Authenticate verifies a PagerDuty V3 webhook delivery. The X-PagerDuty-Signature
// header carries one or more comma-separated `v1=<hex>` signatures (multiple during
// a zero-downtime secret rotation), each an HMAC-SHA256 of the raw request body
// keyed by the signing secret. The request is authentic if ANY signature matches
// (constant-time compare).
//
// When the source has no secret configured the webhook is open (returns true),
// mirroring the alertmanager source's optional bearer token. The serve path
// enforces fail-closed policy (a configured model, or mode=auto, requires a
// secret) before this is reached — see app.RequirePagerDutyAuth and Build.
func (s Source) Authenticate(body []byte, h http.Header) bool {
	if s.secret == "" {
		return true
	}
	raw := h.Get(signatureHeader)
	if raw == "" {
		return false // secret configured but no signature: fail closed
	}
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write(body)
	want := mac.Sum(nil)

	for _, sig := range strings.Split(raw, ",") {
		sig = strings.TrimSpace(sig)
		hexSig, ok := strings.CutPrefix(sig, "v1=")
		if !ok {
			continue // only the v1 scheme is understood
		}
		got, err := hex.DecodeString(hexSig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return true
		}
	}
	return false
}
