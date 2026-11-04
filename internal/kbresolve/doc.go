// SPDX-License-Identifier: Apache-2.0

// Package kbresolve records a fix RunLore saw merged and soaked in a knowledge-
// base entry: the `resolution` frontmatter block and the entry's `## Resolution`
// (or `## Unresolved`) section. Everything it writes is idempotent, and every
// piece of text it copies in is untrusted and sanitized first.
package kbresolve
