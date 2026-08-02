// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/forge/gitlab"
	"github.com/Smana/runlore/internal/providers"
)

// compile-time assertion: the GitHub client satisfies CurationForge.
var _ providers.CurationForge = (*github.Client)(nil)

// compile-time assertion: the GitLab client satisfies CurationForge.
var _ providers.CurationForge = (*gitlab.Client)(nil)
