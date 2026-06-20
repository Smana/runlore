package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads, strictly parses, and validates a RunLore config file. Unknown keys
// are rejected (KnownFields) so a typo in a safety-critical field — e.g. an
// autonomy gate — fails loudly instead of being silently ignored.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()
	var c Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &c, nil
}
