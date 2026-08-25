// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"strings"
	"testing"
	"time"
)

// TestSilenceAckCarriesTheWarning pins the four facts that must survive any
// rewording of the ack: it must read as a warning, name the severity that still
// breaks through, name the escape hatch, and state the chosen window.
func TestSilenceAckCarriesTheWarning(t *testing.T) {
	got := SilenceAck("bob", 4*time.Hour, time.Date(2026, 8, 25, 18, 42, 0, 0, time.UTC))
	// Substrings, not the whole string: the wording should be tunable without
	// breaking the test, but these four facts must always survive a rewrite.
	for _, want := range []string{"will NOT investigate", "CRITICAL", "👎", "4h"} {
		if !strings.Contains(got, want) {
			t.Errorf("SilenceAck() = %q, missing %q", got, want)
		}
	}
}
