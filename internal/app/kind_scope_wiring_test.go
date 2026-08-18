// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/providers/cluster"
)

// scopelessReader is a spec reader with no discovery behind it — the shape a future
// implementation (a recorded fixture, a test double, another backend) could take.
type scopelessReader struct{}

func (scopelessReader) ResourceSpec(context.Context, providers.ResourceSpecQuery) (providers.ResourceSpec, error) {
	return providers.ResourceSpec{}, nil
}

// TestKindScoperForRecognisesTheDiscoveryBackedReader pins the wiring hop that can
// break in total silence.
//
// The investigator reaches the cluster's namespaced-ness answer through a TYPE
// ASSERTION on the spec reader, for the same reason resource_spec reaches Invalidate()
// through one — and a type assertion that stops matching does not fail to compile. It
// just stops resolving scopes, every delivered Workload falls back to ScopeUnknown, and
// the renderer silently returns to guessing from hardcoded kind lists. Nothing else
// would notice.
func TestKindScoperForRecognisesTheDiscoveryBackedReader(t *testing.T) {
	if ks := kindScoperFor(cluster.NewSpecReader(nil, nil)); ks == nil {
		t.Fatal("the discovery-backed spec reader no longer answers as a providers.KindScoper: " +
			"delivered resources now carry no scope and the card is back to guessing from kind names")
	}
	if ks := kindScoperFor(nil); ks != nil {
		t.Error("no reader must mean no scoper — a non-nil interface holding nothing would be " +
			"asked for scopes it cannot resolve")
	}
	if ks := kindScoperFor(scopelessReader{}); ks != nil {
		t.Error("a reader with no discovery behind it must not be advertised as a scoper")
	}
}

// TestKindScoperFromToolsReusesTheOneSpecReader pins where the scoper comes from.
//
// It is deliberately taken from the ALREADY-BUILT resource_spec tool rather than by
// calling BuildResourceSpecReader a second time: a second call means a second memoised
// discovery client, so the process pays two full discovery fan-outs and the two caches
// go stale independently. Deps exists to stop exactly that class of duplication for the
// catalog; the same argument applies here.
func TestKindScoperFromToolsReusesTheOneSpecReader(t *testing.T) {
	reader := cluster.NewSpecReader(nil, nil)
	tools := []investigate.Tool{
		investigate.KBSearchTool{},
		investigate.ResourceSpecTool{Reader: reader},
	}
	ks := kindScoperFromTools(tools)
	if ks == nil {
		t.Fatal("the scoper must be taken from the registered resource_spec tool")
	}
	if ks != providers.KindScoper(reader) {
		t.Error("the scoper must be the SAME reader the tool holds, not a second discovery client")
	}
	// No cluster ⇒ no resource_spec tool ⇒ nothing to ask, which is the ordinary case
	// for an eval run, the demo, and any local run without a kubeconfig.
	if ks := kindScoperFromTools([]investigate.Tool{investigate.KBSearchTool{}}); ks != nil {
		t.Error("with no resource_spec tool registered there is no discovery to reach")
	}
	if ks := kindScoperFromTools(nil); ks != nil {
		t.Error("no tools must mean no scoper")
	}
}

// TestBuildInvestigatorWiresTheKindScoper pins the last hop: the scoper reaching the
// LOOP. Everything else can be correct — discovery answering, the renderer preferring
// the carried scope — and the feature still be a complete no-op if the field is never
// set on the investigator that runs.
func TestBuildInvestigatorWiresTheKindScoper(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent-kubeconfig"))
	log := discardLog()
	cfg := &config.Config{Model: config.Model{
		Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "test-model",
	}}
	deps := BuildDeps(context.Background(), cfg, nil, nil, nil, log)
	if deps == nil {
		t.Fatal("BuildDeps returned nil for a configured model")
	}
	// No cluster here (the kubeconfig above does not exist), so resource_spec is not
	// registered and there is nothing to scope with — the ordinary degraded case. Stand
	// one in, exactly as the production wiring would have.
	reader := cluster.NewSpecReader(nil, nil)
	deps.Tools = append(deps.Tools, investigate.ResourceSpecTool{Reader: reader})

	inv, _, _, err := BuildInvestigator(context.Background(), cfg, deps, nil, nil, nil, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	li, ok := inv.(*investigate.LoopInvestigator)
	if !ok {
		t.Fatalf("want *LoopInvestigator, got %T", inv)
	}
	if li.KindScope == nil {
		t.Fatal("the investigator carries no KindScope: every delivered resource falls back to " +
			"ScopeUnknown and the card guesses from kind names again")
	}
}
