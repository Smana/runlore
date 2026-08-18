// SPDX-License-Identifier: Apache-2.0

package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// policyRule is the slice of a Kubernetes RBAC rule this guard checks.
type policyRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

// readResourceSpecRules decodes rbac.resourceSpecRules out of the chart's values.yaml —
// the real parse target Helm itself reads, not a copy of it.
func readResourceSpecRules(t *testing.T) []policyRule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	var v struct {
		RBAC struct {
			ResourceSpecRules []policyRule `yaml:"resourceSpecRules"`
		} `yaml:"rbac"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("values.yaml is not valid YAML: %v", err)
	}
	if len(v.RBAC.ResourceSpecRules) == 0 {
		t.Fatal("rbac.resourceSpecRules is empty: resource_spec is non-functional on a stock " +
			"install — every kind it was built for comes back forbidden")
	}
	return v.RBAC.ResourceSpecRules
}

// TestResourceSpecRBACCoversTheMotivatingKinds: the tool resolves kinds through discovery,
// so it handles anything — but only what RBAC grants. The ClusterRole covered Flux, Argo CD,
// events and pods, which meant every example in the issue this tool closes (a Service
// selector matching nothing, a NetworkPolicy with no egress to kube-dns, an HPA on a metric
// nothing reports, a PVC bound to a vanished storage class, and the VMServiceScrape the
// issue is actually about) answered "forbidden" on a stock install.
//
// That is a security problem before it is a functional one: the obvious operator response
// to a wall of denials is `resources: ["*"]`, which includes secrets.
func TestResourceSpecRBACCoversTheMotivatingKinds(t *testing.T) {
	rules := readResourceSpecRules(t)
	granted := map[string]bool{}
	for _, r := range rules {
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				granted[g+"/"+res] = true
			}
		}
	}
	for _, want := range []string{
		"/services",                                     // a selector that matches no pods
		"networking.k8s.io/networkpolicies",             // no egress rule to kube-dns
		"autoscaling/horizontalpodautoscalers",          // a metric that never reports
		"/persistentvolumeclaims",                       // a storage class that no longer exists
		"operator.victoriametrics.com/vmservicescrapes", // the CR the issue is about
		"apps/deployments",                              // the workload behind most incidents
	} {
		if !granted[want] {
			t.Errorf("resource_spec cannot read %q: it is not in rbac.resourceSpecRules", want)
		}
	}
}

// TestResourceSpecRBACNeverGrantsSecrets is the boundary itself.
//
// The tool refuses the Secret kind before AND after resolution, but that is an
// application-layer policy: RBAC is what actually stops the ServiceAccount's token from
// reading every Secret in the cluster. A wildcard here would quietly undo it, and the
// refusal — which today makes a stock Secret read an existence oracle rather than a dump —
// would be the only thing left standing.
func TestResourceSpecRBACNeverGrantsSecrets(t *testing.T) {
	for _, r := range readResourceSpecRules(t) {
		for _, res := range r.Resources {
			if res == "secrets" || res == "*" {
				t.Errorf("rbac.resourceSpecRules grants %q on apiGroups %v — a wildcard includes "+
					"secrets, and RBAC is the boundary the Secret refusal is NOT", res, r.APIGroups)
			}
		}
		// Read-only, cluster-wide: this list is bound to the ClusterRole, so a write verb
		// here is a cluster-wide write grant. Execution rights are namespace-scoped Roles
		// gated on rbac.allowActions, deliberately and separately.
		for _, v := range r.Verbs {
			if !slices.Contains([]string{"get", "list", "watch"}, v) {
				t.Errorf("rbac.resourceSpecRules grants the write verb %q on %v", v, r.Resources)
			}
		}
	}
}

// TestResourceSpecRBACIsWiredAndWarned: the list is inert unless the template renders it,
// and the wildcard warning is the only thing standing between an operator staring at a
// denial and `resources: ["*"]`. Both are one careless edit from disappearing.
func TestResourceSpecRBACIsWiredAndWarned(t *testing.T) {
	tpl, err := os.ReadFile(filepath.Join(chartDir, "templates", "rbac.yaml"))
	if err != nil {
		t.Fatalf("read rbac.yaml: %v", err)
	}
	if !strings.Contains(string(tpl), ".Values.rbac.resourceSpecRules") {
		t.Error("the ClusterRole does not render rbac.resourceSpecRules: the values key is inert")
	}
	values, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	for _, want := range []string{`resources: ["*"]`, "secrets"} {
		if !strings.Contains(string(values), want) {
			t.Errorf("values.yaml no longer warns about %q next to resourceSpecRules", want)
		}
	}
}

// readStaticClusterRoleRules decodes the rules the ClusterRole template hard-codes —
// the ones an operator cannot narrow through values. The template is not YAML (it
// carries Helm actions), so the `rules:` block of the first document is extracted and
// every template line dropped; what is left is the literal, unconditional grant.
func readStaticClusterRoleRules(t *testing.T) []policyRule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir, "templates", "rbac.yaml"))
	if err != nil {
		t.Fatalf("read rbac.yaml: %v", err)
	}
	var block []string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		if !in {
			in = line == "rules:"
			continue
		}
		// The rules block ends at the document break or any other column-0 key.
		if strings.HasPrefix(line, "---") {
			break
		}
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line), "{{") {
			continue // a Helm action, not a literal rule
		}
		block = append(block, line)
	}
	if len(block) == 0 {
		t.Fatal("no `rules:` block found in templates/rbac.yaml")
	}
	var rules []policyRule
	if err := yaml.Unmarshal([]byte(strings.Join(block, "\n")), &rules); err != nil {
		t.Fatalf("the static rules of templates/rbac.yaml do not parse as YAML: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("templates/rbac.yaml renders no static rules at all")
	}
	return rules
}

// grantedGetSet indexes rules as "apiGroup/resource" for the rules that grant `get`.
func grantedGetSet(rules []policyRule) map[string]bool {
	out := map[string]bool{}
	for _, r := range rules {
		if !slices.Contains(r.Verbs, "get") {
			continue
		}
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				out[g+"/"+res] = true
			}
		}
	}
	return out
}

// clientGroupAccessors maps a client-go typed-client group accessor to the RBAC
// apiGroup it reads. Only the accessors ownership.go actually uses need an entry; an
// unmapped one fails the test rather than passing vacuously.
var clientGroupAccessors = map[string]string{
	"AppsV1":  "apps",
	"BatchV1": "batch",
	"CoreV1":  "",
}

// ownerWalkGrants derives, from the real source of internal/providers/cluster/ownership.go,
// every apiGroup/resource the owner-chain walk performs a typed Get on. It reads the
// `fetchOwner` switch rather than a copy of it: each arm is a
// `r.client.<GroupAccessor>().<Plural>(ns).Get(...)` chain, and both halves of the RBAC
// rule fall out of the AST — the accessor names the apiGroup, the method IS the plural
// resource name. Adding a `case "CronJob"` therefore fails this test until the chart
// grants batch/cronjobs.
func ownerWalkGrants(t *testing.T) map[string]bool {
	t.Helper()
	src := filepath.Join("..", "providers", "cluster", "ownership.go")
	f, err := parser.ParseFile(token.NewFileSet(), src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Name.Name == "fetchOwner" {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatalf("fetchOwner not found in %s: the owner-walk RBAC guard has lost its parse target", src)
	}
	grants := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		// Match `<something>.<GroupAccessor>().<Plural>(...)`.
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := sel.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		innerSel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		group, ok := clientGroupAccessors[innerSel.Sel.Name]
		if !ok {
			if strings.HasSuffix(innerSel.Sel.Name, "V1") || strings.HasSuffix(innerSel.Sel.Name, "V1beta1") {
				t.Errorf("fetchOwner reads through the client-go accessor %q, which this guard cannot map "+
					"to an apiGroup: add it to clientGroupAccessors so its RBAC grant is checked",
					innerSel.Sel.Name)
			}
			return true
		}
		grants[group+"/"+strings.ToLower(sel.Sel.Name)] = true
		return true
	})
	if len(grants) == 0 {
		t.Fatal("no typed Gets found in fetchOwner: the owner-walk RBAC guard is asserting nothing")
	}
	return grants
}

// TestOwnerWalkRBACIsGrantedStatically is the fix for a silent break.
//
// workload_ownership walks Pod → ReplicaSet → Deployment/StatefulSet/DaemonSet/Job. Those
// kinds arrived in the ClusterRole as a side effect of rbac.resourceSpecRules — a list
// documented as belonging to a DIFFERENT tool. An operator narrowing that allowlist (or
// setting it to [], which the values file now tells them how to do) would have severed the
// walk, and it does not fail loudly: fetchOwner treats an error as "top of chain reached",
// so every "Deployment X, owned by Kustomization Y" silently degrades to a bare
// "ReplicaSet X" with the whole suite green.
//
// So the grant belongs to its consumer: these rules are STATIC in the template, and this
// test reads both ends — the kinds from the walk's own source, the grant from the template's
// literal rules.
func TestOwnerWalkRBACIsGrantedStatically(t *testing.T) {
	granted := grantedGetSet(readStaticClusterRoleRules(t))
	for want := range ownerWalkGrants(t) {
		if !granted[want] {
			t.Errorf("workload_ownership Gets %q, but no STATIC rule in templates/rbac.yaml grants it: "+
				"an RBAC denial there does not error, it truncates the owner chain silently", want)
		}
	}
}

// TestOwnerWalkRBACSurvivesDecliningResourceSpec: values.yaml now tells operators that
// `resourceSpecRules: []` "does NOT break workload_ownership". That promise is only true
// while the static rules carry the walk, so the claim and the template are checked
// together — a template edit that folds the owner-chain grants back into the values-driven
// block turns a documented safe opt-out into a silent truncation.
func TestOwnerWalkRBACSurvivesDecliningResourceSpec(t *testing.T) {
	static := grantedGetSet(readStaticClusterRoleRules(t))
	walk := ownerWalkGrants(t)
	// The decline path renders the static rules and nothing else.
	missing := 0
	for want := range walk {
		if !static[want] {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("with rbac.resourceSpecRules: [] the ClusterRole misses %d of the %d kinds "+
			"workload_ownership walks", missing, len(walk))
	}
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	if !strings.Contains(string(raw), "does NOT break workload_ownership") {
		t.Error("values.yaml no longer promises that declining rbac.resourceSpecRules leaves " +
			"workload_ownership intact: that promise is why the opt-out is safe to document")
	}
}

// specLessKinds have neither .spec nor .status, so resource_spec renders
// "spec: (none — this kind has no spec)" for them: the read returns nothing while the
// cluster-wide grant is entirely real. serviceaccounts, endpointslices and storageclasses
// all shipped in the default allowlist under the heading "SPEC-BEARING kinds".
var specLessKinds = []string{
	"serviceaccounts", // annotations carry IRSA / Workload-Identity role ARNs; names every Secret it mounts
	"endpointslices",
	"storageclasses",
	"configmaps",
	"endpoints",
	"roles",
	"clusterroles",
	"rolebindings",
	"clusterrolebindings",
}

// TestResourceSpecRBACGrantsNothingItCannotRead: the list is introduced to operators as an
// allowlist of SPEC-BEARING kinds, and every entry is a cluster-wide `get`. A kind with no
// spec and no status buys the tool literally nothing and costs the full grant, which is the
// worst trade in the file.
func TestResourceSpecRBACGrantsNothingItCannotRead(t *testing.T) {
	for _, r := range readResourceSpecRules(t) {
		for _, res := range r.Resources {
			if slices.Contains(specLessKinds, res) {
				t.Errorf("rbac.resourceSpecRules grants %q (apiGroups %v), which has neither .spec nor "+
					".status: resource_spec renders \"spec: (none — this kind has no spec)\" while the "+
					"cluster-wide read grant is real", res, r.APIGroups)
			}
		}
	}
}

// TestResourceSpecRBACSaysItIsADefaultGrant. The block ships POPULATED and renders into the
// ClusterRole, so a `helm upgrade` changing no values widens the ServiceAccount. Nothing in
// the file said so, and nothing said `resourceSpecRules: []` declines it cleanly — the block
// read as an opt-in menu. Both facts are load-bearing for an operator reviewing what they
// grant, so both are pinned.
func TestResourceSpecRBACSaysItIsADefaultGrant(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	values := string(raw)
	for _, want := range []string{
		"DEFAULT GRANT, NOT AN OPT-IN MENU",
		"resourceSpecRules: []",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml no longer states %q next to rbac.resourceSpecRules: the block reads as "+
				"an opt-in menu again, and it is a default grant", want)
		}
	}
	// The documented `resourceSpecRules: []` opt-out only renders because the toYaml is
	// GUARDED. Verified empirically against helm 3.14: an unguarded
	// `{{- toYaml .Values.rbac.resourceSpecRules | nindent 2 }}` emits a bare `[]` into the
	// middle of the rules sequence and the whole template fails to render, so an operator
	// following the instruction in values.yaml would get an error instead of a narrow
	// ClusterRole. Either guard form works (`with` and `if` are both falsy on an empty
	// list); what must not happen is losing the guard.
	tpl, err := os.ReadFile(filepath.Join(chartDir, "templates", "rbac.yaml"))
	if err != nil {
		t.Fatalf("read rbac.yaml: %v", err)
	}
	guarded := strings.Contains(string(tpl), "{{- with .Values.rbac.resourceSpecRules }}") ||
		strings.Contains(string(tpl), "{{- if .Values.rbac.resourceSpecRules }}")
	if !guarded {
		t.Error("templates/rbac.yaml renders rbac.resourceSpecRules without a `with`/`if` guard: " +
			"the documented `resourceSpecRules: []` opt-out then emits a bare `[]` into the rules " +
			"sequence and the chart fails to render")
	}
}

// TestChartDocumentsForgeGitHost: forge.git_host and forge.github_api_url appeared NOWHERE
// in the chart — not values.yaml, not any profile. GitHub Enterprise with subdomain
// isolation is a hard startup refusal without git_host (config.validateForgeGitHost), so a
// Helm-only operator met that refusal as a CrashLoopBackOff with no key in the file to
// point at. Pinned in values.yaml and in every profile that ships a forge block.
func TestChartDocumentsForgeGitHost(t *testing.T) {
	files := []string{"values.yaml"}
	for _, p := range valuesProfiles {
		raw, err := os.ReadFile(filepath.Join(chartDir, p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if strings.Contains(string(raw), "\n  forge:") {
			files = append(files, p)
		}
	}
	if len(files) < 2 {
		t.Fatal("no values profile ships a forge block: this guard has lost its subject")
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(chartDir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, want := range []string{"git_host", "github_api_url"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s ships a forge block but never mentions %q: a GitHub Enterprise install "+
					"with subdomain isolation fails config load, and the operator has no key in the "+
					"chart to set", f, want)
			}
		}
	}
}

// TestThreadRegistryWarningCoversBothTransports: NOTES.txt gated the thread-registry
// persistence warning on notify.slack.thread_capture ALONE, so a Matrix-only thread-capture
// deployment was never warned that its registry does not survive a restart — even though
// Validate's Matrix arm requires outcome.ledger_path for exactly that durability. The
// warning is the only place the VOLUME under that path is checked.
func TestThreadRegistryWarningCoversBothTransports(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatalf("read NOTES.txt: %v", err)
	}
	notes := string(raw)
	idx := strings.Index(notes, "thread registry")
	if idx < 0 {
		t.Fatal("NOTES.txt no longer warns about the thread registry at all")
	}
	// The guard expression is the `{{- if ... }}` immediately preceding the warning body.
	guardStart := strings.LastIndex(notes[:idx], "{{- if ")
	if guardStart < 0 {
		t.Fatal("the thread-registry warning in NOTES.txt has no `{{- if }}` guard")
	}
	guard := notes[guardStart:idx]
	for _, want := range []string{
		`"notify" "slack" "thread_capture"`,
		`"notify" "matrix" "thread_capture"`,
	} {
		if !strings.Contains(guard, want) {
			t.Errorf("the thread-registry warning is not gated on %s: that transport's operator is "+
				"never told the registry does not survive a restart", want)
		}
	}
	if !strings.Contains(guard, "or ") {
		t.Error("the thread-registry warning gate does not combine its transports with `or`: one of " +
			"them can no longer trigger the warning on its own")
	}
}
