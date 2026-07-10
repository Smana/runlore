// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/Smana/runlore/internal/config"
)

// NewHTTPServer builds the serving http.Server with every inbound bound set. Go's
// zero defaults leave each of these unlimited, exposing the long-lived server to
// Slowloris (slow header/body), unbounded idle keep-alives, and oversized headers.
// Payloads (Alertmanager/Slack) are small and synchronous, so 30s read/write is
// generous while still cutting off slow attackers; the body itself is capped per
// handler (1 MiB).
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// Leader-election timings. Package vars (not consts) so tests can shrink them;
// production always runs the defaults.
var (
	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second
)

// RunLeaderElection blocks running Lease-based leader election; the leader runs
// startWork and reports ready. Lost leadership cancels the work context and the
// replica REJOINS the election as a standby: client-go's RunOrDie returns when
// the lease is lost without the process dying (e.g. an API-server blip past
// RenewDeadline), and without the re-entry loop the replica would never contend
// again — a permanent zombie standby that /healthz keeps reporting alive,
// silently shrinking the HA pool until no replica leads at all.
func RunLeaderElection(ctx context.Context, cfg *config.Config, cs kubernetes.Interface, leader *atomic.Bool, log *slog.Logger, startWork func(context.Context)) {
	name := cfg.LeaderElection.Name
	if name == "" {
		name = "runlore-leader"
	}
	id := PodName()
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: name, Namespace: PodNamespace()},
		Client:     cs.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}
	for ctx.Err() == nil {
		// RunOrDie blocks until leadership is lost or ctx is cancelled; the loop
		// never spins hot — RunOrDie's internal acquire loop already paces every
		// retry by RetryPeriod.
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:            lock,
			ReleaseOnCancel: true,
			LeaseDuration:   leaseDuration,
			RenewDeadline:   renewDeadline,
			RetryPeriod:     retryPeriod,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(workCtx context.Context) {
					log.Info("acquired leadership", "id", id)
					leader.Store(true)
					startWork(workCtx)
				},
				OnStoppedLeading: func() {
					log.Info("lost leadership", "id", id)
					leader.Store(false)
				},
				OnNewLeader: func(current string) {
					if current != id {
						log.Info("standby; another replica leads", "leader", current)
					}
				},
			},
		})
		if ctx.Err() == nil {
			log.Warn("leadership lost without shutdown; rejoining election as standby", "id", id)
		}
	}
}

// SetMemoryLimitFromCgroup sets GOMEMLIMIT to ~90% of the cgroup v2 memory limit
// so the Go GC respects the container's memory cap — keeping the heap under a soft
// ceiling and returning memory to the OS under pressure — instead of letting RSS
// grow across investigations until the cgroup OOM-kills the process. No-op when
// GOMEMLIMIT is set explicitly, or there is no cgroup memory limit.
func SetMemoryLimitFromCgroup(log *slog.Logger) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return // an explicit operator override wins
	}
	b, err := os.ReadFile("/sys/fs/cgroup/memory.max") // cgroup v2 (EKS)
	if err != nil {
		return
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return // unlimited
	}
	cgroupMax, err := strconv.ParseInt(s, 10, 64)
	if err != nil || cgroupMax <= 0 {
		return
	}
	limit := cgroupMax / 10 * 9 // 90%: leave headroom for non-heap (stacks, bleve, runtime)
	debug.SetMemoryLimit(limit)
	log.Info("GOMEMLIMIT set from cgroup", "cgroup_max_bytes", cgroupMax, "gomemlimit_bytes", limit)
}
