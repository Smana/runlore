// SPDX-License-Identifier: Apache-2.0

package app

// TestEveryLoopInvestigatorCarriesTheSpendControls pins that no construction site of
// the paid ReAct loop in this package is built without a spend ceiling.
//
// The ceiling only bounds the loops it is actually handed to, and package app builds
// FOUR of them: the serve investigator, the reinvestigate poller, `lore investigate`,
// and the demo. Three of the four omitted the caps entirely — `lore investigate` and
// the demo set neither token nor cost ceiling, and the poller set no Pricing, which
// leaves the cost ceiling structurally inert there. internal/config/load.go's comment
// already claimed the opposite ("anyone running `lore serve --config` or `lore
// investigate` directly gets the same bounded defaults as the Helm chart").
//
// A per-site unit test would not have caught that: each site is correct in isolation
// and wrong only by omission, and the next site added would be wrong the same way. So
// the guard is structural — every composite literal, discovered by parsing, must set
// the whole set. It reads the field NAMES off the literal, not the values: what the
// operator configured is config.Validate's business, whether the loop was told at all
// is this one's.
import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestEveryLoopInvestigatorCarriesTheSpendControls(t *testing.T) {
	// Pricing is in the set because the cost ceiling cannot fire without it: a loop
	// given MaxCostPerInvestigation and no Pricing produces no cost to compare, which
	// is the same silent no-op CostCeilingWithoutPricingWarning exists to announce —
	// except here the operator DID configure model.pricing, so nothing would warn.
	required := []string{"MaxTokensPerInvestigation", "MaxCostPerInvestigation", "Pricing"}

	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	sites := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // test-owned path in this package
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isLoopInvestigatorType(lit.Type) {
				return true
			}
			sites++
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
			for _, field := range required {
				if !set[field] {
					missing = append(missing, field)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s:%d builds an investigate.LoopInvestigator without %s — "+
					"a spend ceiling bounds only the loops it is handed to, and this one runs "+
					"unbounded against the operator's paid model",
					filepath.Base(path), fset.Position(lit.Pos()).Line, strings.Join(missing, ", "))
			}
			return true
		})
	}
	if sites == 0 {
		t.Fatal("no investigate.LoopInvestigator literals found in package app — this guard is inert")
	}
}

// isLoopInvestigatorType reports whether a composite literal's type is
// investigate.LoopInvestigator.
func isLoopInvestigatorType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "LoopInvestigator" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "investigate"
}
