// SPDX-License-Identifier: Apache-2.0

// Package trigger ingests incidents (Alertmanager/VMAlert webhooks) and decides,
// per the configured policy, which ones start an investigation.
package trigger

import (
	"github.com/Smana/runlore/internal/config"
)

// MatchRequest reports whether the trigger policy passes for a normalized
// investigation request. It reads the request fields produced by the source
// adapter / investigate.FromFailureEvent.
//
// Note: the trigger package is imported by investigate (for Deduper), so
// trigger cannot import investigate without a cycle. Callers bridge the gap
// by extracting the relevant fields from investigate.Request:
//
//	trigger.MatchRequest(pol, r.Title, r.Severity, r.Environment, r.Workload.Namespace, r.Labels)
func MatchRequest(t config.IncidentTrigger, title, severity, environment, namespace string, labels map[string]string) bool {
	return t.MatchFields(title, severity, environment, namespace, labels)
}
