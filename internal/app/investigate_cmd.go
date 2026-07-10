// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
)

// RunInvestigate runs a single on-demand investigation and prints the findings.
func RunInvestigate(args []string) error {
	fs := flag.NewFlagSet("investigate", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	alert := fs.String("alert", "", "alert/symptom name to investigate")
	namespace := fs.String("namespace", "", "namespace of the affected workload")
	message := fs.String("message", "", "free-text symptom description")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *alert == "" && *message == "" {
		return fmt.Errorf("provide --alert and/or --message")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !ModelConfigured(cfg) {
		return fmt.Errorf("investigate requires a configured model (set config.model)")
	}
	// Progress logs go to stderr; the findings go to stdout.
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	ctx := context.Background()

	model, tools, recall, _ := BuildModelAndTools(ctx, cfg, GitOpsFromKube(cfg, log), nil, log)
	var result *providers.Investigation
	li := &investigate.LoopInvestigator{
		Model: model, VerifyModel: BuildVerifyModel(cfg), Tools: tools, Recall: recall, Actions: action.New(cfg.Actions), Log: log, Verify: true,
		ModelProvider: cfg.Model.Provider,
		Timeout:       cfg.Investigation.Timeout.Std(),
		OnComplete:    func(inv providers.Investigation) { result = &inv },
	}
	title := *alert
	if title == "" {
		title = "on-demand investigation"
	}
	req := investigate.Request{
		Source: investigate.SourceAlert, Title: title, Message: *message,
		Workload: providers.Workload{Namespace: *namespace},
	}
	if err := li.Investigate(ctx, req); err != nil {
		return err
	}
	if result == nil {
		return fmt.Errorf("investigation produced no findings")
	}
	fmt.Println(notify.Format(*result))
	return nil
}
