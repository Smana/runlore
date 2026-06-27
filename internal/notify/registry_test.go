package notify

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// resetForTest saves the current registry and restores it via t.Cleanup,
// then starts each test with an empty registry. This prevents test pollution
// while letting TestSlackRegistered_buildsWithEnv use the init()-populated state.
func resetForTest(t *testing.T) {
	t.Helper()
	saved := make(map[string]Descriptor, len(registry))
	for k, v := range registry {
		saved[k] = v
	}
	registry = map[string]Descriptor{}
	t.Cleanup(func() { registry = saved })
}

// nopNotifier is a no-op notifier for test use.
type nopNotifier struct{}

func (nopNotifier) Deliver(context.Context, providers.Investigation) error { return nil }

var discardLog = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

func TestBuildEnabled_returnsNotifier(t *testing.T) {
	resetForTest(t)
	Register(Descriptor{
		Name:  "fake",
		Build: func(_ Deps) (providers.Notifier, error) { return nopNotifier{}, nil },
	})
	m, err := BuildEnabled(Deps{Cfg: &config.Config{}, Log: discardLog})
	if err != nil {
		t.Fatalf("BuildEnabled: %v", err)
	}
	if m.Len() != 1 {
		t.Fatalf("Multi.Len() = %d, want 1", m.Len())
	}
}

func TestBuildEnabled_skipsNilNotifier(t *testing.T) {
	resetForTest(t)
	Register(Descriptor{
		Name:  "disabled",
		Build: func(_ Deps) (providers.Notifier, error) { return nil, nil },
	})
	m, err := BuildEnabled(Deps{Cfg: &config.Config{}, Log: discardLog})
	if err != nil {
		t.Fatalf("BuildEnabled: %v", err)
	}
	if m.Len() != 0 {
		t.Fatalf("Multi.Len() = %d, want 0 (nil notifier must be skipped)", m.Len())
	}
}

func TestBuildEnabled_propagatesError(t *testing.T) {
	resetForTest(t)
	sentinel := errors.New("build failed")
	Register(Descriptor{
		Name:  "broken",
		Build: func(_ Deps) (providers.Notifier, error) { return nil, sentinel },
	})
	_, err := BuildEnabled(Deps{Cfg: &config.Config{}, Log: discardLog})
	if err == nil {
		t.Fatal("expected error from BuildEnabled")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v; want to wrap %v", err, sentinel)
	}
}

func TestRegister_duplicatePanics(t *testing.T) {
	resetForTest(t)
	Register(Descriptor{Name: "dup", Build: func(_ Deps) (providers.Notifier, error) { return nil, nil }})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	Register(Descriptor{Name: "dup", Build: func(_ Deps) (providers.Notifier, error) { return nil, nil }})
}

func TestBuildEnabled_deterministicOrder(t *testing.T) {
	resetForTest(t)
	var order []string
	makeBuilder := func(name string) func(Deps) (providers.Notifier, error) {
		return func(_ Deps) (providers.Notifier, error) {
			order = append(order, name)
			return nopNotifier{}, nil
		}
	}
	// Register in reverse alphabetical order; BuildEnabled must sort them.
	Register(Descriptor{Name: "z-last", Build: makeBuilder("z-last")})
	Register(Descriptor{Name: "a-first", Build: makeBuilder("a-first")})
	Register(Descriptor{Name: "m-middle", Build: makeBuilder("m-middle")})

	_, err := BuildEnabled(Deps{Cfg: &config.Config{}, Log: discardLog})
	if err != nil {
		t.Fatalf("BuildEnabled: %v", err)
	}
	want := []string{"a-first", "m-middle", "z-last"}
	for i, got := range order {
		if got != want[i] {
			t.Fatalf("order[%d] = %q, want %q (full order: %v)", i, got, want[i], order)
		}
	}
}

func TestSlackRegistered_buildsWithEnv(t *testing.T) {
	// Verify the slack init() self-registration: with a configured cfg.Notify.Slack
	// and the env var set, BuildEnabled returns a Multi of Len 1. This test does NOT
	// call resetForTest — it relies on the real init()-populated registry (slack +
	// matrix). Matrix is unconfigured, so only slack contributes.
	t.Setenv("TEST_SLACK_BOT_TOKEN", "xoxb-test-token")

	cfg := &config.Config{}
	cfg.Notify.Slack.BotTokenEnv = "TEST_SLACK_BOT_TOKEN"
	cfg.Notify.Slack.Channel = "C12345"
	// Matrix not configured → its Build returns nil.

	m, err := BuildEnabled(Deps{Cfg: cfg, Log: discardLog})
	if err != nil {
		t.Fatalf("BuildEnabled: %v", err)
	}
	if m.Len() != 1 {
		t.Fatalf("Multi.Len() = %d, want 1 (slack only)", m.Len())
	}
}
