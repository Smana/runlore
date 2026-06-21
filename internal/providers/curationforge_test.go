package providers_test

import (
	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/providers"
)

// compile-time assertion: the GitHub client satisfies CurationForge.
var _ providers.CurationForge = (*github.Client)(nil)
