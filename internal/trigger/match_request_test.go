// SPDX-License-Identifier: Apache-2.0

package trigger_test

import (
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/trigger"
)

func TestMatchRequestSeverityAndNamespace(t *testing.T) {
	pol := config.IncidentTrigger{}
	pol.Match.Severity = []string{"critical"}
	pol.Match.Namespaces = []string{"prod-*"}

	yes := investigate.Request{Severity: "critical", Workload: providers.Workload{Namespace: "prod-web"}}
	no := investigate.Request{Severity: "warning", Workload: providers.Workload{Namespace: "prod-web"}}

	if !trigger.MatchRequest(pol, yes.Title, yes.Severity, yes.Environment, yes.Workload.Namespace, yes.Labels) {
		t.Fatal("expected match")
	}
	if trigger.MatchRequest(pol, no.Title, no.Severity, no.Environment, no.Workload.Namespace, no.Labels) {
		t.Fatal("expected no match (severity)")
	}
}
