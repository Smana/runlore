// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseSchedule(t *testing.T) {
	for _, tc := range []struct {
		in      string
		day     time.Weekday
		h, m    int
		wantErr bool
	}{
		{"Mon 08:00", time.Monday, 8, 0, false},
		{"Sun 23:59", time.Sunday, 23, 59, false},
		{"Fri 07:05", time.Friday, 7, 5, false},
		{"monday 8am", 0, 0, 0, true},
		{"mon 08:00", 0, 0, 0, true},
		{"Mon 24:00", 0, 0, 0, true},
		{"Mon 08:60", 0, 0, 0, true},
		{"Mon 8:00", 0, 0, 0, true},
		{"Mon  08:00", 0, 0, 0, true},
		{"", 0, 0, 0, true},
	} {
		d, h, m, err := ParseSchedule(tc.in)
		if (err != nil) != tc.wantErr || (err == nil && (d != tc.day || h != tc.h || m != tc.m)) {
			t.Errorf("%q: got %v %d:%d %v", tc.in, d, h, m, err)
		}
	}
}

func TestQueueIssueConfig(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("curate:\n  queue_issue:\n    schedule: Mon 08:00\n    slack: true\n"), &c); err != nil {
		t.Fatal(err)
	}
	if qi := c.Curate.QueueIssue; !qi.Enabled() || !qi.Slack || qi.Schedule != "Mon 08:00" {
		t.Fatalf("%+v", qi)
	}
	if (QueueIssue{}).Enabled() {
		t.Fatal("empty schedule means off")
	}
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "queue_issue") {
		t.Fatalf("a valid schedule passes: %v", err)
	}
	bad := Config{Curate: Curate{QueueIssue: QueueIssue{Schedule: "monday 8am"}}}
	err := bad.Validate()
	if err == nil || err.Error() != `curate.queue_issue.schedule must be "<Day> HH:MM" (UTC)` {
		t.Fatalf("got %v", err)
	}
}
