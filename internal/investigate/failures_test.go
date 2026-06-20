package investigate

import (
	"context"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/trigger"
)

type collectEnqueuer struct{ reqs []Request }

func (c *collectEnqueuer) Enqueue(r Request) { c.reqs = append(c.reqs, r) }

func TestDrainFailures(t *testing.T) {
	src := make(chan providers.FailureEvent, 3)
	wl := providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}
	src <- providers.FailureEvent{Workload: wl, Reason: "BuildFailed"}
	src <- providers.FailureEvent{Workload: wl, Reason: "BuildFailed"} // duplicate within window → deduped
	src <- providers.FailureEvent{Workload: providers.Workload{Kind: "Kustomization", Name: "infra", Namespace: "flux-system"}, Reason: "HealthCheckFailed"}
	close(src)

	enq := &collectEnqueuer{}
	DrainFailures(context.Background(), src, enq, trigger.NewDeduper(30*time.Minute))

	if len(enq.reqs) != 2 {
		t.Fatalf("want 2 enqueued (one deduped), got %d", len(enq.reqs))
	}
	if enq.reqs[0].Source != SourceGitOpsFailure {
		t.Fatalf("unexpected source: %v", enq.reqs[0].Source)
	}
}
