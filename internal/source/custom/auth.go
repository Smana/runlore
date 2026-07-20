// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Smana/runlore/internal/source"
)

// Authenticate verifies a delivery's bearer token. Implementing
// source.Authenticator skips the core's shared webhook auth, so the ladder is
// re-established here explicitly: the instance's own token when configured
// (shared no longer accepted — tighter, per-vendor secrets), else the shared
// server.webhook_token_env value resolved at Build, else open (mirroring the
// alertmanager source; app.RequireWebhookAuth still refuses to start a
// model-configured server with an empty shared token, and Build fails closed
// under actions.mode=auto below).
func (s *Source) Authenticate(_ []byte, h http.Header) bool {
	inst, ok := s.instances[h.Get(source.InstanceHeader)]
	if !ok {
		return false // unknown instance: fail closed before Decode
	}
	want := inst.token
	if want == "" {
		want = s.shared
	}
	if want == "" {
		return true
	}
	const prefix = "Bearer "
	got := h.Get("Authorization")
	return strings.HasPrefix(got, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, prefix)), []byte(want)) == 1
}
