// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"log/slog"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
	_ "github.com/Smana/runlore/internal/notify/templated" // self-register the templated notifier
	_ "github.com/Smana/runlore/internal/notify/webhook"   // self-register the webhook notifier
)

// TestBuildEnabledRoundTripAllNotifiers pins the unified registry: every notifier
// — the built-ins (notify.slack, notify.matrix) with their existing typed config
// keys verbatim, and the drop-ins (notify.webhook, notify.templated) under the
// inline Extra map — is constructed through the same BuildEnabled path from one
// YAML document. It is the guarantee the unification can never silently regress.
func TestBuildEnabledRoundTripAllNotifiers(t *testing.T) {
	t.Setenv("RT_SLACK", "https://hooks.slack.example/x")
	t.Setenv("RT_MATRIX_TOK", "syt_x")
	t.Setenv("RT_WEBHOOK", "https://sink.example/w")
	t.Setenv("RT_TEAMS", "https://teams.example/hook")
	y := `
notify:
  slack:
    webhook_url_env: RT_SLACK
  matrix:
    homeserver: https://m.example
    room_id: "!r:m.example"
    access_token_env: RT_MATRIX_TOK
  webhook:
    url_env: RT_WEBHOOK
  templated:
    - name: teams
      url_env: RT_TEAMS
      template: '{"text": {{ toJSON .Title }}}'
`
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(y), &cfg); err != nil {
		t.Fatal(err)
	}
	m, err := notify.BuildEnabled(notify.Deps{Cfg: &cfg, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Len(); got != 4 {
		t.Errorf("BuildEnabled built %d notifiers, want 4 (slack, matrix, webhook, templated)", got)
	}
}
