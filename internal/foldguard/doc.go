// SPDX-License-Identifier: Apache-2.0

// Package foldguard holds a source guard against ONE defect class: a security
// check and the code it protects normalise the same string DIFFERENTLY, so an
// input passes the check and then means something else.
//
// The class shipped twice in unrelated code within hours, and both instances
// put a GitHub App installation token on the wrong side of the disagreement:
//
//   - internal/whatchanged compared clone hosts with strings.ToLower (Unicode
//     SIMPLE case mapping) while net/http resolved the same URL through
//     idna.Lookup.ToASCII. ToLower maps U+0130 to 'i'; IDNA maps it to
//     "i"+U+0307, a different registrable label. "git@gİthub.com:org/repo.git"
//     therefore read as github.com to the check and dialled xn--github-qyd.com
//     with the token attached.
//   - internal/providers/cluster refused Secrets with
//     strings.ToLower(kind) == "secret" and then resolved the kind with
//     strings.EqualFold. EqualFold does Unicode simple FOLDING and ToLower does
//     not, so kind "ſecret" (U+017F) skipped the refusal and still resolved to
//     v1/secrets.
//
// A third instance shipped after the guard did, and is why it has three arms
// rather than two: internal/app derived the forge git host from
// forge.gitlab.base_url / forge.github_api_url with a bare strings.ToLower and
// stored the result in whatchanged.Differ.TokenHost. A GitLab base_url of
// "https://g\u0130tlab.example.com" became the credential boundary
// "gitlab.example.com" — a separately registrable host that then collected the
// forge token, while the operator's own instance was refused it and what_changed
// went silently empty. The fold and the comparison sat in DIFFERENT PACKAGES,
// which is exactly what the first two arms cannot see, so the third arm keys on
// the assignment that carries a host across that edge.
//
// The guard itself lives entirely in foldguard_test.go, which states what it
// does and does not cover.
package foldguard
