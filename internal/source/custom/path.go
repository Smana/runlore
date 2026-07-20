// SPDX-License-Identifier: Apache-2.0

// Package custom is the config-only generic webhook source: operators map any
// vendor's alert JSON to investigation requests with dot-path field extraction
// (sources.custom.instances.<name>), no Go required.
package custom

import (
	"fmt"
	"strconv"
	"strings"
)

// step is one segment of a dot-path: a map key, optionally followed by one
// array index (`labels`, `alerts[0]`).
type step struct {
	key   string
	idx   int
	isIdx bool
}

type path []step

// parsePath parses the supported dot-path subset: dot-separated map keys, each
// optionally suffixed with a single non-negative `[n]` index. Deliberately NOT
// JSONPath (no wildcards, filters, recursion) — YAGNI, zero dependencies.
func parsePath(s string) (path, error) {
	if s == "" {
		return nil, fmt.Errorf("empty path")
	}
	var out path
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return nil, fmt.Errorf("path %q: empty segment", s)
		}
		key, rest := seg, ""
		if i := strings.IndexByte(seg, '['); i >= 0 {
			key, rest = seg[:i], seg[i:]
		}
		if key == "" {
			return nil, fmt.Errorf("path %q: segment %q lacks a key before '['", s, seg)
		}
		st := step{key: key}
		if rest != "" {
			if !strings.HasSuffix(rest, "]") {
				return nil, fmt.Errorf("path %q: unterminated index in %q", s, seg)
			}
			n, err := strconv.Atoi(rest[1 : len(rest)-1])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("path %q: bad index in %q", s, seg)
			}
			st.idx, st.isIdx = n, true
		}
		out = append(out, st)
	}
	return out, nil
}

// lookup walks doc (json.Unmarshal-into-any shapes: map[string]any / []any).
func (p path) lookup(doc any) (any, bool) {
	cur := doc
	for _, st := range p {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[st.key]
		if !ok {
			return nil, false
		}
		if st.isIdx {
			arr, ok := cur.([]any)
			if !ok || st.idx >= len(arr) {
				return nil, false
			}
			cur = arr[st.idx]
		}
	}
	return cur, true
}

// coerce renders a scalar leaf as a string; composite values refuse (a mapping
// that lands on an object is a config mistake surfaced by absence, not garbage).
func coerce(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64: // encoding/json numbers
		return strconv.FormatFloat(t, 'g', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	default:
		return "", false
	}
}
