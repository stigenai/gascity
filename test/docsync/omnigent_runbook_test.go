package docsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteOmnigentRunbookDocumentsNestedKubernetesSandbox(t *testing.T) {
	path := filepath.Join(repoRoot(), "docs", "runbooks", "remote-omnigent-capsules.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"os_env:\n  cwd: .\n  sandbox:\n    type: none",
		"allowPrivilegeEscalation=false",
		"RuntimeDefault",
		"SYS_ADMIN",
		"separate agent file for SSH",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("remote Omnigent runbook must document the restricted Kubernetes nested-sandbox boundary; missing %q", want)
		}
	}
}
