package omnigent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRemoteCapsuleObservableSurfacesExcludeSensitiveSentinels(t *testing.T) {
	t.Parallel()
	const (
		secretValue     = "secret-value-canary-5e583a9cb1"
		authEnvironment = "CLAUDE_CANARY_AUTH_HOME"
		credentialPath  = "/srv/private-canary/claude-auth"
		statePath       = "/provider/private-canary/capsule-state"
		runtimeAddress  = "remote-canary-user@host.example"
		policyContent   = "policy-content-canary-1c66e9"
		transcript      = "transcript-content-canary-53e0cf"
		publicProfileID = "profile"
		publicBlurb     = "Compatible backend profile."
	)

	input := testCapsuleLaunchInput(t)
	input.Runtime = "ssh:" + runtimeAddress
	input.SecretReferences = []runtime.SecretReference{{
		ID: publicProfileID, Environment: authEnvironment,
		Kubernetes: &runtime.KubernetesSecretKeyReference{Name: "remote-canary", Key: "auth"},
		SSH:        &runtime.SSHSecretPathReference{Path: credentialPath},
	}}
	plan, err := ResolveAttachmentLaunchPlan(input)
	if err != nil {
		t.Fatal(err)
	}
	status := plan.RemoteStatus()
	if status.ProfileID != publicProfileID || status.Blurb != publicBlurb || status.Backend != "compatible" {
		t.Fatalf("remote status lost public profile context: %#v", status)
	}

	statusJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var drift bytes.Buffer
	runtime.LogCoreFingerprintDrift(&drift, "capsule", `{}`, runtime.Config{SecretReferences: plan.SecretReferences})
	missing := input.SecretReferences[0]
	missing.SSH = nil
	_, projectionErr := input.Catalog.ProjectProfileCredentials(publicProfileID, runtime.SecretProviderSSH, []runtime.SecretReference{missing})
	if !errors.Is(projectionErr, runtime.ErrSecretSourceUnavailable) {
		t.Fatalf("projection error = %v, want typed unavailable error", projectionErr)
	}

	redactor := newRemoteRedactor(
		[]string{authEnvironment + "=" + secretValue},
		input.SecretReferences,
		statePath, policyContent, transcript,
	)
	artifacts := map[string]string{
		"provider-facing command plan": strings.Join(plan.CommandArgs(), "\x00"),
		"process arguments":            strings.Join(plan.CommandArgs(), "\x00"),
		"status and CLI JSON":          string(statusJSON),
		"fingerprint diagnostics":      drift.String(),
		"typed projection error":       projectionErr.Error(),
		"log output":                   redactor.Redact(fmt.Sprintf("%s %s %s %s %s %s", secretValue, authEnvironment, credentialPath, statePath, policyContent, transcript)),
		"crash error":                  redactRemoteError(errors.New("child crashed: "+secretValue+" "+credentialPath), redactor).Error(),
		"metrics and events":           fmt.Sprintf("profile=%s backend=%s state=unavailable", status.ProfileID, status.Backend),
		"Beads metadata":               fmt.Sprintf("session=%s profile=%s state=retained", status.SessionID, status.ProfileID),
	}
	for surface, artifact := range artifacts {
		t.Run(surface, func(t *testing.T) {
			for _, forbidden := range []string{secretValue, authEnvironment, credentialPath, statePath, runtimeAddress, policyContent, transcript} {
				if strings.Contains(artifact, forbidden) {
					t.Fatalf("%s leaked %q: %q", surface, forbidden, artifact)
				}
			}
		})
	}
}

func TestRemoteStatusDocumentsProviderConfinedMetadataBoundary(t *testing.T) {
	t.Parallel()
	input := testCapsuleLaunchInput(t)
	input.Runtime = "k8s"
	plan, err := ResolveAttachmentLaunchPlan(input)
	if err != nil {
		t.Fatal(err)
	}
	internal, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(plan.RemoteStatus())
	if err != nil {
		t.Fatal(err)
	}
	for _, requiredInternal := range []string{"CLAUDE_AUTH_TOKEN", "claude-primary"} {
		if !bytes.Contains(internal, []byte(requiredInternal)) {
			t.Fatalf("provider-confined plan lost required metadata %q: %s", requiredInternal, internal)
		}
		if bytes.Contains(public, []byte(requiredInternal)) {
			t.Fatalf("remote public status leaked provider-confined metadata %q: %s", requiredInternal, public)
		}
	}
}
