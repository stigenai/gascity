package config

import (
	"strings"
	"testing"
)

func TestValidateAgentsSecretReferences(t *testing.T) {
	t.Parallel()
	valid := Agent{
		Name: "worker",
		SecretReferences: []SecretReference{{
			ID: "claude-primary", Environment: "CLAUDE_CONFIG_DIR", MountPath: "/run/gascity/omnigent/credentials/claude-primary",
			Kubernetes: &KubernetesSecretKeyReference{Name: "claude-primary", Key: "token"},
			SSH:        &SSHSecretPathReference{Path: "/srv/gc-secrets/claude-primary-token"},
		}},
	}
	if err := ValidateAgents([]Agent{valid}); err != nil {
		t.Fatalf("ValidateAgents(valid secret reference): %v", err)
	}

	invalid := valid.Clone()
	invalid.SecretReferences = append(invalid.SecretReferences, invalid.SecretReferences[0])
	if err := ValidateAgents([]Agent{invalid}); err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("ValidateAgents(duplicate secret reference) = %v, want duplicate id", err)
	}

	invalid = valid.Clone()
	invalid.SecretReferences[0].Environment = "GC_SESSION_ID"
	if err := ValidateAgents([]Agent{invalid}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("ValidateAgents(reserved environment) = %v, want reserved", err)
	}

	invalid = valid.Clone()
	invalid.Env = map[string]string{"CLAUDE_CONFIG_DIR": "ambient-value"}
	if err := ValidateAgents([]Agent{invalid}); err == nil || !strings.Contains(err.Error(), "also declared in env") {
		t.Fatalf("ValidateAgents(ambient override) = %v, want conflict", err)
	}
}

func TestAgentSecretReferencesCloneDeeply(t *testing.T) {
	t.Parallel()
	agent := Agent{Name: "worker", SecretReferences: []SecretReference{{
		ID: "codex", MountPath: "/run/secrets/codex",
		Kubernetes: &KubernetesSecretKeyReference{Name: "codex", Key: "config"},
		SSH:        &SSHSecretPathReference{Path: "/srv/gc-secrets/codex"},
	}}}
	clone := agent.Clone()
	clone.SecretReferences[0].ID = "changed"
	clone.SecretReferences[0].Kubernetes.Name = "changed"
	clone.SecretReferences[0].SSH.Path = "/changed"
	if agent.SecretReferences[0].ID != "codex" || agent.SecretReferences[0].Kubernetes.Name != "codex" || agent.SecretReferences[0].SSH.Path != "/srv/gc-secrets/codex" {
		t.Fatalf("Agent.Clone aliased secret references: %+v", agent.SecretReferences)
	}
}

func TestAgentPatchReplacesSecretReferences(t *testing.T) {
	t.Parallel()
	agent := Agent{Name: "worker", SecretReferences: []SecretReference{{ID: "old", Environment: "OLD", SSH: &SSHSecretPathReference{Path: "/old"}}}}
	patch := AgentPatch{SecretReferences: []SecretReference{{ID: "new", Environment: "NEW", SSH: &SSHSecretPathReference{Path: "/new"}}}}
	applyAgentPatchFields(&agent, &patch)
	if len(agent.SecretReferences) != 1 || agent.SecretReferences[0].ID != "new" {
		t.Fatalf("patched SecretReferences = %+v, want replacement", agent.SecretReferences)
	}
}

func TestSecretReferencesParseAndMarshalTOML(t *testing.T) {
	t.Parallel()
	input := []byte(`
[workspace]
name = "capsule-city"

[[agent]]
name = "worker"

[[agent.secret]]
id = "claude-primary"
environment = "CLAUDE_AUTH_TOKEN"
[agent.secret.kubernetes]
name = "claude-primary"
key = "token"
optional = true
[agent.secret.ssh]
path = "/srv/gc-secrets/claude-primary-token"
`)
	city, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse(secret references): %v", err)
	}
	if len(city.Agents) != 1 || len(city.Agents[0].SecretReferences) != 1 {
		t.Fatalf("parsed agents = %+v", city.Agents)
	}
	ref := city.Agents[0].SecretReferences[0]
	if ref.ID != "claude-primary" || ref.Kubernetes == nil || ref.Kubernetes.Key != "token" || !ref.Kubernetes.Optional || ref.SSH == nil || ref.SSH.Path != "/srv/gc-secrets/claude-primary-token" {
		t.Fatalf("parsed secret reference = %+v", ref)
	}
	encoded, err := city.Marshal()
	if err != nil {
		t.Fatalf("Marshal(secret references): %v", err)
	}
	roundTrip, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Parse(Marshal(secret references)): %v\n%s", err, encoded)
	}
	got := roundTrip.Agents[0].SecretReferences[0]
	if got.Kubernetes == nil || got.Kubernetes.Name != "claude-primary" || !got.Kubernetes.Optional || got.SSH == nil || got.SSH.Path != ref.SSH.Path {
		t.Fatalf("round-trip secret reference = %+v", got)
	}
}
