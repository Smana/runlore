package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load walks dir and parses every concept .md file into an Entry. The reserved
// OKF files index.md and log.md are skipped.
//
// A file that fails to parse (e.g. malformed YAML frontmatter) is skipped rather
// than failing the whole load — its path is returned in skipped so the caller can
// warn — so one bad entry never empties the catalog. A genuine walk/IO error is
// still returned as the fatal error.
func Load(dir string) (entries []Entry, skipped []string, err error) {
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
		if strings.HasPrefix(base, ".") || !strings.HasSuffix(base, ".md") {
			return nil
		}
		if base == "index.md" || base == "log.md" {
			return nil
		}
		e, perr := parseEntry(dir, path)
		if perr != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", path, perr))
			return nil // skip the bad entry; keep indexing the rest
		}
		entries = append(entries, e)
		return nil
	})
	if werr != nil {
		return nil, nil, werr
	}
	return entries, skipped, nil
}

func parseEntry(root, path string) (Entry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is within the operator-configured catalog directory
	if err != nil {
		return Entry{}, err
	}
	fm, body := splitFrontmatter(data)
	var meta struct {
		Type        string   `yaml:"type"`
		Title       string   `yaml:"title"`
		Description string   `yaml:"description"`
		Resource    string   `yaml:"resource"`
		Tags        []string `yaml:"tags"`
		Timestamp   string   `yaml:"timestamp"`
		Fingerprint string   `yaml:"fingerprint"`
	}
	if len(fm) > 0 {
		if err := yaml.Unmarshal(fm, &meta); err != nil {
			return Entry{}, err
		}
	}
	rel, _ := filepath.Rel(root, path)
	return Entry{
		Type: meta.Type, Title: meta.Title, Description: meta.Description,
		Resource: meta.Resource, Tags: meta.Tags,
		Timestamp: meta.Timestamp, Fingerprint: meta.Fingerprint,
		Body: string(body), Path: rel,
	}, nil
}

// splitFrontmatter separates a leading "---\n...\n---\n" YAML block from the body.
func splitFrontmatter(data []byte) (frontmatter, body []byte) {
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
