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
func Load(dir string) ([]Entry, error) {
	var entries []Entry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		base := filepath.Base(path)
		if base == "index.md" || base == "log.md" {
			return nil
		}
		e, perr := parseEntry(dir, path)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func parseEntry(root, path string) (Entry, error) {
	data, err := os.ReadFile(path)
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
	}
	if len(fm) > 0 {
		if err := yaml.Unmarshal(fm, &meta); err != nil {
			return Entry{}, err
		}
	}
	rel, _ := filepath.Rel(root, path)
	return Entry{
		Type: meta.Type, Title: meta.Title, Description: meta.Description,
		Resource: meta.Resource, Tags: meta.Tags, Body: string(body), Path: rel,
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
