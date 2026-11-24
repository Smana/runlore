// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load walks dir and parses every concept .md file into an Entry. Reserved files
// (see reservedBundleFiles) are skipped: they are human-facing docs, not knowledge
// entries, so indexing them would pollute search and — carrying no OKF frontmatter —
// trip the validator for no reason.
//
// A file that fails to parse (e.g. malformed YAML frontmatter) is skipped rather
// than failing the whole load — its path is returned in skipped so the caller can
// warn — so one bad entry never empties the catalog. A genuine walk/IO error is
// still returned as the fatal error.
func Load(dir string) (entries []Entry, skipped []string, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = root.Close() }()

	werr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		base := d.Name()
		if d.IsDir() {
			// Skip hidden dirs — notably ConfigMap mounts' ..data / ..2026_* symlink
			// trees, which would otherwise double-index every entry.
			if path != dir && strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// Only regular files and symlinks that resolve strictly inside dir are
		// considered. A catalog may be synced from a Git repo (where a hostile
		// mode-120000 symlink could attempt to read the ServiceAccount token or
		// /proc/self/environ) OR mounted from a Kubernetes ConfigMap (where every
		// top-level file is a symlink to ..data/<file> -> ..2026_*/<file>).
		// Opening through os.OpenRoot enforces kernel-level confinement to dir
		// with no TOCTOU window, and f.Stat().Mode().IsRegular() rejects targets
		// that are directories, FIFOs, or device nodes before reading.
		isSymlink := d.Type()&fs.ModeSymlink != 0
		if !d.Type().IsRegular() && !isSymlink {
			return nil
		}
		if !IsEntryFile(base) {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		e, ok, perr := parseEntry(root, rel, isSymlink)
		if perr != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", path, perr))
			return nil // skip the bad entry; keep indexing the rest
		}
		if !ok {
			return nil
		}
		entries = append(entries, e)
		return nil
	})
	if werr != nil {
		return nil, nil, werr
	}
	return entries, skipped, nil
}

// reservedBundleFiles are filenames that are never knowledge entries: the OKF
// bundle files (index.md, log.md) plus the conventional repo documents any
// catalog served from a Git repository will carry.
//
// The repo-document set is not hypothetical. The public commons repo has a
// CONTRIBUTING.md — as does every repo that accepts contributions — and the
// loader used to index it as a knowledge entry: it pollutes kb_search, and
// because it carries no OKF frontmatter it also trips the validator for no
// reason. Matched case-insensitively, since these are conventionally uppercase.
var reservedBundleFiles = map[string]bool{
	"index.md":           true,
	"log.md":             true,
	"readme.md":          true,
	"contributing.md":    true,
	"code_of_conduct.md": true,
	"security.md":        true,
	"changelog.md":       true,
	"license.md":         true,
	"governance.md":      true,
	"maintainers.md":     true,
	"support.md":         true,
}

// IsEntryFile reports whether a base filename is an OKF catalog entry file: a
// non-hidden .md that is not one of the reserved bundle files. Load and
// `lore kb import` share it so "what counts as an entry" is defined once and
// their notions can't drift.
func IsEntryFile(base string) bool {
	if strings.HasPrefix(base, ".") || !strings.HasSuffix(base, ".md") {
		return false
	}
	return !reservedBundleFiles[strings.ToLower(base)]
}

// entryMeta is the exact set of frontmatter keys the loader parses, keyed by
// their real yaml tags. Named rather than anonymous so TestOKFFormatDocMatchesLoader
// can reflect over its tags — see that test for why they, not Entry's Go field
// names, are the contract worth pinning.
type entryMeta struct {
	Type          string   `yaml:"type"`
	Title         string   `yaml:"title"`
	Description   string   `yaml:"description"`
	Resource      string   `yaml:"resource"`
	AlertResource string   `yaml:"alert_resource"`
	Tags          []string `yaml:"tags"`
	Timestamp     string   `yaml:"timestamp"`
	Fingerprint   string   `yaml:"fingerprint"`
	Status        string   `yaml:"status"`
	LastValidated string   `yaml:"last_validated"`
}

func parseEntry(root *os.Root, rel string, isSymlink bool) (Entry, bool, error) {
	f, err := root.Open(rel)
	if err != nil {
		if isSymlink {
			// Symlinks escaping root (rejected by os.OpenRoot) or dangling links
			// are silently skipped rather than reported as parse errors.
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		if isSymlink {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	if !st.Mode().IsRegular() {
		return Entry{}, false, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return Entry{}, false, err
	}
	fm, body := SplitFrontmatter(data)
	var meta entryMeta
	if len(fm) > 0 {
		if err := yaml.Unmarshal(fm, &meta); err != nil {
			return Entry{}, false, err
		}
	}
	return Entry{
		Type: meta.Type, Title: meta.Title, Description: meta.Description,
		AlertResource: meta.AlertResource,
		Resource:      meta.Resource, Tags: meta.Tags,
		Timestamp: meta.Timestamp, Fingerprint: meta.Fingerprint,
		Status: meta.Status, LastValidated: meta.LastValidated,
		Body: string(body), Path: rel,
	}, true, nil
}

// SplitFrontmatter separates a leading "---\n...\n---\n" YAML block from the body.
// Exported because `lore kb import` reuses the exact same split when normalizing
// source runbooks, so import and load agree on where frontmatter ends.
func SplitFrontmatter(data []byte) (frontmatter, body []byte) {
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return nil, data
	}
	parts := strings.SplitN(s, "\n---", 2)
	if len(parts) < 2 {
		return nil, data
	}
	fm := strings.TrimPrefix(parts[0], "---\n")
	b := strings.TrimPrefix(parts[1], "\n")
	return []byte(fm), []byte(b)
}
