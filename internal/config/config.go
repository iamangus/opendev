// Package config loads the per-repo OpenDev configuration file.
//
// Each target repository is expected to contain a file at
// .opendev/config.yaml relative to its root. opendev reads this file
// whenever a worktree is registered (at startup or on branch creation) to
// automatically register the repo-wide test command without requiring any
// agent involvement.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigPath is the path within a worktree where the OpenDev config is expected.
const ConfigPath = ".opendev/config.yaml"

// OpenDevConfig holds the OpenDev configuration for a target repository.
type OpenDevConfig struct {
	// TestCommand is the shell command used to run the full test suite for
	// the repository (e.g. "go test ./..." or "npm test").
	TestCommand string `yaml:"test_command"`
}

// Load reads and parses the OpenDev config file from the given worktree
// directory. It returns an error if:
//   - the file does not exist
//   - the file cannot be parsed as valid YAML
//   - test_command is empty or missing
func Load(worktreeDir string) (*OpenDevConfig, error) {
	path := filepath.Join(worktreeDir, ConfigPath)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s not found in worktree %q: add a .opendev/config.yaml with a test_command field", ConfigPath, worktreeDir)
		}
		return nil, fmt.Errorf("reading %s: %w", ConfigPath, err)
	}

	var cfg OpenDevConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", ConfigPath, err)
	}

	cfg.TestCommand = strings.TrimSpace(cfg.TestCommand)
	if cfg.TestCommand == "" {
		return nil, fmt.Errorf("%s: test_command is required but was not set", ConfigPath)
	}

	return &cfg, nil
}
