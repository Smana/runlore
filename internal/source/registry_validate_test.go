package source

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
)

// TestBuildEnabledRejectsUnknownSourceKey ensures a typo under `sources:` fails
// loudly. The sources map escapes the config loader's strict (KnownFields)
// decode, so without this guard a mistyped key (e.g. "alertmanagr") would
// silently disable ingestion and leave the agent healthy but deaf.
func TestBuildEnabledRejectsUnknownSourceKey(t *testing.T) {
	resetForTest()
	Register(Descriptor{Name: "alertmanager", Kind: Webhook, Path: "/webhook/alertmanager",
		Build: func(Deps) (any, error) { return fakeWebhook{}, nil }})

	raw := map[string]yaml.Node{"alertmanagr": {}} // typo
	_, err := BuildEnabled(Deps{Cfg: &config.Config{}, Raw: raw})
	if err == nil {
		t.Fatal("expected BuildEnabled to reject unknown source key 'alertmanagr'")
	}
	if !strings.Contains(err.Error(), "alertmanagr") {
		t.Fatalf("error should name the unknown key, got: %v", err)
	}
}

// TestBuildEnabledAcceptsKnownSourceKey guards against over-rejection: a
// correctly-spelled key must still build.
func TestBuildEnabledAcceptsKnownSourceKey(t *testing.T) {
	resetForTest()
	Register(Descriptor{Name: "alertmanager", Kind: Webhook, Path: "/webhook/alertmanager",
		Build: func(Deps) (any, error) { return fakeWebhook{}, nil }})

	raw := map[string]yaml.Node{"alertmanager": {}}
	built, err := BuildEnabled(Deps{Cfg: &config.Config{}, Raw: raw})
	if err != nil {
		t.Fatalf("known source key should build: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("expected 1 built source, got %d", len(built))
	}
}
