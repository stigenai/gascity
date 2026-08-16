package omnigent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalModeValidationAllowsModelEndpointsAndLocalSandbox(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	data := `name: worker
executor:
  harness: claude-sdk
  auth:
    type: api_key
    api_key: $MODEL_TOKEN
    base_url: https://model.example/v1
os_env:
  cwd: .
  sandbox:
    type: none
telemetry: false
update_check: false
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateLocalModeYAML(path, "agent"); err != nil {
		t.Fatal(err)
	}
}

func TestLocalModeValidationRejectsRemotePlacementAndHiddenOverrides(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"Kubernetes host", "host_type: kubernetes\n", "placement"},
		{"Daytona sandbox", "sandbox:\n  type: daytona\n", "remote placement"},
		{"remote host URL", "remote_host: https://host.example\n", "placement"},
		{"tunnel", "tunnel: true\n", "placement"},
		{"telemetry", "telemetry: true\n", "disabled"},
		{"update", "auto_update: true\n", "disabled"},
		{"checkout", "checkout: /alternate/repo\n", "placement"},
		{"worktree", "worktree: ../other\n", "placement"},
		{"cwd escape", "os_env:\n  cwd: ../other\n", "must be '.'"},
		{"alias", "defaults: &defaults\n  telemetry: false\ncopy: *defaults\n", "aliases"},
		{"merge redirect", "defaults: &defaults\n  telemetry: false\nconfig:\n  <<: *defaults\n", "merge redirects"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			err := validateLocalModeYAML(path, "test")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
