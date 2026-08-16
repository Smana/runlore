// SPDX-License-Identifier: Apache-2.0

package investigate

// TestEverySpendControlSiteNamesItsCeilings pins that no construction site of the
// paid ReAct loop — anywhere in the repo — is built without a spend ceiling.
//
// The ceiling only bounds the loops it is actually handed to. The first version of
// this guard globbed package `app` alone, and package `app` is where it started:
// three of the four loops there omitted the caps entirely. But `internal/eval` builds
// three MORE (replay, compare, live) and every one of them set no token ceiling, no
// cost ceiling and no Pricing at all — so `lore eval`, which runs the loop over a
// whole case corpus unattended, was the least bounded caller in the repo while
// `lore serve` and `lore investigate` were both fixed. A guard scoped to one package
// could not see that, and would not see the next package either.
//
// So the guard walks the module: every composite literal of a spend-bearing type,
// discovered by parsing, must NAME the whole set. It reads the field names off the
// literal, not the values — what the operator configured is config.Validate's
// business, whether the construction site was told at all is this one's.
//
// The eval runners are in the table for the same reason the loop is. They own a
// `Spend` field they forward into the LoopInvestigator they build, which means the
// loop literal can name all three ceilings and still run unbounded because the
// runner's own literal left `Spend` at its zero value — the identical defect, moved
// one level up. Guarding only the inner literal would bless exactly that.
import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// spendControlSite is one guarded construction site: a package-qualified type and
// the fields its literal must name for the thing it builds to be bounded.
type spendControlSite struct {
	pkg, typ string
	required []string
}

// spendControlSites is the guarded set.
//
// Pricing is in the LoopInvestigator set because the cost ceiling cannot fire
// without it: a loop given MaxCostPerInvestigation and no Pricing produces no cost
// to compare, which is the same silent no-op CostCeilingWithoutPricingWarning exists
// to announce — except there the operator DID configure model.pricing, so nothing
// would warn.
var spendControlSites = []spendControlSite{
	{"investigate", "LoopInvestigator", []string{"MaxTokensPerInvestigation", "MaxCostPerInvestigation", "Pricing"}},
	{"eval", "Runner", []string{"Spend"}},
	{"eval", "ComparisonRunner", []string{"Spend"}},
	{"eval", "LiveRunner", []string{"Spend"}},
}

func TestEverySpendControlSiteNamesItsCeilings(t *testing.T) {
	root := moduleRoot(t)
	found := map[string]int{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS metadata, vendored/third-party trees and the built website:
			// none of them is RunLore source, and .git alone is tens of thousands of
			// files this walk has no business parsing.
			switch d.Name() {
			case ".git", "vendor", "node_modules", "public", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // walking the module's own source tree
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			site, ok := matchSpendControlSite(lit.Type)
			if !ok {
				return true
			}
			found[site.pkg+"."+site.typ]++
			set := map[string]bool{}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok {
					set[key.Name] = true
				}
			}
			var missing []string
			for _, field := range site.required {
				if !set[field] {
					missing = append(missing, field)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s:%d builds a %s.%s without %s — a spend ceiling bounds only the "+
					"loops it is handed to, and this one runs unbounded against the operator's paid model",
					rel, fset.Position(lit.Pos()).Line, site.pkg, site.typ, strings.Join(missing, ", "))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	// A guard that matches nothing passes forever. Every entry must have found at
	// least one live site, so a rename that moves a construction site out from under
	// the guard fails here instead of silently disarming it.
	for _, site := range spendControlSites {
		if found[site.pkg+"."+site.typ] == 0 {
			t.Errorf("no %s.%s literals found anywhere in the module — this guard entry is inert "+
				"(the type was renamed, moved, or is now built by a constructor this parse cannot see)",
				site.pkg, site.typ)
		}
	}
}

// matchSpendControlSite reports whether a composite literal's type is one of the
// guarded package-qualified types. Both `T{}` and `&T{}` reach here as the same
// CompositeLit, so the address-of form needs no special case.
func matchSpendControlSite(expr ast.Expr) (spendControlSite, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return spendControlSite{}, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return spendControlSite{}, false
	}
	for _, site := range spendControlSites {
		if site.pkg == pkg.Name && site.typ == sel.Sel.Name {
			return site, true
		}
	}
	return spendControlSite{}, false
}

// moduleRoot walks up from the test's working directory to the go.mod that roots
// this module, so the guard scans the whole repo regardless of which package
// directory `go test` runs it from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root (go.mod) not found")
		}
		dir = parent
	}
}
