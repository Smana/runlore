// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/factory"
	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/kbresolve"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// factoryHTTPTimeout bounds each call to the identity provider and the factory API.
const factoryHTTPTimeout = 15 * time.Second

// BuildFactory builds the agent-factory client and its ledger from factory.*; nil, nil when
// factory.url is empty. The ledger is factory.jsonl beside the outcome ledger (a no-op ledger when
// outcome.ledger_path is unset, which is warned about). An empty credential env var leaves the
// client disabled and logs one warning naming the variable, never a value.
func BuildFactory(cfg *config.Config, log *slog.Logger) (*factory.Ledger, *factory.Client, error) {
	f := cfg.Factory
	if !f.Enabled() {
		return nil, nil, nil
	}
	hc, err := factoryHTTPClient(f)
	if err != nil {
		return nil, nil, err
	}
	path := ""
	if cfg.Outcome.LedgerPath != "" {
		path = filepath.Join(filepath.Dir(cfg.Outcome.LedgerPath), "factory.jsonl")
	} else {
		log.Warn("factory.url is set but outcome.ledger_path is not: the factory ledger is off, so webhooks cannot be matched to incidents")
	}
	ledger, err := factory.OpenLedger(path, time.Now)
	if err != nil {
		return nil, nil, err
	}
	client := factory.NewClient(f, os.Getenv, hc, time.Now)
	if !client.Enabled() {
		var empty []string
		for _, name := range []string{f.ClientIDEnv, f.ClientSecretEnv, f.ProjectIDEnv} {
			if os.Getenv(name) == "" {
				empty = append(empty, name)
			}
		}
		log.Warn("factory.url is set but a credential env var is empty: the factory client is off",
			"empty_env", strings.Join(empty, ","))
	}
	return ledger, client, nil
}

// factoryHTTPClient is the client of the factory API and of its token endpoint: httpx.SecureClient,
// and with factory.ca_file set, one that also trusts that PEM, added to the system roots. No other
// client of the process sees the file. An unreadable or invalid file is an error naming the key.
func factoryHTTPClient(f config.Factory) (*http.Client, error) {
	if f.CAFile == "" {
		return httpx.SecureClient(factoryHTTPTimeout), nil
	}
	roots, err := httpx.RootsWithCAFile(nil, f.CAFile)
	if err != nil {
		return nil, fmt.Errorf("factory.ca_file: %w", err)
	}
	return httpx.SecureClientWithRoots(factoryHTTPTimeout, roots), nil
}

// BuildFactoryReceiver builds the POST /factory/webhook handler; nil when the factory is off (a nil
// ledger). The signing keys are read from the environment at each request, so a rotation needs no
// restart; an empty or malformed value yields no keys, and the receiver then refuses everything.
// notifier (nil-safe) supplies the Slack replier.
func BuildFactoryReceiver(cfg *config.Config, ledger *factory.Ledger, notifier *notify.Multi, log *slog.Logger) *factory.Receiver {
	if !cfg.Factory.Enabled() || ledger == nil {
		return nil
	}
	secretEnv := cfg.Factory.Webhook.SecretEnv
	if _, err := factory.ReadKeys(os.Getenv(secretEnv)); err != nil {
		log.Warn("the factory webhook signing key is missing or malformed: every webhook will be refused until it is set",
			"env", secretEnv, "err", err)
	}
	rc := &factory.Receiver{
		Ledger: ledger,
		Now:    time.Now,
		Log:    log,
		Keys: func() [][]byte {
			keys, err := factory.ReadKeys(os.Getenv(secretEnv))
			if err != nil {
				return nil
			}
			return keys
		},
	}
	if notifier != nil {
		if r, ok := notifier.ThreadRepliers()["slack"]; ok {
			rc.Replier = r
		}
	}
	return rc
}

// BuildFactoryVerifier builds the leader-only soak check; nil unless the factory client is on and
// factory.verify.alertmanager_url is set. Without a model (nil deps) it still takes the verdict
// from Alertmanager and sends fixed evidence, rather than not verifying at all. notifier (nil-safe)
// supplies the Slack replier.
func BuildFactoryVerifier(cfg *config.Config, deps *Deps, ledger *factory.Ledger, client *factory.Client,
	notifier *notify.Multi, metrics *telemetry.Metrics, log *slog.Logger) *factory.Verifier {
	f := cfg.Factory
	if !f.Enabled() || ledger == nil || !client.Enabled() || f.Verify.AlertmanagerURL == "" {
		return nil
	}
	hc := httpx.SecureClient(factoryHTTPTimeout)
	v := &factory.Verifier{
		Ledger: ledger, Client: client, Soak: f.Soak.Std(), Now: time.Now, Log: log,
		Firing: func(ctx context.Context, alert, ns string) (bool, error) {
			return factory.Firing(ctx, hc, f.Verify.AlertmanagerURL, alert, ns)
		},
	}
	if deps != nil {
		run := reinvestigateRun(cfg, deps, metrics, log, f.Verify.MaxSteps)
		v.Reinvestigate = func(ctx context.Context, r factory.Record) (factory.Findings, error) {
			inv, err := run(ctx, verifyRequest(r))
			if err != nil {
				return factory.Findings{}, err
			}
			f := factory.Findings{Text: notify.Format(inv)}
			if len(inv.RootCauses) > 0 && len(inv.RootCauses[0].Evidence) > 0 {
				f.Cause = inv.RootCauses[0].Evidence[0]
			}
			return f, nil
		}
	} else {
		log.Info("factory verification runs without a model: the verdict is Alertmanager's, the evidence a fixed line")
	}
	if notifier != nil {
		if r, ok := notifier.ThreadRepliers()["slack"]; ok {
			v.Replier = r
		}
	}
	log.Info("factory verification enabled", "soak", f.Soak.Std(), "max_steps", f.Verify.MaxSteps)
	return v
}

// verifyRequest is the short re-investigation of a merged fix, on the incident's own TriggerKey.
func verifyRequest(r factory.Record) investigate.Request {
	var w providers.Workload
	if kind, name, ok := strings.Cut(r.Workload, "/"); ok {
		w = providers.Workload{Kind: kind, Name: name, Namespace: r.Namespace}
	}
	return investigate.Request{
		Source:     investigate.SourceAlert,
		Title:      "Verify the fix for " + r.Alert,
		Workload:   w,
		Reason:     fmt.Sprintf("the fix merged in #%d should have resolved this alert; check whether it still fires and why", r.PR),
		TriggerKey: r.TriggerKey,
	}
}

// newFactoryNotifier is the notifier that files an incident with the factory.
func newFactoryNotifier(cfg *config.Config, ledger *factory.Ledger, client *factory.Client, log *slog.Logger) *factory.Notifier {
	return &factory.Notifier{Client: client, Ledger: ledger,
		Repository: cfg.Factory.Repository, MaxCostUSD: cfg.Factory.MaxCostUSD, Log: log}
}

// addFactoryNotifier puts the factory notifier into the delivery Multi, unless a Curation files
// instead: filing then happens at curation, ahead of delivery, and a second post would only repeat it.
func addFactoryNotifier(notifier *notify.Multi, cfg *config.Config, ledger *factory.Ledger, client *factory.Client,
	curation *factory.Curation, log *slog.Logger) {
	if notifier == nil || !client.Enabled() || curation != nil {
		return
	}
	notifier.Add(newFactoryNotifier(cfg, ledger, client, log))
}

// BuildFactoryCuration builds what replaces the curator when factory.curation.defer is on: it files
// the incident first, holds its entry, and drafts it once the fix has an outcome or the window ends.
// Nil unless defer is on, and nil with a warning when the KB's GitHub client cannot be built. The
// caller passes it to BuildInvestigator through WithFactoryCuration, which sets Inner; cat may be nil.
//
// Drafts and Resolve go through one audited curate.Guard over the same KB repository as Inner's forge,
// since a PR number names no repository. Promote is set under curate.sweeps.mode: apply only: the label
// is what the merge gate trusts, so no other mode lays it, or withdraws it, from a tick.
func BuildFactoryCuration(cfg *config.Config, ledger *factory.Ledger, client *factory.Client, outcomes *outcome.Ledger,
	cat *catalog.Catalog, aud audit.Auditor, log *slog.Logger) *factory.Curation {
	if !cfg.Factory.Curation.Defer || ledger == nil {
		return nil
	}
	gh := buildGitHubClient(cfg, log)
	if gh == nil {
		log.Warn("factory.curation.defer is on but the KB's GitHub client cannot be built: entries are drafted as without it",
			"kb_repo", cfg.Forge.KBRepo, "provider", forgeProviderName(cfg))
		return nil
	}
	dry := cfg.Curate.Sweeps.DryRun()
	guard := curate.Guard{Inner: gh, DryRun: dry, Audit: aud, Log: log}
	c := &factory.Curation{
		Filer:   newFactoryNotifier(cfg, ledger, client, log),
		Ledger:  ledger,
		Outcome: outcomes,
		Pulls:   gh,
		Drafts:  guard,
		Resolve: func(ctx context.Context, t kbresolve.Target, r providers.AppliedResolution) (kbresolve.Result, error) {
			return kbresolve.Resolve(ctx, guard, t, r)
		},
		FindEntry: func(fingerprint string) (string, bool) {
			if cat == nil {
				return "", false
			}
			e, ok := cat.FindFingerprint(fingerprint)
			return e.Path, ok
		},
		Window:     cfg.Factory.Curation.Window.Std(),
		Repository: cfg.Factory.Repository,
		DryRun:     dry,
		Log:        log,
	}
	if cfg.Curate.Sweeps.Mode == config.SweepApply {
		c.Promote = curate.Queue{Forge: guard, Evidence: outcomes, KBRepo: cfg.Forge.KBRepo, Log: log}.PromoteOne
	}
	return c
}
