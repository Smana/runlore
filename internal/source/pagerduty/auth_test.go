package pagerduty

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func header(sigs ...string) http.Header {
	h := http.Header{}
	for _, s := range sigs {
		if h.Get("X-PagerDuty-Signature") == "" {
			h.Set("X-PagerDuty-Signature", s)
		} else {
			h.Set("X-PagerDuty-Signature", h.Get("X-PagerDuty-Signature")+","+s)
		}
	}
	return h
}

// TestAuthenticateKnownVector pins the signature scheme to an independently
// computed vector (openssl dgst -sha256 -hmac) so the implementation and the
// test can't drift together.
func TestAuthenticateKnownVector(t *testing.T) {
	body := []byte(`{"event":{"event_type":"incident.triggered"}}`)
	h := header("v1=68849d40f1a568b635d5ecd1ef218196974faba516f08b3a57330de1bd378770")
	if !(Source{secret: "it-is-a-secret"}).Authenticate(body, h) {
		t.Fatal("known-good signature must verify")
	}
}

// TestAuthenticate is the signature-verification table: valid, invalid,
// multiple signatures (zero-downtime rotation), missing header, unset secret.
func TestAuthenticate(t *testing.T) {
	const secret = "it-is-a-secret"
	body := []byte(`{"event":{"event_type":"incident.triggered"}}`)
	valid := sign(secret, body)
	other := sign("rotated-secret", body)

	cases := []struct {
		name   string
		secret string
		header http.Header
		want   bool
	}{
		{"valid signature", secret, header(valid), true},
		{"invalid signature", secret, header(other), false},
		{"tampered body", secret, header(sign(secret, []byte(`{}`))), false},
		{"multiple signatures, one valid", secret, header(other, valid), true},
		{"multiple signatures with spaces", secret, header(other, " "+valid), true},
		{"multiple signatures, none valid", secret, header(other, sign("x", body)), false},
		{"missing header", secret, http.Header{}, false},
		{"garbage header", secret, header("v1=zzzz"), false},
		{"unset secret leaves webhook open", "", http.Header{}, true},
		{"unset secret ignores any signature", "", header(other), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Source{secret: tc.secret}.Authenticate(body, tc.header)
			if got != tc.want {
				t.Fatalf("Authenticate = %v, want %v", got, tc.want)
			}
		})
	}
}
