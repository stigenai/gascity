package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSharedCISetupGoVersionsMatchModuleMinimum(t *testing.T) {
	root := repoRoot(t)
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`).FindSubmatch(goMod)
	if match == nil {
		t.Fatal("go.mod has no Go version directive")
	}
	want := string(match[1])

	for _, platform := range []string{"ubuntu", "macos"} {
		t.Run(platform, func(t *testing.T) {
			actionPath := filepath.Join(root, ".github", "actions", "setup-gascity-"+platform, "action.yml")
			contents, err := os.ReadFile(actionPath)
			if err != nil {
				t.Fatal(err)
			}
			var action struct {
				Inputs map[string]struct {
					Default string `yaml:"default"`
				} `yaml:"inputs"`
			}
			if err := yaml.Unmarshal(contents, &action); err != nil {
				t.Fatalf("decode %s: %v", actionPath, err)
			}
			if got := action.Inputs["go-version"].Default; got != want {
				t.Errorf("go-version default = %q, want go.mod minimum %q", got, want)
			}
		})
	}
}
