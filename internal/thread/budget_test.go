// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// TestBudgetFreshAllowsThenDeniesOnCallCap pins the call-ceiling half of the
// contract: a fresh budget allows, and once maxCalls attempts have landed in
// the window, the next Allow denies.
func TestBudgetFreshAllowsThenDeniesOnCallCap(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBudget(2, 0, time.Hour, nil)
	b.now = func() time.Time { return now }

	if allowed, reason := b.Allow(0); !allowed {
		t.Fatalf("first call within budget should be allowed, reason=%q", reason)
	}
	if allowed, reason := b.Allow(0); !allowed {
		t.Fatalf("second call within budget should be allowed, reason=%q", reason)
	}
	if allowed, _ := b.Allow(0); allowed {
		t.Fatal("third call should be denied (maxCalls=2)")
	}
}

// TestBudgetTokensIndependentOfCalls pins the distinction that matters: Record
// folds in actual provider-reported usage, and once the accumulated total
// crosses maxTokens the next Allow denies even though the call count still
// has headroom — the two ceilings are independent, not alternatives.
func TestBudgetTokensIndependentOfCalls(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBudget(100, 1000, time.Hour, nil)
	b.now = func() time.Time { return now }

	if allowed, reason := b.Allow(0); !allowed {
		t.Fatalf("first call should be allowed: fresh budget, plenty of call headroom, reason=%q", reason)
	}
	b.Record(providers.Usage{InputTokens: 600, OutputTokens: 500}) // 1100 > 1000

	if allowed, _ := b.Allow(0); allowed {
		t.Fatal("Allow should deny once recorded usage crosses maxTokens, even with call-count headroom (98 calls left)")
	}

	calls, tokens := b.Remaining()
	if calls <= 0 {
		t.Fatalf("Remaining calls = %d, want > 0 — the call ceiling was never the reason for the denial", calls)
	}
	if tokens > 0 {
		t.Fatalf("Remaining tokens = %d, want <= 0 — the token ceiling is what denied", tokens)
	}
}

// TestBudgetAllowReasonPerCeiling pins that Allow's second return value
// names the ceiling that actually denied, determined atomically inside the
// same critical section as the decision — not inferred afterwards from a
// second, racy call to Remaining(). The two ceilings are checked
// independently: a call-count denial while tokens still have headroom, and a
// token denial while calls still have headroom.
func TestBudgetAllowReasonPerCeiling(t *testing.T) {
	t.Run("calls ceiling denies, tokens have headroom", func(t *testing.T) {
		now := time.Unix(0, 0)
		b := NewBudget(1, 1000, time.Hour, nil)
		b.now = func() time.Time { return now }

		if allowed, reason := b.Allow(0); !allowed {
			t.Fatalf("first call should be allowed, reason=%q", reason)
		}

		allowed, reason := b.Allow(0)
		if allowed {
			t.Fatal("second call should be denied: maxCalls=1 already spent")
		}
		if reason != DenyCalls {
			t.Fatalf("reason = %q, want %q", reason, DenyCalls)
		}
		if _, tokens := b.Remaining(); tokens <= 0 {
			t.Fatalf("Remaining tokens = %d, want > 0 — the token ceiling was never touched", tokens)
		}
	})

	t.Run("tokens ceiling denies, calls have headroom", func(t *testing.T) {
		now := time.Unix(0, 0)
		b := NewBudget(100, 500, time.Hour, nil)
		b.now = func() time.Time { return now }

		if allowed, reason := b.Allow(0); !allowed {
			t.Fatalf("first call should be allowed, reason=%q", reason)
		}
		b.Record(providers.Usage{InputTokens: 300, OutputTokens: 300}) // 600 > 500

		allowed, reason := b.Allow(0)
		if allowed {
			t.Fatal("second call should be denied: recorded usage crosses maxTokens")
		}
		if reason != DenyTokens {
			t.Fatalf("reason = %q, want %q", reason, DenyTokens)
		}
		if calls, _ := b.Remaining(); calls <= 0 {
			t.Fatalf("Remaining calls = %d, want > 0 — the call ceiling was never touched", calls)
		}
	})
}

// TestBudgetBothLimitsSlideWithWindow pins that both dimensions slide, like
// ratelimit.Window: once old entries age out of the window, the budget must
// look fresh again.
func TestBudgetBothLimitsSlideWithWindow(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBudget(1, 1000, time.Minute, nil)
	b.now = func() time.Time { return now }

	if allowed, _ := b.Allow(0); !allowed {
		t.Fatal("first call should be allowed")
	}
	b.Record(providers.Usage{InputTokens: 900, OutputTokens: 200}) // 1100 > 1000

	if allowed, _ := b.Allow(0); allowed {
		t.Fatal("second call should be denied: both the call cap and the token cap are already spent")
	}

	// Roll the window forward past both the call entry and the token entry.
	now = now.Add(2 * time.Minute)

	if allowed, _ := b.Allow(0); !allowed {
		t.Fatal("after the window slides clear, a new call should be allowed again")
	}
	calls, tokens := b.Remaining()
	if calls != 0 {
		// maxCalls=1 and the Allow() above just consumed the only slot.
		t.Fatalf("Remaining calls = %d, want 0 right after consuming the sole slot post-slide", calls)
	}
	if tokens != 1000 {
		t.Fatalf("Remaining tokens = %d, want 1000 — the old 1100-token entry should have slid out of the window", tokens)
	}
}

// TestBudgetZeroOrNegativeMeansUnlimitedPerDimension pins that <= 0 means
// unlimited for that dimension ONLY — the ratelimit.Window convention this
// type follows. A call cap of 0 must not accidentally make tokens unlimited
// too, and vice versa.
func TestBudgetZeroOrNegativeMeansUnlimitedPerDimension(t *testing.T) {
	t.Run("calls unlimited, tokens enforced", func(t *testing.T) {
		now := time.Unix(0, 0)
		b := NewBudget(0, 1000, time.Hour, nil)
		b.now = func() time.Time { return now }

		for i := 0; i < 50; i++ {
			if allowed, _ := b.Allow(0); !allowed {
				t.Fatalf("call %d: maxCalls<=0 must be unlimited", i)
			}
		}
		b.Record(providers.Usage{InputTokens: 600, OutputTokens: 500}) // 1100 > 1000
		if allowed, _ := b.Allow(0); allowed {
			t.Fatal("tokens must still be enforced even though calls are unlimited")
		}
	})

	t.Run("tokens unlimited, calls enforced", func(t *testing.T) {
		now := time.Unix(0, 0)
		b := NewBudget(2, -1, time.Hour, nil)
		b.now = func() time.Time { return now }

		if allowed, _ := b.Allow(0); !allowed {
			t.Fatal("first call should be allowed")
		}
		b.Record(providers.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
		if allowed, _ := b.Allow(0); !allowed {
			t.Fatal("maxTokens<=0 must be unlimited regardless of recorded usage")
		}
		if allowed, _ := b.Allow(0); allowed {
			t.Fatal("calls must still be enforced (maxCalls=2, this is the third attempt)")
		}
	})
}

// TestBudgetUnlimitedDimensionAccumulatesNothing pins the other half of the
// <= 0 convention: an unlimited dimension must not just be un-enforced, it
// must be un-tracked. ratelimit.Window.Allow returns before taking its lock
// when max <= 0; Budget follows that shape per dimension, because entries an
// unlimited ceiling records are entries Remaining() never reads (it reports
// math.MaxInt / math.MaxInt64 outright) — they only grow the slice and hold
// the lock for nothing, unbounded, for the whole life of the process.
func TestBudgetUnlimitedDimensionAccumulatesNothing(t *testing.T) {
	t.Run("calls unlimited: no call entries recorded", func(t *testing.T) {
		now := time.Unix(0, 0)
		b := NewBudget(0, 1000, time.Hour, nil)
		b.now = func() time.Time { return now }

		for i := 0; i < 50; i++ {
			if allowed, _ := b.Allow(0); !allowed {
				t.Fatalf("call %d: maxCalls<=0 must be unlimited", i)
			}
		}
		if len(b.calls) != 0 {
			t.Fatalf("len(b.calls) = %d after 50 Allow calls, want 0 — an unlimited ceiling must record nothing", len(b.calls))
		}
	})

	t.Run("tokens unlimited: no spend entries recorded", func(t *testing.T) {
		now := time.Unix(0, 0)
		b := NewBudget(100, 0, time.Hour, nil)
		b.now = func() time.Time { return now }

		for i := 0; i < 50; i++ {
			b.Record(providers.Usage{InputTokens: 1000, OutputTokens: 1000})
		}
		if len(b.spent) != 0 {
			t.Fatalf("len(b.spent) = %d after 50 Record calls, want 0 — an unlimited ceiling must record nothing", len(b.spent))
		}
		// The enforced dimension must be unaffected by the short-circuit.
		if allowed, _ := b.Allow(0); !allowed {
			t.Fatal("the call ceiling still has headroom; Allow must grant")
		}
		if len(b.calls) != 1 {
			t.Fatalf("len(b.calls) = %d, want 1 — the ENFORCED dimension must still record", len(b.calls))
		}
	})

	t.Run("both unlimited: nothing recorded at all", func(t *testing.T) {
		now := time.Unix(0, 0)
		b := NewBudget(0, -1, time.Hour, nil)
		b.now = func() time.Time { return now }

		for i := 0; i < 50; i++ {
			allowed, reason := b.Allow(0)
			if !allowed {
				t.Fatalf("call %d: both ceilings unlimited must always allow, reason=%q", i, reason)
			}
			b.Record(providers.Usage{InputTokens: 1000, OutputTokens: 1000})
		}
		if len(b.calls) != 0 || len(b.spent) != 0 {
			t.Fatalf("len(b.calls)=%d len(b.spent)=%d, want 0 and 0 — nothing is enforced, so nothing should be tracked",
				len(b.calls), len(b.spent))
		}
		calls, tokens := b.Remaining()
		if calls != math.MaxInt || tokens != math.MaxInt64 {
			t.Fatalf("Remaining() = (%d, %d), want both unlimited", calls, tokens)
		}
	})
}

// TestBudgetNilAllowsEverything pins the nil-safety contract every optional
// dependency in this codebase follows: an unconfigured (nil) *Budget must
// allow everything, never a hidden deny.
func TestBudgetNilAllowsEverything(t *testing.T) {
	var b *Budget

	for i := 0; i < 10; i++ {
		allowed, reason := b.Allow(0)
		if !allowed {
			t.Fatalf("call %d: nil *Budget must allow everything", i)
		}
		if reason != DenyNone {
			t.Fatalf("call %d: reason = %q, want %q (DenyNone) when allowed", i, reason, DenyNone)
		}
	}
	// Record and Remaining must not panic on a nil receiver either.
	b.Record(providers.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	calls, tokens := b.Remaining()
	if calls <= 0 || tokens <= 0 {
		t.Fatalf("Remaining on nil budget = (%d, %d), want both unlimited (> 0)", calls, tokens)
	}
}

// TestBudgetConcurrentAllowRecord exercises Allow and Record from many real
// goroutines at once. Run with -race: the point is that neither method
// corrupts shared state under concurrent access, not any particular outcome.
func TestBudgetConcurrentAllowRecord(t *testing.T) {
	b := NewBudget(1000, 1_000_000, time.Hour, nil)

	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allowed, _ := b.Allow(0); allowed {
				b.Record(providers.Usage{InputTokens: 10, OutputTokens: 5})
			}
		}()
	}
	wg.Wait()

	calls, tokens := b.Remaining()
	if calls < 0 || tokens < 0 {
		t.Fatalf("Remaining after concurrent use = (%d, %d), want both >= 0", calls, tokens)
	}
}

// TestDefaultTokenCeilingCanBind pins the claim DefaultChatTokensPerHour's own
// doc comment makes — that a sustained-max-output runaway trips the token
// ceiling before it exhausts the call ceiling, "which is the whole point of
// having two independent limits". It was arithmetically false at the shipped
// defaults: this branch's prompt ceiling caps one call at maxChatCallTokens, so
// DefaultChatCallsPerHour worst-case calls could never reach a 200,000-token
// ceiling and DenyTokens was unreachable at the defaults.
//
// Driven through the real Budget rather than asserted on the constants alone:
// the arithmetic is only interesting because of which DenyReason it produces.
func TestDefaultTokenCeilingCanBind(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBudget(DefaultChatCallsPerHour, DefaultChatTokensPerHour, time.Hour, silentLog())
	b.now = func() time.Time { return now }

	// The runaway: every call as expensive as this layer's caps allow.
	worst := providers.Usage{
		InputTokens:  maxChatCallTokens - DefaultChatMaxOutputTokens,
		OutputTokens: DefaultChatMaxOutputTokens,
	}
	for i := 1; i <= DefaultChatCallsPerHour; i++ {
		allowed, reason := b.Allow(0)
		if !allowed {
			if reason != DenyTokens {
				t.Fatalf("call %d denied by %q, want %q — the token ceiling must be what binds first", i, reason, DenyTokens)
			}
			return
		}
		b.Record(worst)
	}
	t.Fatalf("a sustained worst-case runaway spent all %d hourly calls (%d tokens) without ever reaching the %d-token ceiling — only one of the two advertised limits is live",
		DefaultChatCallsPerHour, int64(DefaultChatCallsPerHour)*worst.Total(), DefaultChatTokensPerHour)
}

// TestBudgetConcurrentAllowChargesInFlightSpend is the concurrency half of the
// claim above, and the half the serial test cannot make: the chat layer runs 16
// concurrent mention handlers per transport (internal/server's
// maxConcurrentMentions, internal/app's matrixMentionConcurrency), and
// Responder.write's per-root lock sits AFTER Chat.Answer, so it does not
// serialise the model call.
//
// Every goroutine here calls Allow and NONE of them calls Record — the state a
// burst really reaches, where every admitted call is still waiting on a
// provider. If Allow charged nothing until Record ran, spend in flight would be
// invisible to the check, every one of the hourly calls would pass the token
// test, and the true bound would be DefaultChatCallsPerHour x maxChatCallTokens
// (164,910 tokens) against a 109,940 ceiling — a runaway stopped by COUNT, the
// exact inverse of the ordering DefaultChatTokensPerHour is derived to have.
//
// Deterministic rather than timing-dependent: grants continue while charged
// spend is below the ceiling and each grant charges the same estimate, so the
// number that fit is arithmetic and holds under every interleaving. The
// WaitGroup barrier only maximises the overlap; nothing is asserted about it.
func TestBudgetConcurrentAllowChargesInFlightSpend(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBudget(DefaultChatCallsPerHour, DefaultChatTokensPerHour, time.Hour, silentLog())
	b.now = func() time.Time { return now }

	// The runaway: every call as expensive as this layer's caps allow.
	const worst = int64(maxChatCallTokens)
	// Grants continue while charged spend is < the ceiling, so the count that
	// fits is ceil(ceiling / worst) — 20 of the 30 hourly calls, the two thirds
	// DefaultChatTokensPerHour is derived to bind at.
	wantAllowed := int((DefaultChatTokensPerHour + worst - 1) / worst)
	if wantAllowed >= DefaultChatCallsPerHour {
		t.Fatalf("setup: %d worst-case calls fit the token ceiling but only %d fit the call ceiling — cost cannot bind before count",
			wantAllowed, DefaultChatCallsPerHour)
	}

	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		allowed int
		reasons = map[DenyReason]int{}
	)
	start.Add(1)
	for i := 0; i < DefaultChatCallsPerHour; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait() // release every goroutine into Allow at once
			ok, reason := b.Allow(worst)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				allowed++
				return
			}
			reasons[reason]++
		}()
	}
	start.Done()
	done.Wait()

	if allowed != wantAllowed {
		t.Fatalf("%d of %d concurrent worst-case calls were admitted, want %d — a call still waiting on its provider must already be charged, or the token ceiling only ever sees spend that has finished",
			allowed, DefaultChatCallsPerHour, wantAllowed)
	}
	if got := reasons[DenyTokens]; got != DefaultChatCallsPerHour-wantAllowed {
		t.Fatalf("%d calls were denied by %q, want %d — the runaway must be stopped by cost", got, DenyTokens, DefaultChatCallsPerHour-wantAllowed)
	}
	if got := reasons[DenyCalls]; got != 0 {
		t.Fatalf("%d calls were denied by %q, want 0 — the call ceiling still had headroom; cost must be what bound", got, DenyCalls)
	}
	calls, tokens := b.Remaining()
	if tokens != 0 {
		t.Fatalf("Remaining tokens = %d, want 0 — the ceiling is spent by the calls in flight", tokens)
	}
	if calls != DefaultChatCallsPerHour-wantAllowed {
		t.Fatalf("Remaining calls = %d, want %d — the call ceiling must be untouched by the denials",
			calls, DefaultChatCallsPerHour-wantAllowed)
	}
}

// TestBudgetRecordReconcilesTheReservation pins the other half of the
// reservation: Allow charges an ESTIMATE so a concurrent Allow can see spend
// that has not landed yet, and Record REPLACES that estimate with what the call
// really cost. Adding the two instead of replacing would bill every call twice
// — once pessimistically, once for real — and a budget that over-charges by 3x
// refuses traffic it was sized to allow.
func TestBudgetRecordReconcilesTheReservation(t *testing.T) {
	const ceiling int64 = 100_000
	const estimate = int64(maxChatCallTokens) // pessimistic: every byte cap maxed at once
	const actualIn, actualOut = 1500, 500     // what the call really cost

	now := time.Unix(0, 0)
	b := NewBudget(100, ceiling, time.Hour, silentLog())
	b.now = func() time.Time { return now }

	if allowed, reason := b.Allow(estimate); !allowed {
		t.Fatalf("a fresh budget must admit the first call, denied by %q", reason)
	}
	if _, tokens := b.Remaining(); tokens != ceiling-estimate {
		t.Fatalf("Remaining tokens with the call still in flight = %d, want %d — Allow must charge its estimate immediately, or a concurrent Allow cannot see it",
			tokens, ceiling-estimate)
	}

	b.Record(providers.Usage{InputTokens: actualIn, OutputTokens: actualOut})

	if _, tokens := b.Remaining(); tokens != ceiling-(actualIn+actualOut) {
		t.Fatalf("Remaining tokens after Record = %d, want %d — Record must REPLACE the reservation with the usage the provider reported, not add to it",
			tokens, ceiling-(actualIn+actualOut))
	}
}

// TestBudgetReconciledOrdinaryCallsAreNotOverDenied is the trade-off the
// reservation has to survive: charging a pessimistic estimate up front must not
// deny the traffic DefaultChatCallsPerHour was sized for. A whole hour of
// ordinary questions — each admitted against the worst case, each settling to
// what a question and a two-sentence reply really cost — must run its full call
// budget without the token ceiling ever refusing one.
func TestBudgetReconciledOrdinaryCallsAreNotOverDenied(t *testing.T) {
	const typicalIn, typicalOut = 1500, 500
	const typical int64 = typicalIn + typicalOut

	now := time.Unix(0, 0)
	b := NewBudget(DefaultChatCallsPerHour, DefaultChatTokensPerHour, time.Hour, silentLog())
	b.now = func() time.Time { return now }

	for i := 1; i <= DefaultChatCallsPerHour; i++ {
		allowed, reason := b.Allow(int64(maxChatCallTokens))
		if !allowed {
			t.Fatalf("call %d of a %d-call conversation was denied by %q — an estimate that is never settled back to the real cost turns the ceiling into %d x maxChatCallTokens",
				i, DefaultChatCallsPerHour, reason, DefaultChatCallsPerHour)
		}
		b.Record(providers.Usage{InputTokens: typicalIn, OutputTokens: typicalOut})
	}

	_, tokens := b.Remaining()
	if want := DefaultChatTokensPerHour - DefaultChatCallsPerHour*typical; tokens != want {
		t.Fatalf("Remaining tokens after %d ordinary calls = %d, want %d — the window must hold what the calls really cost, not what they reserved",
			DefaultChatCallsPerHour, tokens, want)
	}
}

// TestBudgetUnreconciledReservationAgesOut answers the one question a
// reservation raises: what happens to a call that is admitted and never
// reaches Record — a panic between the two, or a goroutine that dies with its
// context. Nothing rolls it back, and nothing needs to: a reservation is an
// entry in the same sliding window as a recorded spend, timestamped when it was
// taken, so it expires exactly like one. The cost of an abandoned call is one
// window of held budget, not a permanent leak — and that is a property of the
// timestamp, which the second subtest is what pins.
func TestBudgetUnreconciledReservationAgesOut(t *testing.T) {
	t.Run("a reservation nothing ever records slides out with the window", func(t *testing.T) {
		now := time.Unix(0, 0)
		b := NewBudget(0, 10_000, time.Minute, silentLog())
		b.now = func() time.Time { return now }

		// Two calls admitted, 12,000 tokens reserved, neither ever returns.
		for i := 1; i <= 2; i++ {
			if allowed, reason := b.Allow(6000); !allowed {
				t.Fatalf("reservation %d was denied by %q; the ceiling still had room", i, reason)
			}
		}
		allowed, reason := b.Allow(6000)
		if allowed {
			t.Fatal("a third call must be denied: 12,000 tokens are reserved against a 10,000 ceiling")
		}
		if reason != DenyTokens {
			t.Fatalf("reason = %q, want %q — calls are unlimited here", reason, DenyTokens)
		}

		now = now.Add(2 * time.Minute)

		if _, tokens := b.Remaining(); tokens != 10_000 {
			t.Fatalf("Remaining tokens a window after two abandoned reservations = %d, want the full 10000 — an unreconciled reservation must age out like any other entry, or one panicking call holds budget for the life of the process",
				tokens)
		}
		if allowed, reason := b.Allow(6000); !allowed {
			t.Fatalf("after the window slides clear the budget must admit again, denied by %q", reason)
		}
	})

	t.Run("a later Record settles its own reservation, not the abandoned one", func(t *testing.T) {
		start := time.Unix(0, 0)
		now := start
		b := NewBudget(0, 100_000, time.Hour, silentLog())
		b.now = func() time.Time { return now }

		// Call A is admitted and never returns.
		if allowed, reason := b.Allow(5000); !allowed {
			t.Fatalf("call A denied by %q on a fresh budget", reason)
		}
		// Call B, half an hour later, returns and reports 1,000 tokens.
		now = start.Add(30 * time.Minute)
		if allowed, reason := b.Allow(5000); !allowed {
			t.Fatalf("call B denied by %q; 95,000 tokens were free", reason)
		}
		b.Record(providers.Usage{InputTokens: 1000})

		if _, tokens := b.Remaining(); tokens != 100_000-6000 {
			t.Fatalf("Remaining tokens = %d, want %d — A's reservation (5000) plus B's real cost (1000)", tokens, 100_000-6000)
		}

		// Past A's reservation, not past B's entry.
		now = start.Add(61 * time.Minute)

		if _, tokens := b.Remaining(); tokens != 100_000-1000 {
			t.Fatalf("Remaining tokens = %d, want %d — Record must settle the reservation belonging to the call that returned, so the abandoned one keeps its own timestamp and expires; settling the oldest instead hands the abandoned call a fresh one every time and it never ages out",
				tokens, 100_000-1000)
		}
	})
}

// TestDefaultTokenCeilingLeavesRoomForARealConversation is the other side of
// the same trade-off: the ceiling has to bind on a runaway without binding on
// the traffic DefaultChatCallsPerHour was sized for. A very chatty incident —
// every one of the hourly calls, each carrying a typical question and a typical
// reply — must still fit.
func TestDefaultTokenCeilingLeavesRoomForARealConversation(t *testing.T) {
	// A typical exchange: identity and evidence framing, a question, a reply of
	// a sentence or two. Deliberately generous against what renderContext
	// actually assembles for one.
	const typicalCallTokens = 1500
	if spent := int64(DefaultChatCallsPerHour) * typicalCallTokens; spent >= DefaultChatTokensPerHour {
		t.Fatalf("%d typical calls spend %d tokens against a %d-token ceiling — the default would deny a legitimate busy thread before its call ceiling",
			DefaultChatCallsPerHour, spent, DefaultChatTokensPerHour)
	}
}
