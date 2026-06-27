package notify

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// Deps holds the shared dependencies passed to every notifier builder.
type Deps struct {
	Cfg *config.Config
	Log *slog.Logger
}

// Descriptor describes a self-registering notifier.
type Descriptor struct {
	Name  string
	Build func(Deps) (providers.Notifier, error) // returns nil when unconfigured (disabled)
}

var registry = map[string]Descriptor{}

// Register adds a notifier descriptor to the registry. Panics on duplicate names.
func Register(d Descriptor) {
	if _, dup := registry[d.Name]; dup {
		panic("notify: duplicate registration for " + d.Name)
	}
	registry[d.Name] = d
}

// BuildEnabled builds every registered notifier whose Build returns non-nil,
// in deterministic (name-sorted) order, and wraps them in a Multi.
func BuildEnabled(deps Deps) (*Multi, error) {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	var ns []providers.Notifier
	for _, n := range names {
		notifier, err := registry[n].Build(deps)
		if err != nil {
			return nil, fmt.Errorf("notify %q: %w", n, err)
		}
		if notifier != nil {
			ns = append(ns, notifier)
		}
	}
	log := deps.Log
	return NewMulti(log, ns...), nil
}
