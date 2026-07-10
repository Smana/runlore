// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Smana/runlore/internal/config"
)

// TestNewHTTPServer asserts the serving http.Server is built with every inbound
// timeout/size bound set (non-zero) — Go's defaults are zero (unlimited), the
// Slowloris/DoS gap R9(a) closes.
func TestNewHTTPServer(t *testing.T) {
	s := NewHTTPServer(":0", http.NewServeMux())
	if s.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is zero (unbounded slow-header read)")
	}
	if s.ReadTimeout == 0 {
		t.Error("ReadTimeout is zero (unbounded slow-body read)")
	}
	if s.WriteTimeout == 0 {
		t.Error("WriteTimeout is zero (unbounded slow write)")
	}
	if s.IdleTimeout == 0 {
		t.Error("IdleTimeout is zero (unbounded idle keep-alive)")
	}
	if s.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes is zero (defaults to 1MB but should be explicit)")
	}
}

// TestRunLeaderElectionRejoinsAfterLoss captures the HA zombie-standby bug: a
// replica that loses the lease WITHOUT dying (e.g. an API-server blip past
// RenewDeadline, or another holder stealing the lease) must re-enter the
// election and be able to lead again. Before the fix, RunOrDie returned after
// the first loss and the election goroutine exited — the replica stayed a
// permanent standby (never ready, never leading) while its kubelet probes
// stayed green, silently shrinking the HA pool to zero over successive flaps.
func TestRunLeaderElectionRejoinsAfterLoss(t *testing.T) {
	// Shrink the election timings so a full lose-then-rejoin cycle fits in a test.
	origLease, origRenew, origRetry := leaseDuration, renewDeadline, retryPeriod
	leaseDuration, renewDeadline, retryPeriod = 400*time.Millisecond, 300*time.Millisecond, 50*time.Millisecond
	defer func() { leaseDuration, renewDeadline, retryPeriod = origLease, origRenew, origRetry }()

	t.Setenv("POD_NAME", "replica-a")
	t.Setenv("POD_NAMESPACE", "default")
	t.Setenv("POD_IP", "10.9.8.7")
	cs := fake.NewSimpleClientset()
	// outage simulates an API-server blip: while set, every lease get/update
	// fails, so the leader cannot renew within renewDeadline and drops the lease
	// — exactly the production trigger for a leadership loss without a restart.
	var outage atomic.Bool
	blip := func(k8stesting.Action) (bool, runtime.Object, error) {
		if outage.Load() {
			return true, nil, errors.New("simulated apiserver outage")
		}
		return false, nil, nil
	}
	cs.PrependReactor("get", "leases", blip)
	cs.PrependReactor("update", "leases", blip)
	cfg := &config.Config{}
	cfg.LeaderElection.Enabled = true
	cfg.LeaderElection.Name = "test-leader"

	var leader atomic.Bool
	var tracker LeaderTracker
	var terms atomic.Int32 // one startWork call per leadership term
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunLeaderElection(ctx, cfg, cs, &leader, &tracker,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			func(context.Context) { terms.Add(1) })
	}()

	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", desc)
	}

	waitFor("initial leadership", leader.Load)

	// #264: the lease identity must be ROUTABLE — <podName>_<podIP> — so a
	// follower can proxy work-bearing requests to the holder. Assert both the
	// written Lease and the OnNewLeader-fed tracker expose it.
	lease, err := cs.CoordinationV1().Leases("default").Get(ctx, "test-leader", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if got := *lease.Spec.HolderIdentity; got != "replica-a_10.9.8.7" {
		t.Errorf("lease holder identity = %q, want replica-a_10.9.8.7", got)
	}
	waitFor("tracker learned the routable holder", func() bool {
		return tracker.Addr("8080") == "10.9.8.7:8080"
	})

	// Blip the API server for longer than renewDeadline: the replica must drop
	// leadership (it cannot renew), but the process stays alive.
	outage.Store(true)
	waitFor("leadership loss during the apiserver blip", func() bool { return !leader.Load() })

	// The blip clears. The replica must REJOIN the election and lead a second
	// term — before the fix the election goroutine had already exited, leaving a
	// permanent zombie standby.
	outage.Store(false)
	waitFor("re-acquired leadership after the blip cleared", leader.Load)
	if got := terms.Load(); got < 2 {
		t.Errorf("startWork ran %d time(s), want >=2 (one per leadership term)", got)
	}

	// Shutdown: cancelling the context must end the election loop.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunLeaderElection did not return after context cancel")
	}
}
