package k8s

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestBuildPod_CapsuleLaunchUsesOneAgentContainerAndIsolatedMounts(t *testing.T) {
	t.Parallel()
	key, err := runtime.NewCapsuleKey("cluster/test-ns/city", "ga-session")
	if err != nil {
		t.Fatal(err)
	}
	command := []string{
		"gc", "omnigent", "attach", "--mode", "capsule", "--profile", "claude-compatible",
		"--socket", "/run/gascity/omnigent/sidecar.sock",
		"--state-root", "/var/lib/gascity/omnigent",
		"--catalog", "/etc/gascity/omnigent/profiles.yaml",
	}
	capsule := &runtime.CapsuleLaunchConfig{
		Key: key,
		State: runtime.CapsuleStateReference{
			Key: key, Provider: "k8s", ResourceID: key.ResourceStem(), ResourceUID: "test-pvc-uid", MountPath: "/var/lib/gascity/omnigent",
		},
		Command:           command,
		RunRoot:           "/run/gascity/omnigent",
		SocketPath:        "/run/gascity/omnigent/sidecar.sock",
		CatalogResourceID: "gco-catalog-a1b2c3",
		CatalogMountPath:  "/etc/gascity/omnigent",
		CatalogSHA256:     "sha256:" + strings.Repeat("a", 64),
		ExecutablePin: runtime.CapsuleExecutablePin{
			Executable: "omnigent", PackageVersion: "0.10.0.dev0",
			Commit: strings.Repeat("b", 40), SHA256: "sha256:" + strings.Repeat("c", 64),
		},
		Network: runtime.CapsuleNetworkExternalModel,
	}

	for _, prebaked := range []bool{false, true} {
		t.Run(map[bool]string{false: "staged image", true: "prebaked image"}[prebaked], func(t *testing.T) {
			p := newProviderWithOps(newFakeK8sOps())
			p.prebaked = prebaked
			pod, err := buildPod("test-session", runtime.Config{
				Command: "controller-command-must-not-run",
				WorkDir: "/city/rig",
				Env:     map[string]string{"GC_CITY": "/city", "GC_AGENT": "worker"},
				Capsule: capsule,
			}, p)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "agent" {
				t.Fatalf("containers = %#v, want one existing agent container", pod.Spec.Containers)
			}
			agent := pod.Spec.Containers[0]
			if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
				t.Fatal("capsule pod enabled service-account token automount")
			}
			assertRestrictedAgentContext(t, agent.SecurityContext)
			wantReadOnlyRoot := !prebaked
			if agent.SecurityContext.ReadOnlyRootFilesystem == nil || *agent.SecurityContext.ReadOnlyRootFilesystem != wantReadOnlyRoot {
				t.Fatalf("ReadOnlyRootFilesystem = %v, want %t", agent.SecurityContext.ReadOnlyRootFilesystem, wantReadOnlyRoot)
			}
			encodedCommand := base64.StdEncoding.EncodeToString([]byte(shellquote.Join(command)))
			if !strings.Contains(strings.Join(agent.Args, " "), encodedCommand) || strings.Contains(strings.Join(agent.Args, " "), "controller-command-must-not-run") {
				t.Fatalf("agent args do not contain exact capsule command: %q", agent.Args)
			}

			mounts := podVolumeMountsByName(agent.VolumeMounts)
			assertPodMount(t, mounts, "capsule-state", "/var/lib/gascity/omnigent", false)
			assertPodMount(t, mounts, "capsule-run", "/run/gascity/omnigent", false)
			assertPodMount(t, mounts, "capsule-catalog", "/etc/gascity/omnigent", true)
			volumes := podVolumesByName(pod.Spec.Volumes)
			if wantReadOnlyRoot && (volumes["capsule-tmp"].EmptyDir == nil || volumes["capsule-home"].EmptyDir == nil) {
				t.Fatalf("read-only capsule lacks writable tmp/home: %#v", volumes)
			}
			if prebaked && pod.Annotations["gascity.dev/security-exception"] != "prebaked-image-workspace" {
				t.Fatalf("prebaked security exception = %q", pod.Annotations["gascity.dev/security-exception"])
			}
			if volumes["capsule-state"].PersistentVolumeClaim == nil || volumes["capsule-state"].PersistentVolumeClaim.ClaimName != key.ResourceStem() {
				t.Fatalf("state volume = %#v", volumes["capsule-state"])
			}
			if volumes["capsule-run"].EmptyDir == nil {
				t.Fatalf("run volume = %#v, want place-local EmptyDir", volumes["capsule-run"])
			}
			if volumes["capsule-catalog"].ConfigMap == nil || volumes["capsule-catalog"].ConfigMap.Name != capsule.CatalogResourceID || volumes["capsule-catalog"].ConfigMap.Optional == nil || *volumes["capsule-catalog"].ConfigMap.Optional {
				t.Fatalf("catalog volume = %#v, want required ConfigMap", volumes["capsule-catalog"])
			}
			if pod.Annotations["gc-capsule-digest"] != key.Digest || pod.Annotations["gc-capsule-catalog-sha256"] != capsule.CatalogSHA256 || pod.Labels["gc-capsule"] != "true" {
				t.Fatalf("capsule metadata = labels=%v annotations=%v", pod.Labels, pod.Annotations)
			}
			if agent.ReadinessProbe == nil || agent.ReadinessProbe.Exec == nil || !strings.Contains(strings.Join(agent.ReadinessProbe.Exec.Command, " "), capsule.SocketPath) {
				t.Fatalf("readiness probe = %#v, want private socket gate", agent.ReadinessProbe)
			}
			if agent.LivenessProbe == nil || agent.LivenessProbe.Exec == nil || !strings.Contains(strings.Join(agent.LivenessProbe.Exec.Command, " "), "tmux has-session") {
				t.Fatalf("liveness probe = %#v, want outer tmux gate", agent.LivenessProbe)
			}
			if len(agent.Resources.Requests) == 0 || len(agent.Resources.Limits) == 0 {
				t.Fatalf("capsule lost configured resource bounds: %#v", agent.Resources)
			}
			if prebaked && len(pod.Spec.InitContainers) != 0 {
				t.Fatalf("prebaked capsule added staging init container: %#v", pod.Spec.InitContainers)
			}
			if !prebaked && len(pod.Spec.InitContainers) != 1 {
				t.Fatalf("staged capsule init containers = %#v, want existing stage container", pod.Spec.InitContainers)
			}
			encoded, err := json.Marshal(pod)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"controller-command-must-not-run"} {
				if strings.Contains(string(encoded), forbidden) {
					t.Fatalf("pod manifest leaked %q: %s", forbidden, encoded)
				}
			}
		})
	}
}

func TestBuildPod_CapsuleDynamicUserOwnsWritableMountsBeforeTmux(t *testing.T) {
	t.Parallel()
	capsule := testK8sCapsuleLaunch(t)
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", runtime.Config{
		Command: "/bin/bash", WorkDir: "/workspace",
		Env: map[string]string{"LINUX_USERNAME": "capsule-user"}, Capsule: capsule,
	}, p)
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := pod.Spec.Containers[0].Args[0]
	for _, required := range []string{capsule.State.MountPath, capsule.RunRoot, `chown -R "capsule-user"`, `su - capsule-user`} {
		if !strings.Contains(entrypoint, required) {
			t.Fatalf("dynamic-user entrypoint missing %q: %s", required, entrypoint)
		}
	}
	if strings.Index(entrypoint, "chown -R") > strings.Index(entrypoint, "tmux new-session") {
		t.Fatalf("writable capsule mounts are not owned before tmux start: %s", entrypoint)
	}
}

func TestBuildPod_RejectsInvalidCapsulePlanBeforeManifest(t *testing.T) {
	t.Parallel()
	valid := testK8sCapsuleLaunch(t)
	cases := map[string]func(*runtime.CapsuleLaunchConfig){
		"forged key":              func(c *runtime.CapsuleLaunchConfig) { c.Key.Digest = strings.Repeat("f", 64) },
		"state key mismatch":      func(c *runtime.CapsuleLaunchConfig) { c.State.Key.SessionID = "other" },
		"wrong state provider":    func(c *runtime.CapsuleLaunchConfig) { c.State.Provider = "ssh" },
		"missing state claim":     func(c *runtime.CapsuleLaunchConfig) { c.State.ResourceID = "" },
		"relative state mount":    func(c *runtime.CapsuleLaunchConfig) { c.State.MountPath = "state" },
		"socket outside run root": func(c *runtime.CapsuleLaunchConfig) { c.SocketPath = "/tmp/service.sock" },
		"missing command":         func(c *runtime.CapsuleLaunchConfig) { c.Command = nil },
		"catalog digest mismatch": func(c *runtime.CapsuleLaunchConfig) { c.CatalogSHA256 = "sha256:bad" },
		"missing network policy":  func(c *runtime.CapsuleLaunchConfig) { c.Network = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			capsule := *valid
			capsule.Command = append([]string(nil), valid.Command...)
			mutate(&capsule)
			_, err := buildPod("test-session", runtime.Config{Command: "/bin/bash", Capsule: &capsule}, newProviderWithOps(newFakeK8sOps()))
			if err == nil {
				t.Fatal("buildPod succeeded, want capsule validation error")
			}
		})
	}
}

func testK8sCapsuleLaunch(t *testing.T) *runtime.CapsuleLaunchConfig {
	t.Helper()
	key, err := runtime.NewCapsuleKey("cluster/test-ns/city", "ga-session")
	if err != nil {
		t.Fatal(err)
	}
	return &runtime.CapsuleLaunchConfig{
		Key:     key,
		State:   runtime.CapsuleStateReference{Key: key, Provider: "k8s", ResourceID: key.ResourceStem(), ResourceUID: "test-pvc-uid", MountPath: "/var/lib/gascity/omnigent"},
		Command: []string{"gc", "omnigent", "attach", "--mode", "capsule"},
		RunRoot: "/run/gascity/omnigent", SocketPath: "/run/gascity/omnigent/sidecar.sock",
		CatalogResourceID: "gco-catalog-a1b2c3", CatalogMountPath: "/etc/gascity/omnigent",
		CatalogSHA256: "sha256:" + strings.Repeat("a", 64),
		ExecutablePin: runtime.CapsuleExecutablePin{
			Executable: "omnigent", PackageVersion: "0.10.0.dev0",
			Commit: strings.Repeat("b", 40), SHA256: "sha256:" + strings.Repeat("c", 64),
		},
		Network: runtime.CapsuleNetworkExternalModel,
	}
}

func podVolumeMountsByName(mounts []corev1.VolumeMount) map[string]corev1.VolumeMount {
	out := make(map[string]corev1.VolumeMount, len(mounts))
	for _, mount := range mounts {
		out[mount.Name] = mount
	}
	return out
}

func podVolumesByName(volumes []corev1.Volume) map[string]corev1.VolumeSource {
	out := make(map[string]corev1.VolumeSource, len(volumes))
	for _, volume := range volumes {
		out[volume.Name] = volume.VolumeSource
	}
	return out
}

func assertPodMount(t *testing.T, mounts map[string]corev1.VolumeMount, name, path string, readOnly bool) {
	t.Helper()
	mount, ok := mounts[name]
	if !ok || mount.MountPath != path || mount.ReadOnly != readOnly {
		t.Fatalf("mount %q = %#v, want path=%q readOnly=%t", name, mount, path, readOnly)
	}
}

func TestBuildPod_NodeSelector(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.nodeSelector = map[string]string{"workload": "gc-agents"}
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.NodeSelector["workload"] != "gc-agents" {
		t.Errorf("NodeSelector[workload] = %q, want \"gc-agents\"", pod.Spec.NodeSelector["workload"])
	}
}

func TestBuildPod_Tolerations(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.tolerations = []corev1.Toleration{{
		Key: "gc-agents", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
	}}
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if len(pod.Spec.Tolerations) != 1 {
		t.Fatalf("len(Tolerations) = %d, want 1", len(pod.Spec.Tolerations))
	}
	if pod.Spec.Tolerations[0].Key != "gc-agents" {
		t.Errorf("Toleration.Key = %q, want \"gc-agents\"", pod.Spec.Tolerations[0].Key)
	}
}

func TestBuildPod_Affinity(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "node-type", Operator: corev1.NodeSelectorOpIn, Values: []string{"gpu"},
					}},
				}},
			},
		},
	}
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.Affinity == nil {
		t.Fatal("Affinity is nil")
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		t.Fatal("NodeAffinity is nil")
	}
	expressions := pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
	if expressions[0].Values[0] != "gpu" {
		t.Fatalf("affinity value = %q, want gpu", expressions[0].Values[0])
	}
}

func TestBuildPod_PriorityClassName(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.priorityClassName = "gc-agent-high"
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.PriorityClassName != "gc-agent-high" {
		t.Errorf("PriorityClassName = %q, want \"gc-agent-high\"", pod.Spec.PriorityClassName)
	}
}

func TestBuildPod_NoSchedulingFields_NoBehaviorChange(t *testing.T) {
	// Zero-value scheduling fields must not alter default pod behavior.
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if pod.Spec.NodeSelector != nil {
		t.Errorf("NodeSelector should be nil when not set")
	}
	if len(pod.Spec.Tolerations) != 0 {
		t.Errorf("Tolerations should be empty when not set")
	}
	if pod.Spec.Affinity != nil {
		t.Errorf("Affinity should be nil when not set")
	}
	if pod.Spec.PriorityClassName != "" {
		t.Errorf("PriorityClassName should be empty when not set")
	}
	if pod.Labels["gc-capsule"] != "" || pod.Annotations["gc-capsule-digest"] != "" {
		t.Fatalf("ordinary pod gained capsule metadata: labels=%v annotations=%v", pod.Labels, pod.Annotations)
	}
	if pod.Spec.Containers[0].ReadinessProbe != nil || pod.Spec.Containers[0].LivenessProbe != nil {
		t.Fatalf("ordinary pod gained capsule probes: %#v", pod.Spec.Containers[0])
	}
	for _, volume := range pod.Spec.Volumes {
		if strings.HasPrefix(volume.Name, "capsule-") {
			t.Fatalf("ordinary pod gained capsule volume: %#v", volume)
		}
	}
}

func TestBuildPod_DefaultSecurityBoundary(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("worker pods must disable service-account token automount")
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("len(Containers) = %d, want 1", len(pod.Spec.Containers))
	}
	assertRestrictedAgentContext(t, pod.Spec.Containers[0].SecurityContext)
}

func TestBuildPod_CredentialsUseNamespaceLocalSecrets(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	pod, err := buildPod(
		"test-session",
		runtime.Config{
			Command: "/bin/bash",
			Env: map[string]string{
				"GITHUB_TOKEN":           "must-not-leak",
				"LITELLM_MASTER_KEY":     "must-not-leak",
				"GC_K8S_DOLT_SECRET":     "dolt-credentials",
				"GC_DOLT_USER":           "must-not-leak",
				"GC_DOLT_PASSWORD":       "must-not-leak",
				"BEADS_DOLT_SERVER_USER": "must-not-leak",
				"BEADS_DOLT_PASSWORD":    "must-not-leak",
			},
		},
		p,
	)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	env := make(map[string]corev1.EnvVar)
	counts := make(map[string]int)
	for _, entry := range pod.Spec.Containers[0].Env {
		env[entry.Name] = entry
		counts[entry.Name]++
	}
	for name, want := range map[string]struct {
		secret   string
		key      string
		optional bool
	}{
		"GITHUB_TOKEN":           {secret: "git-credentials", key: "token", optional: true},
		"LITELLM_MASTER_KEY":     {secret: "litellm-credentials", key: "credential", optional: true},
		"GC_DOLT_USER":           {secret: "dolt-credentials", key: "username"},
		"GC_DOLT_PASSWORD":       {secret: "dolt-credentials", key: "password"},
		"BEADS_DOLT_SERVER_USER": {secret: "dolt-credentials", key: "username"},
		"BEADS_DOLT_PASSWORD":    {secret: "dolt-credentials", key: "password"},
	} {
		if counts[name] != 1 {
			t.Fatalf("%s appears %d times in Pod env, want exactly once", name, counts[name])
		}
		entry := env[name]
		if entry.Value != "" {
			t.Fatalf("%s leaked a literal value into the Pod spec", name)
		}
		if entry.ValueFrom == nil || entry.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s does not use a SecretKeyRef", name)
		}
		ref := entry.ValueFrom.SecretKeyRef
		if ref.Name != want.secret || ref.Key != want.key {
			t.Fatalf(
				"%s SecretKeyRef = %s/%s, want %s/%s",
				name,
				ref.Name,
				ref.Key,
				want.secret,
				want.key,
			)
		}
		if ref.Optional == nil || *ref.Optional != want.optional {
			t.Fatalf("%s SecretKeyRef optional = %v, want %t", name, ref.Optional, want.optional)
		}
	}
}

func TestBuildPod_RejectsLiteralDoltCredentialsWithoutSecretProjection(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	_, err := buildPod(
		"test-session",
		runtime.Config{
			Command: "/bin/bash",
			Env: map[string]string{
				"GC_DOLT_USER":     "must-not-leak",
				"GC_DOLT_PASSWORD": "must-not-leak",
			},
		},
		p,
	)
	if err == nil {
		t.Fatal("buildPod error = nil, want literal Dolt credential rejection")
	}
	if got := err.Error(); !strings.Contains(got, "GC_K8S_DOLT_SECRET") {
		t.Fatalf("buildPod error = %q, want GC_K8S_DOLT_SECRET guidance", got)
	}
}

func TestBuildPod_InitContainerUsesDefaultSecurityBoundary(t *testing.T) {
	p := newProviderWithOps(newFakeK8sOps())
	p.prebaked = false
	pod, err := buildPod(
		"test-session",
		runtime.Config{
			Command: "/bin/bash",
			Env:     map[string]string{"GC_CITY": "/city"},
			WorkDir: "/workspace",
		},
		p,
	)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("len(InitContainers) = %d, want 1", len(pod.Spec.InitContainers))
	}
	assertRestrictedAgentContext(t, pod.Spec.InitContainers[0].SecurityContext)
}

func assertRestrictedAgentContext(t *testing.T, security *corev1.SecurityContext) {
	t.Helper()
	if security == nil {
		t.Fatal("SecurityContext is nil")
	}
	if security.RunAsNonRoot == nil || !*security.RunAsNonRoot {
		t.Fatal("RunAsNonRoot must be true")
	}
	if security.RunAsUser == nil || *security.RunAsUser != 1001 {
		t.Fatalf("RunAsUser = %v, want 1001", security.RunAsUser)
	}
	if security.RunAsGroup == nil || *security.RunAsGroup != 1001 {
		t.Fatalf("RunAsGroup = %v, want 1001", security.RunAsGroup)
	}
	if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
		t.Fatal("AllowPrivilegeEscalation must be false")
	}
	if security.Capabilities == nil ||
		len(security.Capabilities.Drop) != 1 ||
		security.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("Capabilities.Drop = %v, want [ALL]", security.Capabilities)
	}
	if security.SeccompProfile == nil ||
		security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("SeccompProfile = %v, want RuntimeDefault", security.SeccompProfile)
	}
}

func TestBuildPod_ClonesSchedulingFields(t *testing.T) {
	seconds := int64(30)
	p := newProviderWithOps(newFakeK8sOps())
	p.nodeSelector = map[string]string{"workload": "gc-agents"}
	p.tolerations = []corev1.Toleration{{
		Key:               "gc-agents",
		Operator:          corev1.TolerationOpExists,
		Effect:            corev1.TaintEffectNoSchedule,
		TolerationSeconds: &seconds,
	}}
	p.affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "node-type", Operator: corev1.NodeSelectorOpIn, Values: []string{"gpu"},
					}},
				}},
			},
		},
	}

	pod, err := buildPod("test-session", runtime.Config{Command: "/bin/bash"}, p)
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}

	pod.Spec.NodeSelector["workload"] = "changed"
	pod.Spec.Tolerations[0].Key = "changed"
	pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0] = "changed"

	if p.nodeSelector["workload"] != "gc-agents" {
		t.Fatalf("provider nodeSelector mutated to %q", p.nodeSelector["workload"])
	}
	if p.tolerations[0].Key != "gc-agents" {
		t.Fatalf("provider toleration key mutated to %q", p.tolerations[0].Key)
	}
	values := p.affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values
	if values[0] != "gpu" {
		t.Fatalf("provider affinity value mutated to %q", values[0])
	}
}
