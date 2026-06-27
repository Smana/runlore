// Package source registers event-source adapters and runs their core-owned
// transports. An adapter implements Webhook (push) or Watcher (pull) and
// self-registers via Register in an init() func; the core owns HTTP auth,
// body-cap, routing, the watcher runner, and the ingest pipeline.
package source

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"gopkg.in/yaml.v3"
)

// Kind identifies whether a source adapter is push (Webhook) or pull (Watcher).
type Kind int

// Webhook is a push source driven by inbound HTTP calls; Watcher is a pull
// source that runs a long-lived goroutine to produce events.
const (
	Webhook Kind = iota
	Watcher
)

// Admission is the gate policy applied to events admitted through the pipeline.
type Admission int

// MatchGated events must pass the triggers.incidents match policy (alertmanager).
// EnableGated events are admitted whenever the source is enabled (gitops failures).
const (
	MatchGated Admission = iota
	EnableGated
)

// Resolution records that a previously firing alert has resolved.
type Resolution struct {
	Fingerprint string
	At          time.Time
}

// DecodeResult is returned by WebhookSource.Decode: a set of investigation
// requests (firing alerts) and a set of resolutions (resolved alerts).
type DecodeResult struct {
	Requests []investigate.Request
	Resolved []Resolution
}

// WebhookSource is implemented by push sources: the core calls Decode for each
// inbound HTTP request after authentication and body-capping.
type WebhookSource interface {
	Decode(body []byte, h http.Header) (DecodeResult, error)
}

// WatcherSource is implemented by pull sources: the core calls Watch once and
// drains the returned channel for the lifetime of the leader term.
type WatcherSource interface {
	Watch(ctx context.Context) (<-chan investigate.Request, error)
}

// Deps carries the shared dependencies passed to each adapter's Build function.
type Deps struct {
	Cfg    *config.Config
	GitOps providers.GitOpsProvider
	Log    *slog.Logger
	Raw    map[string]yaml.Node // per-adapter raw config, keyed by Descriptor.ConfigKey
}

// Descriptor is the self-registration record an adapter supplies to Register.
type Descriptor struct {
	Name      string
	ConfigKey string
	Kind      Kind
	Admission Admission
	Path      string // webhook only
	Build     func(Deps) (any, error)
}

// Built pairs a Descriptor with the concrete adapter instance returned by its
// Build function. The core uses it to mount webhooks and run watchers.
type Built struct {
	Desc Descriptor
	Impl any // WebhookSource or WatcherSource
}

var registry = map[string]Descriptor{}

// Register adds a source adapter to the global registry. It panics on duplicate
// names and is intended to be called from adapter init() functions.
func Register(d Descriptor) {
	if _, dup := registry[d.Name]; dup {
		panic("source: duplicate registration for " + d.Name)
	}
	registry[d.Name] = d
}

func resetForTest() { registry = map[string]Descriptor{} }

// Registered returns all registered source descriptors in deterministic
// (alphabetical) order.
func Registered() []Descriptor {
	out := make([]Descriptor, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// BuildEnabled calls each registered adapter's Build function and returns the
// non-nil results. A nil return from Build means the adapter is disabled (no
// config); an error aborts startup.
func BuildEnabled(deps Deps) ([]Built, error) {
	var built []Built
	for _, d := range Registered() {
		impl, err := d.Build(deps)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", d.Name, err)
		}
		if impl == nil {
			continue // disabled (no config)
		}
		built = append(built, Built{Desc: d, Impl: impl})
	}
	return built, nil
}
