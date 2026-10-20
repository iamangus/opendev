//go:build integration

package validation

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReusableWorkflowValidationContract(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for workflow integration")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "opendev-ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["ci"].Steps {
		if step.Name == "Run OpenDev CI contract" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing reusable validation contract")
	}
	for _, test := range []struct {
		name, policy string
		success      bool
		want         string
	}{
		{"unsupported", "", false, "No supported validations found"},
		{"explicit policy", "validations:\n  smoke:\n    command: printf validated\n", true, "Executed 1 validation group(s)"},
		{"empty policy", "validations: {}\n", false, "at least one validation"},
		{"escaping directory", "validations:\n  bad:\n    command: printf unsafe\n    directory: ../outside\n", false, "Invalid validation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if test.policy != "" {
				if err := exec.Command(python, "-c", "import yaml").Run(); err != nil {
					t.Skip("workflow installs PyYAML when a policy is present")
				}
				os.MkdirAll(filepath.Join(root, ".opendev"), 0755)
				os.WriteFile(filepath.Join(root, ".opendev", "validations.yml"), []byte(test.policy), 0644)
			}
			cmd := exec.Command(python, "-c", script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "OPENDEV_CI_PROFILE=auto")
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.success || !strings.Contains(string(output), test.want) {
				t.Fatalf("workflow result: %v\n%s", err, output)
			}
		})
	}
}
