package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCapsuleKeyIsDeterministicContainedAndCollisionResistant(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, scope, session string
	}{
		{"ordinary", "cluster-a/namespace-a", "ga-session"},
		{"unicode", "ssh://worker@example", "会話/worker"},
		{"punctuation", "host/account:/srv/gc", "!!!"},
		{"long", strings.Repeat("scope", 40), strings.Repeat("session", 40)},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := NewCapsuleKey(tc.scope, tc.session)
			if err != nil {
				t.Fatalf("NewCapsuleKey: %v", err)
			}
			second, err := NewCapsuleKey(tc.scope, tc.session)
			if err != nil || first != second {
				t.Fatalf("key is not deterministic: first=%+v second=%+v err=%v", first, second, err)
			}
			if len(first.Token) != 26 || len(first.Digest) != 64 {
				t.Fatalf("token/digest lengths = %d/%d, want 26/64", len(first.Token), len(first.Digest))
			}
			if len(first.ResourceStem()) > 51 || strings.ContainsAny(first.ResourceStem(), "/_.") {
				t.Fatalf("resource stem is not portable: %q", first.ResourceStem())
			}
			if seen[first.Token] {
				t.Fatalf("test corpus collision for token %q", first.Token)
			}
			seen[first.Token] = true
		})
	}
	for _, invalid := range [][2]string{{"", "session"}, {"scope", ""}, {"bad\x00scope", "session"}, {"scope", "bad\x00session"}} {
		if _, err := NewCapsuleKey(invalid[0], invalid[1]); err == nil {
			t.Fatalf("NewCapsuleKey(%q, %q) succeeded, want error", invalid[0], invalid[1])
		}
	}
}

func TestCapsuleKeyValidationRejectsForgedIdentity(t *testing.T) {
	t.Parallel()
	key, err := NewCapsuleKey("city-a", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Validate(); err != nil {
		t.Fatalf("canonical key Validate: %v", err)
	}
	for name, mutate := range map[string]func(*CapsuleKey){
		"city":    func(k *CapsuleKey) { k.CityScope = "city-b" },
		"session": func(k *CapsuleKey) { k.SessionID = "session-b" },
		"token":   func(k *CapsuleKey) { k.Token = strings.Repeat("a", 26) },
		"digest":  func(k *CapsuleKey) { k.Digest = strings.Repeat("b", 64) },
		"version": func(k *CapsuleKey) { k.Version++ },
	} {
		t.Run(name, func(t *testing.T) {
			forged := key
			mutate(&forged)
			if err := forged.Validate(); !errors.Is(err, ErrCapsuleStateConflict) {
				t.Fatalf("forged key Validate error = %v, want ErrCapsuleStateConflict", err)
			}
		})
	}
}

func TestValidateSecretReferencesRejectsAmbiguityAndNeverFormatsValues(t *testing.T) {
	t.Parallel()
	valid := []SecretReference{
		{
			ID: "codex-home", MountPath: "/run/secrets/codex",
			Kubernetes: &KubernetesSecretKeyReference{Name: "codex-auth", Key: "config"},
			SSH:        &SSHSecretPathReference{Path: "/srv/gc-secrets/codex"},
		},
		{
			ID: "claude-token", Environment: "CLAUDE_AUTH_TOKEN",
			Kubernetes: &KubernetesSecretKeyReference{Name: "claude-primary", Key: "token"},
			SSH:        &SSHSecretPathReference{Path: "/srv/gc-secrets/claude-primary-token"},
		},
	}
	if err := ValidateSecretReferences(valid); err != nil {
		t.Fatalf("ValidateSecretReferences(valid): %v", err)
	}
	for name, refs := range map[string][]SecretReference{
		"empty id":             {{Environment: "TOKEN", Kubernetes: &KubernetesSecretKeyReference{Name: "s", Key: "k"}}},
		"bad id":               {{ID: "bad/id", Environment: "TOKEN", Kubernetes: &KubernetesSecretKeyReference{Name: "s", Key: "k"}}},
		"both destinations":    {{ID: "a", Environment: "TOKEN", MountPath: "/secret", Kubernetes: &KubernetesSecretKeyReference{Name: "s", Key: "k"}}},
		"no destination":       {{ID: "a", Kubernetes: &KubernetesSecretKeyReference{Name: "s", Key: "k"}}},
		"relative mount":       {{ID: "a", MountPath: "secret", Kubernetes: &KubernetesSecretKeyReference{Name: "s", Key: "k"}}},
		"reserved environment": {{ID: "a", Environment: "GC_SESSION_ID", Kubernetes: &KubernetesSecretKeyReference{Name: "s", Key: "k"}}},
		"no source":            {{ID: "a", Environment: "TOKEN"}},
		"bad kubernetes":       {{ID: "a", Environment: "TOKEN", Kubernetes: &KubernetesSecretKeyReference{Name: "bad name", Key: "k"}}},
		"relative ssh":         {{ID: "a", Environment: "TOKEN", SSH: &SSHSecretPathReference{Path: "relative"}}},
		"duplicate id":         {valid[0], valid[0]},
		"duplicate destination": {valid[0], {
			ID: "other", MountPath: valid[0].MountPath,
			Kubernetes: &KubernetesSecretKeyReference{Name: "other", Key: "key"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateSecretReferences(refs); err == nil {
				t.Fatal("ValidateSecretReferences succeeded, want error")
			}
		})
	}

	ref := valid[1]
	formatted := fmt.Sprintf("%v %+v %#v", ref, ref, ref)
	if strings.Contains(formatted, "super-secret-value") {
		t.Fatal("SecretReference formatting retained a secret value")
	}
}

func TestSelectSecretReferencesConfinesSourcesToSelectedProvider(t *testing.T) {
	t.Parallel()
	refs := []SecretReference{{
		ID: "claude", Environment: "CLAUDE_AUTH_TOKEN",
		Kubernetes: &KubernetesSecretKeyReference{Name: "claude-primary", Key: "token"},
		SSH:        &SSHSecretPathReference{Path: "/srv/gc-secrets/claude-primary-token"},
	}}
	kubernetes, err := SelectSecretReferences(SecretProviderKubernetes, refs)
	if err != nil {
		t.Fatalf("SelectSecretReferences(k8s): %v", err)
	}
	if kubernetes[0].Kubernetes == nil || kubernetes[0].SSH != nil {
		t.Fatalf("kubernetes selection leaked another provider source: %+v", kubernetes[0])
	}
	ssh, err := SelectSecretReferences(SecretProviderSSH, refs)
	if err != nil {
		t.Fatalf("SelectSecretReferences(ssh): %v", err)
	}
	if ssh[0].SSH == nil || ssh[0].Kubernetes != nil {
		t.Fatalf("SSH selection leaked another provider source: %+v", ssh[0])
	}

	missing := []SecretReference{{
		ID: "k8s-only", Environment: "TOKEN",
		Kubernetes: &KubernetesSecretKeyReference{Name: "credential", Key: "token"},
	}}
	_, err = SelectSecretReferences(SecretProviderSSH, missing)
	if !errors.Is(err, ErrSecretSourceUnavailable) {
		t.Fatalf("missing SSH source error = %v, want ErrSecretSourceUnavailable", err)
	}
	var providerErr *SecretProviderError
	if !errors.As(err, &providerErr) || providerErr.ReferenceID != "k8s-only" || providerErr.Provider != SecretProviderSSH {
		t.Fatalf("missing SSH source typed error = %#v", providerErr)
	}
	_, err = SelectSecretReferences("daytona", refs)
	if !errors.Is(err, ErrUnsupportedSecretProvider) {
		t.Fatalf("unsupported provider error = %v, want ErrUnsupportedSecretProvider", err)
	}
}

func TestSecretReferenceIdentityControlsProvisionFingerprintWithoutRotationValue(t *testing.T) {
	t.Parallel()
	base := Config{SecretReferences: []SecretReference{{
		ID: "claude", Environment: "CLAUDE_AUTH_TOKEN",
		Kubernetes: &KubernetesSecretKeyReference{Name: "profile-a", Key: "token"},
	}}}
	same := base
	same.Env = map[string]string{"CLAUDE_AUTH_TOKEN": "rotated-value-must-not-hash"}
	if ProvisionFingerprint(base) != ProvisionFingerprint(same) {
		t.Fatal("resolved credential rotation changed provision fingerprint")
	}
	changed := base
	changed.SecretReferences = []SecretReference{{
		ID: "claude", Environment: "CLAUDE_AUTH_TOKEN",
		Kubernetes: &KubernetesSecretKeyReference{Name: "profile-b", Key: "token"},
	}}
	if ProvisionFingerprint(base) == ProvisionFingerprint(changed) {
		t.Fatal("secret reference identity did not change provision fingerprint")
	}
	optional := base
	optional.SecretReferences = append([]SecretReference(nil), base.SecretReferences...)
	optionalSource := *base.SecretReferences[0].Kubernetes
	optionalSource.Optional = true
	optional.SecretReferences[0].Kubernetes = &optionalSource
	if ProvisionFingerprint(base) == ProvisionFingerprint(optional) {
		t.Fatal("secret optionality did not change provision fingerprint")
	}
	if LaunchFingerprint(base) != LaunchFingerprint(changed) {
		t.Fatal("secret reference identity changed launch fingerprint; want provision-only")
	}
}

func TestCapsuleLaunchConfigValidationAndProvisionFingerprint(t *testing.T) {
	t.Parallel()
	key, err := NewCapsuleKey("cluster/namespace/city", "ga-session")
	if err != nil {
		t.Fatal(err)
	}
	capsule := &CapsuleLaunchConfig{
		Key:     key,
		State:   CapsuleStateReference{Key: key, Provider: "k8s", ResourceID: key.ResourceStem(), ResourceUID: "test-pvc-uid", MountPath: "/var/lib/gascity/omnigent"},
		Command: []string{"gc", "omnigent", "attach", "--mode", "capsule"},
		RunRoot: "/run/gascity/omnigent", SocketPath: "/run/gascity/omnigent/sidecar.sock",
		CatalogResourceID: "catalog", CatalogMountPath: "/etc/gascity/omnigent",
		CatalogSHA256: "sha256:" + strings.Repeat("a", 64),
		Network:       CapsuleNetworkExternalModel,
	}
	if err := capsule.Validate(); err != nil {
		t.Fatal(err)
	}
	base := Config{Capsule: capsule}
	changedCapsule := *capsule
	changedCapsule.CatalogSHA256 = "sha256:" + strings.Repeat("b", 64)
	changed := Config{Capsule: &changedCapsule}
	if CoreFingerprint(base) == CoreFingerprint(changed) || ProvisionFingerprint(base) == ProvisionFingerprint(changed) {
		t.Fatal("capsule catalog generation did not change provision identity")
	}
	if LaunchFingerprint(base) != LaunchFingerprint(changed) {
		t.Fatal("capsule catalog generation changed launch-only fingerprint")
	}

	overlap := *capsule
	overlap.RunRoot = overlap.State.MountPath + "/run"
	overlap.SocketPath = overlap.RunRoot + "/sidecar.sock"
	if err := overlap.Validate(); err == nil {
		t.Fatal("CapsuleLaunchConfig accepted nested writable roots")
	}
}

func TestFakeCapsuleStateLifecycleRetainsAcrossPlaceTeardownAndPurgesExplicitly(t *testing.T) {
	t.Parallel()
	fake := NewFake()
	stateRuntime := CapsuleStateRuntime(fake)
	key, err := NewCapsuleKey("test-scope", "ga-session")
	if err != nil {
		t.Fatal(err)
	}
	ref, created, err := stateRuntime.EnsureCapsuleState(context.Background(), key)
	if err != nil || !created {
		t.Fatalf("EnsureCapsuleState = %+v, %v, %v; want created", ref, created, err)
	}
	if _, created, err := stateRuntime.EnsureCapsuleState(context.Background(), key); err != nil || created {
		t.Fatalf("repeated ensure created=%v err=%v, want open existing", created, err)
	}
	if err := fake.Start(context.Background(), "place", Config{}); err != nil {
		t.Fatal(err)
	}
	if err := fake.AttachCapsuleState(context.Background(), "place", ref); err != nil {
		t.Fatalf("AttachCapsuleState: %v", err)
	}
	if err := fake.Stop("place"); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := stateRuntime.OpenCapsuleState(context.Background(), key); err != nil || !ok || got != ref {
		t.Fatalf("state after Place teardown = %+v,%v,%v; want retained %+v", got, ok, err, ref)
	}
	if err := fake.Start(context.Background(), "replacement", Config{}); err != nil {
		t.Fatal(err)
	}
	if err := fake.AttachCapsuleState(context.Background(), "replacement", ref); err != nil {
		t.Fatalf("replacement AttachCapsuleState: %v", err)
	}
	if err := fake.DetachCapsuleState(context.Background(), "replacement"); err != nil {
		t.Fatalf("replacement DetachCapsuleState: %v", err)
	}
	if err := stateRuntime.PurgeCapsuleState(context.Background(), key); err != nil {
		t.Fatalf("PurgeCapsuleState: %v", err)
	}
	if _, ok, err := stateRuntime.OpenCapsuleState(context.Background(), key); err != nil || ok {
		t.Fatalf("Open after purge ok=%v err=%v, want absent", ok, err)
	}
	if err := stateRuntime.PurgeCapsuleState(context.Background(), key); err != nil {
		t.Fatalf("repeated purge is not idempotent: %v", err)
	}
}

func TestFakeCapsuleStateRejectsForgedCrossBoundaryKey(t *testing.T) {
	t.Parallel()
	fake := NewFake()
	key, _ := NewCapsuleKey("city-a", "session-a")
	if _, _, err := fake.EnsureCapsuleState(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	forged := key
	forged.CityScope = "city-b"
	if _, _, err := fake.EnsureCapsuleState(context.Background(), forged); !errors.Is(err, ErrCapsuleStateConflict) {
		t.Fatalf("EnsureCapsuleState(forged) error = %v, want ErrCapsuleStateConflict", err)
	}
	if err := fake.PurgeCapsuleState(context.Background(), forged); !errors.Is(err, ErrCapsuleStateConflict) {
		t.Fatalf("PurgeCapsuleState(forged) error = %v, want ErrCapsuleStateConflict", err)
	}
	if _, ok, err := fake.OpenCapsuleState(context.Background(), key); err != nil || !ok {
		t.Fatalf("canonical allocation after forged purge ok=%v err=%v", ok, err)
	}
}

func TestFakeCapsuleStateInventoryIsDeterministicAndReportsProviderFailure(t *testing.T) {
	t.Parallel()
	fake := NewFake()
	for _, sessionID := range []string{"zeta", "alpha"} {
		key, err := NewCapsuleKey("city", sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := fake.EnsureCapsuleState(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	refs, err := fake.ListCapsuleStates(context.Background())
	if err != nil || len(refs) != 2 || refs[0].Key.Digest > refs[1].Key.Digest {
		t.Fatalf("ListCapsuleStates = %#v, %v", refs, err)
	}
	want := errors.New("provider unavailable")
	fake.CapsuleListError = want
	if _, err := fake.ListCapsuleStates(context.Background()); !errors.Is(err, want) {
		t.Fatalf("ListCapsuleStates error = %v, want %v", err, want)
	}
}

func TestFakeCapsuleStateConflictsAndFailuresFailClosed(t *testing.T) {
	t.Parallel()
	fake := NewFake()
	key, _ := NewCapsuleKey("test-scope", "ga-session")
	ref, _, err := fake.EnsureCapsuleState(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(context.Background(), "first", Config{}); err != nil {
		t.Fatal(err)
	}
	if err := fake.Start(context.Background(), "second", Config{}); err != nil {
		t.Fatal(err)
	}
	if err := fake.AttachCapsuleState(context.Background(), "first", ref); err != nil {
		t.Fatal(err)
	}
	if err := fake.AttachCapsuleState(context.Background(), "second", ref); !errors.Is(err, ErrCapsuleStateConflict) {
		t.Fatalf("second attach error = %v, want ErrCapsuleStateConflict", err)
	}
	if err := fake.PurgeCapsuleState(context.Background(), key); !errors.Is(err, ErrCapsuleStateConflict) {
		t.Fatalf("purge attached error = %v, want ErrCapsuleStateConflict", err)
	}
	fake.CapsuleStateErrors[key.Digest] = errors.New("provider unavailable")
	if _, _, err := fake.OpenCapsuleState(context.Background(), key); err == nil {
		t.Fatal("OpenCapsuleState ignored provider failure")
	}
	delete(fake.CapsuleStateErrors, key.Digest)
	fake.CapsuleDetachErrors["first"] = errors.New("detach failed")
	if err := fake.DetachCapsuleState(context.Background(), "first"); err == nil {
		t.Fatal("DetachCapsuleState ignored teardown failure")
	}
	if _, ok, err := fake.OpenCapsuleState(context.Background(), key); err != nil || !ok {
		t.Fatalf("detach failure lost retained state: ok=%v err=%v", ok, err)
	}
}
