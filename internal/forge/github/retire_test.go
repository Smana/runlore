// SPDX-License-Identifier: Apache-2.0

package github

import (
	"strings"
	"testing"
)

func TestSetStatusRetired(t *testing.T) {
	t.Run("inserts status after the opening fence", func(t *testing.T) {
		in := "---\ntype: Incident\ntitle: t\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		want := "---\nstatus: retired\ntype: Incident\ntitle: t\n---\nbody\n"
		if string(out) != want {
			t.Errorf("got:\n%s\nwant:\n%s", out, want)
		}
	})
	t.Run("replaces an existing status line in place", func(t *testing.T) {
		in := "---\ntype: Incident\nstatus: active\ntitle: t\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if !strings.Contains(string(out), "\nstatus: retired\n") || strings.Contains(string(out), "active") {
			t.Errorf("status not replaced in place:\n%s", out)
		}
	})
	t.Run("already retired reports already, content unchanged", func(t *testing.T) {
		in := "---\nstatus: retired\ntype: Incident\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || !already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if string(out) != in {
			t.Errorf("content changed on already-retired entry")
		}
	})
	t.Run("no frontmatter is an error, never a blind write", func(t *testing.T) {
		if _, _, err := setStatusRetired([]byte("just a body\n")); err == nil {
			t.Fatal("want error on missing frontmatter")
		}
	})
	t.Run("status in the BODY does not fool the fence scan", func(t *testing.T) {
		in := "---\ntype: Incident\n---\nstatus: retired appears in prose\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if !strings.HasPrefix(string(out), "---\nstatus: retired\n") {
			t.Errorf("status not inserted into frontmatter:\n%s", out)
		}
	})
}
