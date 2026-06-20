package eval

import (
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// Result is the score for one case.
type Result struct {
	Name       string
	Pass       bool
	Confidence float64
	Missing    []string // expected keywords not found (or an error note)
}

// Score reports whether the investigation identifies the expected root cause:
// every must_contain keyword appears in the findings and confidence meets the floor.
func Score(name string, inv providers.Investigation, exp Expected) Result {
	hay := strings.ToLower(investigationText(inv))
	var missing []string
	for _, kw := range exp.MustContain {
		if !strings.Contains(hay, strings.ToLower(kw)) {
			missing = append(missing, kw)
		}
	}
	return Result{
		Name:       name,
		Pass:       len(missing) == 0 && inv.Confidence >= exp.MinConfidence,
		Confidence: inv.Confidence,
		Missing:    missing,
	}
}

// investigationText flattens the findings into one searchable string.
func investigationText(inv providers.Investigation) string {
	var b strings.Builder
	b.WriteString(inv.Title)
	for _, rc := range inv.RootCauses {
		b.WriteString(" " + rc.Summary + " " + rc.SuggestedAction + " " + strings.Join(rc.Evidence, " "))
	}
	b.WriteString(" " + strings.Join(inv.Unresolved, " "))
	return b.String()
}
