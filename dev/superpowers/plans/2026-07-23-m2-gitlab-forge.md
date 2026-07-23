# M2 — GitLab forge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add GitLab — gitlab.com **and self-hosted GitLab** (the strategic case: sovereignty-conscious EU teams run their own instance) — as a knowledge-base forge alongside GitHub. The curator opens **merge requests** with drafted OKF entries, comments on open MRs for duplicate incidents, closes MRs (dedup/lifecycle), opens knowledge-gap issues, and the catalog syncs/clones the KB repo over GitLab auth. Auth is a **project access token** (the simplest self-hosted-friendly path — no instance-admin setup), referenced from a Kubernetes Secret by env-var name, exactly like the GitHub App private key. **No new required config for existing GitHub users**: `forge.provider` defaults to `github`.

**Architecture:** A new `internal/forge/gitlab` package mirrors `internal/forge/github` — a plain REST client over `httpx.SecureClient` (internal/httpx/client.go:27), implementing the **same consumer interfaces**: `providers.CurationForge` (internal/providers/providers.go:529-533), `providers.ReinvestForge` (providers.go:548-556), `curate.Forge` (internal/curate/forge.go:16-23), `curate.ContestedForge` (internal/curate/contested.go:21-25), and `curate.RetireForge` (internal/curate/retirement.go:25-28). The forge-agnostic OKF rendering that today lives inside the github package (`renderEntry`, `prBody`, `slugify`, `neutralizeImages`, bundle `updateIndex`/`updateLog`, retire `setStatusRetired`) is first extracted into a shared root package `internal/forge` (package `forge`) so GitLab never re-implements security-relevant text handling. App wiring gains one seam — `app.BuildForgeClient` — that returns the provider-selected client; `app.BuildForgeTokenSource` (internal/app/forge.go:21) gains a GitLab branch returning the static project token, which transparently feeds catalog git-sync (internal/app/catalog.go:79-82) and source-diff clone auth (internal/app/gitops.go:23) because both use git basic-auth with username `x-access-token` + token password (internal/catalog/sync.go:62, internal/whatchanged/differ.go:143) — GitLab accepts **any non-blank username** with a project-access-token password, so zero changes are needed there.

**Tech Stack:** Go stdlib `net/http` + `internal/httpx` (no new dependency), GitLab REST API v4 (`PRIVATE-TOKEN` header auth), `httptest` fakes with real GitLab JSON for tests, go-git for the (unchanged) clone paths, Helm chart values comments + docs.

**Locked decisions:**

| Decision | Choice | Why |
|---|---|---|
| Client library | **Plain REST via `internal/httpx`** — NOT `gitlab.com/gitlab-org/api/client-go` | The repo's dependency posture is strict: go.mod has **no** GitHub SDK either — `internal/forge/github` is hand-rolled REST over `httpx.SecureClient`. We need ~12 endpoints; the official client would add a large transitive tree for JSON we can decode in a struct literal. Mirroring the github package also keeps the two forges reviewable side by side. |
| Auth | **Project access token** in an env var (`forge.gitlab.token_env`), `PRIVATE-TOKEN` header | Per-project, role-scoped, expiring, creatable by a project Maintainer on ANY self-hosted instance (a GitLab "Application"/OAuth flow needs instance-level setup — hostile to the self-hosted case). Matches design.md §14: "Non-GitHub hosts (GitLab, self-hosted) fall back to a scoped access token" (docs/design.md:523). |
| Provider selection | `forge.provider: gitlab` (default `""` ⇒ `github`) | One explicit key; empty keeps every existing GitHub config byte-identical in behavior (simplicity constraint: no new required config). |
| KB repo addressing | `forge.kb_repo` holds the **full GitLab project path** (`group/subgroup/project`, nested groups allowed) | GitLab's REST `:id` accepts the URL-encoded path; no numeric project-ID hunting for users. GitHub keeps its `owner/name` split (the `strings.Cut` stays in the GitHub arm only). |
| Closed-unmerged listing | `GET /merge_requests?state=closed&labels=…` | GitLab MR states are `opened/closed/merged/locked` — `closed` **is** closed-without-merge (merged is a separate state), so the rejection-suppression set needs no search API, unlike GitHub (internal/forge/github/github.go:285-302). |
| iid namespace split | Per-client artifact-kind memo + MR-endpoint probe fallback | GitHub shares ONE number space between issues and PRs; GitLab numbers MRs and issues independently, but every consumer interface passes a bare `int`. Verified call-flow: every mutation site learns the number from a listing or `IsPROpen` **on the same client** first (curate/dedup.go:31→56/60, lifecycle.go:35→45/49, resolution.go:32→50, contested.go:75→91/100, reinvestigate.go:51→77/82, curator.go:176→125), so the memo is always primed in practice; the probe is a safety net. |
| Git clone auth | Reuse the shared forge token via the existing `x-access-token` basic-auth | GitLab ignores the username when the password is a valid token — `catalog.Syncer.auth` and `whatchanged.Differ.auth` work unmodified. |

**Out of scope (state in the PR description):** mixed-host setups (KB on GitLab + *private* source repos on GitHub get no GitHub token — public source repos still diff fine); GitLab webhooks (RunLore polls the forge outbound, same as GitHub — providers.go:548-550); GitLab CI templates.

---

### Task 1: Extract forge-agnostic OKF rendering into `internal/forge` (package `forge`)

The github package owns ~300 lines of pure, host-independent text handling that the GitLab client must reuse, not duplicate — including the security-relevant `neutralizeImages` (github.go:372-388) and the YAML-marshal frontmatter injection guard (github.go:411-439). This is a behavior-preserving move; the existing github test suite is the net.

**Files:**
- Create: `internal/forge/render.go`, `internal/forge/bundle.go`, `internal/forge/retire.go`
- Modify: `internal/forge/github/github.go`, `internal/forge/github/bundle.go`, `internal/forge/github/retire.go`, `internal/curate/retirement.go`, `internal/curate/retirement_test.go`
- Test: existing `internal/forge/github/*_test.go`, `internal/curate/retirement_test.go` (unchanged assertions)

- [ ] **Step 1: Baseline — record the green suite**

  ```bash
  go test ./internal/forge/... ./internal/curate/ ./internal/curator/
  ```

  Expected: `ok` for all three (this is the refactor's before/after contract).

- [ ] **Step 2: Create `internal/forge/render.go`** — move these, verbatim bodies, exported names, receiver-free:

  | New name (package `forge`) | Moved from |
  |---|---|
  | `func Slugify(s string) string` | github.go:551-568 `slugify` |
  | `func EntryPath(e providers.KBEntry, slug string, now int64) string` | github.go:540-549 `entryPath` |
  | `func NeutralizeImages(s string) string` (+ `imageRe`) | github.go:372-388 |
  | `type kbFrontmatter` (stays unexported) | github.go:414-439 |
  | `func RenderEntry(e providers.KBEntry) string` | github.go:509-531 `renderEntry` |
  | `func IssueTitle(inv providers.Investigation) string` | github.go:390-395 `issueTitle` |
  | `func IssueBody(inv providers.Investigation) string` | github.go:397-409 `issueBody` |
  | `func PRBody(e providers.KBEntry, blobURL func(path string) string) string` | github.go:446-460 `(c *Client) prBody` — the ONE signature change: `c.blobURL` becomes the `blobURL` parameter |
  | `func relatedSection(e providers.KBEntry, blobURL func(string) string) string` (unexported helper of PRBody) | github.go:465-484 `(c *Client) relatedSection`, same parameterization |
  | `var LifecycleLabels = []string{"runlore", "triggered"}` | github.go:98 `lifecycleLabels` |
  | `var RetireLabels = []string{"runlore", "runlore-retire"}` | retire.go:55 `retireLabels` |

  File header: `// SPDX-License-Identifier: Apache-2.0` + package doc:

  ```go
  // Package forge holds the forge-agnostic half of RunLore's git-forge clients:
  // OKF entry/PR-body rendering, bundle (index.md/log.md) maintenance, and the
  // retirement frontmatter stamp. Host clients (github, gitlab) own transport,
  // auth, and endpoint mapping; everything that decides WHAT text reaches a
  // forge-rendered surface lives here exactly once — including the untrusted-
  // markdown image neutralization, which must never fork between hosts.
  package forge
  ```

- [ ] **Step 3: Create `internal/forge/bundle.go`** — move verbatim:
  - `func UpdateIndex(existing []byte, e providers.KBEntry, entryPath string) []byte` ← github/bundle.go:51-81 `updateIndex`
  - `func UpdateLog(existing []byte, e providers.KBEntry, entryPath, date string) []byte` ← github/bundle.go:85-113 `updateLog`

- [ ] **Step 4: Create `internal/forge/retire.go`** — move verbatim:
  - `var ErrAlreadyRetired = errors.New("entry already retired on base branch")` ← github/retire.go:19
  - `func SetStatusRetired(content []byte) (out []byte, already bool, err error)` ← github/retire.go:28-50 `setStatusRetired`

- [ ] **Step 5: Delegate from the github package** (import `forge "github.com/Smana/runlore/internal/forge"`). Replace each moved definition with a thin binding so every internal call site and test compiles unchanged:

  ```go
  // github.go — moved to the shared forge package; bound here so call sites/tests are unchanged.
  var (
      slugify           = forge.Slugify
      entryPath         = forge.EntryPath
      neutralizeImages  = forge.NeutralizeImages
      renderEntry       = forge.RenderEntry
      issueTitle        = forge.IssueTitle
      issueBody         = forge.IssueBody
      lifecycleLabels   = forge.LifecycleLabels
  )

  func (c *Client) prBody(e providers.KBEntry) string { return forge.PRBody(e, c.blobURL) }
  ```

  ```go
  // bundle.go
  var (
      updateIndex = forge.UpdateIndex
      updateLog   = forge.UpdateLog
  )
  ```

  ```go
  // retire.go
  // ErrAlreadyRetired aliases the shared sentinel so errors.Is works across packages.
  var ErrAlreadyRetired = forge.ErrAlreadyRetired

  var setStatusRetired = forge.SetStatusRetired

  var retireLabels = forge.RetireLabels
  ```

  Delete the moved bodies (and `imageRe`, `kbFrontmatter`, `relatedSection`) from the github package. Keep `(c *Client) blobURL` (github.go:495-507) — it is host-specific.

- [ ] **Step 6: Point `internal/curate` at the shared sentinel** — in `internal/curate/retirement.go:107` and `internal/curate/retirement_test.go:140`, replace `github.ErrAlreadyRetired` with `forge.ErrAlreadyRetired` and swap the import from `internal/forge/github` to `internal/forge`. (The github alias in Step 5 keeps both spellings `errors.Is`-equal, but curate should not import a host package for a host-neutral sentinel.)

- [ ] **Step 7: Verify the net**

  ```bash
  go build ./... && go test ./internal/forge/... ./internal/curate/ ./internal/curator/ ./internal/app/
  ```

  Expected: all `ok`, zero test edits beyond the two import/identifier swaps in Step 6.

- [ ] **Step 8: Commit**

  ```bash
  git add internal/forge internal/curate
  git commit -m "refactor(forge): extract forge-agnostic OKF rendering into internal/forge"
  ```

### Task 2: Config — `forge.provider` + `forge.gitlab` keys, validation

**Files:**
- Modify: `internal/config/config.go` (Forge struct at line 1290, Validate at line 1013)
- Test: `internal/config/config_test.go` (append)

- [ ] **Step 1: Write the failing test** — append to `internal/config/config_test.go`:

  ```go
  // writeForgeCfg loads a config whose only block is forge — provider selection
  // must be validatable without any other subsystem configured.
  func writeForgeCfg(t *testing.T, forgeYAML string) (*Config, error) {
      t.Helper()
      p := filepath.Join(t.TempDir(), "runlore.yaml")
      if err := os.WriteFile(p, []byte("forge:\n"+forgeYAML), 0o600); err != nil {
          t.Fatal(err)
      }
      return Load(p)
  }

  func TestForgeProviderValidation(t *testing.T) {
      t.Run("unknown provider rejected", func(t *testing.T) {
          _, err := writeForgeCfg(t, "  provider: bitbucket\n")
          if err == nil || !strings.Contains(err.Error(), "forge.provider") {
              t.Fatalf("want forge.provider error, got %v", err)
          }
      })
      t.Run("gitlab requires token_env", func(t *testing.T) {
          _, err := writeForgeCfg(t, "  provider: gitlab\n  kb_repo: group/runlore-kb\n")
          if err == nil || !strings.Contains(err.Error(), "forge.gitlab.token_env") {
              t.Fatalf("want token_env error, got %v", err)
          }
      })
      t.Run("gitlab full config valid, nested project path allowed", func(t *testing.T) {
          cfg, err := writeForgeCfg(t, strings.Join([]string{
              "  provider: gitlab",
              "  kb_repo: group/subgroup/runlore-kb",
              "  base_branch: main",
              "  gitlab:",
              "    base_url: https://gitlab.example.com",
              "    token_env: GITLAB_TOKEN",
          }, "\n")+"\n")
          if err != nil {
              t.Fatalf("valid gitlab forge rejected: %v", err)
          }
          if cfg.Forge.GitLab.BaseURL != "https://gitlab.example.com" || cfg.Forge.GitLab.TokenEnv != "GITLAB_TOKEN" {
              t.Fatalf("gitlab block not parsed: %+v", cfg.Forge.GitLab)
          }
      })
      t.Run("empty provider stays valid (github default, existing configs untouched)", func(t *testing.T) {
          if _, err := writeForgeCfg(t, "  kb_repo: owner/name\n"); err != nil {
              t.Fatalf("github-default forge rejected: %v", err)
          }
      })
  }
  ```

- [ ] **Step 2: Run it — expect FAIL**

  ```bash
  go test ./internal/config/ -run TestForgeProviderValidation
  ```

  Expected: `FAIL github.com/Smana/runlore/internal/config [build failed]` — `cfg.Forge.GitLab` and `Provider` are undefined (Load's `KnownFields(true)` at internal/config/load.go:24 would also reject the new YAML keys).

- [ ] **Step 3: Implement.** In `internal/config/config.go`, extend `Forge` (line 1290) and add `GitLabForge` after `GitHubApp` (line 1314):

  ```go
  // Forge holds git-forge authentication and the curation target repo.
  type Forge struct {
      // Provider selects the forge kind: "github" (default when empty) or "gitlab"
      // (gitlab.com or self-hosted via gitlab.base_url). Existing GitHub configs
      // need no change — the empty default preserves their exact behavior.
      Provider      string      `yaml:"provider"`
      GitHubApp     GitHubApp   `yaml:"github_app"`
      GitLab        GitLabForge `yaml:"gitlab"`
      KBRepo        string      `yaml:"kb_repo"`        // GitHub: "owner/name". GitLab: full project path ("group/sub/project")
      BaseBranch    string      `yaml:"base_branch"`    // PR/MR target branch (default "main")
      GitHubAPIURL  string      `yaml:"github_api_url"` // override for GHES/tests (default https://api.github.com)
      DupScore      float64     `yaml:"dup_score"`      // file-time catalog BM25 dedup threshold (default 5.0)
      MinConfidence float64     `yaml:"min_confidence"` // file-time quality gate: min overall confidence (default 0.75)
      // ... SkipVerdicts unchanged (keep the existing field + comment, config.go:1297-1302)
      SkipVerdicts []string `yaml:"skip_verdicts"`
  }

  // GitLabForge holds GitLab forge settings (gitlab.com or self-hosted). Auth is a
  // project access token — per-project, role-scoped, expiring; the self-hosted-
  // friendly counterpart of the GitHub App — referenced from a Secret by env-var
  // name, never inlined (same *_env convention as github_app.private_key_env).
  type GitLabForge struct {
      BaseURL  string `yaml:"base_url"`  // default https://gitlab.com; self-hosted: https://gitlab.example.com
      TokenEnv string `yaml:"token_env"` // env var holding the project access token
  }
  ```

  In `Validate()` (config.go:1013), next to the `forge.skip_verdicts` gate at config.go:1159-1166, add:

  ```go
  // Forge provider gate: an unknown provider or a GitLab selection without its
  // token env must fail at startup, not as a silent GitHub fallback that then
  // 401s against the wrong host.
  switch c.Forge.Provider {
  case "", "github", "gitlab":
  default:
      return fmt.Errorf("forge.provider: unknown provider %q (want github|gitlab)", c.Forge.Provider)
  }
  if c.Forge.Provider == "gitlab" && c.Forge.GitLab.TokenEnv == "" {
      return fmt.Errorf("forge.provider gitlab requires forge.gitlab.token_env (the env var holding the project access token)")
  }
  ```

- [ ] **Step 4: Run — expect PASS**

  ```bash
  go test ./internal/config/
  ```

- [ ] **Step 5: Commit**

  ```bash
  git add internal/config
  git commit -m "feat(config): forge.provider + forge.gitlab keys for the GitLab forge"
  ```

### Task 3: GitLab client core — `New`/`do`, auth header, error mapping, `OpenIssue`

**Files:**
- Create: `internal/forge/gitlab/gitlab.go`
- Test: `internal/forge/gitlab/gitlab_test.go`

- [ ] **Step 1: Write the failing tests** — create `internal/forge/gitlab/gitlab_test.go` (mirrors `internal/forge/github/github_test.go` style: httptest fake, real JSON, request assertions):

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package gitlab

  import (
      "context"
      "encoding/json"
      "net/http"
      "net/http/httptest"
      "strings"
      "testing"

      "github.com/Smana/runlore/internal/providers"
  )

  func staticToken() func(context.Context) (string, error) {
      return func(context.Context) (string, error) { return "glpat-tok", nil }
  }

  func TestOpenIssue(t *testing.T) {
      var gotAuth, gotPath string
      var gotBody map[string]any
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          // EscapedPath, not Path: the project path is %2F-encoded in the URL and
          // the client must NOT send a literal slash (GitLab would 404 the route).
          gotAuth, gotPath = r.Header.Get("PRIVATE-TOKEN"), r.URL.EscapedPath()
          _ = json.NewDecoder(r.Body).Decode(&gotBody)
          w.WriteHeader(http.StatusCreated)
          _, _ = w.Write([]byte(`{"id":83,"iid":7,"project_id":42,"title":"Boom","state":"opened",
            "labels":["runlore","triggered"],
            "web_url":"https://gitlab.example.com/platform/runlore-kb/-/issues/7"}`))
      }))
      defer srv.Close()

      c := New(srv.URL, "platform/runlore-kb", "main", staticToken())
      ref, err := c.OpenIssue(context.Background(), providers.Investigation{Title: "Boom", Confidence: 0.4,
          RootCauses: []providers.Hypothesis{{Summary: "db down"}}})
      if err != nil {
          t.Fatalf("OpenIssue: %v", err)
      }
      if gotAuth != "glpat-tok" || gotPath != "/api/v4/projects/platform%2Frunlore-kb/issues" {
          t.Fatalf("auth=%q path=%q", gotAuth, gotPath)
      }
      if title, _ := gotBody["title"].(string); title != "Boom" {
          t.Fatalf("title=%v", gotBody["title"])
      }
      if labels, _ := gotBody["labels"].(string); labels != "runlore,triggered" {
          t.Fatalf("labels=%v (GitLab takes a comma-joined string at creation)", gotBody["labels"])
      }
      if ref.URL != "https://gitlab.example.com/platform/runlore-kb/-/issues/7" {
          t.Fatalf("ref=%s", ref.URL)
      }
  }

  func TestDoMapsErrorStatuses(t *testing.T) {
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          switch r.Header.Get("PRIVATE-TOKEN") {
          case "expired":
              w.WriteHeader(http.StatusUnauthorized)
              _, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
          case "wrong-project":
              w.WriteHeader(http.StatusNotFound)
              _, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
          default: // duplicate MR for the same source branch
              w.WriteHeader(http.StatusConflict)
              _, _ = w.Write([]byte(`{"message":["Another open merge request already exists for this source branch: !12"]}`))
          }
      }))
      defer srv.Close()

      mk := func(tok string) *Client {
          return New(srv.URL, "g/p", "main", func(context.Context) (string, error) { return tok, nil })
      }
      _, err := mk("expired").OpenIssue(context.Background(), providers.Investigation{Title: "x"})
      if err == nil || !strings.Contains(err.Error(), "status 401") || !strings.Contains(err.Error(), "forge.gitlab.token_env") {
          t.Fatalf("401 must name the token env knob, got: %v", err)
      }
      _, err = mk("wrong-project").OpenIssue(context.Background(), providers.Investigation{Title: "x"})
      if err == nil || !strings.Contains(err.Error(), "status 404") || !strings.Contains(err.Error(), "forge.kb_repo") {
          t.Fatalf("404 must point at the project path knob, got: %v", err)
      }
      _, err = mk("ok").OpenIssue(context.Background(), providers.Investigation{Title: "x"})
      if err == nil || !strings.Contains(err.Error(), "status 409") || !strings.Contains(err.Error(), "Another open merge request") {
          t.Fatalf("409 must surface GitLab's conflict message, got: %v", err)
      }
  }
  ```

- [ ] **Step 2: Run — expect FAIL**

  ```bash
  go test ./internal/forge/gitlab/
  ```

  Expected: `FAIL github.com/Smana/runlore/internal/forge/gitlab [build failed]` (`undefined: New`, `undefined: Client`).

- [ ] **Step 3: Implement** — create `internal/forge/gitlab/gitlab.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  // Package gitlab is RunLore's GitLab forge client (curation + re-investigation)
  // over the GitLab REST API v4, authenticated with a project access token. It
  // serves gitlab.com AND self-hosted GitLab (baseURL override) and satisfies the
  // same consumer surfaces as the GitHub client: providers.CurationForge /
  // providers.ReinvestForge / curate.Forge / curate.ContestedForge /
  // curate.RetireForge.
  //
  // One structural difference from GitHub is load-bearing everywhere here:
  // GitHub shares a single number space between issues and PRs; GitLab numbers
  // merge requests and issues independently (MR !5 and issue #5 are different
  // artifacts). The consumer interfaces pass a bare int, so this client keeps a
  // per-artifact kind memo — see kindOf.
  package gitlab

  import (
      "bytes"
      "context"
      "encoding/json"
      "errors"
      "fmt"
      "io"
      "net/http"
      "net/url"
      "strings"
      "sync"
      "time"

      forge "github.com/Smana/runlore/internal/forge"
      "github.com/Smana/runlore/internal/httpx"
      "github.com/Smana/runlore/internal/providers"
  )

  // TokenFunc returns a valid token. In production this is a static project
  // access token read from the env once; the indirection matches the GitHub
  // client's TokenFunc so app wiring stays uniform across providers.
  type TokenFunc func(ctx context.Context) (string, error)

  // DefaultBaseURL is public gitlab.com. Override for self-hosted GitLab
  // (e.g. https://gitlab.example.com) or tests. Unlike the GitHub client's
  // DefaultBaseURL, this is the WEB host — the client appends /api/v4 itself,
  // so config stays "the URL you open in a browser".
  const DefaultBaseURL = "https://gitlab.com"

  type artifactKind int

  const (
      kindUnknown artifactKind = iota
      kindMR
      kindIssue
  )

  // Client is a GitLab forge client scoped to one project.
  type Client struct {
      baseURL    string // https://host, no /api/v4 suffix
      project    string // full project path ("group/sub/repo"); URL-encoded on use
      baseBranch string
      token      TokenFunc
      http       *http.Client

      mu    sync.Mutex
      kinds map[int]artifactKind // iid → namespace memo; see kindOf
  }

  var (
      _ providers.CurationForge = (*Client)(nil)
      _ providers.ReinvestForge = (*Client)(nil)
  )

  // New builds a client. baseURL may be empty (defaults to DefaultBaseURL);
  // project is the FULL project path (nested groups allowed); baseBranch is the
  // MR target (e.g. "main").
  func New(baseURL, project, baseBranch string, token TokenFunc) *Client {
      if baseURL == "" {
          baseURL = DefaultBaseURL
      }
      return &Client{
          baseURL: strings.TrimRight(baseURL, "/"), project: project,
          baseBranch: baseBranch, token: token,
          http:  httpx.SecureClient(30 * time.Second),
          kinds: make(map[int]artifactKind),
      }
  }

  // proj is the URL-encoded project path used as the :id in every API route.
  func (c *Client) proj() string { return url.PathEscape(c.project) }

  func (c *Client) base() string {
      if c.baseBranch == "" {
          return "main"
      }
      return c.baseBranch
  }

  // statusError carries the HTTP status so callers can branch on 404 (kindOf's
  // MR-endpoint probe, getFile's optional bundle files) without string-matching.
  type statusError struct {
      status int
      msg    string
  }

  func (e *statusError) Error() string { return e.msg }

  func isNotFound(err error) bool {
      var se *statusError
      return errors.As(err, &se) && se.status == http.StatusNotFound
  }

  // do performs an authenticated JSON request and decodes the response into out
  // (if non-nil). Non-2xx maps to a statusError whose message names the config
  // knob to check — a 401 against a self-hosted instance must say "rotate the
  // token", not just echo a status code.
  func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
      tok, err := c.token(ctx)
      if err != nil {
          return fmt.Errorf("token: %w", err)
      }
      var rdr io.Reader
      if body != nil {
          b, err := json.Marshal(body)
          if err != nil {
              return err
          }
          rdr = bytes.NewReader(b)
      }
      req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v4"+path, rdr)
      if err != nil {
          return err
      }
      req.Header.Set("PRIVATE-TOKEN", tok)
      if body != nil {
          req.Header.Set("Content-Type", "application/json")
      }
      resp, err := c.http.Do(req)
      if err != nil {
          return err
      }
      defer func() { _ = resp.Body.Close() }()
      data, _ := io.ReadAll(resp.Body)
      if resp.StatusCode/100 != 2 {
          hint := ""
          switch resp.StatusCode {
          case http.StatusUnauthorized:
              hint = " (token rejected — check forge.gitlab.token_env: expired, revoked, or missing the api scope)"
          case http.StatusForbidden:
              hint = " (token lacks permission — the project access token needs the Developer role)"
          case http.StatusNotFound:
              hint = " (not found — check forge.kb_repo is the full project path and the token's project can see it)"
          }
          return &statusError{status: resp.StatusCode,
              msg: fmt.Sprintf("gitlab %s %s: status %d%s: %s", method, path, resp.StatusCode, hint, string(data[:min(len(data), 512)]))}
      }
      if out != nil {
          return json.Unmarshal(data, out)
      }
      return nil
  }

  func (c *Client) remember(iid int, k artifactKind) {
      c.mu.Lock()
      c.kinds[iid] = k
      c.mu.Unlock()
  }

  // OpenIssue files an issue describing the investigation. Unlike GitHub, labels
  // are set at creation (comma-joined) — no follow-up labelling call.
  func (c *Client) OpenIssue(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
      body := map[string]any{
          "title":       forge.IssueTitle(inv),
          "description": forge.IssueBody(inv),
          "labels":      strings.Join(forge.LifecycleLabels, ","),
      }
      var out struct {
          IID    int    `json:"iid"`
          WebURL string `json:"web_url"`
      }
      if err := c.do(ctx, http.MethodPost, "/projects/"+c.proj()+"/issues", body, &out); err != nil {
          return providers.Ref{}, err
      }
      c.remember(out.IID, kindIssue)
      return providers.Ref{URL: out.WebURL}, nil
  }
  ```

  (The two `var _` interface pins will not compile until Tasks 4-6 add the remaining methods — for THIS task only, comment out the `providers.ReinvestForge` pin and re-enable it in Task 6. `providers.CurationForge` pins in Task 6 too; keep both commented with a `// TODO(M2 Task 6): re-enable` marker so the suite stays runnable per-task.)

- [ ] **Step 4: Run — expect PASS**

  ```bash
  go test ./internal/forge/gitlab/
  ```

- [ ] **Step 5: Commit**

  ```bash
  git add internal/forge/gitlab
  git commit -m "feat(forge): GitLab client core — project-token auth, error mapping, OpenIssue"
  ```

### Task 4: Listings — open MRs/issues by label, closed-unmerged MRs, pagination

**Files:**
- Modify: `internal/forge/gitlab/gitlab.go`
- Test: `internal/forge/gitlab/gitlab_test.go` (append)

- [ ] **Step 1: Write the failing tests** — append:

  ```go
  func TestListPRsByLabel(t *testing.T) {
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          if r.URL.EscapedPath() != "/api/v4/projects/g%2Fp/merge_requests" ||
              r.URL.Query().Get("labels") != "runlore" || r.URL.Query().Get("state") != "opened" {
              t.Fatalf("unexpected request: %s?%s", r.URL.EscapedPath(), r.URL.RawQuery)
          }
          // GitLab labels are a plain string array; description is the MR body.
          _, _ = w.Write([]byte(`[
            {"iid":48,"title":"KB: HarborRegistryDown","description":"b <!-- runlore-fingerprint: abc123 -->",
             "labels":["runlore","triggered"],"state":"opened","updated_at":"2026-06-01T12:00:00Z",
             "web_url":"https://gitlab.example.com/g/p/-/merge_requests/48"}
          ]`))
      }))
      defer srv.Close()

      c := New(srv.URL, "g/p", "main", staticToken())
      prs, err := c.ListPRsByLabel(context.Background(), "runlore")
      if err != nil {
          t.Fatalf("ListPRsByLabel: %v", err)
      }
      if len(prs) != 1 || prs[0].Number != 48 || prs[0].Title != "KB: HarborRegistryDown" {
          t.Fatalf("want MR !48, got %+v", prs)
      }
      if len(prs[0].Labels) != 2 || prs[0].Labels[0] != "runlore" {
          t.Fatalf("labels not parsed: %+v", prs[0].Labels)
      }
      if prs[0].UpdatedAt.IsZero() {
          t.Fatalf("updated_at not parsed: %+v", prs[0])
      }
      if got := providers.ParseFingerprintMarker(prs[0].Body); got != "abc123" {
          t.Fatalf("description must map to CuratedIssue.Body (fingerprint dedup reads it), got %q", got)
      }
  }

  func TestListIssuesByLabel(t *testing.T) {
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          if r.URL.EscapedPath() != "/api/v4/projects/g%2Fp/issues" || r.URL.Query().Get("state") != "opened" {
              t.Fatalf("unexpected request: %s?%s", r.URL.EscapedPath(), r.URL.RawQuery)
          }
          _, _ = w.Write([]byte(`[
            {"iid":39,"title":"Knowledge gap: harbor cascades","description":"b","labels":["runlore"],
             "state":"opened","updated_at":"2026-06-02T08:00:00Z"}
          ]`))
      }))
      defer srv.Close()

      c := New(srv.URL, "g/p", "main", staticToken())
      issues, err := c.ListIssuesByLabel(context.Background(), "runlore")
      if err != nil {
          t.Fatalf("ListIssuesByLabel: %v", err)
      }
      if len(issues) != 1 || issues[0].Number != 39 {
          t.Fatalf("want issue #39, got %+v", issues)
      }
  }

  func TestListClosedUnmergedPRsByLabel(t *testing.T) {
      // GitLab MR states are opened/closed/merged/locked — state=closed IS the
      // closed-without-merge (human-rejected) set; no search API needed.
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          if r.URL.Query().Get("state") != "closed" || r.URL.Query().Get("labels") != "runlore" {
              t.Fatalf("unexpected query: %s", r.URL.RawQuery)
          }
          _, _ = w.Write([]byte(`[
            {"iid":50,"title":"KB: rejected entry","description":"b","labels":["runlore","not-kb-worthy"],
             "state":"closed","updated_at":"2026-06-01T12:00:00Z"}
          ]`))
      }))
      defer srv.Close()

      c := New(srv.URL, "g/p", "main", staticToken())
      prs, err := c.ListClosedUnmergedPRsByLabel(context.Background(), "runlore")
      if err != nil {
          t.Fatalf("ListClosedUnmergedPRsByLabel: %v", err)
      }
      if len(prs) != 1 || prs[0].Number != 50 || prs[0].Labels[1] != "not-kb-worthy" {
          t.Fatalf("want closed MR !50, got %+v", prs)
      }
  }

  func TestListPRsByLabelPaginates(t *testing.T) {
      // Page 1 returns a full page (100) → the client must fetch page 2. Same
      // silent-truncation guard as the GitHub client (github.go:193-211).
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          switch r.URL.Query().Get("page") {
          case "1":
              items := make([]string, 100)
              for i := range items {
                  items[i] = fmt.Sprintf(`{"iid":%d,"title":"KB: t%d","labels":["runlore"],"state":"opened"}`, i+1, i+1)
              }
              _, _ = w.Write([]byte(`[` + strings.Join(items, ",") + `]`))
          case "2":
              _, _ = w.Write([]byte(`[{"iid":101,"title":"KB: t101","labels":["runlore"],"state":"opened"}]`))
          default:
              _, _ = w.Write([]byte(`[]`))
          }
      }))
      defer srv.Close()

      c := New(srv.URL, "g/p", "main", staticToken())
      prs, err := c.ListPRsByLabel(context.Background(), "runlore")
      if err != nil {
          t.Fatalf("ListPRsByLabel: %v", err)
      }
      if len(prs) != 101 || prs[100].Number != 101 {
          t.Fatalf("pagination lost items: got %d", len(prs))
      }
  }
  ```

  (Add `"fmt"` to the test imports.)

- [ ] **Step 2: Run — expect FAIL** (`undefined: (*Client).ListPRsByLabel` etc.)

  ```bash
  go test ./internal/forge/gitlab/
  ```

- [ ] **Step 3: Implement** — append to `gitlab.go`:

  ```go
  // rawItem is one entry from the MR or issue collection endpoints. GitLab keeps
  // the two collections separate (no pull_request-marker filtering, unlike
  // GitHub's shared issues endpoint) and serves labels as a plain string array.
  type rawItem struct {
      IID         int       `json:"iid"`
      Title       string    `json:"title"`
      Description string    `json:"description"`
      Labels      []string  `json:"labels"`
      State       string    `json:"state"`
      UpdatedAt   time.Time `json:"updated_at"`
  }

  func (ri rawItem) curated() providers.CuratedIssue {
      return providers.CuratedIssue{Number: ri.IID, Title: ri.Title, Body: ri.Description,
          Labels: ri.Labels, UpdatedAt: ri.UpdatedAt}
  }

  // listAll fetches ALL pages of a collection endpoint (GitLab caps a page at 100).
  // Without the loop the curate passes would be blind past the first 100 — the
  // same silent-truncation hazard the GitHub client guards against.
  func (c *Client) listAll(ctx context.Context, base string) ([]rawItem, error) {
      var all []rawItem
      for page := 1; ; page++ {
          var raw []rawItem
          if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s&per_page=100&page=%d", base, page), nil, &raw); err != nil {
              return nil, err
          }
          all = append(all, raw...)
          if len(raw) < 100 { // last page (a full page is exactly 100)
              break
          }
      }
      return all, nil
  }

  func (c *Client) listCurated(ctx context.Context, base string, kind artifactKind) ([]providers.CuratedIssue, error) {
      raw, err := c.listAll(ctx, base)
      if err != nil {
          return nil, err
      }
      out := make([]providers.CuratedIssue, 0, len(raw))
      for _, ri := range raw {
          c.remember(ri.IID, kind)
          out = append(out, ri.curated())
      }
      return out, nil
  }

  // ListPRsByLabel returns all open merge requests carrying the label.
  func (c *Client) ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
      return c.listCurated(ctx,
          fmt.Sprintf("/projects/%s/merge_requests?state=opened&labels=%s", c.proj(), url.QueryEscape(label)), kindMR)
  }

  // ListIssuesByLabel returns all open issues carrying the label. GitLab's issues
  // endpoint returns only issues — no PR filtering needed.
  func (c *Client) ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
      return c.listCurated(ctx,
          fmt.Sprintf("/projects/%s/issues?state=opened&labels=%s", c.proj(), url.QueryEscape(label)), kindIssue)
  }

  // ListClosedUnmergedPRsByLabel returns closed-but-not-merged MRs carrying the
  // label — the KB entries a human deliberately rejected (drives the curate
  // suppression set). GitLab models "merged" as its own MR state, so
  // state=closed is exactly the closed-unmerged set: server-side filtering with
  // no search API and no MergedAt backstop needed.
  func (c *Client) ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
      return c.listCurated(ctx,
          fmt.Sprintf("/projects/%s/merge_requests?state=closed&labels=%s", c.proj(), url.QueryEscape(label)), kindMR)
  }
  ```

- [ ] **Step 4: Run — expect PASS**

  ```bash
  go test ./internal/forge/gitlab/
  ```

- [ ] **Step 5: Commit**

  ```bash
  git add internal/forge/gitlab
  git commit -m "feat(forge): GitLab listings — open MRs/issues, closed-unmerged MRs, pagination"
  ```

### Task 5: Comment / Close / ReplaceLabel / notes / IsPROpen across the split iid namespaces

**Files:**
- Modify: `internal/forge/gitlab/gitlab.go`
- Test: `internal/forge/gitlab/gitlab_test.go` (append)

- [ ] **Step 1: Write the failing tests** — append:

  ```go
  // fakeProject wires a minimal stateful GitLab: MR !48 exists and is open,
  // issue #39 exists; note posts and updates are recorded per namespace.
  func fakeProject(t *testing.T) (*httptest.Server, *[]string) {
      t.Helper()
      var calls []string
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          p := r.URL.EscapedPath()
          calls = append(calls, r.Method+" "+p)
          switch {
          case p == "/api/v4/projects/g%2Fp/merge_requests/48" && r.Method == http.MethodGet:
              _, _ = w.Write([]byte(`{"iid":48,"state":"opened","title":"KB: X"}`))
          case p == "/api/v4/projects/g%2Fp/merge_requests/39" && r.Method == http.MethodGet:
              w.WriteHeader(http.StatusNotFound) // 39 is an issue, not an MR
              _, _ = w.Write([]byte(`{"message":"404 Not found"}`))
          case strings.HasPrefix(p, "/api/v4/projects/g%2Fp/merge_requests/48/notes"):
              if r.Method == http.MethodGet {
                  _, _ = w.Write([]byte(`[
                    {"id":301,"body":"human review comment","system":false},
                    {"id":302,"body":"added runlore label","system":true},
                    {"id":303,"body":"dup notice <!-- runlore-contested: k1 -->","system":false}
                  ]`))
                  return
              }
              w.WriteHeader(http.StatusCreated)
              _, _ = w.Write([]byte(`{"id":304,"body":"posted"}`))
          case strings.HasPrefix(p, "/api/v4/projects/g%2Fp/issues/39/notes"):
              w.WriteHeader(http.StatusCreated)
              _, _ = w.Write([]byte(`{"id":305,"body":"posted"}`))
          case p == "/api/v4/projects/g%2Fp/merge_requests/48" && r.Method == http.MethodPut:
              _, _ = w.Write([]byte(`{"iid":48,"state":"closed"}`))
          case p == "/api/v4/projects/g%2Fp/issues/39" && r.Method == http.MethodPut:
              _, _ = w.Write([]byte(`{"iid":39,"state":"opened"}`))
          default:
              t.Fatalf("unexpected request: %s %s", r.Method, p)
          }
      }))
      return srv, &calls
  }

  func TestIsPROpenAndNamespaceMemo(t *testing.T) {
      srv, calls := fakeProject(t)
      defer srv.Close()
      c := New(srv.URL, "g/p", "main", staticToken())

      open, err := c.IsPROpen(context.Background(), 48)
      if err != nil || !open {
          t.Fatalf("IsPROpen(48)=%v,%v want true", open, err)
      }
      // The memo learned 48 is an MR: the follow-up Comment must go straight to
      // MR notes with NO second probe GET.
      if err := c.Comment(context.Background(), 48, "coalesce"); err != nil {
          t.Fatalf("Comment(48): %v", err)
      }
      probes := 0
      for _, cl := range *calls {
          if cl == "GET /api/v4/projects/g%2Fp/merge_requests/48" {
              probes++
          }
      }
      if probes != 1 {
          t.Fatalf("want exactly 1 MR probe (IsPROpen), got %d: %v", probes, *calls)
      }
  }

  func TestCommentFallsBackToIssueNamespace(t *testing.T) {
      // An unknown iid probes the MR endpoint; a 404 there means "issue" —
      // the comment lands on /issues/39/notes.
      srv, calls := fakeProject(t)
      defer srv.Close()
      c := New(srv.URL, "g/p", "main", staticToken())
      if err := c.Comment(context.Background(), 39, "reinvestigated"); err != nil {
          t.Fatalf("Comment(39): %v", err)
      }
      want := "POST /api/v4/projects/g%2Fp/issues/39/notes"
      found := false
      for _, cl := range *calls {
          if cl == want {
              found = true
          }
      }
      if !found {
          t.Fatalf("comment did not reach the issue namespace: %v", *calls)
      }
  }

  func TestListIssueCommentBodiesSkipsSystemNotes(t *testing.T) {
      srv, _ := fakeProject(t)
      defer srv.Close()
      c := New(srv.URL, "g/p", "main", staticToken())
      bodies, err := c.ListIssueCommentBodies(context.Background(), 48)
      if err != nil {
          t.Fatalf("ListIssueCommentBodies: %v", err)
      }
      // System notes (label/state churn) are noise for marker scans; the hidden
      // idempotency marker in note 303 must survive.
      if len(bodies) != 2 || !strings.Contains(bodies[1], "runlore-contested: k1") {
          t.Fatalf("bodies=%q", bodies)
      }
  }

  func TestReplaceLabelAndClose(t *testing.T) {
      var gotMRUpdate map[string]any
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          p := r.URL.EscapedPath()
          switch {
          case p == "/api/v4/projects/g%2Fp/merge_requests/48" && r.Method == http.MethodGet:
              _, _ = w.Write([]byte(`{"iid":48,"state":"opened"}`))
          case p == "/api/v4/projects/g%2Fp/merge_requests/48" && r.Method == http.MethodPut:
              _ = json.NewDecoder(r.Body).Decode(&gotMRUpdate)
              _, _ = w.Write([]byte(`{"iid":48,"state":"opened"}`))
          default:
              t.Fatalf("unexpected request: %s %s", r.Method, p)
          }
      }))
      defer srv.Close()
      c := New(srv.URL, "g/p", "main", staticToken())

      if err := c.ReplaceLabel(context.Background(), 48, "solved", "ready-to-merge"); err != nil {
          t.Fatalf("ReplaceLabel: %v", err)
      }
      if gotMRUpdate["remove_labels"] != "solved" || gotMRUpdate["add_labels"] != "ready-to-merge" {
          t.Fatalf("label update body=%v", gotMRUpdate)
      }
      if err := c.Close(context.Background(), 48); err != nil {
          t.Fatalf("Close: %v", err)
      }
      if gotMRUpdate["state_event"] != "close" {
          t.Fatalf("close body=%v", gotMRUpdate)
      }
  }
  ```

- [ ] **Step 2: Run — expect FAIL** (`undefined: (*Client).IsPROpen` etc.)

  ```bash
  go test ./internal/forge/gitlab/
  ```

- [ ] **Step 3: Implement** — append to `gitlab.go`:

  ```go
  // mrState fetches an MR's state ("opened"/"closed"/"merged"/"locked").
  func (c *Client) mrState(ctx context.Context, iid int) (string, error) {
      var out struct {
          State string `json:"state"`
      }
      if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%s/merge_requests/%d", c.proj(), iid), nil, &out); err != nil {
          return "", err
      }
      return out.State, nil
  }

  // IsPROpen reports whether iid is an OPEN merge request. It hits the MR
  // endpoint only, so an issue iid 404s instead of passing — the same
  // cannot-be-fooled contract as the GitHub client (github.go:335-347).
  func (c *Client) IsPROpen(ctx context.Context, number int) (bool, error) {
      st, err := c.mrState(ctx, number)
      if err != nil {
          return false, err
      }
      c.remember(number, kindMR)
      return st == "opened", nil
  }

  // kindOf resolves which iid namespace a number belongs to. Every consumer flow
  // learns the number from a listing or IsPROpen on this same client before
  // mutating it (dedup/lifecycle/queue list MRs first; reinvestigate lists
  // issues first; contested calls IsPROpen first), so this is a memo read in
  // practice. An unknown iid is resolved by probing the MR endpoint — 404 there
  // means "not an MR", i.e. issue — and memoized.
  func (c *Client) kindOf(ctx context.Context, iid int) (artifactKind, error) {
      c.mu.Lock()
      k := c.kinds[iid]
      c.mu.Unlock()
      if k != kindUnknown {
          return k, nil
      }
      if _, err := c.mrState(ctx, iid); err != nil {
          if !isNotFound(err) {
              return kindUnknown, err
          }
          c.remember(iid, kindIssue)
          return kindIssue, nil
      }
      c.remember(iid, kindMR)
      return kindMR, nil
  }

  // nsPath is the REST collection for the iid's namespace.
  func (c *Client) nsPath(ctx context.Context, iid int) (string, error) {
      k, err := c.kindOf(ctx, iid)
      if err != nil {
          return "", err
      }
      if k == kindIssue {
          return "issues", nil
      }
      return "merge_requests", nil
  }

  // Comment posts a note on an issue or MR.
  func (c *Client) Comment(ctx context.Context, number int, body string) error {
      ns, err := c.nsPath(ctx, number)
      if err != nil {
          return err
      }
      return c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/%s/%d/notes", c.proj(), ns, number),
          map[string]any{"body": body}, nil)
  }

  // ListIssueCommentBodies fetches ALL pages of an issue/MR's note bodies.
  // System notes (label churn, state events — GitLab auto-notes) are skipped:
  // callers scan bodies for hidden idempotency markers, and only human/bot
  // comments carry them; pagination matters for the same
  // marker-past-page-1-would-repost-forever reason as GitHub (github.go:315-333).
  func (c *Client) ListIssueCommentBodies(ctx context.Context, number int) ([]string, error) {
      ns, err := c.nsPath(ctx, number)
      if err != nil {
          return nil, err
      }
      var out []string
      for page := 1; ; page++ {
          var raw []struct {
              Body   string `json:"body"`
              System bool   `json:"system"`
          }
          path := fmt.Sprintf("/projects/%s/%s/%d/notes?per_page=100&page=%d", c.proj(), ns, number, page)
          if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
              return nil, err
          }
          for _, r := range raw {
              if r.System {
                  continue
              }
              out = append(out, r.Body)
          }
          if len(raw) < 100 { // last page (a full page is exactly 100)
              break
          }
      }
      return out, nil
  }

  // ReplaceLabel removes one label and adds another in a single update call
  // (GitLab's add_labels/remove_labels params — no separate label endpoints).
  // Either side may be empty; removing an absent label is a server-side no-op,
  // matching the GitHub client's best-effort removal.
  func (c *Client) ReplaceLabel(ctx context.Context, number int, remove, add string) error {
      if remove == "" && add == "" {
          return nil
      }
      ns, err := c.nsPath(ctx, number)
      if err != nil {
          return err
      }
      body := map[string]any{}
      if remove != "" {
          body["remove_labels"] = remove
      }
      if add != "" {
          body["add_labels"] = add
      }
      return c.do(ctx, http.MethodPut, fmt.Sprintf("/projects/%s/%s/%d", c.proj(), ns, number), body, nil)
  }

  // Close closes an issue or MR via its namespace's state_event.
  func (c *Client) Close(ctx context.Context, number int) error {
      ns, err := c.nsPath(ctx, number)
      if err != nil {
          return err
      }
      return c.do(ctx, http.MethodPut, fmt.Sprintf("/projects/%s/%s/%d", c.proj(), ns, number),
          map[string]any{"state_event": "close"}, nil)
  }
  ```

  Re-enable the `var _ providers.ReinvestForge = (*Client)(nil)` pin from Task 3.

- [ ] **Step 4: Run — expect PASS**

  ```bash
  go test ./internal/forge/gitlab/
  ```

- [ ] **Step 5: Commit**

  ```bash
  git add internal/forge/gitlab
  git commit -m "feat(forge): GitLab comment/label/close across the split iid namespaces"
  ```

### Task 6: `OpenPR` — branch, OKF entry file, bundle maintenance, MR

**Files:**
- Create: `internal/forge/gitlab/files.go` (files API + bundle)
- Modify: `internal/forge/gitlab/gitlab.go` (OpenPR, blobURL, interface pins)
- Test: `internal/forge/gitlab/openpr_test.go`

- [ ] **Step 1: Write the failing test** — create `internal/forge/gitlab/openpr_test.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package gitlab

  import (
      "context"
      "encoding/base64"
      "encoding/json"
      "net/http"
      "net/http/httptest"
      "strings"
      "testing"

      "github.com/Smana/runlore/internal/providers"
  )

  func TestOpenPR(t *testing.T) {
      var branchQuery string
      var fileBody, mrBody map[string]any
      var filePath string
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          p := r.URL.EscapedPath()
          switch {
          case p == "/api/v4/projects/g%2Fp/repository/branches" && r.Method == http.MethodPost:
              branchQuery = r.URL.RawQuery
              w.WriteHeader(http.StatusCreated)
              _, _ = w.Write([]byte(`{"name":"runlore/kb-harbor-registry-down-1753272000","commit":{"id":"deadbeef"}}`))
          case strings.HasPrefix(p, "/api/v4/projects/g%2Fp/repository/files/") && r.Method == http.MethodGet:
              // bundle probe: neither index.md nor log.md exists in this repo
              w.WriteHeader(http.StatusNotFound)
              _, _ = w.Write([]byte(`{"message":"404 File Not Found"}`))
          case strings.HasPrefix(p, "/api/v4/projects/g%2Fp/repository/files/incidents%2F") && r.Method == http.MethodPost:
              filePath = p
              _ = json.NewDecoder(r.Body).Decode(&fileBody)
              w.WriteHeader(http.StatusCreated)
              _, _ = w.Write([]byte(`{"file_path":"incidents/harbor-registry-down-abc12345.md","branch":"runlore/kb-harbor-registry-down-1753272000"}`))
          case strings.HasPrefix(p, "/api/v4/projects/g%2Fp/repository/files/log.md") && r.Method == http.MethodPost:
              w.WriteHeader(http.StatusCreated)
              _, _ = w.Write([]byte(`{"file_path":"log.md"}`))
          case p == "/api/v4/projects/g%2Fp/merge_requests" && r.Method == http.MethodPost:
              _ = json.NewDecoder(r.Body).Decode(&mrBody)
              w.WriteHeader(http.StatusCreated)
              _, _ = w.Write([]byte(`{"id":900,"iid":5,"state":"opened",
                "title":"KB: Harbor registry down",
                "web_url":"https://gitlab.example.com/g/p/-/merge_requests/5"}`))
          default:
              t.Fatalf("unexpected request: %s %s", r.Method, p)
          }
      }))
      defer srv.Close()

      c := New(srv.URL, "g/p", "main", staticToken())
      ref, err := c.OpenPR(context.Background(), providers.KBEntry{
          Type: "Incident", Title: "Harbor registry down",
          Description: "registry pods crashloop on bad secret",
          Resource:    "tooling/harbor-registry",
          Body:        "## Cause\nbad secret ![beacon](https://evil/x)\n",
          Fingerprint: "abc12345ffff",
          Related: []providers.RelatedEntry{{Path: "incidents/old.md", Title: "Old harbor incident", Score: 6.2}},
      })
      if err != nil {
          t.Fatalf("OpenPR: %v", err)
      }
      if ref.URL != "https://gitlab.example.com/g/p/-/merge_requests/5" {
          t.Fatalf("ref=%s", ref.URL)
      }
      // Branch off main, under runlore/.
      if !strings.Contains(branchQuery, "ref=main") || !strings.Contains(branchQuery, "branch=runlore%2Fkb-harbor-registry-down-") {
          t.Fatalf("branch query=%q", branchQuery)
      }
      // Entry path: type dir + slug + 8-char fingerprint, URL-encoded in the route.
      if !strings.HasSuffix(filePath, "incidents%2Fharbor-registry-down-abc12345.md") {
          t.Fatalf("file path=%q", filePath)
      }
      // File content is base64 OKF markdown with the image neutralized.
      raw, _ := base64.StdEncoding.DecodeString(fileBody["content"].(string))
      if !strings.HasPrefix(string(raw), "---\n") || strings.Contains(string(raw), "](https://evil/x)") {
          t.Fatalf("entry content unsafe or not OKF: %q", raw)
      }
      if fileBody["encoding"] != "base64" {
          t.Fatalf("encoding=%v", fileBody["encoding"])
      }
      // MR: labels at creation, description carries the related-knowledge blob link.
      if mrBody["labels"] != "runlore,triggered" || mrBody["target_branch"] != "main" {
          t.Fatalf("mr body=%v", mrBody)
      }
      if !strings.Contains(mrBody["description"].(string), "/g/p/-/blob/main/incidents/old.md") {
          t.Fatalf("related blob URL missing: %v", mrBody["description"])
      }
  }
  ```

- [ ] **Step 2: Run — expect FAIL** (`undefined: (*Client).OpenPR`)

  ```bash
  go test ./internal/forge/gitlab/ -run TestOpenPR
  ```

- [ ] **Step 3: Implement the files API** — create `internal/forge/gitlab/files.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package gitlab

  // Repository-files API + OKF bundle maintenance. GitLab needs no blob SHA for
  // updates (the branch head is the implicit precondition), so this is simpler
  // than the GitHub contents API — but the best-effort contract is identical:
  // a bundle-maintenance failure must never lose the entry MR.

  import (
      "context"
      "encoding/base64"
      "fmt"
      "net/http"
      "net/url"
      "strings"
      "time"

      forge "github.com/Smana/runlore/internal/forge"
      "github.com/Smana/runlore/internal/providers"
  )

  func (c *Client) filePath(path string) string {
      return fmt.Sprintf("/projects/%s/repository/files/%s", c.proj(), url.PathEscape(path))
  }

  // getFile reads a file's content at ref. A 404 is not an error: found=false
  // says "the bundle doesn't have this file" (same contract as the GitHub
  // client's getFile, github/bundle.go:115-153).
  func (c *Client) getFile(ctx context.Context, path, ref string) (data []byte, found bool, err error) {
      var out struct {
          Content string `json:"content"` // base64
      }
      if err := c.do(ctx, http.MethodGet, c.filePath(path)+"?ref="+url.QueryEscape(ref), nil, &out); err != nil {
          if isNotFound(err) {
              return nil, false, nil
          }
          return nil, false, err
      }
      raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
      if err != nil {
          return nil, false, err
      }
      return raw, true, nil
  }

  // createFile writes a NEW file on branch (POST); updateFile overwrites an
  // existing one (PUT). GitLab 400s a POST for an existing path and a PUT for a
  // missing one, so the caller must know which it holds (getFile tells it).
  func (c *Client) createFile(ctx context.Context, path, branch, message string, content []byte) error {
      return c.writeFile(ctx, http.MethodPost, path, branch, message, content)
  }

  func (c *Client) updateFile(ctx context.Context, path, branch, message string, content []byte) error {
      return c.writeFile(ctx, http.MethodPut, path, branch, message, content)
  }

  func (c *Client) writeFile(ctx context.Context, method, path, branch, message string, content []byte) error {
      return c.do(ctx, method, c.filePath(path), map[string]any{
          "branch":         branch,
          "encoding":       "base64",
          "content":        base64.StdEncoding.EncodeToString(content),
          "commit_message": message,
      }, nil)
  }

  // maintainBundle updates index.md (only when the bundle already has one — its
  // structure is the owner's choice) and creates/appends log.md on the MR
  // branch. Best-effort by contract: the caller ignores the returned error
  // beyond logging (same as the GitHub client, github/bundle.go:26-46).
  func (c *Client) maintainBundle(ctx context.Context, e providers.KBEntry, entryPath, branch string) error {
      date := time.Now().UTC().Format("2006-01-02")

      idx, found, err := c.getFile(ctx, "index.md", branch)
      if err != nil {
          return fmt.Errorf("read index.md: %w", err)
      }
      if found {
          if err := c.updateFile(ctx, "index.md", branch, "runlore: index "+e.Title,
              forge.UpdateIndex(idx, e, entryPath)); err != nil {
              return err
          }
      }

      logMD, logFound, err := c.getFile(ctx, "log.md", branch)
      if err != nil {
          return fmt.Errorf("read log.md: %w", err)
      }
      put := c.createFile
      if logFound {
          put = c.updateFile
      }
      return put(ctx, "log.md", branch, "runlore: log "+e.Title, forge.UpdateLog(logMD, e, entryPath, date))
  }
  ```

- [ ] **Step 4: Implement `OpenPR` + `blobURL`** — append to `gitlab.go`:

  ```go
  // createBranch creates branch off ref (GitLab takes both as query params).
  func (c *Client) createBranch(ctx context.Context, branch, ref string) error {
      return c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/repository/branches?branch=%s&ref=%s",
          c.proj(), url.QueryEscape(branch), url.QueryEscape(ref)), nil, nil)
  }

  // openMR opens a merge request with labels set at creation.
  func (c *Client) openMR(ctx context.Context, branch, title, description string, labels []string) (providers.Ref, error) {
      var out struct {
          IID    int    `json:"iid"`
          WebURL string `json:"web_url"`
      }
      if err := c.do(ctx, http.MethodPost, "/projects/"+c.proj()+"/merge_requests", map[string]any{
          "source_branch": branch,
          "target_branch": c.base(),
          "title":         title,
          "description":   description,
          "labels":        strings.Join(labels, ","),
      }, &out); err != nil {
          return providers.Ref{}, err
      }
      c.remember(out.IID, kindMR)
      return providers.Ref{URL: out.WebURL}, nil
  }

  // OpenPR drafts the KB entry on a new branch and opens a merge request —
  // step-for-step the GitHub OpenPR flow (github.go:113-159), minus the
  // separate labelling call (labels ride the MR-create body).
  func (c *Client) OpenPR(ctx context.Context, e providers.KBEntry) (providers.Ref, error) {
      slug := forge.Slugify(e.Title)
      now := time.Now().Unix()
      branch := fmt.Sprintf("runlore/kb-%s-%d", slug, now)
      path := forge.EntryPath(e, slug, now)

      // 1. create the branch off the base.
      if err := c.createBranch(ctx, branch, c.base()); err != nil {
          return providers.Ref{}, err
      }
      // 2. write the OKF file on the branch.
      if err := c.createFile(ctx, path, branch, "runlore: draft KB entry "+e.Title,
          []byte(forge.RenderEntry(e))); err != nil {
          return providers.Ref{}, err
      }
      // 3. bundle maintenance — best-effort, must not lose the entry MR.
      _ = c.maintainBundle(ctx, e, path, branch)
      // 4. open the MR (labels at creation).
      return c.openMR(ctx, branch, "KB: "+e.Title, forge.PRBody(e, c.blobURL), forge.LifecycleLabels)
  }

  // blobURL is the web URL of a catalog file on the base branch:
  // https://<host>/<project>/-/blob/<branch>/<path>. GitLab web routes use the
  // literal (un-encoded) project path with the /-/ separator.
  func (c *Client) blobURL(path string) string {
      return fmt.Sprintf("%s/%s/-/blob/%s/%s", c.baseURL, c.project, c.base(), path)
  }
  ```

  Re-enable the `var _ providers.CurationForge = (*Client)(nil)` pin from Task 3.

- [ ] **Step 5: Run — expect PASS**

  ```bash
  go test ./internal/forge/gitlab/
  ```

- [ ] **Step 6: Commit**

  ```bash
  git add internal/forge/gitlab
  git commit -m "feat(forge): GitLab OpenPR — branch, OKF entry file, bundle maintenance, MR"
  ```

### Task 7: `OpenRetirePR`

**Files:**
- Create: `internal/forge/gitlab/retire.go`
- Test: `internal/forge/gitlab/retire_test.go`

- [ ] **Step 1: Write the failing test** — create `internal/forge/gitlab/retire_test.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package gitlab

  import (
      "context"
      "encoding/base64"
      "encoding/json"
      "errors"
      "net/http"
      "net/http/httptest"
      "strings"
      "testing"

      forge "github.com/Smana/runlore/internal/forge"
      "github.com/Smana/runlore/internal/curate"
  )

  // Compile-time: the GitLab client serves every curate pass the GitHub client does.
  var (
      _ curate.Forge          = (*Client)(nil)
      _ curate.ContestedForge = (*Client)(nil)
      _ curate.RetireForge    = (*Client)(nil)
  )

  func retireServer(t *testing.T, entryB64 string) (*httptest.Server, *map[string]any) {
      t.Helper()
      var putBody map[string]any
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          p := r.URL.EscapedPath()
          switch {
          case p == "/api/v4/projects/g%2Fp/repository/files/incidents%2Fa.md" && r.Method == http.MethodGet:
              _, _ = w.Write([]byte(`{"file_name":"a.md","content":"` + entryB64 + `","encoding":"base64","ref":"main"}`))
          case p == "/api/v4/projects/g%2Fp/repository/branches" && r.Method == http.MethodPost:
              w.WriteHeader(http.StatusCreated)
              _, _ = w.Write([]byte(`{"name":"runlore/retire-incidents-a-md-1753272000"}`))
          case p == "/api/v4/projects/g%2Fp/repository/files/incidents%2Fa.md" && r.Method == http.MethodPut:
              _ = json.NewDecoder(r.Body).Decode(&putBody)
              _, _ = w.Write([]byte(`{"file_path":"incidents/a.md"}`))
          case p == "/api/v4/projects/g%2Fp/merge_requests" && r.Method == http.MethodPost:
              w.WriteHeader(http.StatusCreated)
              _, _ = w.Write([]byte(`{"iid":9,"web_url":"https://gitlab.example.com/g/p/-/merge_requests/9"}`))
          default:
              t.Fatalf("unexpected request: %s %s", r.Method, p)
          }
      }))
      return srv, &putBody
  }

  func TestOpenRetirePR(t *testing.T) {
      entry := "---\ntype: Incident\ntitle: A\n---\n\nbody\n"
      srv, putBody := retireServer(t, base64.StdEncoding.EncodeToString([]byte(entry)))
      defer srv.Close()

      c := New(srv.URL, "g/p", "main", staticToken())
      ref, err := c.OpenRetirePR(context.Background(), "incidents/a.md", "track record <!-- runlore-retire: incidents/a.md -->")
      if err != nil {
          t.Fatalf("OpenRetirePR: %v", err)
      }
      if ref.URL != "https://gitlab.example.com/g/p/-/merge_requests/9" {
          t.Fatalf("ref=%s", ref.URL)
      }
      raw, _ := base64.StdEncoding.DecodeString((*putBody)["content"].(string))
      if !strings.Contains(string(raw), "status: retired") {
          t.Fatalf("frontmatter not stamped: %q", raw)
      }
  }

  func TestOpenRetirePRAlreadyRetired(t *testing.T) {
      entry := "---\nstatus: retired\ntype: Incident\ntitle: A\n---\n\nbody\n"
      srv, _ := retireServer(t, base64.StdEncoding.EncodeToString([]byte(entry)))
      defer srv.Close()

      c := New(srv.URL, "g/p", "main", staticToken())
      _, err := c.OpenRetirePR(context.Background(), "incidents/a.md", "b")
      if !errors.Is(err, forge.ErrAlreadyRetired) {
          t.Fatalf("want ErrAlreadyRetired (curate treats it as done-skip), got %v", err)
      }
  }
  ```

- [ ] **Step 2: Run — expect FAIL** (`undefined: (*Client).OpenRetirePR`)

  ```bash
  go test ./internal/forge/gitlab/ -run TestOpenRetirePR
  ```

- [ ] **Step 3: Implement** — create `internal/forge/gitlab/retire.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package gitlab

  import (
      "context"
      "fmt"
      "time"

      forge "github.com/Smana/runlore/internal/forge"
      "github.com/Smana/runlore/internal/providers"
  )

  // OpenRetirePR opens a human-reviewed MR that stamps status:retired into an
  // existing catalog entry's frontmatter. It never merges and never deletes — a
  // human is the load-bearing gate. Returns forge.ErrAlreadyRetired when the
  // entry is already retired on the base branch (the curate pass treats it as a
  // done-skip); a missing entry file surfaces as an error (entry deleted → the
  // pass logs and skips it). Mirrors github/retire.go:64-126.
  func (c *Client) OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error) {
      raw, found, err := c.getFile(ctx, entryPath, c.base())
      if err != nil {
          return providers.Ref{}, err
      }
      if !found {
          return providers.Ref{}, fmt.Errorf("gitlab: entry %s not found on %s", entryPath, c.base())
      }
      stamped, already, err := forge.SetStatusRetired(raw)
      if err != nil {
          return providers.Ref{}, fmt.Errorf("%s: %w", entryPath, err)
      }
      if already {
          return providers.Ref{}, forge.ErrAlreadyRetired
      }
      branch := fmt.Sprintf("runlore/retire-%s-%d", forge.Slugify(entryPath), time.Now().Unix())
      if err := c.createBranch(ctx, branch, c.base()); err != nil {
          return providers.Ref{}, err
      }
      if err := c.updateFile(ctx, entryPath, branch, "runlore: retire "+entryPath, stamped); err != nil {
          return providers.Ref{}, err
      }
      return c.openMR(ctx, branch, "KB retire: "+entryPath, body, forge.RetireLabels)
  }
  ```

- [ ] **Step 4: Run the whole package — expect PASS (interface pins prove full parity)**

  ```bash
  go test ./internal/forge/gitlab/
  ```

- [ ] **Step 5: Commit**

  ```bash
  git add internal/forge/gitlab
  git commit -m "feat(forge): GitLab OpenRetirePR — status:retired stamp behind a human-gated MR"
  ```

### Task 8: App wiring — provider-selected forge client + token source

**Files:**
- Modify: `internal/app/forge.go`, `internal/app/curator.go`, `internal/app/curate.go`, `internal/app/investigate.go` (line 403)
- Test: `internal/app/forge_test.go` (create)

- [ ] **Step 1: Write the failing test** — create `internal/app/forge_test.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package app

  import (
      "bytes"
      "context"
      "log/slog"
      "testing"

      "github.com/Smana/runlore/internal/config"
      gitlab "github.com/Smana/runlore/internal/forge/gitlab"
      github "github.com/Smana/runlore/internal/forge/github"
  )

  func testLogger() *slog.Logger {
      return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
  }

  func TestBuildForgeClientGitLab(t *testing.T) {
      t.Setenv("GITLAB_TOKEN_TEST", "glpat-x")
      cfg := &config.Config{}
      cfg.Forge.Provider = "gitlab"
      cfg.Forge.KBRepo = "group/sub/runlore-kb" // nested path: must NOT be owner/name-split
      cfg.Forge.GitLab.TokenEnv = "GITLAB_TOKEN_TEST"

      fc := BuildForgeClient(cfg, testLogger())
      if fc == nil {
          t.Fatal("gitlab forge not built")
      }
      if _, ok := fc.(*gitlab.Client); !ok {
          t.Fatalf("want *gitlab.Client, got %T", fc)
      }
      tok := BuildForgeTokenSource(cfg, testLogger())
      if tok == nil {
          t.Fatal("gitlab token source not built (catalog git-sync + source-diff need it)")
      }
      if got, _ := tok(context.Background()); got != "glpat-x" {
          t.Fatalf("token=%q", got)
      }
  }

  func TestBuildForgeClientGitHubDefaultUnchanged(t *testing.T) {
      // Empty provider = github; without App credentials the forge stays off —
      // byte-identical behavior to today for every existing config.
      cfg := &config.Config{}
      cfg.Forge.KBRepo = "owner/name"
      if fc := BuildForgeClient(cfg, testLogger()); fc != nil {
          t.Fatalf("forge must stay disabled without github_app, got %T", fc)
      }
      t.Setenv("GH_APP_KEY_TEST", testRSAPEM(t))
      cfg.Forge.GitHubApp = config.GitHubApp{AppID: 1, InstallationID: 2, PrivateKeyEnv: "GH_APP_KEY_TEST"}
      fc := BuildForgeClient(cfg, testLogger())
      if _, ok := fc.(*github.Client); !ok {
          t.Fatalf("want *github.Client, got %T", fc)
      }
  }

  // testRSAPEM generates a throwaway PKCS#1 RSA key PEM for the GitHub arm.
  func testRSAPEM(t *testing.T) string {
      t.Helper()
      key, err := rsa.GenerateKey(rand.Reader, 2048)
      if err != nil {
          t.Fatal(err)
      }
      return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
  }
  ```

  (Add `"crypto/rand"`, `"crypto/rsa"`, `"crypto/x509"`, `"encoding/pem"` to imports.)

- [ ] **Step 2: Run — expect FAIL** (`undefined: BuildForgeClient`)

  ```bash
  go test ./internal/app/ -run TestBuildForgeClient
  ```

- [ ] **Step 3: Implement** — rewrite `internal/app/forge.go`:

  ```go
  // SPDX-License-Identifier: Apache-2.0

  package app

  import (
      "context"
      "log/slog"
      "os"
      "strings"

      "github.com/Smana/runlore/internal/config"
      "github.com/Smana/runlore/internal/providers"

      github "github.com/Smana/runlore/internal/forge/github"
      gitlab "github.com/Smana/runlore/internal/forge/gitlab"
  )

  // ForgeToken yields the forge git/API credential: a minted GitHub App
  // installation token, or a static GitLab project access token.
  type ForgeToken func(context.Context) (string, error)

  // BuildForgeTokenSource builds the credential shared by the curator (issues/
  // PRs/MRs), catalog git-sync (clone auth), and source-diff clones — one
  // identity for both forge writes and git reads. Returns nil when no forge
  // auth is configured.
  //
  // The GitLab token needs no minting/refresh: it is a static project access
  // token read once from the env. It works as the git HTTP password with ANY
  // non-blank username, so catalog.Syncer.auth (catalog/sync.go:62) and
  // whatchanged.Differ.auth (whatchanged/differ.go:143) — both of which send
  // username "x-access-token" — need no change.
  func BuildForgeTokenSource(cfg *config.Config, log *slog.Logger) ForgeToken {
      if cfg.Forge.Provider == "gitlab" {
          env := cfg.Forge.GitLab.TokenEnv
          if env == "" {
              return nil // unreachable after config.Validate, kept for direct callers
          }
          tok := os.Getenv(env)
          if tok == "" {
              log.Warn("forge auth disabled: empty gitlab token env", "env", env)
              return nil
          }
          return func(context.Context) (string, error) { return tok, nil }
      }
      ga := cfg.Forge.GitHubApp
      if ga.AppID == 0 || ga.InstallationID == 0 || ga.PrivateKeyEnv == "" {
          return nil
      }
      pemData := os.Getenv(ga.PrivateKeyEnv)
      if pemData == "" {
          log.Warn("forge auth disabled: empty private key env", "env", ga.PrivateKeyEnv)
          return nil
      }
      key, err := github.ParsePrivateKey(pemData)
      if err != nil {
          log.Warn("forge auth disabled: bad private key", "err", err)
          return nil
      }
      return github.NewAppTokenSource(cfg.Forge.GitHubAPIURL, ga.AppID, ga.InstallationID, key).Token
  }

  // ForgeClient is the full forge surface the app wires — the union of the
  // consumer interfaces (providers.CurationForge, providers.ReinvestForge,
  // curate.Forge, curate.ContestedForge, curate.RetireForge). Implemented by
  // *github.Client and *gitlab.Client.
  type ForgeClient interface {
      OpenIssue(ctx context.Context, inv providers.Investigation) (providers.Ref, error)
      OpenPR(ctx context.Context, e providers.KBEntry) (providers.Ref, error)
      OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error)
      ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
      ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
      ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
      ListIssueCommentBodies(ctx context.Context, number int) ([]string, error)
      IsPROpen(ctx context.Context, number int) (bool, error)
      Comment(ctx context.Context, number int, body string) error
      ReplaceLabel(ctx context.Context, number int, remove, add string) error
      Close(ctx context.Context, number int) error
  }

  var (
      _ ForgeClient = (*github.Client)(nil)
      _ ForgeClient = (*gitlab.Client)(nil)
  )

  // BuildForgeClient returns the provider-selected forge client, or nil when the
  // forge is not configured. forge.provider empty/"github" keeps the exact
  // pre-M2 GitHub behavior (owner/name split, App token); "gitlab" takes
  // kb_repo as the FULL project path (nested groups allowed).
  func BuildForgeClient(cfg *config.Config, log *slog.Logger) ForgeClient {
      if cfg.Forge.KBRepo == "" {
          return nil
      }
      tok := BuildForgeTokenSource(cfg, log)
      if tok == nil {
          return nil
      }
      base := cfg.Forge.BaseBranch
      if base == "" {
          base = "main"
      }
      if cfg.Forge.Provider == "gitlab" {
          return gitlab.New(cfg.Forge.GitLab.BaseURL, cfg.Forge.KBRepo, base, gitlab.TokenFunc(tok))
      }
      owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
      if !ok {
          log.Warn("forge disabled: kb_repo must be owner/name", "kb_repo", cfg.Forge.KBRepo)
          return nil
      }
      return github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(tok))
  }
  ```

- [ ] **Step 4: Rewire the three construction sites** (each currently builds `github.New` directly):

  1. `internal/app/curator.go` — `BuildCurator` (lines 21-61): change the signature from `(cfg, token ForgeToken, cat, metrics, log)` to `(cfg *config.Config, client ForgeClient, cat *catalog.Catalog, metrics *telemetry.Metrics, log *slog.Logger)`; delete the `strings.Cut`/`base`/`github.New` block (lines 24-35, 54) and gate on `client == nil || cfg.Forge.KBRepo == ""`; assign `cur.Forge = client`. Update the caller at `internal/app/investigate.go:403` to `BuildCurator(cfg, BuildForgeClient(cfg, log), cat, metrics, log)`.
  2. `internal/app/curator.go` — `BuildReinvestigator` (lines 66-104): replace lines 67-75 with `client := BuildForgeClient(cfg, log); if client == nil { return nil }` and pass `client` as `Forge` (it satisfies `providers.ReinvestForge`).
  3. `internal/app/curate.go` — `RunCurate` (lines 43-55): replace the token/Cut/`github.New` block with:

     ```go
     forge := BuildForgeClient(cfg, log)
     if forge == nil {
         return fmt.Errorf("curate requires a configured forge (forge.github_app, or forge.provider: gitlab with forge.gitlab.token_env)")
     }
     ```

     and drop the now-unused `strings` + `github` imports.

- [ ] **Step 5: Full suite — expect PASS**

  ```bash
  go build ./... && go test ./internal/app/ ./internal/curate/ ./internal/curator/ ./internal/investigate/ ./internal/config/ ./internal/forge/...
  ```

- [ ] **Step 6: Commit**

  ```bash
  git add internal/app
  git commit -m "feat(app): wire the GitLab forge — provider-selected client and token source"
  ```

### Task 9: Chart values + docs (getting-started GitLab section, configuration.md, design.md)

**Files:**
- Modify: `deploy/helm/runlore/values.yaml` (lines 264-267), `docs/getting-started.md` (lines 39-40, after line 164, lines 173-180, lines 337-346), `docs/configuration.md` (§ `forge`, line 283), `docs/design.md` (lines 127, 333, 523)

- [ ] **Step 1: values.yaml — document the GitLab alternative.** Replace the current forge comment block (values.yaml:264-267):

  ```yaml
  # forge:
  #   kb_repo: owner/name
  #   base_branch: main
  #   github_app: { app_id: 0, installation_id: 0, private_key_env: GITHUB_APP_PRIVATE_KEY }
  ```

  with:

  ```yaml
  # forge:
  #   kb_repo: owner/name
  #   base_branch: main
  #   github_app: { app_id: 0, installation_id: 0, private_key_env: GITHUB_APP_PRIVATE_KEY }
  #   # GitLab instead of GitHub (gitlab.com or self-hosted). Auth is a project
  #   # access token referenced from the Secret by env name — same pattern as the
  #   # GitHub App key. kb_repo becomes the FULL project path (nested groups OK).
  #   # provider: gitlab
  #   # kb_repo: group/subgroup/runlore-kb
  #   # gitlab: { base_url: https://gitlab.example.com, token_env: GITLAB_TOKEN }
  ```

  Verify the chart still lints: `helm lint deploy/helm/runlore` → `1 chart(s) linted, 0 chart(s) failed` (the `config` block is schema-free — values.schema.json marks it `additionalProperties: true`).

- [ ] **Step 2: getting-started.md — add Step 2b.** Insert the following section between the end of Step 2 (after line 164, before the `---` at line 166):

  ````markdown

  ## Step 2b — GitLab instead of GitHub (optional)

  RunLore's forge is pluggable: if your knowledge base lives on **GitLab** — gitlab.com or a
  **self-hosted GitLab instance** (the common choice for sovereignty-conscious teams) — the curator
  opens **merge requests** instead of pull requests, and everything else in the Learn loop works the
  same: lifecycle labels, duplicate-incident comments on open MRs, stale/duplicate MR closes, and
  catalog git-sync over the same credential.

  Auth is a **project access token**: per-project, role-scoped, and expiring — the
  self-hosted-friendly counterpart of the GitHub App (no instance-admin or OAuth-application setup).

  ### Create the token

  1. In the KB project: **Settings → Access tokens → Add new token.**
  2. Role: **Developer** (push non-protected branches, open/close MRs and issues, comment).
  3. Scopes: **`api`** (REST writes: branches, entry files, MRs, issues, notes) and
     **`read_repository`** (catalog git-sync clone).
  4. Set an expiry you can operate with (GitLab warns project maintainers before it lapses) and note
     the token value — you only see it once.

  > RunLore pushes branches under `runlore/*`. If your project protects wildcard branches, allow
  > Developers to push `runlore/*` (Settings → Repository → Protected branches), or raise the token
  > role to Maintainer.

  ### Configure

  Add the token to the credentials Secret (step 3), e.g. `--from-literal=GITLAB_TOKEN='glpat-…'`,
  then select the provider in your `values.yaml` `config:` block:

  ```yaml
  config:
    forge:
      provider: gitlab
      kb_repo: platform/runlore-kb            # FULL project path — nested groups work: group/sub/project
      base_branch: main
      gitlab:
        base_url: https://gitlab.example.com  # omit for gitlab.com
        token_env: GITLAB_TOKEN
  ```

  The same token authenticates the **catalog git-sync** clone of a private KB repo — no separate
  `catalog.git.token_env` needed (the same shared-identity behavior as the GitHub App).

  ### Security notes

  - A project access token is **confined to the one KB project** — it cannot reach anything else.
  - It is referenced by env-var name (`token_env`) and lives only in the `Secret` — never in
    `values.yaml`. Rotate it on its expiry cadence.
  - RunLore's writes remain confined to the forge; it has **no cluster-mutating permissions**.
  ````

- [ ] **Step 3: getting-started.md — cross-references.**
  - Line 39 (`- A **GitHub App** for curation — [step 2](...)`): append to the bullet: `On GitLab, a **project access token** replaces the App — [step 2b](#step-2b--gitlab-instead-of-github-optional).`
  - Step 3 Secret example (lines 173-180): add the line `  --from-literal=GITLAB_TOKEN='glpat-...' \` with a trailing comment removed (keep the "Only include the keys you use" note at line 187 doing the explaining).
  - Step 4 values example, forge block (lines 337-346): after the `github_app:` sub-block, add the comment lines:

    ```yaml
      # On GitLab (gitlab.com or self-hosted) use instead — see step 2b:
      # provider: gitlab
      # gitlab: { base_url: https://gitlab.example.com, token_env: GITLAB_TOKEN }
    ```

- [ ] **Step 4: configuration.md — extend the `forge` section (line 283).** After the existing first paragraph (`kb_repo … private_key_env`), add:

  ```markdown
  `provider` selects the forge kind: `github` (default when omitted — existing configs are
  untouched) or `gitlab`. With `gitlab`, `kb_repo` is the **full project path** (nested groups
  allowed, e.g. `group/subgroup/runlore-kb`) and the `gitlab` block applies: `base_url` (default
  `https://gitlab.com`; set your host for self-hosted) and `token_env` (**required** — the env var
  holding a project access token with the `api` + `read_repository` scopes, Developer role). The
  curator then opens **merge requests**; closed-unmerged MRs drive the same rejection-suppression
  set as on GitHub. The token also authenticates catalog git-sync clones (shared identity, like
  the GitHub App).
  ```

- [ ] **Step 5: design.md — retire the "later" markers.**
  - Line 127: `└─► GitHub (now) / GitLab (later)` → `└─► GitHub / GitLab`.
  - Line 333 (providers table): move GitLab from the "later" column to built-in: `| Issue | \`IssueProvider\` | **GitHub** (App auth), **GitLab** (project access token) | — |` (match the table's existing column layout when editing).
  - Line 523-524: replace `Non-GitHub hosts (GitLab, self-hosted) fall back to a scoped access token / deploy key — auth is per-host.` with `GitLab (gitlab.com or self-hosted) is built in via a project access token (\`forge.provider: gitlab\`) — auth stays per-host.`

- [ ] **Step 6: Verify docs + full suite**

  ```bash
  grep -n "provider: gitlab" deploy/helm/runlore/values.yaml docs/getting-started.md docs/configuration.md \
    && helm lint deploy/helm/runlore && go build ./... && go test ./...
  ```

  Expected: hits in all three files, chart lints, suite green.

- [ ] **Step 7: Commit**

  ```bash
  git add deploy/helm/runlore/values.yaml docs/getting-started.md docs/configuration.md docs/design.md
  git commit -m "docs+chart: GitLab forge — project-token setup, provider config, values example"
  ```

---

## Acceptance criteria

- [ ] `internal/forge/gitlab.Client` compiles against **all five** consumer interfaces (compile-time pins in the gitlab tests): `providers.CurationForge`, `providers.ReinvestForge`, `curate.Forge`, `curate.ContestedForge`, `curate.RetireForge` — MR create with the drafted OKF entry file on a `runlore/*` branch, comment on open MRs (duplicate incidents), close MRs/issues, label lifecycle transitions, closed-unmerged listing, knowledge-gap issues, retire MRs.
- [ ] Works against **self-hosted GitLab**: `forge.gitlab.base_url` overrides the host everywhere (API routes and blob links), verified by every test using an `httptest` base URL.
- [ ] Catalog git-sync and source-diff clone the KB/source repos over the GitLab project token with **zero changes** to `internal/catalog/sync.go` and `internal/whatchanged/differ.go` (any-username basic auth), wired through `BuildForgeTokenSource`.
- [ ] **No new required config for GitHub users**: an empty `forge.provider` is byte-identical to today's behavior (`TestBuildForgeClientGitHubDefaultUnchanged`), and `go test ./...` passes with zero edits to existing GitHub forge tests (Task 1 refactor is delegation-only).
- [ ] Config validation fails fast on `forge.provider: gitlab` without `forge.gitlab.token_env`, and on unknown providers; nested project paths are accepted.
- [ ] Error paths are actionable: 401 names `forge.gitlab.token_env`, 404 names `forge.kb_repo`, 409 surfaces GitLab's duplicate-MR message (tested).
- [ ] Shared OKF rendering (image neutralization, frontmatter marshaling, bundle index/log, retire stamp) lives **once** in `internal/forge` and is exercised by both host clients.
- [ ] Docs shipped: getting-started **Step 2b** (token creation, scopes/role, Secret, config), `configuration.md` forge section, chart `values.yaml` example, design.md "later" markers retired.
- [ ] `go build ./... && go test ./... && helm lint deploy/helm/runlore` all green at the final commit.
