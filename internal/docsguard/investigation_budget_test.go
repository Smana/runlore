// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// configurationPath is the operator-facing configuration reference.
const configurationPath = "../../website/content/docs/configuration/configuration.md"

// TestInvestigationSpendCeilingsMatchTheDocs pins the `investigation` block's spend
// knobs to reality on both axes — the key NAME off the struct's yaml tag, the VALUE
// off ApplyDefaults — so a rename and a retuning each fail here.
//
// These are the numbers an operator budgets money against, and the page had already
// drifted: it stated "0 = unlimited" for both caps when 0 has meant "apply the bounded
// default" since safe-by-default landed, and -1 is the actual opt-out. Nothing failed,
// because nothing checked. An operator following that page to disable a cap got the
// default instead — the opposite of what they asked for.
func TestInvestigationSpendCeilingsMatchTheDocs(t *testing.T) {
	var c config.Config
	config.ApplyDefaults(&c)
	inv := c.Investigation

	doc := readDoc(t, configurationPath)
	for _, want := range []string{
		fmt.Sprintf("`%s` (**default %d**", tagOf(inv, "MaxToolOutputBytes"), inv.MaxToolOutputBytes),
		fmt.Sprintf("`%s` (**default %d**", tagOf(inv, "MaxTokensPerInvestigation"), inv.MaxTokensPerInvestigation),
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("configuration.md must state the shipped cap verbatim as %q — "+
				"the key or its default changed underneath the page", want)
		}
	}

	// The cost ceiling is the opposite claim: it must be documented as having NO
	// default, and ApplyDefaults must agree. A default quietly introduced later would
	// change what every existing deployment spends, so the page and the code have to
	// stay pinned to each other in this direction too.
	if inv.MaxCostPerInvestigation != 0 {
		t.Errorf("ApplyDefaults now sets %s = %v, but configuration.md documents it as opt-in "+
			"with no default — introducing one silently changes what existing deployments spend",
			tagOf(inv, "MaxCostPerInvestigation"), inv.MaxCostPerInvestigation)
	}
	if want := fmt.Sprintf("`%s` (**no default — opt-in**)", tagOf(inv, "MaxCostPerInvestigation")); !strings.Contains(doc, want) {
		t.Errorf("configuration.md must state the cost ceiling verbatim as %q", want)
	}

	// The page must not still be telling operators that 0 disables these caps: 0 means
	// "use the bounded default" and -1 is the opt-out, so the old wording sent anyone
	// trying to remove a cap to exactly the wrong value.
	for _, key := range []string{
		tagOf(inv, "MaxToolOutputBytes"),
		tagOf(inv, "MaxTokensPerInvestigation"),
	} {
		if strings.Contains(doc, "`"+key+"` (0 = unlimited") {
			t.Errorf("configuration.md still says `%s` (0 = unlimited); 0 applies the bounded "+
				"default and -1 is the opt-out", key)
		}
	}
}

// TestConfigurationPageStatesTheCumulativeTokenSemantics pins the CLAIM, not just the
// number: max_tokens_per_investigation bounds an investigation's accumulated tokens,
// not the size of one request. It bounded only the request until this was fixed, and
// the difference is the whole reason the same 100000 now stops runs it used to let
// through — a page that describes it as a context/request ceiling would leave an
// operator unable to explain their own budget_exceeded results.
func TestConfigurationPageStatesTheCumulativeTokenSemantics(t *testing.T) {
	doc := flattenProse(readDoc(t, configurationPath))
	if !strings.Contains(doc, "a **cumulative** ceiling on one investigation's model tokens") {
		t.Error("configuration.md must describe max_tokens_per_investigation as a cumulative " +
			"ceiling over the whole investigation, not a per-request one")
	}
	// The inertness of a cost ceiling without rates is the finding this whole feature
	// exists to close ("a limit that is neither enforced nor signalled"). It is stated
	// at startup by CostCeilingWithoutPricingWarning; it must be stated on the page too,
	// because an operator reads the page BEFORE they get the warning.
	if !strings.Contains(doc, "It does nothing without `model.pricing`") {
		t.Error("configuration.md must say plainly that max_cost_per_investigation is inert " +
			"without model.pricing — a ceiling that cannot fire has to be documented as such")
	}
}
