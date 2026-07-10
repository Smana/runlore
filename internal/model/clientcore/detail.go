// SPDX-License-Identifier: Apache-2.0

package clientcore

import (
	"fmt"
	"strings"
)

// maxDetailLen truncates the sanitized upstream message so an
// attacker-controlled error body can't bloat logs.
const maxDetailLen = 300

// Detail formats a provider error's structured kind/message pair as a
// sanitized ": kind: message" error suffix. Control characters are collapsed
// to spaces (log-injection defense) and the message is truncated, so a 4xx
// cause is diagnosable without echoing arbitrary upstream content into an
// Error-level log. Each provider parses its own error-body JSON shape and
// passes the extracted fields here.
func Detail(kind, message string) string {
	msg := sanitizeLine(message)
	if len(msg) > maxDetailLen {
		msg = msg[:maxDetailLen] + "…"
	}
	return fmt.Sprintf(": %s: %s", sanitizeLine(kind), msg)
}

// sanitizeLine collapses control characters (including newlines) to spaces so
// an upstream-controlled string can't inject additional log lines.
func sanitizeLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s))
}
