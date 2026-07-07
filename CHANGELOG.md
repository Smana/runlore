# Changelog

All notable changes to RunLore are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

RunLore is **pre-1.0 and under active development** — there are no tagged releases
yet, so everything currently lives under `[Unreleased]`.

## [0.7.0](https://github.com/Smana/runlore/compare/v0.6.1...v0.7.0) (2026-07-07)


### Features

* **chart:** StatefulSet mode for real multi-node HA (per-replica storage) ([#263](https://github.com/Smana/runlore/issues/263)) ([f180fd0](https://github.com/Smana/runlore/commit/f180fd06fda2e2e0a7eb556121b3b7660997a5ed))
* **cli:** lore kb search/show — human search over the knowledge catalog ([#260](https://github.com/Smana/runlore/issues/260)) ([6ce138f](https://github.com/Smana/runlore/commit/6ce138f7e650642341257668310f94450469899c))
* **curate:** add skip_verdicts gate so benign findings don't draft KB PRs ([#256](https://github.com/Smana/runlore/issues/256)) ([89ef79f](https://github.com/Smana/runlore/commit/89ef79ff78a9183a476956f5050b773d92ca1041))
* **curator:** Related knowledge section in drafted KB PR bodies ([#259](https://github.com/Smana/runlore/issues/259)) ([f91ddf6](https://github.com/Smana/runlore/commit/f91ddf6d52638b325afe1a0a38aa0fc9ab8ac322))
* **eval:** exercise recall + verify + decay in the replay eval ([11dd107](https://github.com/Smana/runlore/commit/11dd107d3799f023c602cc3eaaec0638d11e18ca))
* **eval:** exercise recall + verify + decay in the replay eval ([efc3a11](https://github.com/Smana/runlore/commit/efc3a119423f8b211a52d4117b7ed2ef083e2246))
* **notify:** quote the prior cause and resolution on recurring incidents ([#261](https://github.com/Smana/runlore/issues/261)) ([8da0e19](https://github.com/Smana/runlore/commit/8da0e192d383bfa2a989250f75e510732bf135f2))
* **outcome:** compact the ledger on load and surface corrupt lines ([801647e](https://github.com/Smana/runlore/commit/801647e0994131742d74be3c4713c5f4ac17dbc9))


### Bug Fixes

* **catalog:** treat fenced code as opaque in Section excerpts ([#262](https://github.com/Smana/runlore/issues/262)) ([02be1c9](https://github.com/Smana/runlore/commit/02be1c9737e1044bdf3bec63dea2bfc18129b3ad))
* **curate:** fall through to Jaccard on divergent open-PR fingerprints ([d106587](https://github.com/Smana/runlore/commit/d1065875ffd26725c1e6da218fd9402a47e98682))
* **curate:** fall through to Jaccard on divergent open-PR fingerprints ([9ee4527](https://github.com/Smana/runlore/commit/9ee452719f694b1d3bf3ca79c1c0e04a40afc50c))
* don't gate readyz on catalog when no model is configured ([#252](https://github.com/Smana/runlore/issues/252)) ([804b6d9](https://github.com/Smana/runlore/commit/804b6d9c8b21585321e6be6cd8dc1724094754d8)), closes [#251](https://github.com/Smana/runlore/issues/251)
* **e2e:** survive Helm 4 SSA + enable the nightly k3d schedule ([#258](https://github.com/Smana/runlore/issues/258)) ([f26d252](https://github.com/Smana/runlore/commit/f26d2522237bf283d03ae5fc07b0ee94403e8c81))
* fall back to max root-cause confidence when overall is omitted ([#253](https://github.com/Smana/runlore/issues/253)) ([d302c39](https://github.com/Smana/runlore/commit/d302c39f019518651bc6b0bf5642e41522f27d50))
* **ha:** rejoin leader election after losing the lease without dying ([#250](https://github.com/Smana/runlore/issues/250)) ([7477e73](https://github.com/Smana/runlore/commit/7477e737154cf392e748caa2b5f7792c8db45b97))
* **investigate:** force a final submit_findings turn at step-budget exhaustion ([#257](https://github.com/Smana/runlore/issues/257)) ([89a131e](https://github.com/Smana/runlore/commit/89a131e3ecad2be3dc58037bad4e240cddb0a4ab))
* **outcome:** capture GitOps and reinvestigate incidents in the ledger ([8cd0a8b](https://github.com/Smana/runlore/commit/8cd0a8b07fc687fdc3070d95cacfbb86be6725c2))
* **outcome:** harden the outcome ledger (GitOps capture, decay poisoning, unbounded growth) ([69d1891](https://github.com/Smana/runlore/commit/69d1891f3f43d2816777d861d6e05afd9f35d46f))
* **outcome:** keep non-resolvable recalls out of decay ([562cd50](https://github.com/Smana/runlore/commit/562cd5082339f1e4fda5272bb871e4b8b7177f9a))
* **recall:** correct outcome-decay factor to documented Beta(1,1) prior ([044bd80](https://github.com/Smana/runlore/commit/044bd803f211f9ecae1bb3fdd347a95437ac9def))
* **recall:** correct outcome-decay factor to documented Beta(1,1) prior ([aa50440](https://github.com/Smana/runlore/commit/aa50440a264be08dd540e0281f5df8bf3ba34e2e))


### Documentation

* fix learning-loop drift against curation and recall code ([89e0cc0](https://github.com/Smana/runlore/commit/89e0cc0b58d6c809a8aefc05012bbaebe9d33574))
* fix learning-loop drift against curation and recall code ([4d845e6](https://github.com/Smana/runlore/commit/4d845e69d4089c86be38021eb2feb58da64166ef))
* **prior-art:** add OpenSRE comparison ([#255](https://github.com/Smana/runlore/issues/255)) ([fa98199](https://github.com/Smana/runlore/commit/fa98199cbe2d011db3fc0eb43d4a5ffe27391abc))
* **readme:** lead with the zero-setup demo before the production install ([#254](https://github.com/Smana/runlore/issues/254)) ([296044f](https://github.com/Smana/runlore/commit/296044fd94a2260b1caf195b04405746df1d065d))

## [0.6.1](https://github.com/Smana/runlore/compare/v0.6.0...v0.6.1) (2026-07-03)


### Bug Fixes

* **whatchanged:** recover the diff when Flux advanced past the break (health-check failures) ([#240](https://github.com/Smana/runlore/issues/240)) ([481f0ee](https://github.com/Smana/runlore/commit/481f0eef100433713ba208683b1ce6070914ed2f))


### Documentation

* refresh Slack notification screenshot (0.6.0) ([#241](https://github.com/Smana/runlore/issues/241)) ([ca396b6](https://github.com/Smana/runlore/commit/ca396b6238d00eb4f2496219755eee75a479ef2f))

## [0.6.0](https://github.com/Smana/runlore/compare/v0.5.0...v0.6.0) (2026-07-03)


### Features

* remove the 👍/👎 verdict-feedback buttons ([#237](https://github.com/Smana/runlore/issues/237)) ([72b089e](https://github.com/Smana/runlore/commit/72b089ebae7c573ce77b1963fbd8c8e9591d8ea7))

## [0.5.0](https://github.com/Smana/runlore/compare/v0.4.0...v0.5.0) (2026-07-03)


### Features

* **kb:** strict OKF entries, deterministic dedup, self-describing bundle ([0491433](https://github.com/Smana/runlore/commit/0491433647d9d9bd26d46f55ddc7da7471f12fe0))
* **kb:** strict OKF entries, deterministic dedup, self-describing bundle ([cf75e45](https://github.com/Smana/runlore/commit/cf75e45c206817452e18e380a6c87ce1806524a2))
* **mcp:** serve the knowledge catalog over MCP (lore mcp) ([b3176d9](https://github.com/Smana/runlore/commit/b3176d9f8ec18a7d1b34219c2734040d429d16e1))
* **mcp:** serve the knowledge catalog over MCP (lore mcp) ([2b2535f](https://github.com/Smana/runlore/commit/2b2535f0e6720de4192250248c4c8e96deeb85b5))
* **mcp:** surface timestamp + fingerprint in kb_get ([4734938](https://github.com/Smana/runlore/commit/47349386902cf9f72e267c2b29617562e316b9b5))
* verdict-first notifications with threading, recurrence and feedback ([#228](https://github.com/Smana/runlore/issues/228)) ([96cec7b](https://github.com/Smana/runlore/commit/96cec7b2351d2b00602c2b63c4d60cc5412967e5))


### Bug Fixes

* **curator:** cap drafted KB entry title at ≤120 chars ([#230](https://github.com/Smana/runlore/issues/230)) ([9487a9b](https://github.com/Smana/runlore/commit/9487a9b59d6a174a5114c8e5bc7e5d66859bc4e4))


### Performance Improvements

* **curate:** fetch closed-unmerged KB PRs via GitHub Search API ([#231](https://github.com/Smana/runlore/issues/231)) ([3ca202b](https://github.com/Smana/runlore/commit/3ca202bf3b198a54854d6872e0b5f7e392d3922d))

## [0.4.0](https://github.com/Smana/runlore/compare/v0.3.0...v0.4.0) (2026-07-02)


### Features

* **curate:** escalate recurring closed-unmerged KB entries via a knowledge-gap issue ([eb3c9a8](https://github.com/Smana/runlore/commit/eb3c9a8ba493c11043092a42adc3948e2118cadc))
* **curate:** escalate recurring closed-unmerged KB entries via knowledge-gap issue ([cdf208f](https://github.com/Smana/runlore/commit/cdf208f4f06fb9a7a5ccf9e625de73bffa291ce6)), closes [#222](https://github.com/Smana/runlore/issues/222)
* **incidents:** add pre-investigation debounce to skip self-resolving alerts ([0524b41](https://github.com/Smana/runlore/commit/0524b41f09ca7d22b61cdd48ef274a5164c0c447)), closes [#221](https://github.com/Smana/runlore/issues/221)
* **incidents:** pre-investigation debounce to skip self-resolving alerts ([d6fdab5](https://github.com/Smana/runlore/commit/d6fdab5fcf57864347d234de30d5fcbaf77903ec))

## [0.3.0](https://github.com/Smana/runlore/compare/v0.2.0...v0.3.0) (2026-07-02)


### Features

* **anthropic:** cache conversation history + report cache tokens ([0b06168](https://github.com/Smana/runlore/commit/0b061686bd7741f868983ac4b2f60979a5115da8))
* **anthropic:** surface sanitized error type/message on non-2xx ([8812f6c](https://github.com/Smana/runlore/commit/8812f6c6783291664b1fb9619c3b1c0ae30fe886))
* **anthropic:** surface sanitized error type/message on non-2xx responses ([ef0ec9f](https://github.com/Smana/runlore/commit/ef0ec9fe44a0e03938d1ca08524d16d21bf0fdfc))
* **app:** wire external MCP tools into the investigation loop ([b46da63](https://github.com/Smana/runlore/commit/b46da63d2b6d747ff59d4c153e8090a0a7893784))
* **audit:** verify chain on open (fail-closed under approve/auto) + lore audit verify ([6fa6b8d](https://github.com/Smana/runlore/commit/6fa6b8df1de3ae9544bbbf315d4b13a9c5f7ae60))
* **config:** add model.max_tokens (default 8192), verify inherits ([145366d](https://github.com/Smana/runlore/commit/145366dc8f6dc92a396951eb9591ed4870cab3d4))
* **config:** mcp.servers (external MCP servers) + validation ([f11f875](https://github.com/Smana/runlore/commit/f11f8751e3c1c112f6891588ce6a5de1a86a7d49))
* **config:** reject a cleartext API key on a public model endpoint ([244ab7d](https://github.com/Smana/runlore/commit/244ab7d447b2fca894ec86d73171a1eba2316c54))
* cross-step prompt caching + cache-token observability ([85309ec](https://github.com/Smana/runlore/commit/85309ecf2f8d0186e47113ed7eba55934bb169be))
* **curator:** host-invariant KB fingerprints — dedup across pods/nodes ([436b5d5](https://github.com/Smana/runlore/commit/436b5d554687129ac8f0c9f25038931d6f949ce6))
* **curator:** host-invariant KB fingerprints (CORE-681) ([22f58e2](https://github.com/Smana/runlore/commit/22f58e21d504754baa1320cf64acdc46ee4ec16d))
* **eval:** multi-model comparison benchmark ([1b02d8e](https://github.com/Smana/runlore/commit/1b02d8ef105822551564eccd5cfa2469f8ec1ad4))
* **eval:** multi-model comparison benchmark ([11f5da0](https://github.com/Smana/runlore/commit/11f5da0e68d3df93679393e9a8c6655f190ea700))
* **gemini:** report cached-content tokens + guard implicit-cache prefix ([d9914a6](https://github.com/Smana/runlore/commit/d9914a636ba1dacb7114ae5d344ead8e940c4f17))
* **helm:** expose pod_log_namespaces (defaults to rbac.controllerLogNamespaces) ([ab31338](https://github.com/Smana/runlore/commit/ab3133892e9a0010c5fee9c54ef1feae945ac007))
* **httpx:** add SSEData SSE-framing primitive ([c790ce3](https://github.com/Smana/runlore/commit/c790ce3e265425ea98c10a730d6f1c5f4365763b))
* **httpx:** streaming-friendly secure client (no flat deadline, idle timeout) ([43ebe3c](https://github.com/Smana/runlore/commit/43ebe3ca3ccc979f809cf12afa6231f2a63ba342))
* **investigate:** compactHistory — elide superseded/old tool outputs ([99b34ad](https://github.com/Smana/runlore/commit/99b34adab3fa9bf6007c59b4e62b4d672113bccd))
* **investigate:** execute a turn's tool calls concurrently ([ab09eb0](https://github.com/Smana/runlore/commit/ab09eb0d2d8aeb5ee81f1f21d82b24459ddffb18))
* **investigate:** execute a turn's tool calls concurrently ([b101178](https://github.com/Smana/runlore/commit/b10117881447beb57af0622f4593340f2b5761ca))
* **investigate:** fail-fast on permanent model errors (CORE-680) ([60545f2](https://github.com/Smana/runlore/commit/60545f2b799d9ebbd9b64f38d285fc440971647d))
* **investigate:** interim progress notifications ([af883cb](https://github.com/Smana/runlore/commit/af883cbd262a626df9618f10bc4464c5535cfb80))
* **investigate:** interim progress notifications ([6317bac](https://github.com/Smana/runlore/commit/6317bac8e18f4a1b5ac06e2c4d16d006aeafd326))
* **investigate:** opt-in LLM-summarization compaction ([c78499a](https://github.com/Smana/runlore/commit/c78499a6b03041e9e7d62c4caeabf949fb4604dc))
* **investigate:** opt-in LLM-summarization compaction ([c396271](https://github.com/Smana/runlore/commit/c39627179d09e3b4000d3f7a670cbd980dfcac10))
* **investigate:** per-investigation token/cost reporting ([19cfe07](https://github.com/Smana/runlore/commit/19cfe07793fc1ff813fa6a76764fd61426880b33))
* **investigate:** per-investigation token/cost reporting ([5900536](https://github.com/Smana/runlore/commit/5900536c6155153a42388f92ee8b8ba0480ee3f9))
* **investigate:** per-tool call timeout so one hung tool can't eat the budget ([33f4688](https://github.com/Smana/runlore/commit/33f46884232c7512c712dab30ec2327918e767bf))
* **investigate:** raise investigation dataset quality (time, attribution, dedup, seed) ([eba2a27](https://github.com/Smana/runlore/commit/eba2a27e0c50db2584bb4ee26301471d65206e9d))
* **investigate:** raise investigation dataset quality (time, attribution, dedup, seed) ([60c0fe0](https://github.com/Smana/runlore/commit/60c0fe008f905d7904669af3bae83ba184437d99))
* **investigate:** treat model refusal as a first-class unresolved outcome ([806ec68](https://github.com/Smana/runlore/commit/806ec68247f01637ef1492b0119725856c0d8db7))
* **investigate:** wire mid-loop compaction before the budget guard ([d85c389](https://github.com/Smana/runlore/commit/d85c3890b2c6a15b9fd56cde5078f2141a5420d3))
* **mcp:** mcpTool adapter (namespaced, bounded description, schema passthrough) ([bd643bb](https://github.com/Smana/runlore/commit/bd643bb4310ab832b94477925454803ebebdaca3))
* **mcp:** outbound MCP client — external MCP servers as read-only investigation tools ([e9421d1](https://github.com/Smana/runlore/commit/e9421d15e6387db12b173789d0c828e82e2237b0))
* **mcp:** streamable-HTTP MCP client (initialize/list/call) ([812ccf5](https://github.com/Smana/runlore/commit/812ccf57b15f114c845753e70de37049af2d6504))
* **metrics,logs:** optional bearer-token + header auth for metrics/logs endpoints ([f23bde4](https://github.com/Smana/runlore/commit/f23bde4eac08a75bbfb96833c455764fd4bdeddb))
* mid-loop tool-output compaction (quality gate pending live-eval) ([0b59fec](https://github.com/Smana/runlore/commit/0b59fec5ddc8769d8fccb4a1d3a28707c1a28d00))
* **model/anthropic:** stream /v1/messages and accumulate; send max_tokens ([1c65eca](https://github.com/Smana/runlore/commit/1c65ecaec71bf45f7a8dcfef4cc37ab9c904bf66))
* **model/gemini:** stream generateContent and accumulate; send maxOutputTokens ([b32104b](https://github.com/Smana/runlore/commit/b32104b38cc8f9c9552a15190bd30377ee53a2d1))
* **model/openai:** stream chat/completions and accumulate; send max_tokens ([2d6205e](https://github.com/Smana/runlore/commit/2d6205e901b4a94a7435c67a04569cd024f0b802))
* **model:** configurable effort/reasoning knob (anthropic, openai) ([63ae11d](https://github.com/Smana/runlore/commit/63ae11d951b141d8472692b8695395d91f26f50a))
* **model:** configurable effort/reasoning knob (anthropic, openai) ([76a3261](https://github.com/Smana/runlore/commit/76a32612dbfdc732e6e6eb57653f9a819f9fa074))
* **model:** opt-in Anthropic adaptive thinking with block replay ([4f6fa59](https://github.com/Smana/runlore/commit/4f6fa593be7cb90a4ed9f19f8f82dd8538dbc9c7))
* **model:** opt-in Anthropic adaptive thinking with block replay ([985c957](https://github.com/Smana/runlore/commit/985c9572aaeff388a0c8bda2a65ee0102397c5a5))
* **model:** support forcing a tool via ToolChoice ([8c15ca4](https://github.com/Smana/runlore/commit/8c15ca41f410baace47d7953173711387c1dc678))
* **model:** support forcing a tool via ToolChoice ([55718e0](https://github.com/Smana/runlore/commit/55718e0dcc489b5e119ca00ebde0bfd4378f3313))
* **openai:** report cached prompt tokens (prompt_tokens_details.cached_tokens) ([9be9f58](https://github.com/Smana/runlore/commit/9be9f5872b4ab9ed5875639433b381c31bd7b50a))
* **providers:** add CompletionResponse.StopReason + Refused() ([e35ee74](https://github.com/Smana/runlore/commit/e35ee74309f86145da4881e17b5f058432947eea))
* **release:** build & sign multi-arch image via GoReleaser ([b77f2e1](https://github.com/Smana/runlore/commit/b77f2e1740c3e956ce6e77f9a6b94e57e915046c))
* **serve:** log successful finding delivery ([2a2d0c9](https://github.com/Smana/runlore/commit/2a2d0c944a4051829cc5a2ecfd8a7157ba347973))
* **source:** PagerDuty V3 webhook source ([5953d92](https://github.com/Smana/runlore/commit/5953d92e433400efb9fdeab03302d7663294f4e9))
* **source:** PagerDuty V3 webhook source ([ca3f638](https://github.com/Smana/runlore/commit/ca3f638fffb33ed05cbc80b2b87281bb338e84c6))
* **telemetry:** history compaction counters ([0a79175](https://github.com/Smana/runlore/commit/0a79175745e72f790cb7eebba28d58bcad23fd7f))
* **telemetry:** record LLM input + cached-input tokens per provider ([ec0867a](https://github.com/Smana/runlore/commit/ec0867ae04ef858dab34587b036c85b773180611))


### Bug Fixes

* **audit:** close verify→append TOCTOU and require audit log for approve ([de39396](https://github.com/Smana/runlore/commit/de3939647f549bfe438d93364dcf74d720511d79))
* case-insensitive severity matching and HTTP handler panic recovery ([b9e8e12](https://github.com/Smana/runlore/commit/b9e8e12d72170e22f4dd2558410679fa46f4fcbb))
* **config:** validate the effective verify endpoint + close cleartext-key gaps ([49c019d](https://github.com/Smana/runlore/commit/49c019dea68ebafbea0b27b6115809e208a46de1))
* **e2e:** audit_log_path for approve-mode install (unbreaks e2e rollout) ([be4d7f5](https://github.com/Smana/runlore/commit/be4d7f580b7aa9b069f7abe2297e7e47cdb4c462))
* **e2e:** set audit_log_path for the approve-mode install ([fdfdb70](https://github.com/Smana/runlore/commit/fdfdb70a3fe0b62d48ec0b1162a22cae5cee725b))
* **e2e:** stream chat completions from the mock (SSE) ([66170ad](https://github.com/Smana/runlore/commit/66170ad1c8594e8ad94e5c461e9b62fa742f8003))
* **e2e:** stream chat completions from the mock (SSE) ([1b427aa](https://github.com/Smana/runlore/commit/1b427aa82aa5586b63473e7367d236660dd988ce))
* harden model API-key egress (cross-host redirect + cleartext base_url) ([a3dfb18](https://github.com/Smana/runlore/commit/a3dfb1836400f4ec66f290bd141cd8f58a8db63c))
* **helm:** bump chart to 0.2.0 and have release-please keep Chart.yaml in sync ([1606686](https://github.com/Smana/runlore/commit/1606686ee98956a8b9230f5f7acd4591625cdaf0))
* **httpx:** strip provider key headers on host-changing redirect ([da2f099](https://github.com/Smana/runlore/commit/da2f09969eda7398996b71baabbaff8ae220f6f4))
* **investigate:** anchor token budget estimate to provider-reported usage ([54e91ca](https://github.com/Smana/runlore/commit/54e91cae81967a99c4d41738493bffe9ef6823f8))
* **investigate:** anchor token budget estimate to provider-reported usage ([2a55535](https://github.com/Smana/runlore/commit/2a5553529d15110d8dd4876c5caad94158fc1e3b))
* **investigate:** constrain pod_logs to the incident namespace plus an allowlist ([0f5239b](https://github.com/Smana/runlore/commit/0f5239bfeb996632d3e9a91f3272e54518ec7ed8))
* **investigate:** per-tool timeout metric, defaults, and validation ([30c7f0a](https://github.com/Smana/runlore/commit/30c7f0a696fe0a3a0932eb09e5c04fba1c7e2c9b))
* **investigate:** redact action Name at the egress chokepoint ([1c32bc3](https://github.com/Smana/runlore/commit/1c32bc3ffda282f5b8ebceecdaf0490082c2ab6c))
* **investigate:** redact action Name at the egress chokepoint ([dfa378e](https://github.com/Smana/runlore/commit/dfa378e64af478b45b994930a442772c82a05959))
* **investigate:** skip compacting tool bodies no larger than the elision marker ([4c435e5](https://github.com/Smana/runlore/commit/4c435e5b88166aedc858bd7f2c3b47cf4796a58e))
* **mcp:** scope retries to discovery + scheme validation + review nits ([6e50bbb](https://github.com/Smana/runlore/commit/6e50bbb932dd2eac750935c87f4cfe89e4b311a4))
* **model:** classify 4xx as permanent and surface sanitized error detail (openai, gemini) ([6814467](https://github.com/Smana/runlore/commit/681446703fbccbcb1dc73c4b00cb3c8d3ab6dd87))
* **model:** classify 4xx as permanent and surface sanitized error detail (openai, gemini) ([d9b9d53](https://github.com/Smana/runlore/commit/d9b9d53abb2f8256dc54786b095fc2852f8173b7))
* **notify:** escape untrusted text in Slack mrkdwn output ([e376c1a](https://github.com/Smana/runlore/commit/e376c1af89bc72598dae7659a1355601f0805ede))
* **notify:** escape untrusted text in Slack mrkdwn output ([bf207d3](https://github.com/Smana/runlore/commit/bf207d3718704d149b4968fd65bf628f3daa0fb4))
* **observability:** bind Grafana dashboard to default datasource ([030bcb0](https://github.com/Smana/runlore/commit/030bcb0b58faee049aa24ebafce0c212fc0b3e77))
* **observability:** bind Grafana dashboard to default datasource ([c26f7c1](https://github.com/Smana/runlore/commit/c26f7c14e37e18c65e2042bcfa1fad8003431058))
* **outcome:** preserve prior ledger cache when a reload read fails ([8d4e31f](https://github.com/Smana/runlore/commit/8d4e31f76ab5e41673a7ccb02170ad8a3169764a))
* **outcome:** re-sync ledger cache on leadership and bound orphan resolves ([b0c472c](https://github.com/Smana/runlore/commit/b0c472c5442b0f277f12b13a5e95097a777f8c64))
* **redact:** mask base64 data values in kind: Secret manifests ([7c35337](https://github.com/Smana/runlore/commit/7c35337eafa1349c6d418a83ebdca8f616b35926))
* satisfy linters — rename max const, escape ESC in test ([ad8a6dc](https://github.com/Smana/runlore/commit/ad8a6dccd2059eb8954d8796005955715350d784))
* **server:** recover from handler panics ([502586f](https://github.com/Smana/runlore/commit/502586f89d9765a552a64a25a284d42b71e614c4))
* **trigger:** match severity case-insensitively ([fd2cced](https://github.com/Smana/runlore/commit/fd2ccedb4777631ab1224d8adb15fc8a3d305fae))


### Performance Improvements

* **outcome:** cache ledger aggregates instead of replaying the JSONL per recall ([b1e0367](https://github.com/Smana/runlore/commit/b1e03671e8b3180b203a813946dc685f3adf6c4f))


### Documentation

* **config:** note the https requirement on model.base_url with a key ([a83db79](https://github.com/Smana/runlore/commit/a83db79c67fbc8e7c50d5814391c1def873f3641))
* design spec for cross-step prompt caching + cache observability (T1+T2) ([1495626](https://github.com/Smana/runlore/commit/14956266fe5abe1e0a0b70c75f13f1f12e6d56a0))
* design spec for mid-loop tool-output compaction (T3, hardened + eval-gated) ([b0a25a5](https://github.com/Smana/runlore/commit/b0a25a57bdbb3b45a1217dc76be83835a0b62bb7))
* design spec for model API-key egress hardening (S1 redirect key-strip + S2 cleartext-key config guard) ([1c14c18](https://github.com/Smana/runlore/commit/1c14c1898bf6eb41779a6b6dbbd31e8664962696))
* design spec for outbound MCP client (E1, HTTP transport, namespaced/defense-in-depth) ([a1a9e6e](https://github.com/Smana/runlore/commit/a1a9e6e87ea0719fbc7b7ced86491e3b8bc0563d))
* document model.pricing and investigation.progress_updates ([7ee383b](https://github.com/Smana/runlore/commit/7ee383b89be80496b9947cbe288e342e4eddf568))
* document model.pricing and investigation.progress_updates ([1ce457b](https://github.com/Smana/runlore/commit/1ce457b193520be2f9896798a9e04043a6363e79))
* **getting-started:** align with fail-closed webhook auth and required resource frontmatter ([89b61de](https://github.com/Smana/runlore/commit/89b61debf8deb99228864037c75a9444b5c90512))
* implementation plan for cross-step prompt caching + observability ([016b5e2](https://github.com/Smana/runlore/commit/016b5e223b2f97ef23315421ab03d9c0fa6746bf))
* implementation plan for mid-loop tool-output compaction ([8bd0c2f](https://github.com/Smana/runlore/commit/8bd0c2fc7507763165e6c52e950026d16db37fc6))
* implementation plan for model API-key egress hardening (S1+S2) ([8490154](https://github.com/Smana/runlore/commit/8490154115fc4060fca944bd395a5a9776ad9da8))
* implementation plan for outbound MCP client (E1) ([65ec4ed](https://github.com/Smana/runlore/commit/65ec4eda930efb781d923b4dd1fdda2dd60cb30c))
* **model:** spec + implementation plan for model-layer streaming/refusal ([e00997c](https://github.com/Smana/runlore/commit/e00997cba8821f8d805e0214c3153eed97a007bd))
* refine T3 keep-list (add engine-agnostic gitops_resource_status/gitops_tree) + defer eval to EKS ([53a483a](https://github.com/Smana/runlore/commit/53a483a424aaea77dbb288acb471b86ffdbd6afb))
* security architecture overview ([f1fd740](https://github.com/Smana/runlore/commit/f1fd740589f50b5edf3d7cda15b233ba04c87a20))
* security architecture overview ([4b3bca1](https://github.com/Smana/runlore/commit/4b3bca1cca21e872ccc19ceac4a916942a1a1273))


### Code Refactoring

* **mcp:** apply /simplify cleanups (reuse, dedup, altitude) ([e175988](https://github.com/Smana/runlore/commit/e17598853d4dced6bea17d03468fad6d85b9512a))
* **model:** extract shared client core (retry/SSE/error plumbing) ([f4f3558](https://github.com/Smana/runlore/commit/f4f3558fb7828f75e32b7b20657c12fe8f7b7663))
* **model:** extract shared client core (retry/SSE/error plumbing) ([851e526](https://github.com/Smana/runlore/commit/851e526f2b2a582e933854c99defcfe26147ea96))


### Continuous Integration

* **build-image:** slim to PR/main validation only ([95caf1e](https://github.com/Smana/runlore/commit/95caf1e94ec61c89418c805596522ad1c6faaa7b))
* **e2e:** run the k3d e2e suite via workflow_dispatch ([b180f4d](https://github.com/Smana/runlore/commit/b180f4d58bbb04f1f1a230dfefe16f065f337bdb))
* **helm:** add chart lint + kubeconform + values schema ([3301f38](https://github.com/Smana/runlore/commit/3301f386d35b8ae517fc89a876079120dbf12742))
* skip nightly eval when API key absent; add gofmt+race checks; pin govulncheck ([f0e28bf](https://github.com/Smana/runlore/commit/f0e28bf0ba05f9efe1baa7463115217bc0c04c75))

## [0.2.0](https://github.com/Smana/runlore/compare/v0.1.0...v0.2.0) (2026-06-28)


### Features

* **config:** sources.&lt;name&gt; enablement map (pragmatic break; auth stays server-level) ([06161b5](https://github.com/Smana/runlore/commit/06161b5e49e5d292b6f991b4667c359aaab00e56))
* **curator:** surface the observed root cause in the coalesce comment ([d193989](https://github.com/Smana/runlore/commit/d1939897355288a34c3cc38c2f7e1c9e77fe5637))
* **curator:** surface the recurring incident's root cause in the coalesce comment ([2519d2b](https://github.com/Smana/runlore/commit/2519d2b10732999d699233d69ab139f754fe761b))
* **investigate:** add pod_logs tool with previous-container crash logs ([f768eeb](https://github.com/Smana/runlore/commit/f768eeb7658666e03d66031456ba1161240ce5ae))
* **investigate:** add query_metrics_range tool for incident-window metric trends ([51ce3fc](https://github.com/Smana/runlore/commit/51ce3fc8a78da21f005c3fe212b58159856eee72))
* **investigate:** incident-window metric trends + crashing-pod log reader ([09ac1fb](https://github.com/Smana/runlore/commit/09ac1fb0d1cd101afcd6a0771c0a91ab39607be7))
* **investigate:** promote Severity/Environment onto Request ([912200b](https://github.com/Smana/runlore/commit/912200b545acdcce3d60f0218fce0cb881eec2ce))
* **notify:** generic outgoing-webhook sink (proves drop-in extensibility) ([47f560c](https://github.com/Smana/runlore/commit/47f560cd2186fc40f0a651aa440eec87760238a4))
* **notify:** notifier registry; retrofit slack/matrix (typed) + inline extras map ([8017ae4](https://github.com/Smana/runlore/commit/8017ae41061b0066683ad34719a43f14ce60f273))
* **source:** adapter registry (Descriptor, Register, BuildEnabled) ([6ab9df4](https://github.com/Smana/runlore/commit/6ab9df4bebedf5d560bdc6772204144e85b8a31d))
* **source:** alertmanager webhook adapter (golden-tested Decode) ([c10277a](https://github.com/Smana/runlore/commit/c10277a9e747153af895118b15e23be7e034d6e0))
* **source:** gitops failure watcher adapter; export IsCascadeFailure ([5998d1f](https://github.com/Smana/runlore/commit/5998d1fd2f958a7fcb7389c8dd9525f5ace6b9aa))
* **source:** ingest pipeline (admission, dedup, ledger routing) ([f2813ef](https://github.com/Smana/runlore/commit/f2813efe067e91a8915af5db291535a9583bbb9b))
* **source:** webhook + watcher transports (RunWatchers preserves gitops dedup+debounce) ([a9ac8d9](https://github.com/Smana/runlore/commit/a9ac8d938491fc37adef3cf90398b4ea0d20bfd5))
* **source:** wire sources through server+main; remove bespoke alert/gitops paths ([f64a097](https://github.com/Smana/runlore/commit/f64a097caf7e6106f43b684e198f3b48c1a7c7a5))
* **trigger:** MatchRequest matcher over normalized Request ([7effa22](https://github.com/Smana/runlore/commit/7effa226b84d41452433b123337d02a450fcbc58))


### Bug Fixes

* **app:** pass Raw: cfg.Sources to source.BuildEnabled (port regression); test alert TriggerKey ([7973d84](https://github.com/Smana/runlore/commit/7973d844342c46c9f93ea469a1d7c8e4563e3539))
* **ci:** publish cosign bundle as the signature artifact ([008f1cf](https://github.com/Smana/runlore/commit/008f1cfeaad43ebc3841f7521e3e887289bcd56d))
* **ci:** publish cosign bundle as the signature artifact ([e8127fc](https://github.com/Smana/runlore/commit/e8127fcf43f9dca46e38048da461d6fed978c869))
* **ci:** skip branch-prefixed SHA image tag on PRs (invalid :-&lt;sha&gt;) ([574cda6](https://github.com/Smana/runlore/commit/574cda6db25a79565039343549505211efd1ccf1))
* **cluster:** order warn-only events by their own timestamp ([0dcbc56](https://github.com/Smana/runlore/commit/0dcbc56fe7b37888a42eb3aa1d18cee93058435a))
* **curator:** scope the coalesce note's 'trigger not cause' to alert/GitOps ([1d59b55](https://github.com/Smana/runlore/commit/1d59b55fea173a26ea5d34bdd86ce337f815f1ad))
* **demo:** repair zero-cluster quickstart broken by sources migration ([4ce6f02](https://github.com/Smana/runlore/commit/4ce6f02831c69f8b9116b7967841b856718fe7de))
* **demo:** repair zero-cluster quickstart broken by sources.&lt;name&gt; migration ([36348ef](https://github.com/Smana/runlore/commit/36348ef6af5bec589cb4fe92dbd20fd11c4ce8e7))
* **investigate:** pod_status falls back to namespace when a selector matches nothing ([#165](https://github.com/Smana/runlore/issues/165)) ([d9c4584](https://github.com/Smana/runlore/commit/d9c45844ce1b5e9d5373e2eaf67b754047e11325))
* **observability:** restore per-incident decision + gitops-watch startup logs ([434c73c](https://github.com/Smana/runlore/commit/434c73c7455c9203669d5e03f75247074ad462c2))
* **source:** alertmanager resolution uses receipt time; tighten tests ([85d580f](https://github.com/Smana/runlore/commit/85d580fccb38fb4d69c398a923381d638df17ca2))
* **source:** preserve dedup behavior (drop floor); harden admit + dedupKey ([a65686d](https://github.com/Smana/runlore/commit/a65686d8f61e4cbe8547251b20aa2f828d9b75ec))
* **source:** reject unknown keys under `sources:` instead of silently ignoring ([da7d040](https://github.com/Smana/runlore/commit/da7d040baba2b8570b1a12c7cdbcade1366114a4))
* two pre-launch correctness bugs (warn-event ordering, silent source-key typo) ([5dc160d](https://github.com/Smana/runlore/commit/5dc160dc24a0e42aaf11592ff0e64123ff554882))


### Documentation

* add operability & config guides (troubleshooting, upgrade/uninstall, configuration, security model) ([2d3af2b](https://github.com/Smana/runlore/commit/2d3af2b950ed5652eca287353b0e77705cbdc613))
* **analysis:** mark query_metrics_range and pod_logs as implemented in this branch ([4b7ae84](https://github.com/Smana/runlore/commit/4b7ae84eabb8d3fe727848546c7e809efce56902))
* **analysis:** per-integration troubleshooting methods, signals, and gaps ([e376a50](https://github.com/Smana/runlore/commit/e376a50484e692ced34d8096e672092b2a2d8df5))
* **analysis:** recall & dedup behaviour when a symptom recurs with a different RCA ([5556ffa](https://github.com/Smana/runlore/commit/5556ffac9af5fb1f08c398acfbcdb1d11256d61c))
* **architecture:** document source/notifier adapter extensibility ([264bcb8](https://github.com/Smana/runlore/commit/264bcb8dd092f1d85f502daabc7974619d59d232))
* **examples:** add worked end-to-end example (Harbor registry down) ([#166](https://github.com/Smana/runlore/issues/166)) ([8824074](https://github.com/Smana/runlore/commit/882407430afa3c0bd4837069c85222c27eea930f))
* **getting-started:** document webhook bearer auth + ingress lockdown ([af96e6a](https://github.com/Smana/runlore/commit/af96e6a8d767325cf7c3179b66353ec998b60593))
* operability & config guides (troubleshooting, upgrade/uninstall, configuration, security model) ([6a636ba](https://github.com/Smana/runlore/commit/6a636ba9c1abf28cd11905dd2e4ad288e4e0a14e))
* **plan:** extensible sources & notifiers implementation plan ([c0429d6](https://github.com/Smana/runlore/commit/c0429d6671344876848388dca9e1bc83dc7186f5))
* **readme:** add a Supported integrations matrix; dedupe install sentence ([b4bb674](https://github.com/Smana/runlore/commit/b4bb674ac1ce49d15f31565c4343597a95141733))
* **readme:** pluggable sources/notifiers; SRE audience; Argo CD e2e-tested ([77b039d](https://github.com/Smana/runlore/commit/77b039d28b1b259c0b59bb5cfa36fb844d6c82bd))
* **readme:** pluggable sources/notifiers; SRE audience; Argo CD e2e-tested ([5f396e6](https://github.com/Smana/runlore/commit/5f396e69a633881c72fd4d84e6f279feee78d0ae))
* **source:** fix Deps.Raw keying comment; drop dead Descriptor.ConfigKey ([8e7c415](https://github.com/Smana/runlore/commit/8e7c4157d26fe2dfded197e3fa8f746dd163fdd3))
* **spec:** extensible source & notifier adapter registries ([25a0220](https://github.com/Smana/runlore/commit/25a02201866640c5b258f5e926837a45f35004b2))


### Code Refactoring

* **app:** port source/notifier wiring onto internal/app; reconcile [#137](https://github.com/Smana/runlore/issues/137) TriggerKey ([0e53c3a](https://github.com/Smana/runlore/commit/0e53c3a0c665c654cba6e9d513c22fdc7bd78275))
* **coalesce:** operate on investigate.Request (+Enqueue); keep alert path green ([f1b6276](https://github.com/Smana/runlore/commit/f1b6276983a3de00550226a3c4486d5165f90706))
* extensible source & notifier adapters (typed registries) ([050a962](https://github.com/Smana/runlore/commit/050a962247546faf4ba8d22643269af5cde08dac))
* **providers:** take a PodLogQuery struct, not a 5-arg bool, for PodLogs ([b347fc0](https://github.com/Smana/runlore/commit/b347fc0b4c4b7c95d78934bedbb432b4d9468581))
* retire config.Incident + dead trigger.Engine; alertmanager adapter owns AM parsing ([ee60686](https://github.com/Smana/runlore/commit/ee60686516ffe74fa179147e241afcfcc1f27ef2))

## 0.1.0 (2026-06-26)


### Features

* **action:** autonomy ladder rung 1 — propose envelope-filtered remediations (never executed) ([83156d5](https://github.com/Smana/runlore/commit/83156d5e975c9693b51dee5b849d292c68e0f7df))
* **action:** autonomy ladder rung 2 — approval-gated execution of reversible Flux ops ([f32d89b](https://github.com/Smana/runlore/commit/f32d89b991c12fc791b7e6ed40ac6c9187768b5d))
* **action:** autonomy ladder rung 3 (auto) with layered safety controls ([516230f](https://github.com/Smana/runlore/commit/516230f8c347837315c8d0d883ff7b58ef0c785f))
* Argo CD GitOps provider (engine parity with Flux) ([af5e071](https://github.com/Smana/runlore/commit/af5e07127739e946facf4a739e7566d37f477efd))
* **argocd:** GitOpsInspector parity — deep status + dependency tree (FEAT-2) ([4b72418](https://github.com/Smana/runlore/commit/4b724184d91fca4d305fb072de001633929901ff))
* **argocd:** GitOpsProvider for Argo CD (Applications -&gt; changes + Degraded failures) ([b7e8799](https://github.com/Smana/runlore/commit/b7e8799f002ca02360ef988068441f1b5096a476))
* **argocd:** implement GitOpsInspector — deep status + dependency tree (FEAT-2) ([11bbb54](https://github.com/Smana/runlore/commit/11bbb5484d088f2114f85fb688e03a41c6c45bf5))
* **argocd:** map multi-source apps + trigger on failed sync operation (R24) ([e97863d](https://github.com/Smana/runlore/commit/e97863d54836d90de1ce2697dabf4d1caadab104))
* **audit:** fsync each log write so the chain tail survives a crash ([5c92a0b](https://github.com/Smana/runlore/commit/5c92a0b6357fac92289601a242db23012bb3b398))
* autonomy ladder rung 1 (suggested actions) ([9c8ebc2](https://github.com/Smana/runlore/commit/9c8ebc20710cb35d4edcce213be30a9900c4ce15))
* autonomy ladder rung 2 (approval-gated execution) ([5493f4d](https://github.com/Smana/runlore/commit/5493f4d1619d92c38ea407c2f2cdebd5f66f49ad))
* autonomy ladder rung 3 (auto) with layered safety design ([7cefa94](https://github.com/Smana/runlore/commit/7cefa944cb88514311c068b0657a098ac3d193f8))
* **aws:** paginate CloudTrail LookupEvents + signal truncation ([aed58e7](https://github.com/Smana/runlore/commit/aed58e74d573e0bce954caf96d66ed1c28954ae8))
* **aws:** paginate ResourceHealth nodegroups + ASGs + signal truncation ([c391882](https://github.com/Smana/runlore/commit/c391882f3dc44ad96c62999aa38827eed892e0b2))
* **catalog/server:** HEAD-diff sync gating + readyz catalog-warmth gate ([#18](https://github.com/Smana/runlore/issues/18)) ([#82](https://github.com/Smana/runlore/issues/82)) ([f23edcf](https://github.com/Smana/runlore/commit/f23edcf67aab41a74b5014aa34f07335ae6b417f))
* **catalog:** git-sync — close the read/write loop ([0d10d50](https://github.com/Smana/runlore/commit/0d10d50e877254837269b6c74a40a54869297640))
* **catalog:** git-sync — reloadable index + Syncer, wired into serve (closes read/write loop) ([e1975c1](https://github.com/Smana/runlore/commit/e1975c12bd5ab9c6dde02b8418bf9f8ab4d22d07))
* **catalog:** hybrid BM25 + embedding retrieval (opt-in) — M4 pt2 [1/3] ([2e98362](https://github.com/Smana/runlore/commit/2e98362feba469d6698f8a39afc7af95351bda03))
* **catalog:** in-memory bleve index + Search ([9478531](https://github.com/Smana/runlore/commit/9478531a7743f1d67b87a1e74965fba60cad16ca))
* **catalog:** load OKF entries (frontmatter + body) ([f7aadb4](https://github.com/Smana/runlore/commit/f7aadb4c92036348a70a74575a5b15843c62647b))
* **catalog:** OKF catalog read + kb_search (Learn pillar, read half) ([e4d7406](https://github.com/Smana/runlore/commit/e4d740693785d1a1089be3dc094dec3a1e885d65))
* **chart:** catalog git-sync writable mirror volume ([2e3eb25](https://github.com/Smana/runlore/commit/2e3eb25e319a4475b4fc3dd35a9e193bb0f9ba6d))
* **chart:** HA — lease RBAC, downward env, /readyz probe, PDB, 2 replicas ([2a52f93](https://github.com/Smana/runlore/commit/2a52f9388ae8f7c836ef4cf8c855ec8890c051af))
* **chart:** scope DNS egress + opt-in strict deny-by-default NetworkPolicy ([cc312b6](https://github.com/Smana/runlore/commit/cc312b68b62e1f05251d3bd67d0c9f146fd34ecf))
* **cli:** implement 'lore catalog sync' — one-shot clone/pull + index ([6b72661](https://github.com/Smana/runlore/commit/6b726617c07588f6122d8a94ce4127ef519f4591))
* **cli:** implement 'lore investigate' — on-demand terminal investigation ([55da3d1](https://github.com/Smana/runlore/commit/55da3d19232aaa29a02c395f48ad54c2dd446617))
* **cli:** lore catalog sync (clone/pull + index the catalog) ([944bba2](https://github.com/Smana/runlore/commit/944bba28d7984e83abac2ab9b2f8623a108a374a))
* **cli:** lore investigate (on-demand terminal investigation) ([fbb303e](https://github.com/Smana/runlore/commit/fbb303e22d83e041e655c5b70a2ec6a8fd14ed62))
* **cloud:** AWS provider — CloudTrail what-changed + EC2/ASG/EKS health ([#55](https://github.com/Smana/runlore/issues/55)) ([456b83d](https://github.com/Smana/runlore/commit/456b83d4c11007cd236274afb5b801f6c2d954ca))
* **coalesce:** Add — buffer, cooldown suppression, critical fast-path ([24c59bf](https://github.com/Smana/runlore/commit/24c59bfb4c2e349567474b8b035b9190077eacc4))
* **coalesce:** correlation key + batch summary ([5f72680](https://github.com/Smana/runlore/commit/5f7268016d402d94e90e45f2882f721f161cfa57))
* **coalesce:** debounce/max-wait sweeper ([5f84832](https://github.com/Smana/runlore/commit/5f8483200e2c1f37dcc82134f249cd7ac3e710b2))
* **config:** add investigation (coalesce/rate-limit/token) + telemetry config ([62bb04a](https://github.com/Smana/runlore/commit/62bb04a917c5e0407fb67744efc3237441c69d40))
* **config:** instant_recall outcome_prior/outcome_floor knobs ([b4db0db](https://github.com/Smana/runlore/commit/b4db0db402dc70231839854e827626ada44eda77))
* **config:** wire per-investigation timeout (default 10m, tunable) ([4112a2d](https://github.com/Smana/runlore/commit/4112a2d96d275c0ca4b4a183bb93cb7f35cbcd16))
* **curate:** grooming passes — dedup, resolution-gated queue, recurrence-&gt;gap-issue, lifecycle ([7e4d294](https://github.com/Smana/runlore/commit/7e4d294e2312a4543a0a058ce9af50ced11fd91e))
* **curate:** orchestrator (Agent/Pass) + lore curate subcommand (dedup pass wired) ([cfffcc5](https://github.com/Smana/runlore/commit/cfffcc5f3e79bf2c3fba21f04cacc4971d8fd1da))
* **curate:** scheduled backlog groom — lore curate CronJob + lifecycle sweep ([#12](https://github.com/Smana/runlore/issues/12)) ([#86](https://github.com/Smana/runlore/issues/86)) ([9bab72c](https://github.com/Smana/runlore/commit/9bab72cbca5a1655bb5f6c0afb3fb146d04de111))
* **curate:** warn when the outcome ledger is missing under lore curate ([f9755e2](https://github.com/Smana/runlore/commit/f9755e271e4ea9c9485bfbd03a54349c9ee5e9ee))
* **curate:** wire Queue + Recurrence Phase-2 passes (ledger-backed, source-neutral) ([#92](https://github.com/Smana/runlore/issues/92)) ([9a65d1d](https://github.com/Smana/runlore/commit/9a65d1d5a1749a651caddcb950ecb5c004eb3a90))
* **curator:** apply KB lifecycle labels (triggered) on curated issues + PRs ([#52](https://github.com/Smana/runlore/issues/52)) ([d4a9d8e](https://github.com/Smana/runlore/commit/d4a9d8ece6d22c4a4e089147f5693c82bdfdbb8f))
* **curator:** confidence-routed curation (PR draft vs issue) + KBEntry drafting ([8190853](https://github.com/Smana/runlore/commit/8190853af475e7dc926490831b3c5ad7912a875a))
* **curator:** deterministic dedup fingerprint replaces title-equality ([#11](https://github.com/Smana/runlore/issues/11)) ([#83](https://github.com/Smana/runlore/issues/83)) ([1778e62](https://github.com/Smana/runlore/commit/1778e6277db451365de71b5f0402b7d251a4ed9f))
* **curator:** fingerprint + catalog novelty check ([bf95643](https://github.com/Smana/runlore/commit/bf956439920d4fed4fb7ebe7cec7ca3a33fd36f6))
* **curator:** merge-ready OKF drafter (decision card + Symptom/Investigate/Cause/Resolution) ([63695ea](https://github.com/Smana/runlore/commit/63695ea1b1b9c58b96313c8261ac9685bb727dc2))
* **curator:** pass metrics into the curator at build time ([f76499b](https://github.com/Smana/runlore/commit/f76499b768f67409b42dcc2fca466b2b655fd74b))
* **curator:** record dedup top-hit score on every Curate ([f2d7e4d](https://github.com/Smana/runlore/commit/f2d7e4d7b12ed1242561634d322a9cd4602dcf23))
* **curator:** require Verified + provenance in the merge bar ([#16](https://github.com/Smana/runlore/issues/16)) ([#85](https://github.com/Smana/runlore/issues/85)) ([d1eb3a0](https://github.com/Smana/runlore/commit/d1eb3a0c6bbdabf87da9c53faae2c8b9b4162696))
* **curator:** skip curation of recalled findings (a cache hit is not novel) ([ca9b70a](https://github.com/Smana/runlore/commit/ca9b70a99927df252fd16ed09bbdeacc3103414d))
* **curator:** store the originating resource on curated entries (enables structural recall) ([dbcdca8](https://github.com/Smana/runlore/commit/dbcdca83b68e0d239c1d3427d9481dcacb9c7971))
* **curator:** three-step file-time gate (dedup -&gt; quality -&gt; PR); delete issue branch ([55ebbbe](https://github.com/Smana/runlore/commit/55ebbbe7b99e271fd80f85f9638d5960b38f48aa))
* **curator:** wire catalog dedup + thresholds into buildCurator ([302bdff](https://github.com/Smana/runlore/commit/302bdff81206ee36118c74bf9698806eb5c52974))
* **curator:** write findings back to the forge (Learn pillar, write half) ([a51c8a3](https://github.com/Smana/runlore/commit/a51c8a310665a0bb3df658c98c4b615637be1289))
* **deploy:** Dockerfile + Helm chart + k3d e2e harness ([a273ef9](https://github.com/Smana/runlore/commit/a273ef936a85a70b2ca893fb5b0bf6e4319713bd))
* **deploy:** helm investigation/telemetry values + VMServiceScrape + storm e2e step ([487da2c](https://github.com/Smana/runlore/commit/487da2c71b9e8a9f3d40b1471bfb817f2ca23bb0))
* **deploy:** package (Dockerfile + Helm chart) + k3d e2e (13/13 features verified) ([c322790](https://github.com/Smana/runlore/commit/c322790d6e0b8793d6de740871fc5684176f1c67))
* **embed:** hybrid-retrieval foundation — embeddings + cosine + RRF (M4 part 1) ([a68d2a7](https://github.com/Smana/runlore/commit/a68d2a72d68a0b753235ad7f6f75cf2885e433b2))
* **eval:** deterministic coverage track (recordingTool + tool-&gt;source map) ([348ced1](https://github.com/Smana/runlore/commit/348ced1394f8a48bfc4e3815554d50611b028fd9))
* **eval:** deterministic entity precision + over-claim penalty in Track A ([d495646](https://github.com/Smana/runlore/commit/d4956463b4edb99ce5fb2dacd53d9c2b43066e40))
* **eval:** deterministic entity precision + over-claim penalty in Track A replay scoring ([0e9f532](https://github.com/Smana/runlore/commit/0e9f532899c10bf9063833e338b15f47347618a5))
* **eval:** exercise the instant-recall short-circuit (was dropped) + poisoned-entry proof    Body: ([b81fc00](https://github.com/Smana/runlore/commit/b81fc00e9d8ba96edc6aeef46727d48bfed1b84a))
* **eval:** exercise the instant-recall short-circuit + poisoned-entry proof ([5d1e699](https://github.com/Smana/runlore/commit/5d1e6990a6ecabeb8ec32d5239a6392f98413ca8))
* **eval:** k-of-n pass gate + flaky-fail + N≥10 ([c834357](https://github.com/Smana/runlore/commit/c83435797712faafef6fef40a443b5f6076b44e3))
* **eval:** k-of-n pass rule + flaky-variance fail (was bare median) ([a0f8d7b](https://github.com/Smana/runlore/commit/a0f8d7ba30be69cf8813e3997ff6ae4224629f82))
* **eval:** live runner — setup/run-N/judge/teardown + median/variance + pass gate ([0ec61a7](https://github.com/Smana/runlore/commit/0ec61a72282fb2ee507d65a159eee0a6962d30b4))
* **eval:** live-fire eval harness — lore eval --live ([574bfc0](https://github.com/Smana/runlore/commit/574bfc0d729da4db355c228beb27e3a74634f54a))
* **eval:** live-fire report (markdown + JSON, heatmap, regression diff) ([34d96ec](https://github.com/Smana/runlore/commit/34d96ec27759a0677547d32a57a220fe494b6684))
* **eval:** live-fire Scenario schema + loader ([0e34c1a](https://github.com/Smana/runlore/commit/0e34c1a75967384cff826650ad9e867b586e9887))
* **eval:** LLM-judge RCA-quality rubric (blind, structured verdict) ([3574e57](https://github.com/Smana/runlore/commit/3574e574a23555ad07cf0e4c04857e4ccac1e733))
* **eval:** lore eval --live — wire live-fire runner + judge + report into the CLI ([f680f08](https://github.com/Smana/runlore/commit/f680f08ce031eac53fced992541e5fcddb94fc05))
* **eval:** RCA benchmark harness — replay incident cases, score root-cause identification ([5e38d6b](https://github.com/Smana/runlore/commit/5e38d6b53edd8321f1033a4d12c2dd8af7632d66))
* **eval:** RCA benchmark harness (lore eval) ([b884b5c](https://github.com/Smana/runlore/commit/b884b5c0ada0287a7ae1b27715020d63cd5da22b))
* **eval:** record live runs into the replay Case corpus ([6b919b2](https://github.com/Smana/runlore/commit/6b919b255b0062fe4a5414428bb45ee8d61554d9))
* **eval:** report states N + pass-rate; FLAKY status; default N=10 ([3e9afee](https://github.com/Smana/runlore/commit/3e9afee8d5dd94afad64bf916faddca9bd64bbe1))
* **eval:** wire 'lore eval' + ship a sample case ([1562d21](https://github.com/Smana/runlore/commit/1562d21065aaf4c66d4d22c553c2c5f32d045b32))
* **eval:** wire recall into the live-eval runner (was dropped) ([d3e5059](https://github.com/Smana/runlore/commit/d3e5059babc5e4e547437499cc8626998e2cf189))
* **eval:** wire replay eval into CI — nightly k-of-n + fail-under gate ([#7](https://github.com/Smana/runlore/issues/7)) ([#81](https://github.com/Smana/runlore/issues/81)) ([2773dd4](https://github.com/Smana/runlore/commit/2773dd4cced5bfe7080930d2f907368b3df50bfe))
* **forge/github:** IssueProvider over GitHub REST (issue + PR draft) ([c3c6962](https://github.com/Smana/runlore/commit/c3c6962cf983c9d17b5a56607b119d3199099bf1))
* **forge:** Close (issue/PR) ([31004a2](https://github.com/Smana/runlore/commit/31004a26d46537207866324c0448424c4996bfaa))
* **forge:** configurable GitHub API URL (GHES support); cover token exchange ([c3c3c7b](https://github.com/Smana/runlore/commit/c3c3c7b25fccb317f175d5c3c2f82804a45157aa))
* **forge:** configurable GitHub API URL + curator e2e (+ title fix) ([1ee504c](https://github.com/Smana/runlore/commit/1ee504c966b39130fe1f576e31c866b399b20fc7))
* **forge:** ListPRsByLabel (open KB PRs, for dedup) ([dbe451e](https://github.com/Smana/runlore/commit/dbe451e701984fc9ea590597a9676b8ebd42f801))
* **gcpfirewall:** follow NextPageToken + signal truncation ([6d26d79](https://github.com/Smana/runlore/commit/6d26d7920db110d54c7a391347e0dcfc85a32a62))
* **helm:** add configurable probes block to values (startup/liveness/readiness) ([fc640c6](https://github.com/Smana/runlore/commit/fc640c67af36fa31d2dcf795061b537b009e5956))
* **helm:** document instant-recall trust knobs ([f6b5d3c](https://github.com/Smana/runlore/commit/f6b5d3cff642bf34fe4d17ee731870012e0f1896))
* **helm:** opt-in RWX persistence for ledger+audit; fsync audit writes ([640062b](https://github.com/Smana/runlore/commit/640062b8fa733ee2b6a990975d33045c54c1bb54))
* **helm:** opt-in RWX persistent volume for ledger + audit log ([af9040b](https://github.com/Smana/runlore/commit/af9040ba65a5273383a5ba94ac8649099a558914))
* **helm:** optional NetworkPolicy ingress scope (default permissive) ([e8aa2b5](https://github.com/Smana/runlore/commit/e8aa2b52e30e41362c622ab510b0309b67151f0c))
* **helm:** ship bounded cost-control defaults out of the box ([a5e6e5e](https://github.com/Smana/runlore/commit/a5e6e5e2bd8067d3f0387fb5e84191593a7b66d2))
* **helm:** startupProbe + configurable, nil-safe liveness/readiness probes (R21) ([c5ec601](https://github.com/Smana/runlore/commit/c5ec6015bcac13eba4cb062d2def1730bd00f290))
* **httpx:** add SanitizeHeader + RequestID for safe upstream correlation in logs ([3d265a7](https://github.com/Smana/runlore/commit/3d265a727fc8985c7ab0f8c13de0b75ae6eb9ea5))
* **httpx:** honor Retry-After / retry-after-ms on 429 in DoWithRetry ([6a37e7a](https://github.com/Smana/runlore/commit/6a37e7a0f5d277653a6df3b5a3b8e97dfdc501ff))
* instant recall (short-circuit on a high-confidence catalog hit) ([ebb1f3b](https://github.com/Smana/runlore/commit/ebb1f3bb3766484df1039dcaa4ed77b74a854be7))
* **investigate:** add controller_logs tool + RBAC for deep introspection ([#47](https://github.com/Smana/runlore/issues/47)) ([befe425](https://github.com/Smana/runlore/commit/befe425b6aba54c4c9a80a48452912c006fa7daf))
* **investigate:** cap investigation starts per window with backoff overflow ([b4c9abc](https://github.com/Smana/runlore/commit/b4c9abc2c81660d759caf86160beaa399e5909ae))
* **investigate:** confront recalled findings with current cluster state before verify ([#13](https://github.com/Smana/runlore/issues/13)) ([#84](https://github.com/Smana/runlore/issues/84)) ([f915d00](https://github.com/Smana/runlore/commit/f915d00d20d93d851bdcc2d6ca7c39702e45f8f4))
* **investigate:** correctness — rigor prompt + adversarial verify pass ([#51](https://github.com/Smana/runlore/issues/51)) ([8ed7454](https://github.com/Smana/runlore/commit/8ed745499a2802b3f29787b03990a566cf7dc917))
* **investigate:** deeper Flux investigation (status, dependency tree) + LogsQL fix ([#46](https://github.com/Smana/runlore/issues/46)) ([a0f9084](https://github.com/Smana/runlore/commit/a0f90847c1c841759de1f8abc64d8d6f2cc3dd98))
* **investigate:** deeper investigations + resilient catalog reload ([#49](https://github.com/Smana/runlore/issues/49)) ([65cd6ff](https://github.com/Smana/runlore/commit/65cd6ff2ff1e8c98513afe3362cba9484eff8b12))
* **investigate:** derive workload name/kind from alert labels ([5f9c122](https://github.com/Smana/runlore/commit/5f9c122ce6b6807c36f073c96c5d6b5186705426))
* **investigate:** don't treat tool errors as evidence; search KB early; log tools used ([#50](https://github.com/Smana/runlore/issues/50)) ([0194562](https://github.com/Smana/runlore/commit/019456219463988aea95096a68fa655a655005fb))
* **investigate:** hard-kill loop when token budget exceeded after nudge ([10d5990](https://github.com/Smana/runlore/commit/10d5990334cc1232c115c16f2b89efa7691eac07))
* **investigate:** hard-stop the ReAct loop at the token budget ([5fad6bd](https://github.com/Smana/runlore/commit/5fad6bdb46a3e0361c3bb807e90158b8736fc0f3))
* **investigate:** instant recall — short-circuit the loop on a high-confidence catalog hit ([6690841](https://github.com/Smana/runlore/commit/669084191b34ba296dc145d1af137e3e981f1785))
* **investigate:** loop prefers the discovered resource over the alert workload ([3e9ff29](https://github.com/Smana/runlore/commit/3e9ff29045b89bd300bed6bb2feda57d652a1afb))
* **investigate:** network_drops tool (Hubble), wired into serve ([cdd546e](https://github.com/Smana/runlore/commit/cdd546e6df6ed78e844af675e83d5e4a4a532dff))
* **investigate:** per-investigation deadline (LoopInvestigator.Timeout) ([0d3e83b](https://github.com/Smana/runlore/commit/0d3e83bb94417256806d2371739a281039a09c8a))
* **investigate:** pod_status + kube_events tools; flux namespace fallback ([#59](https://github.com/Smana/runlore/issues/59)) ([55f1f12](https://github.com/Smana/runlore/commit/55f1f12f931591aa07d31b687c8738d1116490fa))
* **investigate:** query_metrics + query_logs tools, wired into serve ([947512a](https://github.com/Smana/runlore/commit/947512a10c6193142f07580208f26c9980a8000b))
* **investigate:** re-run investigations via the "reinvestigate" KB label ([#54](https://github.com/Smana/runlore/issues/54)) ([f4d3a31](https://github.com/Smana/runlore/commit/f4d3a31685bc3e39805d51fc178bbe970ae4a24f))
* **investigate:** submit_findings affected_resource -&gt; inv.Resource ([335008e](https://github.com/Smana/runlore/commit/335008e676da62b821b39f67309c37729902425e))
* **investigate:** surface response truncation (warn + metric + one-shot nudge) ([d296c20](https://github.com/Smana/runlore/commit/d296c2017d28d37f150a64b09a7f92c2c8064c17))
* **investigate:** thread alert fingerprint + recalled-entry for outcome attribution ([1a1e955](https://github.com/Smana/runlore/commit/1a1e9557de20559c42171d548bd1125eb0ed28cd))
* **investigate:** tolerate fenced/double-encoded findings args + tests ([d9408ab](https://github.com/Smana/runlore/commit/d9408ab75f0963f9fa7e49876707015042ff2590))
* investigation coalescing, rate limiting, token efficiency + OTel metrics ([d8dfea1](https://github.com/Smana/runlore/commit/d8dfea1c53368b107f47ec3b19ddfeb97bc5bfdd))
* KB-entry validator — lore validate-kb (structural gate + semantic advisory) ([#99](https://github.com/Smana/runlore/issues/99)) ([dde4998](https://github.com/Smana/runlore/commit/dde4998f96f9e0c4f8bd0b67976a158a3083dec9))
* leader election for HA (verified on k3d: single leader + failover) ([9e29882](https://github.com/Smana/runlore/commit/9e29882ae15e22bbfd0ce072ee3bafd420049fd1))
* **learning:** curation workflow — file-time gate + lore curate agent ([bedc303](https://github.com/Smana/runlore/commit/bedc303a0683938d78a0b48b0ee181add709e646))
* **loop:** token-budget nudge + investigation-tokens metric ([5b22492](https://github.com/Smana/runlore/commit/5b22492ac568f1409a5aa044859e4b00f4c82cff))
* **loop:** truncate oversized tool outputs before they enter history ([97e5ded](https://github.com/Smana/runlore/commit/97e5ded7b7b1a6fcf676fe275a7ab0f5f9f85d61))
* **mcp:** lore mcp — serve GitOps what-changed over MCP (POS-4 Path A MVP) ([72947ce](https://github.com/Smana/runlore/commit/72947ced78baf4cf155a0a16fb23343ee35a826c))
* **mcp:** lore mcp — serve GitOps what-changed over MCP (POS-4 Path A) ([288ba8a](https://github.com/Smana/runlore/commit/288ba8aac941758063639d58b3a580eb19d9e676))
* metrics + logs investigation tools ([3197ba3](https://github.com/Smana/runlore/commit/3197ba36e9c93ac0c0d00ad55c8c6001f0aaaaa5))
* **model/anthropic:** parse usage + max_tokens stop_reason ([99265fd](https://github.com/Smana/runlore/commit/99265fdfda510c3667acd088eeec15384ae733ff))
* **model/anthropic:** prompt-cache the system prompt + tool schemas ([d822498](https://github.com/Smana/runlore/commit/d82249801e5c5effe16f0692cd9e13d90ccad600))
* **model/gemini:** parse usageMetadata + MAX_TOKENS finishReason ([15d48e3](https://github.com/Smana/runlore/commit/15d48e352665b8362ca85560586abdd93ebc1b87))
* **model/openai:** parse usage + length finish_reason ([e929342](https://github.com/Smana/runlore/commit/e929342d5a8e65ad339705ede6cdae8986502560))
* **model:** add native Google Gemini provider ([11c8d58](https://github.com/Smana/runlore/commit/11c8d585eb065b679d32e71a0847d3bffd56308a))
* **model:** native Anthropic Messages API client (tool use; OpenAI-&gt;Anthropic message mapping) ([f34bcb7](https://github.com/Smana/runlore/commit/f34bcb7ece790db8daa4453280ba591e0240499f))
* **model:** native Anthropic support ([0fc31f7](https://github.com/Smana/runlore/commit/0fc31f75cd157c228b1993231f973330073dd04c))
* **model:** native Google Gemini provider ([4a5750c](https://github.com/Smana/runlore/commit/4a5750c966b8de7fed1a25084974f2d4dfdb00ec))
* **model:** route the adversarial verify pass to an optional cheaper model ([8c6d463](https://github.com/Smana/runlore/commit/8c6d4638991ea11f70b838b33f6691239c076efa))
* network/Hubble investigation tool ([09be0d0](https://github.com/Smana/runlore/commit/09be0d08749ac6a9d806714d89a458c250333539))
* **network:** Hubble NetworkProvider (Relay gRPC observer) for dropped flows ([f271e43](https://github.com/Smana/runlore/commit/f271e43a405d982b9a5d237709a9f0c5367e00fb))
* **network:** pluggable, CNI-agnostic network-flow sources (Hubble + AWS VPC Flow Logs + GCP Firewall Logs) ([#95](https://github.com/Smana/runlore/issues/95)) ([2cd89e3](https://github.com/Smana/runlore/commit/2cd89e3521d36460a27349a6f5070605922ef6e9))
* **notify:** add Slack bot-token (chat.postMessage) delivery ([#41](https://github.com/Smana/runlore/issues/41)) ([cd2f044](https://github.com/Smana/runlore/commit/cd2f044c4bf8e8a20aeb098d90d5cb2682c13f85))
* **notify:** deliver findings to Slack + Matrix ([8ee251e](https://github.com/Smana/runlore/commit/8ee251e671b4e4686c988050efb9d332227656ce))
* **notify:** Investigation formatter + Slack webhook notifier + best-effort fan-out ([8c8f56c](https://github.com/Smana/runlore/commit/8c8f56cbe35fcd2596a165bf5c5247b47c435411))
* **notify:** Matrix client-server notifier ([e3d9f58](https://github.com/Smana/runlore/commit/e3d9f58e2285a187c351138e08fa455283a3b29d))
* **notify:** Matrix HTML formatted_body + restart-safe txn ids (R16) ([bf5fe93](https://github.com/Smana/runlore/commit/bf5fe934df0632286cf88ba49665ba9090268588))
* **notify:** richer Slack Block Kit message + KB link ([#44](https://github.com/Smana/runlore/issues/44)) ([e2f91c7](https://github.com/Smana/runlore/commit/e2f91c71173f6397ed44e6c3d8e7ef6100ee184c))
* **observability:** panels for truncation, batch size & dedup score ([c259ebe](https://github.com/Smana/runlore/commit/c259ebe8645ecba033d8ec4a3d0c8fc9beb5f59a))
* **observability:** structured logging, richer metrics, portable Grafana dashboard + alert rules ([#96](https://github.com/Smana/runlore/issues/96)) ([9b9195b](https://github.com/Smana/runlore/commit/9b9195b72d31632079f6e472f2448d708c6ffa38))
* outcome capture — record whether investigated incidents resolve (learning-loop A1) ([fa37bf2](https://github.com/Smana/runlore/commit/fa37bf250da34ddb2a241ec5f55bf596fd9b9fb5))
* **outcome:** add Resolved flag to Episode ([39476db](https://github.com/Smana/runlore/commit/39476dbfdf95eacdbdf3eca24a800bd29dbcea21))
* **outcome:** append-only JSONL outcome ledger with replayed open-index ([925eb8f](https://github.com/Smana/runlore/commit/925eb8f9bc0bc5e33a8339ea792fdd41599e4583))
* **outcome:** carry dedup fingerprint on ledger event + episode ([8410fc7](https://github.com/Smana/runlore/commit/8410fc718cf2f168abbf6a3907466969d6aec94f))
* **outcome:** Episodes() reconstructs open-&gt;resolve history ([644f57f](https://github.com/Smana/runlore/commit/644f57f2d8dcb186b2fc0183b2d005e7f8c433ac))
* **outcome:** Episodes()/OpenCounts() ledger read API (learning-loop A1→A2 seam) ([89e14f1](https://github.com/Smana/runlore/commit/89e14f174f42bfa9b98c3d9c060e33f7857f9c87))
* **outcome:** OpenCounts() per-entry recall/resolved aggregate ([3ea87dd](https://github.com/Smana/runlore/commit/3ea87dd99bf04f620a80d28d628c2df47a234b79))
* **outcome:** per-fingerprint coalesce attribution + order-independent Episodes ([f151411](https://github.com/Smana/runlore/commit/f151411568ba95f78e61303b7ae29afd817f2eac))
* **outcome:** record investigation outcomes + wire ledger/config/helm ([91102cc](https://github.com/Smana/runlore/commit/91102cce273ea07950e0fd1a78a1e25eda7bd27b))
* **outcome:** stamp curator dedup fingerprint on the recorded open ([01fce4d](https://github.com/Smana/runlore/commit/01fce4d248e7e5edd2f6b7c1670ddc67816c06c8))
* **providers:** CurationForge interface (open PR + list PRs + comment) ([58e81a8](https://github.com/Smana/runlore/commit/58e81a8441faa983276b84802f643a181f9d230b))
* **providers:** expose token Usage + Truncated on CompletionResponse ([c06ad20](https://github.com/Smana/runlore/commit/c06ad201ac1927e5f2cdd34b23e77965e97f1db8))
* **providers:** MetricsProvider (Prometheus/VM) + LogsProvider (VictoriaLogs) + structured payloads ([ca9d6d0](https://github.com/Smana/runlore/commit/ca9d6d01f4e1664b81a5b832b1dcb8666418384a))
* **ratelimit:** sliding-window start limiter ([8f32844](https://github.com/Smana/runlore/commit/8f328443f50d00ce1eecd6c8615e9b03fad7c7c3))
* real LLM investigator in serve (OpenAI-compatible model provider) ([af08688](https://github.com/Smana/runlore/commit/af086886894e6513947be8b66bc0322cfb67f720))
* recall trustworthiness — confident-recall gate + derived confidence ([9c540e3](https://github.com/Smana/runlore/commit/9c540e355fee220a60b48e040bfdd27d5194d296))
* **recall:** confident-recall gate — BM25 margin + structural agreement + derived confidence ([710918c](https://github.com/Smana/runlore/commit/710918c837b5943d43a47a8d8e3317504e12a99a))
* **recall:** config knobs (margin/solo_floor/structural) + recall_rejections metric ([f8db4fb](https://github.com/Smana/runlore/commit/f8db4fb5f2eaa5b0f0b8b68ead098a346c8a80a8))
* **recall:** cosine-gated hybrid recall path (opt-in) — M4 pt2 [2/3] ([e104cf8](https://github.com/Smana/runlore/commit/e104cf85ed735534781ae3d5691b97cb838b45f4))
* **recall:** default margin/solo/min thresholds when instant_recall enabled ([2f45ab9](https://github.com/Smana/runlore/commit/2f45ab9abd0e6ba845e8caea0589697490daf8f1))
* **recall:** Gate 2 disambiguates workloads in the same namespace ([4470b99](https://github.com/Smana/runlore/commit/4470b997c392d63795255a5d6b4a2b8a90aa02ea))
* **recall:** instrument KB cache hits (score, result-labelled hits, tokens saved) ([c8504e6](https://github.com/Smana/runlore/commit/c8504e651a232139d9a4c004b15a60fd85e4ff9e))
* **recall:** outcome-driven confidence decay + low_outcome gate ([1288e20](https://github.com/Smana/runlore/commit/1288e20a75186b1f39c413028ac4cd945ab8702d))
* **recall:** outcome-driven recall decay (the learning-loop self-heal edge) ([29df513](https://github.com/Smana/runlore/commit/29df513517604bf7911d0e227079edb5350f9551))
* **recall:** outcomeFactor optimistic decay function ([0c18b14](https://github.com/Smana/runlore/commit/0c18b141f3f96bb382cfa1a6f0cf0a3ff5b3d897))
* **recall:** wider candidate set + structural pre-filter so the right entry is findable ([8a7caa6](https://github.com/Smana/runlore/commit/8a7caa6fa7a22ed821e653aac0e67ecd5c4c9f47))
* **recall:** wire margin/structural config into the Recall constructor ([5b0bd19](https://github.com/Smana/runlore/commit/5b0bd192a6e512a62e5e2ce7c30c05ad58e7ab29))
* **recall:** wire outcome decay (config + ledger) into the serve path ([6a9bbe2](https://github.com/Smana/runlore/commit/6a9bbe213dfc08334089efbdf4d9a761dc4b5159))
* **redact:** mask secrets before they reach the model (F1) ([837ad26](https://github.com/Smana/runlore/commit/837ad262467d27e6b32b77d49d672715db4d24a4))
* reuse the curation GitHub App identity for git-sync auth ([c259400](https://github.com/Smana/runlore/commit/c2594009b2068f25bfe41df435267b3d7dd37c70))
* **serve:** curate findings to the forge when a GitHub App is configured ([9b7dec7](https://github.com/Smana/runlore/commit/9b7dec744e9a2479837b44ea7495db6bff2aef7c))
* **serve:** deliver findings to configured Slack/Matrix notifiers ([de00ab6](https://github.com/Smana/runlore/commit/de00ab632e38d38a193d59f23a5d3c98cacd6477))
* **serve:** kb_search tool grounds the loop in the OKF catalog ([5f59520](https://github.com/Smana/runlore/commit/5f595200f7da08551a5b75b34901229228ed6e08))
* **serve:** leader election for HA (leader-only loops; readiness gated on leadership) ([676a9cb](https://github.com/Smana/runlore/commit/676a9cb3c4bf4223d1db1c43a966081ce46385ba))
* **server:** cap Alertmanager webhook body at 1 MiB (413) ([40fff60](https://github.com/Smana/runlore/commit/40fff602510f90c24eb5bc4571d14d392ee30632))
* **server:** coalesce correlated alerts into one investigation ([bbabff9](https://github.com/Smana/runlore/commit/bbabff914ad804da7e8883a60fbffb694c816b85))
* **serve:** reuse the curation GitHub App identity for git-sync auth ([5ca427a](https://github.com/Smana/runlore/commit/5ca427a69b7aef3b1136480a7c9a612ea3e7f3c0))
* **server:** HTTP server timeouts + MaxHeaderBytes (Slowloris/DoS) ([0ebfc1b](https://github.com/Smana/runlore/commit/0ebfc1b47b85f4be47363edc6fdc41fd5444d05e))
* **server:** ingest resolved alerts and record them in the outcome ledger ([f2525cb](https://github.com/Smana/runlore/commit/f2525cbe2585a4ec1c251f34fbd9efb1ef9dd9ce))
* **server:** require webhook token when the LLM investigator is wired ([7398523](https://github.com/Smana/runlore/commit/7398523dd17b47e3cc698b84fad4145c58a4ad09))
* **server:** serve OTel metrics on GET /metrics when enabled ([a35cf06](https://github.com/Smana/runlore/commit/a35cf06096dce8ffd22c35d6500a41ca4f9298bb))
* **serve:** select GitOps engine (flux default | argocd); Application RBAC ([4be72c7](https://github.com/Smana/runlore/commit/4be72c747b1521e7294aa14b7b2899b1af08bbd5))
* **serve:** select model provider (openai default | anthropic) ([880c370](https://github.com/Smana/runlore/commit/880c3706522fd81b84bf6ebe1fc3a974cf52d5e0))
* **serve:** wire single OTel metrics instance into investigation path ([0a6a2f1](https://github.com/Smana/runlore/commit/0a6a2f134d6b8408665a5e8d3ab811973581c05e))
* **slack:** interactive Approve/Reject buttons + signed /slack/interactions endpoint ([9158a3b](https://github.com/Smana/runlore/commit/9158a3bbd42f92c92fac36981e73468fd48bcd23))
* **slack:** interactive Approve/Reject buttons for rung-2 actions ([79d1780](https://github.com/Smana/runlore/commit/79d17807e88c21a005ac4fafec0bcd2b24253c45))
* **telemetry:** add OTel runlore_* metric instrument set ([05d0ed8](https://github.com/Smana/runlore/commit/05d0ed8bdea2efbda04363960a997b845e216f4e))
* **telemetry:** curation_dedup_score histogram ([621b69d](https://github.com/Smana/runlore/commit/621b69d73246d740385770e354a37b098772b141))
* **telemetry:** outcome metrics (opened/resolved/recall-outcome/resolution-seconds) ([c54efcd](https://github.com/Smana/runlore/commit/c54efcda6b9eaad7cabe0185cb1f912cd32d236d))
* **telemetry:** Prometheus exporter + /metrics handler ([e6af4b6](https://github.com/Smana/runlore/commit/e6af4b636ea0ecaf25d844a9db936c15689cb857))
* **telemetry:** SLO-aligned explicit buckets for latency histograms ([7981b85](https://github.com/Smana/runlore/commit/7981b85d3d0561e52632da337fb460ea77114890))
* **trigger:** thread Alertmanager groupKey onto Incident ([a97bf7a](https://github.com/Smana/runlore/commit/a97bf7ab07e00d3073aacac0bb11864f893510b0))
* **victorialogs:** limit+offset pagination + signal truncation ([db8605d](https://github.com/Smana/runlore/commit/db8605d31eda8f92e85c27b2c06482d0a61ccc16))
* **whatchanged:** thread context through the Differ for cancellable clone/patch ([a3f7227](https://github.com/Smana/runlore/commit/a3f7227f0922ae77eeef402bc10105fcc8d7a744))
* wire hybrid recall (config + embeddings client) — M4 pt2 [3/3] ([f522495](https://github.com/Smana/runlore/commit/f522495911082fde528e11dff380307f75809031))


### Bug Fixes

* **action,server:** harden the action approve/reject path ([27cef4e](https://github.com/Smana/runlore/commit/27cef4e9dacf69a43bd45bf33414135b24195869))
* **action:** harden the autonomy ladder — server-authoritative gating, audit, fail-closed ([b1f3c46](https://github.com/Smana/runlore/commit/b1f3c46bb175f5d4f57e11551cb01743bfb40b2b))
* **action:** harden the autonomy ladder — server-authoritative gating, audit, fail-closed ([e321312](https://github.com/Smana/runlore/commit/e3213120d0558e4184ebb6787f25537d9dc94297))
* address review findings (verify bypass, trigger authz, scoped reads) ([#56](https://github.com/Smana/runlore/issues/56)) ([29e0b4c](https://github.com/Smana/runlore/commit/29e0b4cde0aa00bcc6ac05b7ee52b03c4794f32b))
* **argocd:** bounded watch send instead of silent backpressure drop (R24) ([bbab937](https://github.com/Smana/runlore/commit/bbab937cd2fb25bdcfd344bd31f01245ff7baf70))
* **argocd:** multi-source ResourceStatus + expand RBAC for applicationsets/appprojects ([4030ca7](https://github.com/Smana/runlore/commit/4030ca716be18bb13e4a4e69739b69a7e7fbd805))
* **argocd:** populate ResourceStatus refs from spec.sources[0] for multi-source apps ([dd7e368](https://github.com/Smana/runlore/commit/dd7e368f407d73e918fa874ea760d30d6b26f316))
* **catalog,outcome:** survive interrupted clone, failed re-index, and crash ([99af960](https://github.com/Smana/runlore/commit/99af960c266f2d0f8fc0b17de5ece135e2a1f950))
* **catalog:** index Resource for BM25 so resource-term queries get lift ([b52ac32](https://github.com/Smana/runlore/commit/b52ac32bac59b18374f8b253b1fe6e054aaa9f7b))
* **catalog:** pin bleve index to BM25 (was silently TF-IDF) ([5ae25d0](https://github.com/Smana/runlore/commit/5ae25d08f649e277ee383ef18f8427ee35c65d39))
* **catalog:** pin bleve index to BM25 + curator dedup-score observability ([0d6eb31](https://github.com/Smana/runlore/commit/0d6eb317c1f82d1385f1a216b5605da35341c514))
* **catalog:** skip hidden entries so ConfigMap ..data mounts don't double-index ([69b95d0](https://github.com/Smana/runlore/commit/69b95d013411fa256ea44b93bd87de06ff6c9b00))
* **chart:** default to Recreate strategy (leadership-gated readiness deadlocks rolling updates) ([126cdbb](https://github.com/Smana/runlore/commit/126cdbb2d4ed04beb29851f05fe3b7694bb335eb))
* **chart:** raise default memory (256Mi OOMKills under real load) ([#48](https://github.com/Smana/runlore/issues/48)) ([0b921fe](https://github.com/Smana/runlore/commit/0b921fed59c66a9eee5caae3dac31e1048a96be1))
* **chart:** Recreate update strategy (fixes leader-election rolling-update deadlock) ([0e6ade1](https://github.com/Smana/runlore/commit/0e6ade1749c273f51a851eaf2a6818800c7d6e9a))
* **ci:** emit a valid image tag on pull_request builds ([b8f1491](https://github.com/Smana/runlore/commit/b8f1491f1d3fe39a1da8fec4adb0a202a58f35b6))
* **ci:** lowercase image ref before cosign sign ([bc4f214](https://github.com/Smana/runlore/commit/bc4f21490b01251b3113f6ba148741c0e0c44d70))
* **ci:** lowercase image ref before cosign sign (ghcr requires lowercase) ([38c9afd](https://github.com/Smana/runlore/commit/38c9afdb414ac6dbba673ba3e39fb387eb6d0e37))
* **coalesce:** don't collapse incidents when all correlation labels are absent (R17) ([22c367d](https://github.com/Smana/runlore/commit/22c367dfefe46f63348115b5e332b12f2b1bdb28))
* **coalesce:** emit alerts_suppressed; default coalesce/rate-limit config + guard sweep tick ([c229a30](https://github.com/Smana/runlore/commit/c229a308da0f723a3b66d46dcbecad91a7327b2f))
* **coalesce:** evict cooldown records and let a new critical bypass cooldown (R17) ([7e31405](https://github.com/Smana/runlore/commit/7e31405efe21059b16cdf238135a454b58a520fb))
* **coalesce:** suppress critical-alert storms (cooldown before fast-path) ([ad310f3](https://github.com/Smana/runlore/commit/ad310f35cd47819ba66d3abf6688bf4d5f02b9a1))
* **config:** honor explicit gitops_failures debounce: 0 (fire immediately) ([a8a9bf4](https://github.com/Smana/runlore/commit/a8a9bf4d89977d6608c722a51ba5c126d4092169))
* **config:** honor explicit gitops_failures debounce: 0 (fire immediately) ([5938723](https://github.com/Smana/runlore/commit/59387232c0bdfcdd79900519d9743d43ce6ecd75))
* **curate:** address [#63](https://github.com/Smana/runlore/issues/63) review — guardrail + correctness fixes ([23450e4](https://github.com/Smana/runlore/commit/23450e49b3db2d428e74eace3823f1089e912c59))
* **curate:** fingerprint-first Phase-2 dedup, title-Jaccard as fallback ([0b1f278](https://github.com/Smana/runlore/commit/0b1f278eff1205e1f408c2c67179e54a7131d153))
* **curate:** resolve curated PRs by dedup fingerprint, not free-text title ([b3c41b2](https://github.com/Smana/runlore/commit/b3c41b2d24208a95ce4f2c457bfb7eadb2512667))
* **curator:** dedup re-investigations by trigger key, not LLM prose ([9e82793](https://github.com/Smana/runlore/commit/9e82793c01c59dee4c35f73641e399ddc48ed6d8))
* **curator:** dedup re-investigations by trigger key, not LLM prose ([818f0b6](https://github.com/Smana/runlore/commit/818f0b668c57a559581328814a198d7a4d40ca3d)), closes [#137](https://github.com/Smana/runlore/issues/137)
* **curator:** derive KBEntry.Type instead of hardcoding Incident ([7594ed4](https://github.com/Smana/runlore/commit/7594ed493c7b3b9820dca35b2e2618fe6d038779))
* **curator:** review nits — drop stale pkg doc, fix label comment, +catalog-error test, use strings.Contains ([aa1c41a](https://github.com/Smana/runlore/commit/aa1c41a7868dcc678bf8129c27e4b175027b8b05))
* **docs:** remove stale flux_* tool name references after ArgoCD parity ([0caad99](https://github.com/Smana/runlore/commit/0caad997a679c8d4f91ff9de6cef1b53d97e3b6c))
* **eval:** induce OOM via a manifest with resources.limits (kubectl run --limits was removed) ([b7b118b](https://github.com/Smana/runlore/commit/b7b118ba6f6a7556cfc4a0c09a4221bcfcb57009))
* **eval:** k3d-pvc-unbound creates its namespace before applying the PVC ([f7a33f6](https://github.com/Smana/runlore/commit/f7a33f6b603e73bd24aba0c23fdf0f234f92dfa8))
* **eval:** sort Bonus, dedup ToolErrors, test wrap (drop nolint) ([84b6760](https://github.com/Smana/runlore/commit/84b6760e92979ae69eee11be02e064b8246e2feb))
* **eval:** wrap LoadScenarios dir error, test mode default, rename test var ([e6c543f](https://github.com/Smana/runlore/commit/e6c543fb0ee3abe9ef6ee9397c84899261726e3f))
* **forge:** emit OKF timestamp in curated entry frontmatter ([be3680e](https://github.com/Smana/runlore/commit/be3680eb2de1bfa30a3bb6e864dc16400819c8a4))
* **gemini:** correlate function responses by id, not name only ([b0fd123](https://github.com/Smana/runlore/commit/b0fd123fc4cbb8fcf1300e6d90d1ad00e3280560))
* GitHub token single-flight + bounded model retry ([abb03cb](https://github.com/Smana/runlore/commit/abb03cbeaf4a55b15178e5932b8f1a8046b08f48))
* **gitops/flux:** resolve ExternalArtifact/OCI sources, not just GitRepository ([#53](https://github.com/Smana/runlore/issues/53)) ([e1df323](https://github.com/Smana/runlore/commit/e1df3239f32394fdb761da20d226d323971c2eda))
* **gitops:** debounce transient Flux failures before investigating ([#101](https://github.com/Smana/runlore/issues/101)) ([591df13](https://github.com/Smana/runlore/commit/591df13b5c9ed3d276ad8177dd8f7c784cf594b1))
* **httpx:** block redirects to internal addresses (P3 SSRF) ([f28a2a2](https://github.com/Smana/runlore/commit/f28a2a2663bae364aeba7f9ecba56a4fc0f6f030))
* **httpx:** block redirects to internal addresses across outbound clients (P3) ([f88a04a](https://github.com/Smana/runlore/commit/f88a04ab1da9239f96c4e6b08f2619faa7afaf02))
* **investigate,notify,model:** egress secret redaction + transport robustness ([22ddab1](https://github.com/Smana/runlore/commit/22ddab1725cecd1e810ed6f589e1f4000b343bfc))
* **investigate:** clamp model confidence to [0,1] (NaN-&gt;0) in parseFindings and applyVerdicts ([66aaffc](https://github.com/Smana/runlore/commit/66aaffc482c7ec7cd58ff7b169ce8e23062a88d9))
* **investigate:** count tool-call args + tool schemas in estimateTokens ([70bdacf](https://github.com/Smana/runlore/commit/70bdacff028f25f9b0af08727b94df6803898f23))
* **investigate:** decouple metrics from rate-limit toggle; once-per-window throttle log; drop-path test ([a37b168](https://github.com/Smana/runlore/commit/a37b1680b0c54412e3aae18cee19c9ae0dd49586))
* **investigate:** give curated findings a title (default to the incident) ([844e2b0](https://github.com/Smana/runlore/commit/844e2b0983de64f11f21b157d49c8661af071913))
* **investigate:** nudge the model to submit_findings instead of bailing on prose ([#42](https://github.com/Smana/runlore/issues/42)) ([3ed8d82](https://github.com/Smana/runlore/commit/3ed8d82993c4929016a235e57b5b595a2fd535ce))
* **investigate:** re-investigate when verify rejects a recalled finding ([c0b98b9](https://github.com/Smana/runlore/commit/c0b98b970023290de723e4df12402a57269725c6))
* **lint:** satisfy golangci-lint v2.12.2 ([65d3730](https://github.com/Smana/runlore/commit/65d3730a085404ddee120661208b88e90929bc0f))
* **model:** drop empty op enum value rejected by Gemini ([#40](https://github.com/Smana/runlore/issues/40)) ([b9ad016](https://github.com/Smana/runlore/commit/b9ad01668aed340ecbd751984ef00eaa8fb0f4d9))
* **model:** stop echoing upstream LLM error bodies into logged errors (R19) ([275141e](https://github.com/Smana/runlore/commit/275141ecd3d6a339c534a6b1116b4f6efe248c08))
* **oom:** clone repos to disk in what_changed (was holding the whole repo in heap) ([#62](https://github.com/Smana/runlore/issues/62)) ([51f8c0f](https://github.com/Smana/runlore/commit/51f8c0fc7b213d4d019f3a17df3a8ee70cd25147))
* **oom:** root-cause memory growth — GOMEMLIMIT from cgroup + bleve index leak ([#61](https://github.com/Smana/runlore/issues/61)) ([d74f341](https://github.com/Smana/runlore/commit/d74f341890d412d504d8b0aaa10fcf4dc11bbc64))
* **openai:** always serialize content on tool-role messages ([95214bb](https://github.com/Smana/runlore/commit/95214bb06506ab131a387feab43b2c06a1bebaa1))
* **outcome:** pair Episodes() resolve-before-open (order-independent) ([1e584eb](https://github.com/Smana/runlore/commit/1e584eb6752ac46bc81749bff558fea343b3e9cf))
* **outcome:** record an open per coalesced fingerprint ([128c1c1](https://github.com/Smana/runlore/commit/128c1c1b4cf2dbaa3d7ad60a339965e718819054))
* **outcome:** thread alert fingerprint onto Investigation; guard non-alert sources ([711dfec](https://github.com/Smana/runlore/commit/711dfec76d44f1d4ddc976527c9ae7beedae66e8))
* **pod_status:** surface LastTerminationState (OOMKilled/exit) + memory limit — tie OOM to the limit ([76a51a9](https://github.com/Smana/runlore/commit/76a51a9fe1d97509f555b4398b87c9b231003f8b))
* **rbac:** scope pods/log to flux-system, keep pods status cluster-wide (R10) ([9c3bb40](https://github.com/Smana/runlore/commit/9c3bb40dd94d9e4d617a857b8870a7505dd27ef6))
* **readyz:** keep 503 when a configured catalog fails to load ([a988fbe](https://github.com/Smana/runlore/commit/a988fbee4e0d91357e9de9c239236e288b57b1fb))
* **recall:** structural gate no longer namespace-matches distinct workloads ([5ba81b0](https://github.com/Smana/runlore/commit/5ba81b01cdf21f33fb2ca4ea3c9d374c8845f1b1))
* **redact:** broaden high-precision secret coverage (anchored token rules, AWS secret cue) ([401abb0](https://github.com/Smana/runlore/commit/401abb00102be73cb9fe0abb3a9db44c9258bfe7))
* **redact:** broaden high-precision secret coverage (anchored token rules, AWS secret cue) ([e373374](https://github.com/Smana/runlore/commit/e373374041a6ce396d3dd66eb7887e2030652e53))
* **review:** DupFingerprint terse-cause collision, unverified-coalesce, curate opt-out, +tests ([#88](https://github.com/Smana/runlore/issues/88)) ([f5693a6](https://github.com/Smana/runlore/commit/f5693a675aa0e80492aab0fdcb2aceca5e101606))
* **security:** redact upstream error bodies, YAML-safe KB frontmatter, correct approve status ([2748047](https://github.com/Smana/runlore/commit/2748047135c6430e8566fd14af9c2c98878cb133))
* **serve:** graceful drain of the in-flight investigation on shutdown (GO-P1B) ([bfb6401](https://github.com/Smana/runlore/commit/bfb64013080c15826503e56fa6f3c9649760273f))
* **serve:** hoist gitops-failure deduper to process scope (no leader-flap stampede) ([e3419c5](https://github.com/Smana/runlore/commit/e3419c52d7839f47e6389d20a17a59ba1834e6b4))
* **server:** emit ingress metrics independent of coalescing toggle ([e79dd5d](https://github.com/Smana/runlore/commit/e79dd5d9db17f939ecd1ad4fa6cec3749f4c587a))
* **trigger:** keep environment in fingerprint-less dedup key (R17) ([90e754c](https://github.com/Smana/runlore/commit/90e754c96008acc3925227f068d5825d5c7ffff7))
* **trigger:** skip dependency-cascade gitops failures ([#45](https://github.com/Smana/runlore/issues/45)) ([b148ff4](https://github.com/Smana/runlore/commit/b148ff405e2ba6a34c5f4087a3719e8880f40411))
* **whatchanged:** authenticate private-repo clones with the GitHub App token ([fee6968](https://github.com/Smana/runlore/commit/fee696844c92579d83ae69c2d51f701f3461959e))
* **whatchanged:** diff lastApplied..HEAD for failing Kustomizations ([#100](https://github.com/Smana/runlore/issues/100)) ([0577c6f](https://github.com/Smana/runlore/commit/0577c6f45252456ff6f1cb14052402d1cb0dc8fc))


### Performance Improvements

* **whatchanged:** cache repo clones per what_changed call (CLONE-1) ([e1876c3](https://github.com/Smana/runlore/commit/e1876c32aa19d448c6c3615bff193ab7e42e0bb0))


### Documentation

* **action:** note TargetPolicy extraction point for future target dimensions ([71f9467](https://github.com/Smana/runlore/commit/71f94677fb6da78358ddf7f2e92183bdf85b5b28))
* **action:** spec+plan for namespace gate tests at the Review boundary (R11) ([b7fa058](https://github.com/Smana/runlore/commit/b7fa05856b48bef810fd776ff75b1a1c128ce2a6))
* add 'Reviewing & approving knowledge' user guide ([#98](https://github.com/Smana/runlore/issues/98)) ([6e1f813](https://github.com/Smana/runlore/commit/6e1f813047049d6294544260b2865801803d32f3))
* add community-health files and project-status section ([5d3a808](https://github.com/Smana/runlore/commit/5d3a808de06213f1f6100390b96435173501d5ff))
* add community-health files and project-status section ([a4d0037](https://github.com/Smana/runlore/commit/a4d0037e1f09c3b0ff21824a0effa0734e94b258))
* **analysis:** [#17](https://github.com/Smana/runlore/issues/17) rollback decided against — record rationale (§9.4) ([ae681e4](https://github.com/Smana/runlore/commit/ae681e4c3bebe60dfca0b3282b534edf43b1c591))
* **analysis:** §0 update banner — A2/Episodes/BM25/Phase-2 now shipped ([f3a2a4e](https://github.com/Smana/runlore/commit/f3a2a4e56be999931bd27ed55ce1fc48e6f225d7))
* **analysis:** deep analysis & learning-loop improvement report ([90ee3db](https://github.com/Smana/runlore/commit/90ee3db9eaf1811d524c09f42c499ef8e0abee9d))
* **analysis:** record implementation status — 11/18 roadmap items merged (Waves 0-2 + eval cluster) ([a5f3854](https://github.com/Smana/runlore/commit/a5f3854a2be57868bb6d72ea98a79ba9c590484d))
* **analysis:** roadmap status — 17/18 merged (only [#17](https://github.com/Smana/runlore/issues/17) deferred); e2e PASS=40 FAIL=0 ([e367b93](https://github.com/Smana/runlore/commit/e367b933e76150361015b910338f4e33ef189e12))
* **architecture:** add architecture diagram (mermaid) ([2361800](https://github.com/Smana/runlore/commit/2361800890df295720aca29c78096ed7529dacf8))
* **architecture:** add architecture diagram (mermaid) + cite as source of truth ([11d1ebe](https://github.com/Smana/runlore/commit/11d1ebe1ead3efeefe51f025fc63bb6e60fa3c75))
* **argocd:** spec + plan for closing genuine Flux-parity gaps (R24) ([a198a02](https://github.com/Smana/runlore/commit/a198a022cc4d93d5e3cbfa0df2517ed89811f538))
* **coalesce:** mark R17 hygiene spec done ([6732041](https://github.com/Smana/runlore/commit/6732041f5e5d6091a0f28049be8bf3c24f4a31e2))
* **contributing:** document the eval harness ([19d0e47](https://github.com/Smana/runlore/commit/19d0e47dc5ba95ba4d97eee4862148e182835fb0))
* **curate:** spec + plan for catalog/curate polish (R22) ([278ad65](https://github.com/Smana/runlore/commit/278ad65af4d66a8c943e0a0b48b15d15089b6316))
* **curate:** spec + plan for fingerprint-keyed resolution join (R6) ([ec61b23](https://github.com/Smana/runlore/commit/ec61b2307d8bb63e2832bbadd36b2ba5a11f5f65))
* **de-dup:** canonical learning-loop home (design.md §6 cross-link + trim) ([a85dabb](https://github.com/Smana/runlore/commit/a85dabbd695ad1ffc922706aff85b6918abe9ef1))
* **de-dup:** make learning-loop.md the canonical home for the loop ([cab97d5](https://github.com/Smana/runlore/commit/cab97d5cee0fd1d747fda5974ab013e2ea9e22df))
* **design:** record decision — no in-cluster mutating rollback op ([#90](https://github.com/Smana/runlore/issues/90)) ([0f653c5](https://github.com/Smana/runlore/commit/0f653c5766db6efdf7bd9d0d64c4e9b986f0f717))
* drop Questions & discussion contact link (Discussions not enabled) ([9650c53](https://github.com/Smana/runlore/commit/9650c539109c3fdd51e9e9a3cb04a0ff502c2b2e))
* **eval:** correct spec (replay harness already exists) + add 2-part implementation plan ([21c0f5c](https://github.com/Smana/runlore/commit/21c0f5c7e1f9d686e60e6913c89c0bd32a355994))
* **eval:** design spec for the live-fire eval harness (lore eval) ([78dd25e](https://github.com/Smana/runlore/commit/78dd25e6d8b6b882e0a29e491f22a2c2c8c21962))
* external review — repositioning, honest limitations, and roadmap ([b4b63b0](https://github.com/Smana/runlore/commit/b4b63b0e9d857cb057741f81114ed148fd80a16b))
* fix stale flux_* tool names and document SlackBot config post-ArgoCD-parity ([eda240d](https://github.com/Smana/runlore/commit/eda240df87866ea91a75ff18121e4d3908881bb1))
* **getting-started:** clarify KB entry format ([#39](https://github.com/Smana/runlore/issues/39)) ([d36733b](https://github.com/Smana/runlore/commit/d36733b00454c2fb130ff054447b9747f0e8bfaa))
* **getting-started:** document metrics + logs investigation config ([d6c1647](https://github.com/Smana/runlore/commit/d6c1647272f5547ba51659d3a8e42d22af913b21))
* **getting-started:** git-sync as the loop-closing catalog option ([86775f7](https://github.com/Smana/runlore/commit/86775f76132f80fc8c842ae71fdac4459775b435))
* **getting-started:** metrics + logs config ([0248e1e](https://github.com/Smana/runlore/commit/0248e1e817c4aeb07d4e9af1bc7fb1ac4cf448a2))
* **getting-started:** native Anthropic model option ([8bca47d](https://github.com/Smana/runlore/commit/8bca47d6dae29291e7fa929f9acaca9f246ccfb6))
* **getting-started:** network/Hubble config ([7bd33d8](https://github.com/Smana/runlore/commit/7bd33d8def4359c7d8a8379d317f4e5e55d97f58))
* **helm:** spec + plan for probe timing (startupProbe + configurable probes, R21) ([865b853](https://github.com/Smana/runlore/commit/865b8531354135ab7b58932dac667493e1ffaab5))
* **investigate:** correct budget estimate comment now that usage is exposed ([bdf80e8](https://github.com/Smana/runlore/commit/bdf80e838073d4f25173edd64613057ed80678eb))
* **investigate:** spec+plan for confidence clamp + budget-estimate fix (R18) ([1069428](https://github.com/Smana/runlore/commit/10694284bf38b565273c5d4ccd5ac71f9693b9ec))
* learning-loop deep-dive (retrieve→capture→curate→compound) with diagrams ([#89](https://github.com/Smana/runlore/issues/89)) ([00cf048](https://github.com/Smana/runlore/commit/00cf048ec7b7faeee48f952298cca7ff62e45890))
* **learning-loop:** event source is pluggable, not Alertmanager-specific ([#91](https://github.com/Smana/runlore/issues/91)) ([e1f19f9](https://github.com/Smana/runlore/commit/e1f19f97acd4d7a00881eaa41ae01e4028d40c7e))
* **learning:** 2-phase implementation plans (file-time gate + curate agent) ([6631933](https://github.com/Smana/runlore/commit/663193305c2056488cd13baee2b88c450023bc8c))
* **learning:** correct KB-state facts; merge bar = resolved OR accepted ([ef43e1b](https://github.com/Smana/runlore/commit/ef43e1b7506676d9a9454a50c58a638545d890eb))
* **learning:** design spec for the curation workflow (compound the KB) ([c213a7e](https://github.com/Smana/runlore/commit/c213a7e2dae1eb704c7732ec7d191bbe442e2a55))
* **notify:** spec+plan for Matrix HTML formatting + txn durability (R16) ([90c6d19](https://github.com/Smana/runlore/commit/90c6d198c797224917bd494f454b9158e7a0fc22))
* **observability:** R20 spec + plan (panels + SLO histogram buckets) ([0e58d9d](https://github.com/Smana/runlore/commit/0e58d9dd9c3a02f936f3a16eda2c09943806f60c))
* **outcome:** correct OpenCounts resolve-rate formula in doc comment ([7a262b8](https://github.com/Smana/runlore/commit/7a262b8d2aff26265ba5b50495d981d360d1912c))
* **plan:** BM25 scorer fix implementation plan ([77dd2f6](https://github.com/Smana/runlore/commit/77dd2f608619308b59263473bc303c63b5836398))
* **plan:** curator (Learn pillar write half) ([19853c9](https://github.com/Smana/runlore/commit/19853c929ba1f37a5730eb4a1b63f9a39fccd203))
* **plan:** entity-precision Track A implementation plan ([1e7fe9c](https://github.com/Smana/runlore/commit/1e7fe9c7170302440933cb77b62e2602270ba365))
* **plan:** eval statistics (k-of-n gate + flaky-fail + N) plan ([72f2878](https://github.com/Smana/runlore/commit/72f28787c3a97430d1ec65dee7193f32caeb12a3))
* **plan:** OKF catalog (read + kb_search) ([9400057](https://github.com/Smana/runlore/commit/94000572cbb327c1172b6b486311aae4133d8ebb))
* **plan:** outcome capture (A1) implementation plan ([6daf2c4](https://github.com/Smana/runlore/commit/6daf2c46aa1618d1b4309772718e22f014158c2f))
* **plan:** outcome ledger episodes read API implementation plan ([6a1f50a](https://github.com/Smana/runlore/commit/6a1f50a69942f0fba214a5b6f254656ed67c4345))
* **plan:** outcome-driven recall decay implementation plan ([0244466](https://github.com/Smana/runlore/commit/02444667a0385ee7fd024d62560f30f4460e384a))
* **plan:** recall Gate 2 disambiguation implementation plan ([d184511](https://github.com/Smana/runlore/commit/d184511fb850d6a4234d7ac40493502f9a17372a))
* **plan:** recall retrieval (wider candidates + structural pre-filter) plan ([0aedd4d](https://github.com/Smana/runlore/commit/0aedd4d8630f4c203255ea1e330a265a1b18954f))
* **plan:** recall trustworthiness implementation plan ([e9b4144](https://github.com/Smana/runlore/commit/e9b414408295167c14c20f2c08625a3040396c60))
* **plan:** recall-in-eval implementation plan ([d199bd6](https://github.com/Smana/runlore/commit/d199bd6528ee10d485b45d232b18c8a13b6f134a))
* **plan:** Slack/Matrix delivery for findings ([89e6675](https://github.com/Smana/runlore/commit/89e6675b990096b0f0c336bd717f80ab5fcf527f))
* **plan:** TDD plan — coalescing, rate-limit, token efficiency, telemetry ([7bd7680](https://github.com/Smana/runlore/commit/7bd76803141119b136b4794dbf5ba716bf854ac6))
* production getting-started + contributing guide ([48d835a](https://github.com/Smana/runlore/commit/48d835a2edb6774bed52b525a004e7c41a0cea30))
* **proposals:** HolmesGPT what-changed toolset proposal (POS-4) ([e3b1303](https://github.com/Smana/runlore/commit/e3b1303ae20daa0b155eb1953ccb2dc225868574))
* **proposals:** HolmesGPT what-changed toolset proposal (POS-4) ([f0f1f5a](https://github.com/Smana/runlore/commit/f0f1f5a486525d9b36e43a9bc08620af6e5b2a7c))
* **providers:** pagination + truncation-signalling spec + plan ([0b35eea](https://github.com/Smana/runlore/commit/0b35eeac52ce1725317f8c9ae6d81860de4388d0))
* Queue + Recurrence now wired (PR [#92](https://github.com/Smana/runlore/issues/92)) — sync analysis + learning-loop ([#93](https://github.com/Smana/runlore/issues/93)) ([a89a4ae](https://github.com/Smana/runlore/commit/a89a4ae2af7edb05691eb61d35c5921ae7d00a98))
* **r15:** spec + plan for LLM-protocol correctness fixes ([1511c57](https://github.com/Smana/runlore/commit/1511c57c9d074f2b7a0b225928589ea7d52d3977))
* **R19:** spec+plan — stop logging upstream LLM error bodies; decline redaction layer ([9e1098b](https://github.com/Smana/runlore/commit/9e1098b5c03f733ea7a1a37cba5d522ee3daca93))
* **r25:** spec + plan for coverage on AWS adapters, loop error paths, lore wiring ([b53dd67](https://github.com/Smana/runlore/commit/b53dd67ad905bf101177724efeb2163a10533a87))
* **rbac:** spec+plan for scoping pods/log to flux-system (R10) ([d929a60](https://github.com/Smana/runlore/commit/d929a605f6393a51608282e496a23bf2b83d0da3))
* README as a project presentation (learning-loop first, Kubernetes-first), refresh getting-started + design ([#97](https://github.com/Smana/runlore/issues/97)) ([859dbf2](https://github.com/Smana/runlore/commit/859dbf2ca16d2cebf649f781789d81dfad7549ad))
* **readme:** add a real Slack notification screenshot as a delivery example ([#60](https://github.com/Smana/runlore/issues/60)) ([1116c80](https://github.com/Smana/runlore/commit/1116c8089554a453dd839c351c584fb4e984b915))
* **readme:** clearer positioning, logical sections, polished getting ([3095b4c](https://github.com/Smana/runlore/commit/3095b4c52bee72b8f184124b714ee58377404813))
* **readme:** clearer positioning, logical sections, polished getting started ([12a55fa](https://github.com/Smana/runlore/commit/12a55fabcea5ce95640e29db09c0405d29ef2cbf))
* **readme:** fold in honesty/eval, fair competitor note, ICP, OKF-as-interop ([87a7bc5](https://github.com/Smana/runlore/commit/87a7bc513173e60654883f3776ba85168a07d2a6))
* **readme:** reflect the full autonomy ladder + current feature set in Status ([beb0086](https://github.com/Smana/runlore/commit/beb0086da3ebec2badafea5596a8809bf9f369a7))
* **readme:** reflect the full autonomy ladder in Status ([b482627](https://github.com/Smana/runlore/commit/b4826275f02b541bb149e6e7a8524d336d1bb657))
* **readme:** soften read-only framing, add note with roadmap hint ([82074bc](https://github.com/Smana/runlore/commit/82074bcfc539f8b06778bb68367ca3916be50893))
* **readme:** tighten intro and getting started section ([feb5159](https://github.com/Smana/runlore/commit/feb5159ccfdb2f182999b9b8eeecabc5665acd69))
* real getting-started (KB repo, GitHub App, helm install) + CONTRIBUTING (k3d) ([48c5734](https://github.com/Smana/runlore/commit/48c5734bab7ed3902eba8c5a5ce15eb28661f3ee))
* reposition around open knowledge + honesty; reconcile prior-art with 2026 landscape ([5464a73](https://github.com/Smana/runlore/commit/5464a73a80af9ba40cb7678736f1fcc89f4fd4e5))
* **roadmap:** defer F2 (2nd attempt regresses rung-2 e2e); refresh progress log ([99d191f](https://github.com/Smana/runlore/commit/99d191f71165e230d4d503c2f8c4c0c3aaabc8f7))
* **roadmap:** defer F2 + refresh progress log ([9d6aae3](https://github.com/Smana/runlore/commit/9d6aae33be666ce492a52bd48405bcfd0e2db332))
* **roadmap:** mark M4 pt2 implemented (eval-tuning pending) ([4869967](https://github.com/Smana/runlore/commit/4869967c059ab3d49bb8fcdf021342b1a1bf3418))
* **roadmap:** mark M4 pt2 implemented (eval-tuning pending) ([0edf716](https://github.com/Smana/runlore/commit/0edf716763ff74e9e4f75ba34999096240e3993c))
* **roadmap:** record POS-4 (hold) + PERSIST-1 (doc-only) decisions ([893a9b3](https://github.com/Smana/runlore/commit/893a9b319480411b2a72e0ff693b1c78b8545806))
* **roadmap:** record POS-4 (hold) + PERSIST-1 (doc-only) decisions ([adf85de](https://github.com/Smana/runlore/commit/adf85de9ed7ba0c18e66cdab1f95bc7a0a805e94))
* **roadmap:** reference the pushed F2 branch ([19df134](https://github.com/Smana/runlore/commit/19df1349898084762c705973e0fa59e3ef92353f))
* **roadmap:** reference the pushed F2 branch for cross-machine resume ([101265d](https://github.com/Smana/runlore/commit/101265dd6efc598d11a35a6b0c2ef333dcd3852d))
* **roadmap:** refresh status to reflect shipped work; record the F2 finding ([c7c703a](https://github.com/Smana/runlore/commit/c7c703a70fc069b8f58a1fb65ff8035cd60efd53))
* **roadmap:** refresh status; record F2 finding ([c9be218](https://github.com/Smana/runlore/commit/c9be218fbd03930ddf0f8bb8df05858c97125bd9))
* **server-hardening:** mark plan tasks complete ([4d4e76d](https://github.com/Smana/runlore/commit/4d4e76d7b84d845d391a89eac4f489768f20c08d))
* **server-hardening:** spec + plan (R9) ([acd35d8](https://github.com/Smana/runlore/commit/acd35d8c528ef8ec82d1a6de18dd1b06b6910f66))
* **spec:** BM25 scorer fix design ([be30358](https://github.com/Smana/runlore/commit/be303587d3a342513e4df82cf3ab753924063f20))
* **spec:** deterministic entity precision in Track A (replay) design ([8b26d2e](https://github.com/Smana/runlore/commit/8b26d2ee1bb55b77c2b0ed8c8b19a63e61d4d900))
* **spec:** durability (opt-in RWX volume + audit fsync) design ([0c837d4](https://github.com/Smana/runlore/commit/0c837d40dc8a09383af229264764d752391b5db2))
* **spec:** eval statistics (k-of-n gate + flaky-fail + N) design ([b17f98d](https://github.com/Smana/runlore/commit/b17f98d17b7537641386c0cc0566176b45c8b180))
* **spec:** investigation coalescing + rate-limit design ([27468d8](https://github.com/Smana/runlore/commit/27468d8dd5865d85c45fdaab595dd244da24d98f))
* **spec:** loop token-budget hard-kill design ([3ef6feb](https://github.com/Smana/runlore/commit/3ef6feb79311e952feeab9187713f484089c0d62))
* **spec:** outcome attribution (coalesce + order-independent Episodes) design ([f4d475e](https://github.com/Smana/runlore/commit/f4d475e2cc4b8578ee7547aa5c21429867e5f66f))
* **spec:** outcome capture (learning-loop A1) design ([c2ba02f](https://github.com/Smana/runlore/commit/c2ba02f990c8fd1025fbab809636bdd24f9129d3))
* **spec:** outcome ledger episodes read API design ([2baf445](https://github.com/Smana/runlore/commit/2baf4457b4e77de05295bdc2cae7902f3b2d6389))
* **spec:** outcome-driven recall decay design ([912529c](https://github.com/Smana/runlore/commit/912529c0cc44b18262686eb6be4b65006bfd0cc7))
* **spec:** recall Gate 2 disambiguation design ([bad7ce7](https://github.com/Smana/runlore/commit/bad7ce7498264fa901b682ffbcecf4c5b28c102e))
* **spec:** recall retrieval (wider candidates + structural pre-filter) design ([0dfb093](https://github.com/Smana/runlore/commit/0dfb09341b0df14fab2b7566e2da30d4deda3c8a))
* **spec:** recall trustworthiness design ([7bee890](https://github.com/Smana/runlore/commit/7bee89032697b893ec46e51cf748255e96c9c592))
* **spec:** refine rate-limit mechanism; add OTel observability + recall metrics ([07bc4f9](https://github.com/Smana/runlore/commit/07bc4f9c5f5b575f3c9c433ca9820b932929723f))
* **specs:** R14 LLM usage accounting + stop_reason design + plan ([6587c69](https://github.com/Smana/runlore/commit/6587c69640c76833f191b79b7a3b33d9d7dd94ac))
* **spec:** wire recall into eval + poisoned-entry proof design ([d7016fd](https://github.com/Smana/runlore/commit/d7016fd3c364668f9441711f35e5614c32f6a2d0))
* **supply-chain:** spec + plan for R13 supply-chain hardening ([660c955](https://github.com/Smana/runlore/commit/660c95572221f0c462b2bf6a99d86b313cd98611))


### Code Refactoring

* /simplify cleanups — op registry, fail-closed-by-construction, audit dedup ([af92327](https://github.com/Smana/runlore/commit/af923277ef32b828843cc521f2defd6d28dc4399))
* **action:** re-validate at the exec boundary + audit at the executor seam ([7bb3e1b](https://github.com/Smana/runlore/commit/7bb3e1be0502594959abf6880cf30bd37fcdbdc2))
* apply /simplify cleanups ([46994e4](https://github.com/Smana/runlore/commit/46994e4a1593cdd1b9e46684f908fd6a2d6c9693))
* **cmd:** extract builder family (catalog, forge, action, curator, investigate, gitops, server) to internal/app ([4b9b944](https://github.com/Smana/runlore/commit/4b9b944105e5c8c5e6e39efa4584b4b6c34b534c))
* **cmd:** extract builder family to internal/app (Phase 2) ([d9c18b9](https://github.com/Smana/runlore/commit/d9c18b99e1efbea156937ced6ad96bc385c7b8ca))
* **cmd:** extract model builders + config predicates to internal/app ([03559a2](https://github.com/Smana/runlore/commit/03559a212ab4dfa9ab4c88597d75a495a42e2de6))
* **cmd:** extract model builders + config predicates to internal/app ([dc55799](https://github.com/Smana/runlore/commit/dc55799408d235c1dc31ac0014188e75cacaca91))
* **cmd:** extract run* subcommand handlers to internal/app; main is now thin dispatch ([c4771b7](https://github.com/Smana/runlore/commit/c4771b794cd93f1eb72ab60ba00fc0c48feff4f0))
* **cmd:** extract run* subcommand handlers to internal/app; main is now thin dispatch ([849b0e0](https://github.com/Smana/runlore/commit/849b0e0bd630e8627f7ba00a32cb02cfbe384b3e))
* **curator:** add Novelty.TopHit; IsDuplicate via it ([ce98797](https://github.com/Smana/runlore/commit/ce9879748354b8120e6d23fed52bbb5ac9102e6c))
* **investigate:** derive submit_findings op enum from providers.Ops ([ff101f4](https://github.com/Smana/runlore/commit/ff101f47f19e736d217ae1332522c03621f16a12))
* **loop:** address Part 3 review minors (const placement, metric desc, test cleanup) ([ecaf7fb](https://github.com/Smana/runlore/commit/ecaf7fb82e5ed721f7831c6f9631d3178d502205))
* **outcome:** DRY workload ref rendering into providers.Workload.Ref() ([8f39df1](https://github.com/Smana/runlore/commit/8f39df1c50fe4c43097b6b2056b63e681ba56d29))
* **outcome:** factor readEvents; New replays via it ([77dcbcf](https://github.com/Smana/runlore/commit/77dcbcf7e6fd38d2575aa359030d9e4587c7ab7a))
* **tools:** rename flux_* deep tools to gitops_* (engine-agnostic, post-FEAT-2) ([f91e4d0](https://github.com/Smana/runlore/commit/f91e4d003a38969c52ef1b802c5e91b3653970ff))
* **tools:** rename flux_* deep tools to gitops_* (post-FEAT-2) ([cdca100](https://github.com/Smana/runlore/commit/cdca100a4de52994f90743236430a1c69a2af281))


### Build System

* add OpenTelemetry metric SDK + prometheus exporter ([80d6590](https://github.com/Smana/runlore/commit/80d659003f9399efb34635af62aff61c6fd8b4d1))
* **deps:** bump actions/checkout from 4.3.1 to 7.0.0 ([1f06df6](https://github.com/Smana/runlore/commit/1f06df62c5744e968ce9aabe773844556f03125b))
* **deps:** bump actions/checkout from 4.3.1 to 7.0.0 ([e392714](https://github.com/Smana/runlore/commit/e392714d06bea3f255955d7764fc9aea9a6b16b6))
* **deps:** bump actions/setup-go from 5.6.0 to 6.5.0 ([7590e73](https://github.com/Smana/runlore/commit/7590e732cbb0060231d88926f9018f19ef172e01))
* **deps:** bump actions/setup-go from 5.6.0 to 6.5.0 ([3eff0f3](https://github.com/Smana/runlore/commit/3eff0f310c23bf9047dc390e2f563679a646d06d))
* **deps:** bump actions/upload-artifact from 4.6.2 to 7.0.1 ([a990ce0](https://github.com/Smana/runlore/commit/a990ce00b574e91d3d926844b0033b4bcd664bd7))
* **deps:** bump actions/upload-artifact from 4.6.2 to 7.0.1 ([f2d6c5a](https://github.com/Smana/runlore/commit/f2d6c5a2b0f2fd70667f64662c9f33bd6e454fd7))
* **deps:** bump github.com/aws/aws-sdk-go-v2/service/ec2 ([859e482](https://github.com/Smana/runlore/commit/859e48299f8a707b6829d08934faceceab4e31eb))
* **deps:** bump github.com/aws/aws-sdk-go-v2/service/ec2 from 1.307.1 to 1.308.0 in the go-minor-patch group ([9c2d6b5](https://github.com/Smana/runlore/commit/9c2d6b5a9eba94d7f5c2a6b03c51bd44a83bc4ef))
* **deps:** bump golangci/golangci-lint-action from 8.0.0 to 9.2.1 ([04061ad](https://github.com/Smana/runlore/commit/04061ad10eb16d82943fbec939c065fa05ce48eb))
* **deps:** bump golangci/golangci-lint-action from 8.0.0 to 9.2.1 ([fd5c291](https://github.com/Smana/runlore/commit/fd5c291f7e21a03dcd01620c5fdab6ffdb97480d))


### Continuous Integration

* add release-please + goreleaser release pipeline ([4ae61e2](https://github.com/Smana/runlore/commit/4ae61e20b175aa3532b0538734e82c598c98327d))
* add release-please + goreleaser release pipeline ([8d54238](https://github.com/Smana/runlore/commit/8d5423893125f38a25324dfc1c76a7a8c4523c52))
* build + push the container image to ghcr.io ([6f59565](https://github.com/Smana/runlore/commit/6f595657fd7590f3b37b394ca643b0f2487cf004))
* build + push the image to ghcr.io (multi-arch, Trivy scan) ([b33ba27](https://github.com/Smana/runlore/commit/b33ba272f3f15a1e6d356e0a876fda958c4a5267))
* **build-image:** sign image with cosign + attach SBOM and SLSA provenance ([450bb0f](https://github.com/Smana/runlore/commit/450bb0f0082122e54b326b01111f4e49833a3794))
* **build-image:** sign image with cosign + attach SBOM and SLSA provenance ([c9b0ef4](https://github.com/Smana/runlore/commit/c9b0ef4a863840550ff205015e37f5b2c2dfd090))
* **release-please:** pin first release to v0.1.0 via initial-version ([72b1ee9](https://github.com/Smana/runlore/commit/72b1ee94b38dbc82f60afe4e048b179309a47a4e))
* **release-please:** start versioning at v0.1.0 (not 1.0.0) ([e5be8ff](https://github.com/Smana/runlore/commit/e5be8ff2c89516580d84158ad7afff5ebd8d245a))
* speed up image build (amd64-only, Go cache mounts, concurrency) ([#43](https://github.com/Smana/runlore/issues/43)) ([c5c147f](https://github.com/Smana/runlore/commit/c5c147f9c57691a56f3a674b3841ffa5b7268393))
* **supply-chain:** add Dependabot for gomod + github-actions (weekly) ([3b13d72](https://github.com/Smana/runlore/commit/3b13d729bdb9273d086d0bd27e07d3801debc48f))
* **supply-chain:** add PR-time Trivy filesystem scan to gate merges ([7447b78](https://github.com/Smana/runlore/commit/7447b78c051089ff2b8b5e1550dc687f42c199b3))
* **supply-chain:** enable gosec; fix HTTP timeouts, file perms, int32 clamp; justify intended findings ([5428eb6](https://github.com/Smana/runlore/commit/5428eb617ad8f9e529cd26ed119d1ff1d12421cc))
* **supply-chain:** run govulncheck against the Go vuln DB ([a29f343](https://github.com/Smana/runlore/commit/a29f3431ae395bb9b74c550c10559a3b5129fdb2))
* **supply-chain:** SHA-pin all GitHub Actions to commit digests ([c0dcde4](https://github.com/Smana/runlore/commit/c0dcde42eeb57984ff9c0b9c66f9dd0e7f8aa7b6))

## [Unreleased]

### Added

- **React → Investigate → Learn loop.** A read-only-first SRE agent that triggers on
  Alertmanager alerts and GitOps-failure events, runs a ReAct investigation
  (`what_changed` → `kb_search` → `submit_findings`), and posts a confidence-scored
  root cause with an evidence trail and suggested next steps.
- **What-changed-first RCA.** Git revision diffing surfaces the exact change behind an
  incident, via the configured GitOps engine.
- **GitOps providers.** Flux (informer-backed) and Argo CD, behind an
  engine-agnostic provider contract.
- **Metrics-agnostic signals.** VictoriaMetrics and Prometheus, with pluggable logs
  (VictoriaLogs) and CNI-agnostic network flows (Hubble).
- **Pluggable models.** Anthropic, Gemini, and any OpenAI-compatible endpoint, in or
  out of cluster.
- **The learning loop.** Every investigation drafts a knowledge-base entry as a GitHub
  pull request into a repo you own (OKF-compatible markdown); merged entries are
  indexed (bleve) so the same incident gets instant recall next time. Curation is
  confidence-routed.
- **Honest-about-uncertainty RCA.** `unresolved` is a first-class answer, and an
  adversarial *verify* pass can only ever lower a finding's confidence.
- **Notifications.** Slack and Matrix notifiers with fan-out.
- **Delivery.** A single Go binary deployed via Helm; `lore serve` (in-cluster,
  webhook-driven) and `lore investigate` (one-shot CLI). Leader election for HA.
- **Eval harness.** `lore eval` replays recorded incident cases and reports the
  root-cause-identification rate; a nightly CI eval guards against RCA regressions.

[Unreleased]: https://github.com/Smana/runlore/commits/main
