package curate

import (
	"github.com/Smana/runlore/internal/forge/github"
)

// compile-time: the GitHub client satisfies Forge.
var _ Forge = (*github.Client)(nil)
