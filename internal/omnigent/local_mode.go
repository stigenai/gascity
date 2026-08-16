package omnigent

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

var forbiddenPlacementKeys = map[string]bool{
	"host": true, "hosts": true, "host_id": true, "host_type": true,
	"managed_host": true, "remote_host": true, "workspace": true,
	"worktree": true, "git": true, "clone": true, "checkout": true,
	"tunnel": true, "tunnels": true, "collaboration": true, "sharing": true,
	"control_plane": true, "hosted": true, "daytona": true, "kubernetes": true, "k8s": true,
}

// validateLocalModeYAML rejects placement, hosted-control, update, telemetry,
// and checkout ownership from operator Omnigent YAML before the child starts.
// Model-provider endpoints and local sandbox policy remain Omnigent-owned.
func validateLocalModeYAML(path, kind string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read omnigent %s config: %w", kind, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("decode omnigent %s config: %w", kind, err)
	}
	if err := validateLocalModeNode(&document, nil, make(map[*yaml.Node]bool)); err != nil {
		return fmt.Errorf("omnigent %s config is not local-only: %w", kind, err)
	}
	return nil
}

func validateLocalModeNode(node *yaml.Node, path []string, visiting map[*yaml.Node]bool) error {
	if node == nil {
		return nil
	}
	if visiting[node] {
		return errors.New("recursive YAML graph")
	}
	visiting[node] = true
	defer delete(visiting, node)
	if node.Kind == yaml.AliasNode {
		return errors.New("YAML aliases and merge redirects are forbidden")
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := validateLocalModeNode(child, path, visiting); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return errors.New("malformed mapping")
		}
		for i := 0; i < len(node.Content); i += 2 {
			keyNode, valueNode := node.Content[i], node.Content[i+1]
			if keyNode.Kind != yaml.ScalarNode {
				return errors.New("non-scalar mapping key")
			}
			key := normalizeLocalModeKey(keyNode.Value)
			if key == "<<" {
				return errors.New("YAML merge redirects are forbidden")
			}
			fieldPath := append(append([]string(nil), path...), key)
			if forbiddenPlacementKeys[key] && !disabledScalar(valueNode) {
				return fmt.Errorf("field %s belongs to Gas City placement", strings.Join(fieldPath, "."))
			}
			if isTelemetryOrUpdateKey(key) && !disabledScalar(valueNode) {
				return fmt.Errorf("field %s must remain disabled", strings.Join(fieldPath, "."))
			}
			if key == "cwd" && pathContains(path, "os_env") && !scalarEquals(valueNode, ".") {
				return fmt.Errorf("field %s must be '.' so Gas City supplies the workspace", strings.Join(fieldPath, "."))
			}
			if key == "type" && pathContainsAny(path, "host", "hosts", "sandbox", "sandboxes") && remotePlacementValue(valueNode.Value) {
				return fmt.Errorf("field %s selects remote placement %q", strings.Join(fieldPath, "."), valueNode.Value)
			}
			if err := validateLocalModeNode(valueNode, fieldPath, visiting); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeLocalModeKey(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_")
}

func disabledScalar(node *yaml.Node) bool {
	if node.Kind != yaml.ScalarNode {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(node.Value)) {
	case "", "0", "false", "off", "disabled", "none", "null", "~":
		return true
	default:
		return false
	}
}

func scalarEquals(node *yaml.Node, value string) bool {
	return node.Kind == yaml.ScalarNode && strings.TrimSpace(node.Value) == value
}

func isTelemetryOrUpdateKey(key string) bool {
	return strings.Contains(key, "telemetry") || strings.HasPrefix(key, "otel") ||
		strings.Contains(key, "update_check") || strings.Contains(key, "auto_update") ||
		strings.Contains(key, "auto_download")
}

func pathContains(path []string, want string) bool {
	for _, part := range path {
		if part == want {
			return true
		}
	}
	return false
}

func pathContainsAny(path []string, wants ...string) bool {
	for _, want := range wants {
		if pathContains(path, want) {
			return true
		}
	}
	return false
}

func remotePlacementValue(value string) bool {
	switch normalizeLocalModeKey(value) {
	case "remote", "managed", "kubernetes", "k8s", "daytona", "hosted":
		return true
	default:
		return false
	}
}
