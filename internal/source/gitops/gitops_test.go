// SPDX-License-Identifier: Apache-2.0

package gitops

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type fakeGP struct{ ch chan providers.FailureEvent }

func (f *fakeGP) WatchFailures(_ context.Context) (<-chan providers.FailureEvent, error) {
	return f.ch, nil
}
func (f *fakeGP) Changes(_ context.Context, _ providers.TimeWindow, _ providers.Selector) ([]providers.Change, error) {
	panic("not used")
}
func (f *fakeGP) Diff(_ context.Context, _ providers.Change) (providers.Diff, error) {
	panic("not used")
}

func TestWatchMapsFailureToRequestAndDropsCascade(t *testing.T) {
	ch := make(chan providers.FailureEvent, 2)
	src := &Source{gp: &fakeGP{ch: ch}}
	out, err := src.Watch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ch <- providers.FailureEvent{Workload: providers.Workload{Namespace: "ns", Kind: "Kustomization", Name: "app"}, Reason: "BuildFailed"}
	ch <- providers.FailureEvent{Workload: providers.Workload{Namespace: "ns", Name: "dep"}, Reason: "DependencyNotReady"}
	close(ch)
	var got []string
	for r := range out {
		got = append(got, r.Workload.Name)
	}
	if len(got) != 1 || got[0] != "app" {
		t.Fatalf("want [app] (cascade dropped), got %v", got)
	}
}
