// Package foundation defines the versioned files required for an OpenDev-ready repository.
package foundation

import "fmt"

const Version = "v3"

const (
	ManifestPath = ".opendev/repository.yml"
	WorkflowPath = ".github/workflows/opendev-ci.yml"
)

// Files returns the deterministic foundation files for a repository. The CI
// implementation is pinned to a versioned reusable workflow so application PRs
// cannot select an executor or arbitrary credentials.
func Files() map[string]string {
	return map[string]string{
		ManifestPath: fmt.Sprintf("version: %s\nci:\n  profile: auto\n  required_checks:\n    - OpenDev CI\n", Version),
		WorkflowPath: `name: OpenDev CI

on:
  pull_request:
    types: [opened, reopened, synchronize]

permissions:
  contents: read

jobs:
  ci:
    uses: iamangus/opendev/.github/workflows/opendev-ci.yml@v3
    with:
      profile: auto
`,
	}
}
