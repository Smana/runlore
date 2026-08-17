// SPDX-License-Identifier: Apache-2.0

package telemetry

// ScoreBuckets exposes scoreBuckets to the EXTERNAL telemetry_test package. It
// exists only in the test build — this file is an _test.go — so it adds nothing
// to the package's API.
//
// The external package is not a style preference, it is the only way the
// alignment guard can run at all. That guard reads the recall gates from
// config.ApplyDefaults rather than restating them, which is the whole point of
// it; but internal/config imports internal/thread for its default constants and
// internal/thread imports this package for its metrics, so a `package telemetry`
// test that imports config closes a cycle: config → thread → telemetry → config.
// Go allows an external xxx_test package to import packages that depend on the
// package under test, which breaks it. The cost is that the guard can only reach
// exported names, hence this file.
var ScoreBuckets = scoreBuckets
