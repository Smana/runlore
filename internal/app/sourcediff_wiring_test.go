// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func TestAppendSourceDiffTool(t *testing.T) {
	log := slog.Default()
	t.Run("unset config registers nothing", func(t *testing.T) {
		cfg := &config.Config{}
		if got := appendSourceDiffTool(cfg, nil, log); len(got) != 0 {
			t.Fatalf("tools = %d, want 0", len(got))
		}
	})
	t.Run("allowlist registers the tool", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.SourceRepos.Allow = []string{"github.com/acme/*"}
		got := appendSourceDiffTool(cfg, nil, log)
		if !toolNames(got)["source_diff"] {
			t.Fatalf("source_diff not registered; got %v", toolNames(got))
		}
	})
}
