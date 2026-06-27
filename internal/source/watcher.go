package source

import (
	"context"
	"log/slog"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/trigger"
)

// RunWatchers starts each Watcher source and drains its Requests into enq,
// applying the same dedup + debounce as the legacy gitops-failure path:
// dedup (when non-nil) suppresses repeated failures for the same workload
// (key "namespace/name"); deb (when non-nil) delays enqueue until the failure
// persists (a zero-window debouncer enqueues immediately). A watch error is
// logged and that source skipped, without affecting siblings.
func RunWatchers(ctx context.Context, built []Built, enq investigate.Enqueuer, dedup *trigger.Deduper, deb *investigate.Debouncer, log *slog.Logger) {
	for _, b := range built {
		if b.Desc.Kind != Watcher {
			continue
		}
		ch, err := b.Impl.(WatcherSource).Watch(ctx)
		if err != nil {
			if log != nil {
				log.Error("source watch failed", "source", b.Desc.Name, "err", err)
			}
			continue
		}
		go drainWatch(ctx, ch, enq, dedup, deb)
	}
}

func drainWatch(ctx context.Context, ch <-chan investigate.Request, enq investigate.Enqueuer, dedup *trigger.Deduper, deb *investigate.Debouncer) {
	for {
		select {
		case <-ctx.Done():
			return
		case r, ok := <-ch:
			if !ok {
				return
			}
			if dedup != nil && dedup.Seen(r.Workload.Namespace+"/"+r.Workload.Name) {
				continue
			}
			if deb != nil {
				go deb.Debounce(ctx, r, enq)
			} else {
				enq.Enqueue(r)
			}
		}
	}
}
