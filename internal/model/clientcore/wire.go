// SPDX-License-Identifier: Apache-2.0

package clientcore

import (
	"encoding/json"
	"strings"
)

// RawObject ensures a tool call's argument payload is a JSON object ("" → {})
// before it goes on the wire.
func RawObject(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}
