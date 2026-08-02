// SPDX-License-Identifier: Apache-2.0

// Package docsguard holds drift guards that pin published documentation to
// the code it describes, rather than letting it be maintained by hand and
// rot. This repo has repeatedly shipped docs that drifted from reality — a
// badge asserting results from a run that never happened, a benchmark
// command that could not run as documented — so a guard here reflects over a
// runtime source of truth (a registry, a constant) and fails CI the moment a
// claim and the code disagree.
package docsguard
