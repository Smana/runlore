// Command lore is the RunLore CLI and in-cluster agent entrypoint.
//
// RunLore is a self-improving, GitOps-native SRE agent: it reacts to incidents,
// investigates by correlating "what changed" across the GitOps engine and the
// observability stack, and learns into an open knowledge catalog.
//
// See docs/design.md.
package main

import (
	"fmt"
	"os"

	"github.com/Smana/runlore/internal/app"
)

// version is injected at build time via -ldflags "-X main.version=…" (see
// Dockerfile). It must stay in package main for the linker to set it.
var version = "0.0.0-dev"

const usage = `lore — the RunLore SRE agent

Usage:
  lore investigate --alert <name> [--namespace <ns>] [--message <text>]   investigate on-demand, print findings
  lore serve [--config <path>] [--addr <addr>]        run the in-cluster agent (react to incidents)
  lore catalog sync [--config <path>]                 clone/pull + index the knowledge catalog
  lore eval [--config <path>] [--cases <dir>]         replay recorded cases, score RCA identification
  lore eval --live [--scenarios <dir>] [--n 3]        live-fire on the cluster: grade coverage + RCA
  lore eval --compare <spec.yaml> [--n 3]             benchmark several models over the replay suite
  lore curate [--config <path>]                       groom the KB backlog (dedup open PRs)
  lore mcp [--config <path>]                          serve GitOps what-changed over MCP (stdio; for HolmesGPT et al.)
  lore audit verify --path <audit.jsonl>              re-walk the action audit log; report the first broken link
  lore version                                        print version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Printf("lore %s\n", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	case "serve":
		if err := app.RunServe(version, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	case "eval":
		if err := app.RunEval(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "eval:", err)
			os.Exit(1)
		}
	case "investigate":
		if err := app.RunInvestigate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "investigate:", err)
			os.Exit(1)
		}
	case "catalog":
		if err := app.RunCatalog(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "catalog:", err)
			os.Exit(1)
		}
	case "curate":
		if err := app.RunCurate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "curate:", err)
			os.Exit(1)
		}
	case "mcp":
		if err := app.RunMCP(version, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "mcp:", err)
			os.Exit(1)
		}
	case "audit":
		if err := runAudit(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "audit:", err)
			os.Exit(1)
		}
	case "validate-kb":
		if err := runValidateKB(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "validate-kb:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
