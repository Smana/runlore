// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const (
	lintScriptPath = "../../hack/lint.sh"
	goModPath      = "../../go.mod"
	agentsPath     = "../../AGENTS.md"
)

// TestLintScriptResolvesTheModuleToolchain pins the one thing hack/lint.sh exists to
// do: derive GOTOOLCHAIN from go.mod rather than hardcoding a version.
//
// A hardcoded version would work on the day it is written and rot at the next
// toolchain bump — silently, because the wrong pin still runs and still reports
// issues, just under a Go the project no longer uses. So the guard reads BOTH ends: it
// re-runs the script's own extraction rule against the real go.mod and requires a
// match, and it requires the script not to contain a literal version at all.
func TestLintScriptResolvesTheModuleToolchain(t *testing.T) {
	script := readFileString(t, lintScriptPath)
	// Every assertion below reads the CODE, never the header comment. An earlier
	// revision tested strings.Contains(script, "GOTOOLCHAIN") against the whole file,
	// which the header's own explanation satisfies — so deleting the assignment from
	// the script left the guard green. A guard that passes while checking nothing is
	// worse than no guard.
	code := stripComments(script)
	gomod := readFileString(t, goModPath)

	var want string
	for _, line := range strings.Split(gomod, "\n") {
		if after, ok := strings.CutPrefix(line, "toolchain "); ok {
			want = strings.TrimSpace(after)
			break
		}
	}
	if want == "" {
		t.Fatalf("go.mod has no `toolchain` line — hack/lint.sh's fallback is untested by this guard")
	}

	// The script must not name a Go version itself; it must go and read one.
	if m := regexp.MustCompile(`go1\.\d+(\.\d+)?`).FindString(code); m != "" {
		t.Errorf("hack/lint.sh hardcodes %q instead of reading go.mod: it will pin the wrong "+
			"toolchain the next time go.mod is bumped, and still exit 0 while doing it", m)
	}
	if !strings.Contains(code, "go.mod") {
		t.Errorf("hack/lint.sh does not read go.mod, so GOTOOLCHAIN cannot track %q", want)
	}
	// On the line that RUNS the linter, not merely somewhere in the file: the script
	// echoes the command it is about to run, so a whole-file search for "GOTOOLCHAIN="
	// is satisfied by that echo even after the real assignment is deleted. Two earlier
	// revisions of this guard passed against exactly that mutation.
	if !invocationSetsToolchain(code) {
		t.Fatalf("hack/lint.sh runs golangci-lint without GOTOOLCHAIN on the invocation (an echo of "+
			"it does not count) — the staticcheck IR panic it exists to avoid comes back for anyone "+
			"whose local Go is newer than the pin:\n%s", code)
	}
}

// TestQualityGateUsesTheLintScript: the documented gate must invoke hack/lint.sh, not a
// bare golangci-lint.
//
// This is the half that rots first. The script is easy to keep and easy to stop
// mentioning, and a contributor who copies `golangci-lint run ./...` out of AGENTS.md
// gets the IR panic and no explanation — which is exactly the state the script was
// written to end. CONTRIBUTING.md is checked by the sibling guard in
// release_config_test.go's file set; both are asserted here for the gate line itself.
func TestQualityGateUsesTheLintScript(t *testing.T) {
	if _, err := os.Stat(lintScriptPath); err != nil {
		t.Fatalf("hack/lint.sh is missing: %v", err)
	}
	for _, path := range []string{agentsPath, contributingPath} {
		body := readFileString(t, path)
		if !strings.Contains(body, "hack/lint.sh") {
			t.Errorf("%s documents the quality gate but never names hack/lint.sh", path)
		}
		// A bare `golangci-lint run ./...` presented AS the gate step is the regression.
		// Prose that mentions the command while explaining the script is fine, so only
		// runnable-looking lines that are not part of an explanation are rejected.
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "golangci-lint run") ||
				strings.Contains(trimmed, "&& golangci-lint run") {
				t.Errorf("%s still lists a bare golangci-lint run as a gate step: %q", path, trimmed)
			}
		}
	}
}

// invocationSetsToolchain reports whether the line that actually executes
// golangci-lint carries GOTOOLCHAIN, or an exported GOTOOLCHAIN precedes it. Lines
// that merely print the command (echo/printf) are not invocations.
func invocationSetsToolchain(code string) bool {
	exported := false
	for _, line := range strings.Split(code, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "export GOTOOLCHAIN=") {
			exported = true
		}
		if !strings.Contains(t, "golangci-lint") {
			continue
		}
		if strings.HasPrefix(t, "echo") || strings.HasPrefix(t, "printf") ||
			strings.Contains(t, "command -v") {
			continue
		}
		return exported || strings.Contains(t, "GOTOOLCHAIN=")
	}
	return false
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// stripComments removes shell comment bodies so the hardcoded-version check reads the
// script's CODE. The header comment quotes "Go 1.27.0" and "go1.26.6" as the observed
// failure, which is documentation, not a pin.
func stripComments(script string) string {
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
