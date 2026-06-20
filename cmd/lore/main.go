// Command lore is the RunLore CLI and in-cluster agent entrypoint.
//
// RunLore is a self-improving, GitOps-native SRE agent: it reacts to incidents,
// investigates by correlating "what changed" across the GitOps engine and the
// observability stack, and learns into an open knowledge catalog.
//
// This is a scaffold — subcommands are not yet implemented. See docs/design.md.
package main

import (
	"fmt"
	"os"
)

var version = "0.0.0-dev"

const usage = `lore — the RunLore SRE agent

Usage:
  lore investigate [--alert <name>] [--since <dur>]   investigate an alert/symptom (on-demand)
  lore serve                                          run the in-cluster agent (react to alerts/events)
  lore catalog sync                                   sync + index the knowledge catalog
  lore eval                                           replay past incidents, score root-cause identification
  lore version                                        print version

Scaffold: subcommands are not yet implemented. See docs/design.md.
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
	case "investigate", "serve", "catalog", "eval":
		fmt.Printf("lore %s: not yet implemented (scaffold). See docs/design.md\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
