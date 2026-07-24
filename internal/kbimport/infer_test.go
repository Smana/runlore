// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"reflect"
	"strings"
	"testing"
)

func TestInfer(t *testing.T) {
	cases := []struct {
		name, source, data                     string
		wantType, wantTitle, wantDesc, wantRes string
		wantTags                               []string
		wantDest, wantTimestamp                string
	}{
		{
			name:     "bare runbook: title from H1, description from first paragraph, Playbook",
			source:   "runbooks/redis-failover.md",
			data:     "# Redis failover\n\nHow to fail over the redis primary.\n\n## Steps\n\n- step one\n",
			wantType: "Playbook", wantTitle: "Redis failover",
			wantDesc: "How to fail over the redis primary.",
			wantTags: []string{"imported", "playbook"},
			wantDest: "playbooks/redis-failover.md",
		},
		{
			name:     "no heading: title humanized from filename",
			source:   "notes/pg_vacuum-tuning.md",
			data:     "Tune autovacuum before it falls behind.\n",
			wantType: "Playbook", wantTitle: "pg vacuum tuning",
			wantDesc: "Tune autovacuum before it falls behind.",
			wantTags: []string{"imported", "playbook"},
			wantDest: "playbooks/pg-vacuum-tuning.md",
		},
		{
			name:   "postmortem with OKF sections + resource becomes Incident, date preserved",
			source: "postmortems/2024-03-payments.md",
			data: "---\ntitle: Payments API outage\ndate: 2024-03-14\ntags: [payments]\nresource: payments/api\ntype: postmortem\n---\n" +
				"## Symptom\n\n5xx spike\n\n## Cause\n\nbad deploy\n\n## Resolution\n\nrollback\n",
			wantType: "Incident", wantTitle: "Payments API outage",
			wantDesc: "5xx spike", wantRes: "payments/api",
			wantTags:      []string{"imported", "incident", "payments"},
			wantDest:      "incidents/payments-api-outage.md",
			wantTimestamp: "2024-03-14",
		},
		{
			name:     "alert names in headings/alert lines become tags",
			source:   "runbooks/oom.md",
			data:     "# KubeContainerOOMKilled\n\nAlert: fires alongside KubePodCrashLooping sometimes.\n",
			wantType: "Playbook", wantTitle: "KubeContainerOOMKilled",
			wantDesc: "Alert: fires alongside KubePodCrashLooping sometimes.",
			wantTags: []string{"imported", "playbook", "kubecontaineroomkilled", "kubepodcrashlooping"},
			wantDest: "playbooks/kubecontaineroomkilled.md",
		},
		{
			name:     "valid existing type passes through untouched",
			source:   "concepts/slo.md",
			data:     "---\ntype: Concept\ntitle: SLO policy\ndescription: how we set SLOs\n---\nbody\n",
			wantType: "Concept", wantTitle: "SLO policy", wantDesc: "how we set SLOs",
			wantTags: []string{"imported", "concept"},
			wantDest: "concepts/slo-policy.md",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Infer([]byte(c.data), c.source)
			if r.Entry.Type != c.wantType {
				t.Errorf("type = %q, want %q", r.Entry.Type, c.wantType)
			}
			if r.Entry.Title != c.wantTitle {
				t.Errorf("title = %q, want %q", r.Entry.Title, c.wantTitle)
			}
			if r.Entry.Description != c.wantDesc {
				t.Errorf("description = %q, want %q", r.Entry.Description, c.wantDesc)
			}
			if r.Entry.Resource != c.wantRes {
				t.Errorf("resource = %q, want %q", r.Entry.Resource, c.wantRes)
			}
			if !reflect.DeepEqual(r.Entry.Tags, c.wantTags) {
				t.Errorf("tags = %v, want %v", r.Entry.Tags, c.wantTags)
			}
			if r.DestPath != c.wantDest {
				t.Errorf("dest = %q, want %q", r.DestPath, c.wantDest)
			}
			if r.Meta.Timestamp != c.wantTimestamp {
				t.Errorf("timestamp = %q, want %q", r.Meta.Timestamp, c.wantTimestamp)
			}
			if r.Source != c.source {
				t.Errorf("source = %q, want %q", r.Source, c.source)
			}
		})
	}
}

func TestInferLongTitleCapped(t *testing.T) {
	r := Infer([]byte("# "+strings.Repeat("verylongword ", 30)+"\n\nbody\n"), "x.md")
	if len(r.Entry.Title) > 120 {
		t.Fatalf("title must satisfy the 120-byte merge gate, got %d bytes", len(r.Entry.Title))
	}
}

func TestInferUnparseableDateDropsWithWarning(t *testing.T) {
	r := Infer([]byte("---\ntitle: t\ndate: last tuesday\n---\nbody\n"), "x.md")
	if r.Meta.Timestamp != "" {
		t.Fatalf("unparseable date must be dropped, got %q", r.Meta.Timestamp)
	}
	if len(r.Warnings) == 0 {
		t.Fatal("dropping the date must warn")
	}
}
