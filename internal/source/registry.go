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

type Kind int
const ( Webhook Kind = iota; Watcher )

type Admission int
const ( MatchGated Admission = iota; EnableGated )

type Resolution struct {
	Fingerprint string
	At          time.Time
}

type DecodeResult struct {
	Requests []investigate.Request
	Resolved []Resolution
}

type WebhookSource interface {
	Decode(body []byte, h http.Header) (DecodeResult, error)
}
type WatcherSource interface {
	Watch(ctx context.Context) (<-chan investigate.Request, error)
}

type Deps struct {
	Cfg    *config.Config
	GitOps providers.GitOpsProvider
	Log    *slog.Logger
	Raw    map[string]yaml.Node // per-adapter raw config, keyed by Descriptor.ConfigKey
}

type Descriptor struct {
	Name      string
	ConfigKey string
	Kind      Kind
	Admission Admission
	Path      string // webhook only
	Build     func(Deps) (any, error)
}

type Built struct {
	Desc Descriptor
	Impl any // WebhookSource or WatcherSource
}

var registry = map[string]Descriptor{}

func Register(d Descriptor) {
	if _, dup := registry[d.Name]; dup {
		panic("source: duplicate registration for " + d.Name)
	}
	registry[d.Name] = d
}

func resetForTest() { registry = map[string]Descriptor{} }

func Registered() []Descriptor {
	out := make([]Descriptor, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

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
