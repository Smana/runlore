// Package gitops is the GitOps-failure watcher source adapter (Flux/Argo CD).
package gitops

import (
	"context"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/source"
)

// Source is a WatcherSource that forwards GitOps failure events as investigation
// requests, dropping cascade symptoms.
type Source struct{ gp providers.GitOpsProvider }

// Watch starts consuming failure events from the GitOps provider and emits
// investigate.Request values on the returned channel, skipping cascade failures.
// The channel is closed when the input closes or ctx is done.
func (s *Source) Watch(ctx context.Context) (<-chan investigate.Request, error) {
	in, err := s.gp.WatchFailures(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan investigate.Request)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case fe, ok := <-in:
				if !ok {
					return
				}
				if investigate.IsCascadeFailure(fe) {
					continue
				}
				select {
				case out <- investigate.FromFailureEvent(fe):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func init() {
	source.Register(source.Descriptor{
		Name:      "gitops",
		ConfigKey: "sources.gitops",
		Kind:      source.Watcher,
		Admission: source.EnableGated,
		Build: func(d source.Deps) (any, error) {
			// Enabled via sources.gitops.enabled. A missing key ⇒ disabled.
			node, ok := d.Raw["gitops"]
			if !ok {
				return nil, nil
			}
			var s struct {
				Enabled bool `yaml:"enabled"`
			}
			if err := node.Decode(&s); err != nil {
				return nil, err
			}
			if !s.Enabled || d.GitOps == nil {
				return nil, nil
			}
			return &Source{gp: d.GitOps}, nil
		},
	})
}
