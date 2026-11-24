// SPDX-License-Identifier: Apache-2.0

package config

import (
	"cmp"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/sourcerepo"
	"github.com/Smana/runlore/internal/thread"
)

// isPrivateHost reports whether host is a loopback / in-cluster / private endpoint
// where sending a key over plain http is acceptable. Pure — no DNS — so config
// validation stays deterministic and network-free. IP literals are classified by
// range; hostnames by well-known private forms. Anything else is treated as public.
func isPrivateHost(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" {
		return true
	}
	if !strings.Contains(h, ".") {
		return true // single-label in-cluster service name, e.g. "vllm"
	}
	for _, suf := range []string{".local", ".internal", ".svc", ".cluster.local"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

// gitlabProjectPathRE matches a GitLab project's namespace path: at least two
// slash-separated segments (a group and a project — GitLab allows nested
// groups, so "group/subgroup/project" is valid too), each segment starting
// with an alphanumeric or underscore and continuing with alphanumerics,
// underscores, dots, or dashes. It is a deliberately conservative subset of
// GitLab's real path grammar — good enough to catch the actual failure mode
// this check exists for (an owner/name-shaped GitHub path pasted into a
// GitLab config, a stray leading/trailing slash, or a bare project name with
// no namespace), not a full re-implementation of GitLab's validator.
var gitlabProjectPathRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*(/[a-zA-Z0-9_][a-zA-Z0-9_.-]*)+$`)

// validGitLabProjectPath reports whether s looks like a GitLab project path
// ("group/project", optionally nested). Used to fail config load closed on an
// obviously-wrong kb_repo rather than let every forge call 404 at runtime in a
// way that reads as a permissions problem.
func validGitLabProjectPath(s string) bool {
	return gitlabProjectPathRE.MatchString(s)
}

// validateForgeGitHost checks that RunLore can name — unambiguously — the ONE
// git host its forge credential may be sent to.
//
// Every clone RunLore makes (what_changed's GitOps source repos, source_diff's
// allowlisted repos) attaches the forge credential only when the clone URL's
// host matches that host, because a GitOps spec.source.repoURL is cluster state:
// in a shared cluster, whoever can create an Argo CD Application or a Flux
// GitRepository chooses where an unconfined credential would be POSTed.
//
// Confining is only safe when the host is KNOWN, and there is exactly one shape
// where it is not. GitHub Enterprise with subdomain isolation serves the API from
// api.HOSTNAME while git and web stay on HOSTNAME, so "https://api.ghe.example.com"
// is consistent with both "git lives at ghe.example.com" (isolation) and "git
// lives at api.ghe.example.com" (an instance that happens to be named api.*).
// Both guesses fail badly and one of them fails SILENTLY: withholding the
// credential from the operator's own GitOps repo empties what_changed on every
// investigation, surfacing only as a data-gaps line at the foot of a finding
// (RunLore #495). So the ambiguity is refused at config load and the operator
// names the host — loud once, at startup, instead of quiet forever.
//
// forge.git_host NAMES that host, but most deployments never set it: the host is
// DERIVED from forge.github_api_url or from forge.gitlab.base_url instead. A
// check that only looked at the explicit key therefore guarded the path almost
// nobody takes, which is how a non-ASCII forge host reached the credential
// boundary twice over (once per provider). Every key that can produce the
// boundary is checked here — see asciiForgeHost.
// validateCloud rejects a cloud block whose keys cannot all take effect.
//
// The flat region/cluster_name pair is AWS-only, kept flat for back-compatibility while
// GCP nests. Nothing read them under provider=gcp and nothing said so: load.go uses
// KnownFields(true), so `cloud: {provider: gcp, cluster_name: my-gke}` parses cleanly,
// the value lands in a field wireCloudProvider never consults, and GKE autodetection
// then either resolves a DIFFERENT cluster in the same project or leaves the scope
// unresolved — while the startup line looks correct.
//
// An error rather than a warning, for the same reason the provider switch grew a
// warning default: a deliberately-set cloud key that does nothing is the failure mode
// this block keeps having. The message names the nested key to move to, because that is
// the whole fix.
func validateCloud(cl Cloud) error {
	if cl.Provider != CloudGCP {
		return nil
	}
	var misplaced []string
	if strings.TrimSpace(cl.Region) != "" {
		misplaced = append(misplaced, "cloud.region (use cloud.gcp.location)")
	}
	if strings.TrimSpace(cl.ClusterName) != "" {
		misplaced = append(misplaced, "cloud.cluster_name (use cloud.gcp.cluster_name)")
	}
	if len(misplaced) == 0 {
		return nil
	}
	return fmt.Errorf("cloud.provider is %q but %s set: those keys are AWS-only and would be "+
		"silently ignored, leaving the GKE scope to autodetection",
		CloudGCP, strings.Join(misplaced, " and "))
}

func validateForgeGitHost(f Forge) error {
	if f.GitHost != "" {
		if !bareHost(f.GitHost) {
			return fmt.Errorf("forge.git_host %q is not a bare host: want just the hostname your git "+
				"remotes use (e.g. \"ghe.example.com\"), with no scheme, path, port, userinfo or "+
				"non-ASCII characters. It is compared against the host of every clone URL, so any "+
				"other spelling matches nothing and silently withholds the forge credential from "+
				"every repository", f.GitHost)
		}
		return nil
	}
	if f.Provider == "gitlab" {
		// gitlab.base_url IS the instance root: git, web and API share that host,
		// so this key alone decides the credential boundary.
		return asciiForgeHost("forge.gitlab.base_url", f.GitLab.BaseURL)
	}
	if err := asciiForgeHost("forge.github_api_url", f.GitHubAPIURL); err != nil {
		return err
	}
	u, err := url.Parse(f.GitHubAPIURL)
	if err != nil {
		return nil // unparseable ⇒ the github.com default; nothing ambiguous to resolve
	}
	h := strings.ToLower(u.Hostname())
	if h == "" || h == "api.github.com" || !strings.HasPrefix(h, "api.") {
		return nil
	}
	return fmt.Errorf("forge.github_api_url %q serves the API from an \"api.\" subdomain, so RunLore "+
		"cannot tell which host serves your git remotes: set forge.git_host to %q if this is GitHub "+
		"Enterprise with subdomain isolation (API on api.HOSTNAME, git on HOSTNAME), or to %q if git "+
		"really is served from the API host. Guessing would either send the installation token to a "+
		"host it is not valid for, or withhold it from every GitOps repository — which shows up only "+
		"as an empty what_changed",
		f.GitHubAPIURL, strings.TrimPrefix(h, "api."), h)
}

// asciiForgeHost refuses a forge URL whose HOST carries a byte above 7-bit
// ASCII, naming the key that carries it.
//
// RunLore derives from this URL the one host its forge credential may be sent
// to, and compares that host against every clone URL after strings.ToLower. On a
// non-ASCII host that fold and the IDNA mapping the dialler applies disagree:
// ToLower maps U+0130 to plain 'i', so a base_url of "https://gİtlab.example.com"
// yields the boundary "gitlab.example.com" — a SEPARATELY REGISTRABLE host that
// now collects the token — while the operator's real instance resolves to
// "xn--gtlab-56a.example.com" and is refused the credential it owns, emptying
// what_changed with nothing but a data-gaps line to show for it (RunLore #495).
// Both halves of that are silent, so this is a startup failure rather than a
// normalisation.
//
// The check is ASCII, not the full bareHost shape the explicit forge.git_host
// gets: url.Hostname() has already stripped the scheme, port, userinfo and path
// that bareHost exists to reject, and an IPv6 literal or an underscore label
// reaching it is a legitimate self-hosted spelling that folds identically either
// way. ASCII is the whole of what makes the normalisers agree.
func asciiForgeHost(key, rawURL string) error {
	if rawURL == "" {
		return nil // the provider's built-in default host, which is ASCII
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return nil // no host to derive from: the built-in default is used instead
	}
	if h := u.Hostname(); !asciiHost(h) {
		return fmt.Errorf("%s %q has the non-ASCII host %q, which RunLore cannot use as a credential "+
			"boundary: it lowercases that host to compare it against every clone URL, and the "+
			"lowercased spelling is a DIFFERENT registrable name from the one your git client "+
			"resolves through IDNA. Accepting it would attach the forge token to whichever host the "+
			"fold names and withhold it from yours, which surfaces only as an empty what_changed. "+
			"Write the host in its ASCII (punycode, \"xn--\") form, or set forge.git_host to that "+
			"form explicitly", key, rawURL, h)
	}
	return nil
}

// asciiHost reports whether s is free of bytes above 7-bit ASCII — the one
// property that makes strings.ToLower, strings.EqualFold and idna.Lookup.ToASCII
// agree about which host is being named. Same guard as whatchanged.isASCII and
// sourcerepo's allowlist character check, applied where a host is DERIVED rather
// than where one arrives.
func asciiHost(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// bareHost reports whether s is a hostname and nothing else: ASCII letters,
// digits, '-' and '.', starting and ending alphanumeric. It is deliberately
// narrower than "what net/url would parse" — a scheme, a path, a port, userinfo
// or a non-ASCII label all mean the operator wrote something that can never
// equal a clone URL's parsed host.
//
// ASCII-only is the same guard internal/whatchanged applies to a rewrite host
// and internal/sourcerepo applies to an allowlist entry: host comparison
// lowercases with strings.ToLower (Unicode simple case mapping) while net/http
// resolves through idna.Lookup.ToASCII, and the two disagree on non-ASCII input
// — "gİthub.com" compares equal to "github.com" while the connection goes to the
// separately registrable "xn--github-qyd.com".
func bareHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && c != '-' && c != '.' {
			return false
		}
		if !alnum && (i == 0 || i == len(s)-1) {
			return false
		}
	}
	return true
}

// checkSecureKeyEndpoint rejects a base_url that would send an API key in cleartext.
// A key is "present" when apiKeyEnv is non-empty; an empty base_url uses the provider's
// built-in (https) default and is always fine. http is allowed only to a private host.
func checkSecureKeyEndpoint(urlField, keyField, baseURL, apiKeyEnv string) error {
	if apiKeyEnv == "" || baseURL == "" {
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL (%s is set): %w", urlField, keyField, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isPrivateHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s is %q with an API key (%s set) on a public host — the key would be sent in cleartext; use https or a loopback/in-cluster endpoint", urlField, baseURL, keyField)
	default:
		return fmt.Errorf("%s must be an http(s) URL when %s is set, got scheme %q", urlField, keyField, u.Scheme)
	}
}

// effortLevels is the per-provider vocabulary of the opt-in model.effort knob.
// Unknown provider names use the "openai" set (NewModelClient routes them to the
// OpenAI-compatible client).
var effortLevels = map[string]map[string]bool{
	"anthropic": {"low": true, "medium": true, "high": true, "max": true},
	"gemini":    {"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true},
	"openai":    {"minimal": true, "low": true, "medium": true, "high": true},
}

// ValidateEffort checks an effective (provider, effort) pair against the
// per-provider effort vocabulary, so callers that build a ModelProvider outside
// config.Load (e.g. the eval model-comparison runner) share one source of truth.
// field names the setting in the returned error. Empty effort is always valid.
func ValidateEffort(field, provider, effort string) error {
	return validateEffort(field, provider, effort)
}

// validateEffort checks an effective (provider, effort) pair against the
// per-provider vocabulary. Empty effort is always valid (the knob is opt-in and
// omitted from requests); an empty provider defaults to the OpenAI-compatible
// wire protocol, mirroring NewModelClient.
func validateEffort(field, provider, effort string) error {
	if effort == "" {
		return nil
	}
	levels, ok := effortLevels[provider]
	if !ok {
		levels = effortLevels["openai"] // "", vllm, ollama, … → OpenAI-compatible client
	}
	if !levels[effort] {
		valid := make([]string, 0, len(levels))
		for l := range levels {
			valid = append(valid, l)
		}
		slices.Sort(valid)
		return fmt.Errorf("%s: %q is not a valid effort for this provider (valid: %s, or empty to omit)",
			field, effort, strings.Join(valid, "|"))
	}
	return nil
}

// validateThinking checks an effective (provider, thinking) pair. Empty thinking is
// always valid (the knob is opt-in and omitted from requests). Only "adaptive" is a
// valid mode, and only for provider anthropic — the thinking-block replay contract
// (signed blocks carried verbatim across the tool loop) is Anthropic-specific, so any
// other provider is rejected with a clear error rather than silently ignored.
func validateThinking(field, provider, thinking string) error {
	if thinking == "" {
		return nil
	}
	if provider != "anthropic" {
		p := provider
		if p == "" {
			p = "openai"
		}
		return fmt.Errorf("%s: thinking is only supported for provider anthropic (got %q)", field, p)
	}
	if thinking != "adaptive" {
		return fmt.Errorf("%s: %q is not a valid thinking mode (valid: adaptive, or empty to omit)", field, thinking)
	}
	return nil
}

// validateOverrideEffort validates one model override's effort and thinking
// against the provider they will ACTUALLY be sent to, so an inherited parent
// value that is invalid for the override's own provider fails at startup rather
// than as a per-request 400. name is the override's config path
// ("model.verify" / "model.chat"), which the error labels are built from.
//
// The inheritance is cmp.Or, which is what BuildVerifyModel and BuildChatModel
// actually use — the two hand-rolled copies this replaces claimed to mirror
// those builders while spelling it out as `if x == "" { x = parent }`, one copy
// per override, so a third override would have been a third copy.
//
// A nil override inherits everything and is validated by the parent's own
// checks, so it is nothing to do here.
func (c *Config) validateOverrideEffort(name string, o *ModelOverride) error {
	if o == nil {
		return nil
	}
	prov := cmp.Or(o.Provider, c.Model.Provider)
	if err := validateEffort(name+".effort (or inherited model.effort)", prov, cmp.Or(o.Effort, c.Model.Effort)); err != nil {
		return err
	}
	return validateThinking(name+".thinking (or inherited model.thinking)", prov, cmp.Or(o.Thinking, c.Model.Thinking))
}

// validateOverrideEndpoint rejects a cleartext API key over a public endpoint
// for one model override, resolving its EFFECTIVE base_url and key the same
// cmp.Or way its builder does. That resolution is the point: it catches an
// override that supplies its own key but inherits an insecure parent base_url,
// which neither value alone reveals.
func (c *Config) validateOverrideEndpoint(name string, o *ModelOverride) error {
	if o == nil {
		return nil
	}
	return checkSecureKeyEndpoint(name+".base_url", name+".api_key_env (or inherited model.api_key_env)",
		cmp.Or(o.BaseURL, c.Model.BaseURL), cmp.Or(o.APIKeyEnv, c.Model.APIKeyEnv))
}

// validatePricing rejects a negative token rate (which would produce a negative
// cost). A nil *Pricing (unconfigured) is valid.
func validatePricing(field string, p *Pricing) error {
	if p == nil {
		return nil
	}
	if p.InputUSDPerMTok < 0 || p.OutputUSDPerMTok < 0 || p.CachedInputUSDPerMTok < 0 {
		return fmt.Errorf("%s: rates must be >= 0 (input=%g output=%g cached_input=%g)",
			field, p.InputUSDPerMTok, p.OutputUSDPerMTok, p.CachedInputUSDPerMTok)
	}
	return nil
}

// ChatWithoutCaptureWarning reports whether model.chat is configured but neither
// notify.slack.thread_capture nor notify.matrix.thread_capture is enabled,
// returning "" when no warning is warranted. Thread capture is what makes
// RunLore listen for an addressed mention at all (Slack's app_mention events,
// Matrix's /sync long-poll) — without it on at least one transport, a
// configured model.chat has no mention ever reaching it and can never fire.
//
// This is deliberately a WARNING, not a Validate error: the operator may be
// staging config (thread_capture next), and a coherent-but-currently-pointless
// block should not block startup while it settles into place. Compare
// model.chat.model, which Validate DOES reject outright — that guards a
// security property (silent inheritance of a frontier model onto a
// member-triggerable path), not a reachability nicety.
func ChatWithoutCaptureWarning(cfg *Config) string {
	if cfg.Model.Chat == nil {
		return ""
	}
	if cfg.Notify.Slack.ThreadCapture || cfg.Notify.Matrix.ThreadCapture {
		return ""
	}
	return "model.chat is configured, but neither notify.slack.thread_capture nor " +
		"notify.matrix.thread_capture is enabled: conversational replies have no " +
		"mention-listening channel to trigger from and will never fire — enable " +
		"one, or this is dead config"
}

// DecisionModelUsable reports whether a decider can actually be built: the block is
// enabled and carries an endpoint. Every consumer checks this rather than Enabled, so
// "configured but switched off" and "not configured" behave identically.
func (c *Config) DecisionModelUsable() bool {
	d := c.DecisionModel
	return d.Enabled && d.BaseURL != "" && d.Model != ""
}

// RerankBackends and DedupBackends are the values Validate accepts for
// catalog.instant_recall.rerank_backend and forge.dedup_backend. Exported so that the
// places that describe them to an operator (internal/docsguard) read the parser's own
// set rather than a hand-written copy — a copy is how `shadow` went undocumented.
var (
	RerankBackends = []string{"llm", "jev", "shadow"}
	DedupBackends  = []string{"bm25", "jev"}
)

// Validate enforces cross-field invariants after loading — fail-closed defaults
// for the autonomy ladder: enabling execution requires the controls that bound
// it. Returns an error that should abort startup.
func (c *Config) Validate() error {
	if err := validateFactory(c.Factory); err != nil {
		return err
	}
	if err := c.validateCuration(); err != nil {
		return err
	}
	if c.Server.MCP.Enabled && c.Catalog.Dir == "" {
		return fmt.Errorf("server.mcp.enabled needs catalog.dir")
	}
	if err := validateCloud(c.Cloud); err != nil {
		return err
	}
	// Reject a nonsensical output-token cap before it reaches a provider request. 0
	// means "use the default"; a negative value is always a misconfiguration.
	if c.Model.MaxTokens < 0 {
		return fmt.Errorf("model.max_tokens must be >= 0 (0 = use the default), got %d", c.Model.MaxTokens)
	}
	if c.Model.Verify != nil && c.Model.Verify.MaxTokens < 0 {
		return fmt.Errorf("model.verify.max_tokens must be >= 0 (0 = inherit), got %d", c.Model.Verify.MaxTokens)
	}
	// model.chat is the one deliberate divergence from Verify's cmp.Or-everything
	// inheritance: chat runs once per addressed thread message, on a path any
	// channel or room member can trigger (verify runs once per investigation, on a
	// path RunLore itself initiates), so a silently-inherited investigation model
	// would make the cheapest way to enable the feature the most expensive way to
	// run it. Require the model be named explicitly.
	if ch := c.Model.Chat; ch != nil {
		if strings.TrimSpace(ch.Model) == "" {
			return fmt.Errorf("model.chat.model must name a model explicitly: chat runs once per addressed thread message, on a path any channel member can trigger, so it must not silently inherit model.model")
		}
		// Rejected exactly like its two siblings above, and for a reason this path
		// makes sharper: max_tokens sizes both the provider's output cap and the
		// conservative charge the hourly token budget applies when a provider
		// reports no usage. A negative silently resolves to the fixed default
		// (chatMaxTokens), so an operator who meant to cap replies harder would
		// get 1024 and no diagnostic. 0 is the documented "use the default" —
		// which for chat is a FIXED small default, deliberately not model.max_tokens.
		if ch.MaxTokens < 0 {
			return fmt.Errorf("model.chat.max_tokens must be >= 0 (0 = use the fixed chat default, NOT model.max_tokens), got %d", ch.MaxTokens)
		}
	}
	// Effort and thinking are validated against the provider they will actually be
	// sent to. Each override resolves its EFFECTIVE provider first — see
	// validateOverrideEffort — so an inherited parent value that is invalid for the
	// override's own provider fails at startup rather than as a per-request 400.
	if err := validateEffort("model.effort", c.Model.Provider, c.Model.Effort); err != nil {
		return err
	}
	if err := validateThinking("model.thinking", c.Model.Provider, c.Model.Thinking); err != nil {
		return err
	}
	if err := c.validateOverrideEffort("model.verify", c.Model.Verify); err != nil {
		return err
	}
	if err := c.validateOverrideEffort("model.chat", c.Model.Chat); err != nil {
		return err
	}
	// Interim progress updates: a non-positive cadence while enabled is a
	// misconfiguration (ApplyDefaults fills an unset 0 with 5, so only an explicit
	// negative reaches here). Validate fail-loud rather than silently never pinging.
	if c.Investigation.ProgressUpdates.Enabled && c.Investigation.ProgressUpdates.EverySteps <= 0 {
		return fmt.Errorf("investigation.progress_updates.every_steps must be > 0 when enabled, got %d", c.Investigation.ProgressUpdates.EverySteps)
	}
	// gitops.mirror.max caps the persistent what_changed mirror count; 0 means the
	// ApplyDefaults value (10). A negative cap is always a misconfiguration.
	if c.GitOps.Mirror.Max < 0 {
		return fmt.Errorf("gitops.mirror.max must be >= 0 (0 = use the default 10), got %d", c.GitOps.Mirror.Max)
	}
	// notify.thread bounds thread-capture volume, cost and note size; each
	// field's own Effective* method already resolves 0 to its documented
	// default (see ThreadNotify), so only a negative value is ever a
	// misconfiguration here.
	if c.Notify.Thread.MaxNotesPerThread < 0 {
		return fmt.Errorf("notify.thread.max_notes_per_thread must be >= 0 (0 = use the default %d), got %d",
			thread.DefaultMaxNotesPerThread, c.Notify.Thread.MaxNotesPerThread)
	}
	if c.Notify.Thread.ForgeWritesPerHour < 0 {
		return fmt.Errorf("notify.thread.forge_writes_per_hour must be >= 0 (0 = use the default %d), got %d",
			thread.DefaultForgeWritesPerHour, c.Notify.Thread.ForgeWritesPerHour)
	}
	if c.Notify.Thread.RegistryTTL < 0 {
		return fmt.Errorf("notify.thread.registry_ttl must be >= 0 (0 = use the default %s), got %s",
			thread.DefaultRegistryTTL, c.Notify.Thread.RegistryTTL.Std())
	}
	if c.Notify.Thread.RegistryMax < 0 {
		return fmt.Errorf("notify.thread.registry_max must be >= 0 (0 = use the default %d), got %d",
			thread.DefaultRegistryMax, c.Notify.Thread.RegistryMax)
	}
	// Not merely >= 0. A positive value below thread.MinMaxNoteBytes leaves no
	// room for both the human's words and the mark saying they were cut, so
	// capNoteText keeps the mark and drops the note — and every surface goes on
	// reporting the write as a success. There is no honest degradation available
	// once the note is gone, so it is refused here instead; see MinMaxNoteBytes
	// for the derivation of the floor.
	if c.Notify.Thread.MaxNoteBytes < 0 || (c.Notify.Thread.MaxNoteBytes > 0 && c.Notify.Thread.MaxNoteBytes < thread.MinMaxNoteBytes) {
		return fmt.Errorf("notify.thread.max_note_bytes must be 0 (= use the default %d) or at least %d, got %d — "+
			"below %d the truncation marker fills the whole budget and nothing of the note is written",
			thread.DefaultMaxNoteBytes, thread.MinMaxNoteBytes, c.Notify.Thread.MaxNoteBytes, thread.MinMaxNoteBytes)
	}
	if c.Notify.Thread.ChatCallsPerHour < 0 {
		return fmt.Errorf("notify.thread.chat_calls_per_hour must be >= 0 (0 = use the default %d), got %d",
			thread.DefaultChatCallsPerHour, c.Notify.Thread.ChatCallsPerHour)
	}
	if c.Notify.Thread.ChatTokensPerHour < 0 {
		return fmt.Errorf("notify.thread.chat_tokens_per_hour must be >= 0 (0 = use the default %d), got %d",
			thread.DefaultChatTokensPerHour, c.Notify.Thread.ChatTokensPerHour)
	}
	// announce_kb_updates is the one key here that is an enum, and a typo in it
	// must fail loud: "treads" or "chanel" silently falling back to channel would
	// leave an operator believing the echo they configured away is gone while
	// every write still restates itself in the channel.
	switch c.Notify.Thread.AnnounceKBUpdates {
	case "", AnnounceOff, AnnounceChannel, AnnounceThread, AnnounceBoth:
	default:
		return fmt.Errorf("unknown notify.thread.announce_kb_updates %q (want a boolean or %s; false = off, true = channel, empty = off)",
			string(c.Notify.Thread.AnnounceKBUpdates), announceModeList)
	}
	// source_repos.allow is compiled at startup; a bad pattern must fail config
	// load, not silently disable the tool at wiring time.
	if len(c.SourceRepos.Allow) > 0 {
		if _, err := sourcerepo.New(c.SourceRepos.Allow); err != nil {
			return fmt.Errorf("source_repos.allow: %w", err)
		}
	}
	// Pricing rates must be non-negative (a negative rate would report a negative
	// cost). Cover the main model and both overrides, each of which carries its own.
	if err := validatePricing("model.pricing", c.Model.Pricing); err != nil {
		return err
	}
	if v := c.Model.Verify; v != nil {
		if err := validatePricing("model.verify.pricing", v.Pricing); err != nil {
			return err
		}
	}
	// model.chat.pricing is checked for the same reason, though NOTHING reads it
	// yet: the chat path has no cost reporting at all (investigate.go costs the
	// main and verify passes only), so this rate cannot produce a wrong number
	// today. It is decoded and documented exactly like its siblings, which is
	// what makes accepting an invalid one at startup the wrong answer — a rate
	// that silently validates reads as a rate that is in use.
	if ch := c.Model.Chat; ch != nil {
		if err := validatePricing("model.chat.pricing", ch.Pricing); err != nil {
			return err
		}
	}
	// Reject a negative cost ceiling. The sibling max_tokens_per_investigation teaches
	// operators that -1 means "unlimited", so the idiom WILL be copied here — where 0
	// already means no ceiling and a negative value would read as an explicit opt-out
	// while being indistinguishable from one only by accident. Say what to write.
	if mc := c.Investigation.MaxCostPerInvestigation; mc < 0 {
		return fmt.Errorf("investigation.max_cost_per_investigation must be >= 0, got %g "+
			"(unlike max_tokens_per_investigation there is no -1 opt-out: 0 or unset already means no cost ceiling)", mc)
	}
	// Reject a negative rate-limit budget: ApplyDefaults fills an unset nil with 30,
	// so only an explicit negative reaches here — fail loud rather than silently
	// treating it as unlimited.
	if mpw := c.Investigation.RateLimit.MaxPerWindow; mpw != nil && *mpw < 0 {
		return fmt.Errorf("investigation.rate_limit.max_per_window must be >= 0 (0 = unlimited), got %d", *mpw)
	}
	// Reject a negative per-tool timeout: time.ParseDuration accepts negative values
	// which silently disable the feature (fails the > 0 guard in runTool) rather than
	// setting the intended timeout. 0 means "use the default (60s)".
	if c.Investigation.ToolTimeout.Std() < 0 {
		return fmt.Errorf("investigation.tool_timeout must be >= 0 (0 = use the default 60s), got %v", c.Investigation.ToolTimeout.Std())
	}
	// Reject an unknown compaction mode at startup rather than silently defaulting a
	// typo to lossy elision. Empty means the default (elide).
	switch c.Investigation.Compaction {
	case "", "elide", "summarize":
	default:
		return fmt.Errorf("investigation.compaction %q is invalid (want elide|summarize; empty = elide)", c.Investigation.Compaction)
	}
	// Reject a cleartext API key over a public endpoint (the key would be sent in the
	// clear, and is the enabler for a redirect-based key leak). Cover the main model,
	// each override that targets its own endpoint, and embeddings. Loopback /
	// in-cluster hosts are exempt.
	if err := checkSecureKeyEndpoint("model.base_url", "model.api_key_env", c.Model.BaseURL, c.Model.APIKeyEnv); err != nil {
		return err
	}
	if err := c.validateOverrideEndpoint("model.verify", c.Model.Verify); err != nil {
		return err
	}
	if err := c.validateOverrideEndpoint("model.chat", c.Model.Chat); err != nil {
		return err
	}
	if e := c.Model.Embeddings; e != nil {
		if err := checkSecureKeyEndpoint("model.embeddings.base_url", "model.embeddings.api_key_env", e.BaseURL, e.APIKeyEnv); err != nil {
			return err
		}
	}
	seenMCP := map[string]bool{}
	for i, s := range c.MCP.Servers {
		if s.Name == "" || s.URL == "" {
			return fmt.Errorf("mcp.servers[%d]: name and url are required", i)
		}
		if strings.Contains(s.Name, "__") || strings.ContainsAny(s.Name, " \t") {
			return fmt.Errorf("mcp.servers[%d]: name %q must not contain '__' or whitespace", i, s.Name)
		}
		if seenMCP[s.Name] {
			return fmt.Errorf("mcp.servers[%d]: duplicate server name %q", i, s.Name)
		}
		seenMCP[s.Name] = true
		if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			scheme := ""
			if err == nil {
				scheme = u.Scheme
			}
			return fmt.Errorf("mcp.servers[%s].url: scheme must be http or https, got %q", s.Name, scheme)
		}
		if err := checkSecureKeyEndpoint("mcp.servers["+s.Name+"].url", "mcp.servers["+s.Name+"].token_env", s.URL, s.TokenEnv); err != nil {
			return err
		}
		seenTool := map[string]bool{}
		for j, tn := range s.Tools {
			if tn == "" || strings.ContainsAny(tn, " \t") {
				return fmt.Errorf("mcp.servers[%s].tools[%d]: tool name must be non-empty without whitespace", s.Name, j)
			}
			if seenTool[tn] {
				return fmt.Errorf("mcp.servers[%s].tools: duplicate tool name %q", s.Name, tn)
			}
			seenTool[tn] = true
		}
		if c.MCP.RequireAllowlist && len(s.Tools) == 0 {
			return fmt.Errorf("mcp.require_allowlist: server %q declares no tools allowlist (add mcp.servers[%s].tools or disable require_allowlist)", s.Name, s.Name)
		}
	}
	// Curation verdict gate: reject an unknown verdict in forge.skip_verdicts at
	// startup rather than silently ignoring a typo (which would leave benign findings
	// drafting PRs into the review queue). Empty is valid — every verdict is eligible.
	for i, v := range c.Forge.SkipVerdicts {
		if !providers.ValidVerdict(providers.Verdict(v)) {
			return fmt.Errorf("forge.skip_verdicts[%d]: unknown verdict %q (want no_action|action_suggested|action_required|inconclusive)", i, v)
		}
	}
	// Forge provider selection: fails CLOSED. An unknown provider, a "gitlab"
	// provider with no token_env (curation would otherwise silently disable
	// rather than the operator learning at startup that it never wired up), or a
	// kb_repo that is not a valid GitLab project path, all abort config load
	// rather than letting `serve` come up with curation quietly turned off.
	switch c.Forge.Provider {
	case "", "github":
		// no additional required fields today — an absent github_app simply
		// disables curation (existing, backward-compatible behaviour).
	case "gitlab":
		if c.Forge.GitLab.TokenEnv == "" {
			return fmt.Errorf("forge.provider is \"gitlab\" but forge.gitlab.token_env is empty — set it to the env var holding a project/group access token (scope: api), or curation would silently never activate")
		}
		if c.Forge.KBRepo != "" && !validGitLabProjectPath(c.Forge.KBRepo) {
			return fmt.Errorf("forge.kb_repo %q is not a valid GitLab project path (want \"group/project\", optionally nested: \"group/subgroup/project\")", c.Forge.KBRepo)
		}
		if err := checkSecureKeyEndpoint("forge.gitlab.base_url", "forge.gitlab.token_env", c.Forge.GitLab.BaseURL, c.Forge.GitLab.TokenEnv); err != nil {
			return err
		}
	default:
		return fmt.Errorf("forge.provider %q is invalid (want \"github\" or \"gitlab\")", c.Forge.Provider)
	}
	if err := validateForgeGitHost(c.Forge); err != nil {
		return err
	}
	// The recurrence cooldown reads the outcome ledger's trigger index; without a
	// ledger it would silently never suppress — fail loud instead. A negative
	// duration is always a misconfiguration (mirrors tool_timeout).
	if d := c.Investigation.RecurrenceCooldown.Std(); d < 0 {
		return fmt.Errorf("investigation.recurrence_cooldown must be >= 0 (0 = off), got %v", d)
	} else if d > 0 && c.Outcome.LedgerPath == "" {
		return fmt.Errorf("investigation.recurrence_cooldown requires outcome.ledger_path (the suppression gate reads the ledger's per-trigger index)")
	}
	// Feedback buttons are click-driven: enabling them without the pieces a click
	// needs would either accept unsigned requests (never) or render buttons whose
	// clicks silently vanish (a lie to the on-call). Fail loud at startup instead.
	if c.Notify.Slack.FeedbackButtons {
		if c.Notify.Slack.SigningSecretEnv == "" {
			return fmt.Errorf("notify.slack.feedback_buttons requires notify.slack.signing_secret_env: clicks arrive on the exposed POST /slack/interactions endpoint and must be signature-verified")
		}
		if c.Outcome.LedgerPath == "" {
			return fmt.Errorf("notify.slack.feedback_buttons requires outcome.ledger_path: ratings are recorded in the outcome ledger")
		}
		// …and a delivery target, because a button only ever exists on a message the
		// Slack notifier delivered. That notifier is built from an incoming webhook OR
		// a bot token + channel; with neither, notify/slack.go's builder returns nil and
		// the notifier is skipped silently — no message, no buttons, no feedback, while
		// startup happily reported the feature on.
		//
		// Either target qualifies: Slack dispatches block_actions for interactive
		// components posted through an incoming webhook exactly as it does for
		// chat.postMessage ("incoming webhooks conform to the same rules and
		// functionality as any of our other messaging APIs"), and the click is answered
		// via the payload's response_url, which needs no bot token. Requiring the bot
		// token here would break working webhook deployments.
		//
		// This mirrors the builder's static preconditions exactly, so it rejects only
		// configs where no notifier could ever be built regardless of the environment —
		// i.e. where the buttons were already dead. Runtime emptiness (an env var set
		// but unmounted) is not knowable here; app.SlackFeedbackDeliverable warns for it.
		if !c.Notify.Slack.hasDeliveryTarget() {
			return fmt.Errorf("notify.slack.feedback_buttons requires a Slack delivery target: set notify.slack.webhook_url_env, or notify.slack.bot_token_env together with notify.slack.channel — with neither the Slack notifier is skipped, so no message is delivered, no buttons render and no feedback can ever be recorded")
		}
	}
	// Silencing is click-driven and CHANGES BEHAVIOUR: enabling it without the
	// pieces a click needs would record every silence durably and then ignore it,
	// while the UI confirmed success. Fail loud at startup instead — the same
	// posture feedback_buttons and recurrence_cooldown already take.
	if c.Notify.Slack.SilenceButton {
		if c.Notify.Slack.SigningSecretEnv == "" {
			return fmt.Errorf("notify.slack.silence_button requires notify.slack.signing_secret_env: clicks arrive on the exposed POST /slack/interactions endpoint and must be signature-verified")
		}
		// …and a delivery target, for the same reason feedback_buttons requires
		// one above: a 🔕 menu only ever exists on a message the Slack notifier
		// delivered, and that notifier is skipped silently with neither target set.
		if !c.Notify.Slack.hasDeliveryTarget() {
			return fmt.Errorf("notify.slack.silence_button requires a Slack delivery target: set notify.slack.webhook_url_env, or notify.slack.bot_token_env together with notify.slack.channel — with neither the Slack notifier is skipped, so no message is delivered, no 🔕 menu renders and no silence can ever be recorded")
		}
	}
	// Same fail-loud contract for the Matrix reaction listener as
	// feedback_reactions above: without the notifier fields it would sync
	// nothing, so a 🔕 reaction could never be recorded.
	if c.Notify.Matrix.SilenceReactions {
		if !c.Notify.Matrix.hasListenerFields() {
			return fmt.Errorf("notify.matrix.silence_reactions requires homeserver, room_id and access_token_env (the reaction listener long-polls the configured room)")
		}
	}
	if c.Notify.SilenceEnabled() {
		if c.Outcome.LedgerPath == "" {
			return fmt.Errorf("notify.silence requires outcome.ledger_path: a silence is recorded in the outcome ledger, and the suppression gate reads it back from there")
		}
		if len(c.Notify.Silence.Windows) == 0 {
			return fmt.Errorf("notify.silence.windows must list at least one duration when a silence transport is enabled: with no presets there is nothing to render, and no duration for a click to record")
		}
		if c.Notify.Silence.MaxWindow.Std() <= 0 {
			return fmt.Errorf("notify.silence.max_window must be positive when a silence transport is enabled: an uncapped silence is indistinguishable from a permanent one")
		}
		for _, w := range c.Notify.Silence.Windows {
			if w.Std() <= 0 {
				return fmt.Errorf("notify.silence.windows entry %v must be positive: zero silences nothing and a negative duration is meaningless as a suppression window", w.Std())
			}
			if w.Std() > c.Notify.Silence.MaxWindow.Std() {
				return fmt.Errorf("notify.silence.windows entry %v exceeds notify.silence.max_window (%v)", w.Std(), c.Notify.Silence.MaxWindow.Std())
			}
		}
		// Both ends of the preset count, together and under the SAME condition,
		// because they are one rule: how many options make a usable dropdown.
		//
		// Both began as Slack's. An overflow element required 2 to 5 options and
		// answered either breach by rejecting the WHOLE message with
		// invalid_blocks — `windows: [4h]` passed startup and then made
		// chat.postMessage fail for EVERY message. The 2026-08-26 swap to a
		// static_select lifted both limits: Slack now accepts one option and six.
		// What survives is a judgement about the 🔕 menu, which is a quick choice
		// on a card someone is reading at 3am — a one-entry dropdown is a button
		// wearing a menu, and a six-item one is a form.
		//
		// Scoped to silence_button because the rule describes the RENDERED CONTROL,
		// and Matrix renders none: a bare 🔕 reaction silences for windows[0] and
		// nothing else, so every preset after the first is equally unreachable
		// there. The ceiling was briefly unconditional on the argument that it cost
		// a Matrix-only deployment nothing it could reach anyway — but that is just
		// as true of the floor, and a rule enforced at two different scopes is one
		// rule pretending to be two. A Matrix-only config is now judged only on
		// what Matrix actually renders.
		//
		// The cost of scoping, stated plainly: a Matrix-only deployment may now
		// hold a preset list that Slack would refuse, and turning silence_button on
		// later fails at startup rather than silently drawing a bad menu. That is
		// the right failure — loud, at boot, naming the key — and it is why the
		// error says which flag to turn off.
		//
		// Checked after the per-entry loop so an entry that is ALSO invalid on its
		// own terms reports the more specific reason.
		if c.Notify.Slack.SilenceButton {
			if n := len(c.Notify.Silence.Windows); n < 2 || n > 5 {
				return fmt.Errorf("notify.silence.windows lists %d, but the 🔕 menu offers 2 to 5 entries: a one-option dropdown is a button wearing a menu and a six-item one is a form, on a card someone is reading at 3am — adjust the list, or turn off notify.slack.silence_button", n)
			}
		}
	}
	// Thread capture cannot work without the same signature verification as
	// feedback_buttons (mentions arrive on an exposed HTTP endpoint) or without
	// the bot-token delivery path: an incoming webhook returns no message ts, so
	// there is no thread root to attribute a reply to.
	if sl := c.Notify.Slack; sl.ThreadCapture {
		if sl.SigningSecretEnv == "" {
			return fmt.Errorf("notify.slack.thread_capture requires notify.slack.signing_secret_env: mentions arrive on the exposed POST /slack/events endpoint and must be signature-verified")
		}
		if sl.BotTokenEnv == "" {
			return fmt.Errorf("notify.slack.thread_capture requires notify.slack.bot_token_env: an incoming webhook returns no message ts, so there is no thread to attribute a reply to")
		}
		if sl.Channel == "" {
			return fmt.Errorf("notify.slack.thread_capture requires notify.slack.channel (it is required alongside bot_token_env)")
		}
		if c.Outcome.LedgerPath == "" {
			return fmt.Errorf("notify.slack.thread_capture requires outcome.ledger_path: the thread registry lives beside the ledger and must survive a restart and a leader failover, so without a ledger path there is nowhere durable to record which thread belongs to which investigation")
		}
	}
	// Same fail-loud contract for the Matrix reaction listener: without the
	// notifier fields it would sync nothing, without the ledger it would record
	// nowhere — both silent lies to whoever enabled the option.
	if c.Notify.Matrix.FeedbackReactions {
		if !c.Notify.Matrix.hasListenerFields() {
			return fmt.Errorf("notify.matrix.feedback_reactions requires homeserver, room_id and access_token_env (the reaction listener long-polls the configured room)")
		}
		if c.Outcome.LedgerPath == "" {
			return fmt.Errorf("notify.matrix.feedback_reactions requires outcome.ledger_path: ratings are recorded in the outcome ledger")
		}
	}
	// Same fail-loud contract for the Matrix thread-capture listener: without the
	// notifier fields it would sync nothing, without the ledger the thread
	// registry has nowhere durable to live — both silent lies to whoever enabled
	// the option.
	if c.Notify.Matrix.ThreadCapture {
		if !c.Notify.Matrix.hasListenerFields() {
			return fmt.Errorf("notify.matrix.thread_capture requires homeserver, room_id and access_token_env (the listener long-polls the configured room and authenticates as the bot)")
		}
		if c.Outcome.LedgerPath == "" {
			return fmt.Errorf("notify.matrix.thread_capture requires outcome.ledger_path: the thread registry lives beside the ledger and must survive a restart and a leader failover, so without a ledger path there is nowhere durable to record which thread belongs to which investigation")
		}
	}
	if g := c.Catalog.Git; g.URL != "" {
		if err := rejectPinnedBranch("catalog.git.branch", g.Branch, catalogGitBranchRemedy); err != nil {
			return err
		}
	}
	// Commons catalog (opt-in): a second, read-only root. Fail closed on a
	// misconfiguration rather than degrading to a silently-absent corpus.
	if cc := c.Catalog.Commons; cc.URL != "" {
		if cc.Dir == "" {
			return fmt.Errorf("catalog.commons.url is set but catalog.commons.dir is empty: the shared catalog needs its own checkout directory")
		}
		if err := validateCommonsRevision(cc); err != nil {
			return err
		}
		// Sharing a directory with the operator's own catalog would let an upstream
		// sync write into their checkout — and, with git-sync enabled, fight the
		// mirror on every reconcile. Distinct roots is the whole basis of the
		// read-only guarantee.
		// Cleaned + containment, not string equality. Load walks recursively, so
		// commons.dir: /var/lib/runlore/catalog/commons under catalog.dir:
		// /var/lib/runlore/catalog passed the old check and then indexed every commons
		// entry TWICE — once prefixed and marked, once as the operator's own, unmarked
		// and competing in the tie-break as a local entry. It also did exactly what the
		// error says it prevents: an upstream sync writing inside the operator's mirror.
		// A trailing slash slipped through for the same reason.
		own := filepath.Clean(c.Catalog.Dir)
		com := filepath.Clean(cc.Dir)
		if own == com {
			return fmt.Errorf("catalog.commons.dir must differ from catalog.dir (both resolve to %q): the shared catalog is a SEPARATE, read-only root and must not share the operator's checkout", com)
		}
		if within(com, own) || within(own, com) {
			return fmt.Errorf("catalog.commons.dir (%q) and catalog.dir (%q) must not contain one another: the loader walks recursively, so a nested root is indexed twice — once as shared, once as the operator's own", com, own)
		}
	}
	// Instant-recall reranker (ON by default): its knobs are only meaningful when enabled.
	// ApplyDefaults fills unset (0) values, so only an explicitly out-of-range setting
	// reaches here — fail loud rather than silently gating on a nonsensical threshold.
	// A threshold in (0,1] is a calibrated PROBABILITY (the reranker returns 0.0–1.0);
	// a value >1 or <=0 could never fire (or always fire), defeating the gate.
	if c.Catalog.InstantRecall.RerankEnabled() {
		ir := c.Catalog.InstantRecall
		if ir.RerankThreshold <= 0 || ir.RerankThreshold > 1 {
			return fmt.Errorf("catalog.instant_recall.rerank_threshold must be in (0,1] (a calibrated match confidence), got %g", ir.RerankThreshold)
		}
		if ir.RerankK < 1 {
			return fmt.Errorf("catalog.instant_recall.rerank_k must be >= 1 (candidates to rank), got %d", ir.RerankK)
		}
		if ir.RerankMinScore < 0 {
			return fmt.Errorf("catalog.instant_recall.rerank_min_score must be >= 0 (retrieval-score cost floor), got %g", ir.RerankMinScore)
		}
	}
	// A decision model that is enabled must be reachable; an unknown backend name is a
	// typo the operator needs told about. But a DISABLED block never errors, however a
	// consumer is set — see DecisionModel's doc for why the kill switch outranks
	// consistency here.
	if c.DecisionModel.Enabled {
		if c.DecisionModel.BaseURL == "" {
			return fmt.Errorf("decision_model.base_url is required when decision_model.enabled is true")
		}
		if c.DecisionModel.Model == "" {
			return fmt.Errorf("decision_model.model is required when decision_model.enabled is true")
		}
		if p := c.DecisionModel.Provider; p != "" && p != "typesafe" {
			return fmt.Errorf("decision_model.provider %q is not supported (valid: typesafe)", p)
		}
		if err := checkSecureKeyEndpoint("decision_model.base_url", "decision_model.api_key_env",
			c.DecisionModel.BaseURL, c.DecisionModel.APIKeyEnv); err != nil {
			return err
		}
	}
	if b := c.Catalog.InstantRecall.RerankBackend; b != "" && !slices.Contains(RerankBackends, b) {
		return fmt.Errorf("catalog.instant_recall.rerank_backend %q is not valid (%s)", b, strings.Join(RerankBackends, "|"))
	}
	// Only the live backend needs a bar. Shadow records the confidence distribution
	// instead, which is how the value gets chosen in the first place.
	if c.Catalog.InstantRecall.RerankBackend == "jev" && c.DecisionModelUsable() {
		if t := c.Catalog.InstantRecall.RerankThresholdJev; t <= 0 || t > 1 {
			return fmt.Errorf("catalog.instant_recall.rerank_threshold_jev must be in (0,1] and is required when rerank_backend is jev, got %g", t)
		}
	}
	// rerank_threshold_jev is read by `shadow` too — it decides whether a shadow
	// comparison counts as a fire (see rerank.go's shadowFired), not only by `jev` — so
	// an out-of-range value must fail loud regardless of backend. 0 (unset) is exempt
	// here; the required-when-jev check above already covers that case for backend jev.
	if t := c.Catalog.InstantRecall.RerankThresholdJev; t != 0 && (t < 0 || t > 1) {
		return fmt.Errorf("catalog.instant_recall.rerank_threshold_jev must be in (0,1] when set, got %g", t)
	}
	if b := c.Forge.DedupBackend; b != "" && !slices.Contains(DedupBackends, b) {
		return fmt.Errorf("forge.dedup_backend %q is not valid (%s)", b, strings.Join(DedupBackends, "|"))
	}
	if c.Forge.DedupBackend == "jev" && c.DecisionModelUsable() {
		if c.Forge.DedupSkipAbove <= 0 || c.Forge.DedupSkipAbove > 1 {
			return fmt.Errorf("forge.dedup_skip_above must be in (0,1] and is required when dedup_backend is jev, got %g", c.Forge.DedupSkipAbove)
		}
		if c.Forge.DedupAnnotateAbove <= 0 || c.Forge.DedupAnnotateAbove > 1 {
			return fmt.Errorf("forge.dedup_annotate_above must be in (0,1] and is required when dedup_backend is jev, got %g", c.Forge.DedupAnnotateAbove)
		}
		if c.Forge.DedupSkipAbove <= c.Forge.DedupAnnotateAbove {
			return fmt.Errorf("forge.dedup_skip_above (%g) must be greater than forge.dedup_annotate_above (%g), or the tiers are unorderable",
				c.Forge.DedupSkipAbove, c.Forge.DedupAnnotateAbove)
		}
	}
	// Retirement pass (opt-in): its knobs are only meaningful when enabled, and a
	// disabled block is never validated. ApplyDefaults fills unset (0) values while
	// enabled, so only an explicitly out-of-range setting reaches here. Floor is a
	// calibrated posterior-mean success rate: it MUST match recall's outcome_floor
	// range (0,1] — a floor >1 would retire everything, <=0 nothing. min_observations
	// is the sustained-decay bar and must be >= 1 (a single bad recall must never retire).
	if c.Curate.Retirement.Enabled {
		r := c.Curate.Retirement
		if r.Floor <= 0 || r.Floor > 1 {
			return fmt.Errorf("curate.retirement.floor must be in (0,1] (a calibrated outcome factor, matching recall's outcome_floor), got %g", r.Floor)
		}
		if r.MinObservations < 1 {
			return fmt.Errorf("curate.retirement.min_observations must be >= 1 (the sustained-decay bar), got %d", r.MinObservations)
		}
	}
	// Revalidation pass (opt-in): validated on exactly the terms retirement is,
	// because the two gate on opposite sides of the SAME factor — a floor outside
	// (0,1] here would break that complementarity as well as being meaningless.
	// ApplyDefaults fills unset (0) values while enabled, so only an explicitly
	// out-of-range setting reaches here.
	if c.Curate.Revalidation.Enabled {
		r := c.Curate.Revalidation
		if r.Floor <= 0 || r.Floor > 1 {
			return fmt.Errorf("curate.revalidation.floor must be in (0,1] (a calibrated outcome factor, matching curate.retirement.floor), got %g", r.Floor)
		}
		if r.MinInterval.Std() <= 0 {
			return fmt.Errorf("curate.revalidation.min_interval must be > 0 (the anti-spam bar between two stamps of one entry), got %v", r.MinInterval.Std())
		}
		if r.MaxOpen < 1 {
			return fmt.Errorf("curate.revalidation.max_open must be >= 1 (the reviewer-queue bound), got %d", r.MaxOpen)
		}
	}
	// With BOTH passes on, their calibration must be identical. They partition
	// entries by comparing one outcome factor against one floor — retirement
	// strictly below it, revalidation at or above — so the disjointness they claim
	// is a property of the arithmetic, not a rule either pass enforces. Unequal
	// floors leave a band where both fire; unequal priors compute different factors
	// from the same ledger. Either way one entry can be proposed for retirement AND
	// revalidation in a single sweep. Leaving revalidation's knobs unset inherits
	// retirement's (ApplyDefaults), so reaching here means two values were set apart
	// deliberately.
	if c.Curate.Retirement.Enabled && c.Curate.Revalidation.Enabled {
		if ret, rev := c.Curate.Retirement.Floor, c.Curate.Revalidation.Floor; ret != rev {
			return fmt.Errorf("curate.revalidation.floor (%g) must equal curate.retirement.floor (%g): "+
				"the two passes gate on opposite sides of the same outcome factor, so unequal floors leave a band where "+
				"one entry is proposed for retirement and revalidation at once — omit curate.revalidation.floor to inherit", rev, ret)
		}
		if ret, rev := c.Curate.Retirement.Prior, c.Curate.Revalidation.Prior; ret != rev {
			return fmt.Errorf("curate.revalidation.prior (%g) must equal curate.retirement.prior (%g): "+
				"a different prior computes a different factor from the same ledger, so the two passes would disagree about "+
				"the same entry's track record — omit curate.revalidation.prior to inherit", rev, ret)
		}
	}
	// In-server sweeps: an unknown mode must fail loud (a typo like "apply" silently
	// falling back to dry-run would mean the operator believes grooming is live when
	// it is not), and a sub-10m interval would hammer the forge listing endpoints.
	switch c.Curate.Sweeps.Mode {
	case "", SweepOff, SweepDryRun, SweepApply:
	default:
		return fmt.Errorf("unknown curate.sweeps.mode %q (want off|dry-run|apply; empty = dry-run)", c.Curate.Sweeps.Mode)
	}
	if iv := c.Curate.Sweeps.Interval.Std(); iv != 0 && iv < 10*time.Minute {
		return fmt.Errorf("curate.sweeps.interval must be >= 10m (forge-listing rate protection), got %v", iv)
	}
	if c.Curate.QueueIssue.Enabled() {
		if _, _, _, err := ParseSchedule(c.Curate.QueueIssue.Schedule); err != nil {
			return err
		}
	}
	// logs.provider is a small enum; reject typos at startup rather than silently
	// falling back to the wrong client against a live backend.
	switch c.Logs.Provider {
	case "", LogsProviderVictoriaLogs, LogsProviderLoki, LogsProviderElasticsearch, LogsProviderOpenSearch:
	default:
		return fmt.Errorf("logs.provider must be %q, %q, %q, %q, or empty (auto-detect); got %q",
			LogsProviderVictoriaLogs, LogsProviderLoki, LogsProviderElasticsearch, LogsProviderOpenSearch, c.Logs.Provider)
	}
	// logs.index goes straight into the request PATH
	// (internal/logs/elasticsearch.Client.indexPath). The client escapes it, so a
	// malformed value can no longer change the request's shape — but escaping
	// turns a typo into a puzzling 4xx from Elasticsearch mid-investigation
	// ("no such index [a%2Fb]") rather than a startup failure naming the config
	// key. Reject the malformed shapes here instead, loudly and once.
	if idx := c.Logs.Index; idx != "" {
		// Elasticsearch itself forbids all of these in an index name; `/` and `?`
		// additionally used to be the path/query-string injections. `*` (wildcard),
		// `,` (multi-index list) and `:` (cross-cluster `remote:index`) are all
		// legal index-EXPRESSION syntax and deliberately allowed.
		if i := strings.IndexAny(idx, "/\\?\"<>|# \t\n\r"); i >= 0 {
			return fmt.Errorf("logs.index %q contains %q, which Elasticsearch/OpenSearch forbid in an index name (allowed: lowercase letters, digits, - _ + . and the `*` wildcard; separate multiple patterns with `,`)", idx, string(idx[i]))
		}
		// `,` IS legal — it is the multi-index list separator (logs-app-*,logs-infra-*)
		// — but an empty element in that list is a typo (a trailing or doubled comma).
		for _, part := range strings.Split(idx, ",") {
			if part == "" {
				return fmt.Errorf("logs.index %q has an empty pattern in its comma-separated list", idx)
			}
			// ES rejects a name starting with -, _ or +; a leading `-` in particular
			// is its EXCLUSION syntax and silently matches nothing on its own.
			if strings.HasPrefix(part, "-") || strings.HasPrefix(part, "_") || strings.HasPrefix(part, "+") {
				return fmt.Errorf("logs.index pattern %q must not start with -, _ or + (Elasticsearch reserves those; a leading `-` is exclusion syntax)", part)
			}
		}
		if idx != strings.ToLower(idx) {
			return fmt.Errorf("logs.index %q must be lowercase (Elasticsearch/OpenSearch index names are), got mixed case", idx)
		}
	}
	if err := c.validateRevertPR(); err != nil {
		return err
	}
	switch c.Actions.Mode {
	case "", ActionOff, ActionSuggest:
		return nil // read-only-ish: nothing to execute
	case ActionApprove, ActionAuto:
		// Both executing rungs require the control/kill-switch token (fail closed).
		if c.Actions.ApprovalTokenEnv == "" {
			return fmt.Errorf("actions.mode=%s requires actions.approval_token_env (control/kill-switch endpoints fail closed without it)", c.Actions.Mode)
		}
		// Both executing rungs mutate the cluster, so both must be audited: the hash
		// chain is verified fail-closed on open (see internal/app.BuildAuditor). Without
		// an audit_log_path approve would silently fall back to a Nop auditor — no chain,
		// no verify, no fail-closed — yet it still executes cluster mutations.
		if c.Actions.AuditLogPath == "" {
			return fmt.Errorf("actions.mode=%s requires actions.audit_log_path (an executing rung must be audited)", c.Actions.Mode)
		}
		if c.Actions.Mode == ActionApprove {
			return nil
		}
		// auto-only: unattended execution additionally needs an authenticated webhook
		// and bounded gates.
		if c.Server.WebhookTokenEnv == "" {
			return fmt.Errorf("actions.mode=auto requires server.webhook_token_env (the alert webhook must be authenticated)")
		}
		if c.Actions.Auto.MinConfidence <= 0 {
			return fmt.Errorf("actions.mode=auto requires actions.auto.min_confidence > 0")
		}
		if c.Actions.Auto.MaxPerWindow <= 0 {
			return fmt.Errorf("actions.mode=auto requires actions.auto.max_per_window > 0 (unbounded auto-execution is unsafe)")
		}
		if len(c.Actions.Allow.Namespaces) == 0 {
			return fmt.Errorf("actions.mode=auto requires actions.allow.namespaces (target-namespace allowlist)")
		}
		return nil
	default:
		return fmt.Errorf("unknown actions.mode %q (want off|suggest|approve|auto)", c.Actions.Mode)
	}
}

// within reports whether child is inside parent. Compared on cleaned, separator-
// terminated paths so /a/b is not treated as inside /a/bc.
func within(child, parent string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validateRevertPR enforces fail-closed invariants for actions.revert_pr.
func (c *Config) validateRevertPR() error {
	rp := c.Actions.RevertPR
	if !rp.Enabled {
		return nil
	}
	if c.Actions.Mode != ActionApprove {
		return fmt.Errorf("actions.revert_pr.enabled requires actions.mode=approve (revert_pr is approve-only and hard-blocked from auto; got mode=%q)", c.Actions.Mode)
	}
	if len(rp.AllowedRepos) == 0 {
		return fmt.Errorf("actions.revert_pr.enabled requires non-empty actions.revert_pr.allowed_repos (fail closed)")
	}
	for i, r := range rp.AllowedRepos {
		trimmed := strings.TrimSpace(r)
		if trimmed == "" || !strings.Contains(trimmed, "/") || strings.HasPrefix(trimmed, "/") || strings.HasSuffix(trimmed, "/") {
			return fmt.Errorf("actions.revert_pr.allowed_repos[%d] %q must be a repository path like \"owner/repo\" or \"group/subgroup/project\"", i, r)
		}
	}
	if rp.Forge != nil {
		switch rp.Forge.Provider {
		case "", "github":
			ga := rp.Forge.GitHubApp
			if ga.AppID == 0 || ga.InstallationID == 0 || (ga.PrivateKeyEnv == "" && ga.PrivateKeyRef == "") {
				return fmt.Errorf("actions.revert_pr.forge (github) requires app_id, installation_id, and private_key_env or private_key_ref")
			}
		case "gitlab":
			if rp.Forge.GitLab.TokenEnv == "" {
				return fmt.Errorf("actions.revert_pr.forge (gitlab) requires gitlab.token_env")
			}
			if err := checkSecureKeyEndpoint("actions.revert_pr.forge.gitlab.base_url", "actions.revert_pr.forge.gitlab.token_env", rp.Forge.GitLab.BaseURL, rp.Forge.GitLab.TokenEnv); err != nil {
				return err
			}
		default:
			return fmt.Errorf("actions.revert_pr.forge.provider %q is invalid (want \"github\" or \"gitlab\")", rp.Forge.Provider)
		}
		return nil
	}
	if !rp.ReuseForgeCredentials {
		return fmt.Errorf("actions.revert_pr.enabled requires dedicated actions.revert_pr.forge credentials or explicit actions.revert_pr.reuse_forge_credentials: true")
	}
	switch c.Forge.Provider {
	case "gitlab":
		if c.Forge.GitLab.TokenEnv == "" {
			return fmt.Errorf("actions.revert_pr.reuse_forge_credentials is true but top-level forge.gitlab.token_env is empty")
		}
	default:
		ga := c.Forge.GitHubApp
		if ga.AppID == 0 || ga.InstallationID == 0 || (ga.PrivateKeyEnv == "" && ga.PrivateKeyRef == "") {
			return fmt.Errorf("actions.revert_pr.reuse_forge_credentials is true but top-level forge.github_app credentials are incomplete")
		}
	}
	return nil
}
