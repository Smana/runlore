package investigate

import (
	"context"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/trigger"
)

// DrainFailures forwards GitOps FailureEvents into the queue as investigation
// requests, deduped by workload (a Ready=False resource emits repeated events).
// A nil dedup disables dedup. It returns when src closes or ctx is done.
func DrainFailures(ctx context.Context, src <-chan providers.FailureEvent, q Enqueuer, dedup *trigger.Deduper) {
	for {
		select {
		case <-ctx.Done():
			return
		case fe, ok := <-src:
			if !ok {
				return
			}
			if dedup != nil && dedup.Seen(fe.Workload.Namespace+"/"+fe.Workload.Name) {
				continue
			}
			q.Enqueue(FromFailureEvent(fe))
		}
	}
}
