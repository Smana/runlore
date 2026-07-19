// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"testing"
)

// Hermetic benchmarks (local fixture repo, no network) contrasting the two
// clone strategies behind what_changed. Run with:
//
//	go test ./internal/whatchanged/ -bench BenchmarkRemote -benchtime 5x -run '^$'
func BenchmarkRemoteClonePerCall(b *testing.B) {
	src, v1, v2 := buildRepo(b)
	d := &Differ{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Remote(context.Background(), src, v1.String(), v2.String(), "apps/harbor"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRemoteMirror(b *testing.B) {
	src, v1, v2 := buildRepo(b)
	mc, err := NewMirrorCache(b.TempDir(), 10)
	if err != nil {
		b.Fatal(err)
	}
	d := &Differ{Mirrors: mc}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Remote(context.Background(), src, v1.String(), v2.String(), "apps/harbor"); err != nil {
			b.Fatal(err)
		}
	}
}
