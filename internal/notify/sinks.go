// SPDX-License-Identifier: Apache-2.0

package notify

import "github.com/Smana/runlore/internal/providers"

// Sinks fans a ThreadSink registration out to several sinks (the thread registry, the agent
// factory's ledger). Nil entries are skipped.
type Sinks []ThreadSink

// Register implements ThreadSink.
func (s Sinks) Register(transport, root, channel string, inv providers.Investigation) {
	for _, k := range s {
		if k != nil {
			k.Register(transport, root, channel, inv)
		}
	}
}

var _ ThreadSink = Sinks(nil)
