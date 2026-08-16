package k8s

import (
	"encoding/base64"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	podManagedDoltHost = "dolt.gc.svc.cluster.local"
	podManagedDoltPort = "3307"
)

func controllerCityPath(cfgEnv map[string]string) string {
	ctrlCity := strings.TrimSpace(cfgEnv["GC_CITY"])
	if ctrlCity == "" {
		ctrlCity = strings.TrimSpace(cfgEnv["GC_CITY_PATH"])
	}
	if ctrlCity == "" {
		ctrlCity = strings.TrimSpace(cfgEnv["GC_CITY_ROOT"])
	}
	return ctrlCity
}

func remapControllerPathToPod(val, ctrlCity string) string {
	val = strings.TrimSpace(val)
	ctrlCity = strings.TrimSpace(ctrlCity)
	if val == "" || ctrlCity == "" {
		return val
	}
	if val == ctrlCity || strings.HasPrefix(val, ctrlCity+"/") {
		return "/workspace" + val[len(ctrlCity):]
	}
	return val
}

func remapControllerOrWorkDirPathToPod(val, ctrlCity, ctrlWorkDir, podWorkDir string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return val
	}
	if remapped := remapControllerPathToPod(val, ctrlCity); remapped != val {
		return remapped
	}
	ctrlWorkDir = strings.TrimSpace(ctrlWorkDir)
	podWorkDir = strings.TrimSpace(podWorkDir)
	if ctrlWorkDir != "" && podWorkDir != "" &&
		(val == ctrlWorkDir || strings.HasPrefix(val, ctrlWorkDir+"/")) {
		return podWorkDir + val[len(ctrlWorkDir):]
	}
	return val
}

func projectedPodWorkDir(cfg runtime.Config) string {
	return projectedPodWorkDirForController(cfg, controllerCityPath(cfg.Env))
}

func projectedPodWorkDirForController(cfg runtime.Config, ctrlCity string) string {
	podWorkDir := "/workspace"
	if ctrlCity != "" && cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		if rel, ok := strings.CutPrefix(cfg.WorkDir, ctrlCity+"/"); ok {
			podWorkDir = "/workspace/" + rel
		} else {
			// External rigs are siblings of the city in common deployments
			// (for example /data/city and /data/rigs/repo). Keep the rig in a
			// distinct pod directory so initCityInPod can materialize city
			// state at /workspace without overwriting the rig's .beads identity.
			podWorkDir = "/workspace/rig"
		}
	}
	return podWorkDir
}

func remapControllerCommandToPod(cmd string, cfg runtime.Config) string {
	ctrlCity := controllerCityPath(cfg.Env)
	if ctrlCity != "" {
		cmd = strings.ReplaceAll(cmd, ctrlCity, "/workspace")
	}
	if cfg.WorkDir != "" {
		cmd = strings.ReplaceAll(cmd, cfg.WorkDir, projectedPodWorkDir(cfg))
	}
	return cmd
}

// agentCommandB64 resolves the agent command, remaps controller-side city path
// references to the pod-side /workspace, and returns its base64 form. Shared by
// buildPod (the pod entrypoint) and Relaunch (respawn over execInPod) so the
// entrypoint launch and a relaunch produce a byte-identical command.
func agentCommandB64(cfg runtime.Config) string {
	cmd := cfg.Command
	if cfg.Capsule != nil {
		cmd = shellquote.Join(cfg.Capsule.Command)
	}
	if cmd == "" {
		cmd = "/bin/bash"
	}
	// The controller expands {{.ConfigDir}} templates using its own city path
	// (e.g. /city/packs/...) but pods have files at /workspace/....
	cmd = remapControllerCommandToPod(cmd, cfg)
	return base64.StdEncoding.EncodeToString([]byte(cmd))
}

// buildRespawnCommand builds the in-pod shell command that respawns the agent in
// the existing tmux "main" session (respawn-pane -k), reusing the warm pod. When
// LINUX_USERNAME is set the entrypoint runs tmux under `su - <user>`, so the
// respawn is wrapped in the same su to reach that user's tmux socket.
func buildRespawnCommand(cfg runtime.Config) string {
	cmdB64 := agentCommandB64(cfg)
	if user := cfg.Env["LINUX_USERNAME"]; user != "" {
		suMode := "-"
		if hasK8sSecretEnvironment(cfg.SecretReferences) {
			suMode = "-m"
		}
		return fmt.Sprintf(
			`CMD=$(echo '%s' | base64 -d) && su %s %s -c "cd %s && tmux respawn-pane -k -t %s \"$CMD\""`,
			cmdB64, suMode, user, projectedPodWorkDir(cfg), tmuxSession,
		)
	}
	return fmt.Sprintf(
		`CMD=$(echo '%s' | base64 -d) && tmux respawn-pane -k -t %s "$CMD"`,
		cmdB64, tmuxSession,
	)
}

func projectedPodStoreRoot(cfg runtime.Config, podWorkDir string) string {
	storeRoot := strings.TrimSpace(cfg.Env["GC_STORE_ROOT"])
	if storeRoot == "" {
		storeRoot = strings.TrimSpace(cfg.WorkDir)
	}
	if storeRoot == "" {
		storeRoot = controllerCityPath(cfg.Env)
	}
	storeRoot = remapControllerOrWorkDirPathToPod(
		storeRoot,
		controllerCityPath(cfg.Env),
		cfg.WorkDir,
		podWorkDir,
	)
	if storeRoot == "" {
		return podWorkDir
	}
	return storeRoot
}

func projectedPodRuntimeDir(cfgEnv map[string]string, ctrlCity string) string {
	podCity := "/workspace"
	runtimeDir := strings.TrimSpace(cfgEnv["GC_CITY_RUNTIME_DIR"])
	if runtimeDir == "" {
		return citylayout.RuntimeDataDir(podCity)
	}
	remapped := remapControllerPathToPod(runtimeDir, ctrlCity)
	if remapped != runtimeDir {
		return remapped
	}
	return citylayout.RuntimeDataDir(podCity)
}

func projectControllerRuntimePathToPod(path, ctrlCity, ctrlRuntimeDir, podRuntimeDir string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	if remapped := remapControllerPathToPod(path, ctrlCity); remapped != path {
		return remapped
	}
	if ctrlRuntimeDir != "" && pathutil.PathWithin(ctrlRuntimeDir, path) {
		normalizedRoot := pathutil.NormalizePathForCompare(ctrlRuntimeDir)
		normalizedPath := pathutil.NormalizePathForCompare(path)
		rel, err := filepath.Rel(normalizedRoot, normalizedPath)
		if err == nil {
			if rel == "." {
				return podRuntimeDir
			}
			return filepath.Join(podRuntimeDir, rel)
		}
	}
	return path
}

// projectedPodDoltEnv adapts the controller projection to a pod-visible Dolt
// target. Managed-local controller projections intentionally omit GC_DOLT_HOST
// and use a host-local runtime port; pods translate that blank-host managed
// shape to the provider-configured in-cluster alias at this adapter edge so
// agents still consume one GC_DOLT_* connection contract. Explicit
// GC_DOLT_HOST values are preserved as written.
// BEADS_DOLT_SERVER_HOST/PORT are compatibility mirrors derived from the GC
// projection, not independent input authorities.
func controllerLocalDoltHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	switch host {
	case "", "127.0.0.1", "localhost", "0.0.0.0", "::1", "::":
		return true
	default:
		return false
	}
}

func projectedPodDoltEnv(cfgEnv map[string]string, managedHost, managedPort string) (map[string]string, error) {
	host := strings.TrimSpace(cfgEnv["GC_DOLT_HOST"])
	port := strings.TrimSpace(cfgEnv["GC_DOLT_PORT"])
	managedHost = strings.TrimSpace(managedHost)
	managedPort = strings.TrimSpace(managedPort)
	if managedHost == "" {
		managedHost = podManagedDoltHost
	}
	if managedPort == "" {
		managedPort = podManagedDoltPort
	}

	switch {
	case host == "" && port == "":
		return map[string]string{}, nil
	case host != "" && port == "":
		return nil, fmt.Errorf("requires both GC_DOLT_HOST and GC_DOLT_PORT when GC_DOLT_HOST is set")
	case controllerLocalDoltHost(host):
		host = managedHost
		port = managedPort
	}

	projected := map[string]string{
		"GC_DOLT_HOST":           host,
		"GC_DOLT_PORT":           port,
		"BEADS_DOLT_SERVER_HOST": host,
		"BEADS_DOLT_SERVER_PORT": port,
	}
	return projected, nil
}

// buildPod creates a pod manifest compatible with gc-session-k8s.
// Same labels, annotations, container names, volumes, and tmux-inside-pod
// pattern so mixed-mode migration works.
func buildPod(name string, cfg runtime.Config, p *Provider) (*corev1.Pod, error) {
	if err := validateK8sCapsuleLaunch(cfg); err != nil {
		return nil, err
	}
	secretProjection, err := projectK8sSecretReferences(cfg)
	if err != nil {
		return nil, fmt.Errorf("projecting Kubernetes secret references: %w", err)
	}
	podName := SanitizeName(name)
	label := SanitizeLabel(name)
	agentName := cfg.Env["GC_ALIAS"]
	if agentName == "" {
		agentName = cfg.Env["GC_AGENT"]
	}
	if agentName == "" {
		agentName = "unknown"
	}
	agentLabel := SanitizeLabel(agentName)

	// Resolve pod-side working directory.
	// Controller resolves dirs relative to its city path; pods use /workspace.
	podWorkDir := projectedPodWorkDir(cfg)
	ctrlCity := controllerCityPath(cfg.Env)

	// Build the agent command (base64-encoded to avoid quoting issues) — shared
	// with the relaunch path so the entrypoint and a respawn launch identically.
	cmdB64 := agentCommandB64(cfg)

	// Pod entrypoint: wait for workspace ready → pre_start → tmux → keepalive.
	// Each pre_start command is base64-encoded and decoded at runtime to prevent
	// shell metacharacter injection from user-supplied commands.
	var preStartCmds string
	for _, cmd := range cfg.PreStart {
		c := remapControllerCommandToPod(cmd, cfg)
		b64 := base64.StdEncoding.EncodeToString([]byte(c))
		preStartCmds += fmt.Sprintf("echo '%s' | base64 -d | sh; ", b64)
	}

	// Dynamic user creation: when LINUX_USERNAME is set, the container starts
	// as root (see securityContext below), creates the user, sets up workspace
	// ownership, then drops privileges via su for the tmux session.
	linuxUsername := cfg.Env["LINUX_USERNAME"]
	var userSetup string
	if linuxUsername != "" {
		userSetup = fmt.Sprintf(
			`id "%s" >/dev/null 2>&1 || useradd -m -s /bin/bash "%s"; `+
				`echo "%s ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/"%s" && chmod 0440 /etc/sudoers.d/"%s"; `+
				`mkdir -p "%s" && chown -R "%s" "%s"; `+
				`export HOME="/home/%s"; `,
			linuxUsername, linuxUsername,
			linuxUsername, linuxUsername, linuxUsername,
			podWorkDir, linuxUsername, podWorkDir,
			linuxUsername,
		)
		if cfg.Capsule != nil {
			for _, writablePath := range []string{cfg.Capsule.State.MountPath, cfg.Capsule.RunRoot} {
				userSetup += fmt.Sprintf(`mkdir -p %q && chown -R %q %q; `, writablePath, linuxUsername, writablePath)
			}
		}
		if secretProjection.hasFileMount {
			userSetup += fmt.Sprintf(`usermod -a -G %d %q; `, agentUserGroupID, linuxUsername)
		}
	}
	credCopy := `git config --global --add safe.directory '*' 2>/dev/null; `
	if cfg.Capsule == nil {
		credCopy = `mkdir -p $HOME/.claude && cp -rL /tmp/claude-secret/. $HOME/.claude/ 2>/dev/null; ` + credCopy
	}
	wsWait := ""
	if !p.prebaked {
		wsWait = `while [ ! -f /workspace/.gc-workspace-ready ]; do sleep 0.5; done; `
	}
	var tmuxCmd string
	if linuxUsername != "" {
		suMode := "-"
		if len(secretProjection.environment) != 0 {
			suMode = "-m"
		}
		// Run tmux session as the dynamic user via su.
		tmuxCmd = fmt.Sprintf(
			"%s%s%s%sCMD=$(echo '%s' | base64 -d) && "+
				`su %s %s -c "cd %s && tmux new-session -d -s %s \"$CMD\" && sleep infinity"`,
			userSetup, credCopy, wsWait, preStartCmds, cmdB64,
			suMode, linuxUsername, podWorkDir, tmuxSession,
		)
	} else {
		tmuxCmd = fmt.Sprintf(
			"%s%s%sCMD=$(echo '%s' | base64 -d) && tmux new-session -d -s %s \"$CMD\" && sleep infinity",
			credCopy, wsWait, preStartCmds, cmdB64, tmuxSession,
		)
	}

	// Build environment, remapping K8s-specific vars.
	env, err := buildPodEnv(cfg.Env, cfg.WorkDir, podWorkDir, p.managedServiceHost, p.managedServicePort)
	if err != nil {
		return nil, err
	}
	for _, projected := range secretProjection.environment {
		env = removePodEnv(env, projected.Name)
		env = append(env, projected)
	}

	// Build volume mounts for the main container.
	// When prebaked, skip the ws EmptyDir — it would shadow baked image content.
	var mainVolMounts []corev1.VolumeMount
	var volumes []corev1.Volume

	if !p.prebaked {
		mainVolMounts = append(mainVolMounts, corev1.VolumeMount{
			Name: "ws", MountPath: "/workspace",
		})
		volumes = append(volumes, corev1.Volume{
			Name: "ws", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	if cfg.Capsule != nil {
		capsule := cfg.Capsule
		mainVolMounts = append(mainVolMounts,
			corev1.VolumeMount{Name: "capsule-state", MountPath: capsule.State.MountPath},
			corev1.VolumeMount{Name: "capsule-run", MountPath: capsule.RunRoot},
			corev1.VolumeMount{Name: "capsule-catalog", MountPath: capsule.CatalogMountPath, ReadOnly: true},
		)
		catalogMode := int32(0o444)
		volumes = append(volumes,
			corev1.Volume{Name: "capsule-state", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: capsule.State.ResourceID},
			}},
			corev1.Volume{Name: "capsule-run", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
			}},
			corev1.Volume{Name: "capsule-catalog", VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: capsule.CatalogResourceID},
					DefaultMode:          &catalogMode,
					Optional:             boolPtr(false),
				},
			}},
		)
	}
	mainVolMounts = append(mainVolMounts, secretProjection.mounts...)
	volumes = append(volumes, secretProjection.volumes...)

	if cfg.Capsule == nil {
		mainVolMounts = append(mainVolMounts, corev1.VolumeMount{
			Name: "claude-config", MountPath: "/tmp/claude-secret", ReadOnly: true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "claude-config", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "claude-credentials",
					Optional:   boolPtr(true),
				},
			},
		})
	}

	// If GC_CITY differs from work_dir, add a city volume (not needed when prebaked).
	if !p.prebaked && ctrlCity != "" && ctrlCity != cfg.WorkDir {
		mainVolMounts = append(mainVolMounts, corev1.VolumeMount{
			Name: "city", MountPath: ctrlCity,
		})
		volumes = append(volumes, corev1.Volume{
			Name:         "city",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	// Resources.
	resources, err := buildResources(p)
	if err != nil {
		return nil, err
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: p.namespace,
			Labels: map[string]string{
				"app":        "gc-agent",
				"gc-session": label,
				"gc-agent":   agentLabel,
			},
			Annotations: map[string]string{
				"gc-session-name": name,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           p.serviceAccount,
			AutomountServiceAccountToken: boolPtr(false),
			RestartPolicy:                corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "agent",
				Image:           p.image,
				ImagePullPolicy: corev1.PullAlways,
				WorkingDir:      podWorkDir,
				Command:         []string{"/bin/sh", "-c"},
				Args:            []string{tmuxCmd},
				Env:             env,
				Stdin:           true,
				TTY:             true,
				Resources:       resources,
				VolumeMounts:    mainVolMounts,
				SecurityContext: agentSecurityContext(linuxUsername),
			}},
			Volumes: volumes,
		},
	}
	if secretProjection.hasFileMount {
		group := agentUserGroupID
		policy := corev1.FSGroupChangeOnRootMismatch
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: &group, FSGroupChangePolicy: &policy}
	}
	if cfg.Capsule != nil {
		capsule := cfg.Capsule
		pod.Labels["gc-capsule"] = "true"
		pod.Annotations["gc-capsule-digest"] = capsule.Key.Digest
		pod.Annotations["gc-capsule-catalog-sha256"] = capsule.CatalogSHA256
		agent := &pod.Spec.Containers[0]
		agent.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
				"sh", "-c", fmt.Sprintf("tmux has-session -t %s && test -S %s", shellquote.Quote(tmuxSession), shellquote.Quote(capsule.SocketPath)),
			}}},
			InitialDelaySeconds: 1, PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 15,
		}
		agent.LivenessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
				"tmux", "has-session", "-t", tmuxSession,
			}}},
			InitialDelaySeconds: 5, PeriodSeconds: 10, TimeoutSeconds: 2, FailureThreshold: 3,
		}
	}

	// Apply optional scheduling fields.
	pod.Spec.NodeSelector = maps.Clone(p.nodeSelector)
	pod.Spec.Tolerations = cloneTolerations(p.tolerations)
	if p.affinity != nil {
		pod.Spec.Affinity = p.affinity.DeepCopy()
	}
	pod.Spec.PriorityClassName = p.priorityClassName

	// Add init container when staging is needed (skip when prebaked).
	if !p.prebaked && needsStaging(cfg, ctrlCity) {
		initVolMounts := []corev1.VolumeMount{
			{Name: "ws", MountPath: "/workspace"},
		}
		if ctrlCity != "" && ctrlCity != cfg.WorkDir {
			initVolMounts = append(initVolMounts, corev1.VolumeMount{
				Name: "city", MountPath: "/city-stage",
			})
		}
		pod.Spec.InitContainers = []corev1.Container{{
			Name:            "stage",
			Image:           p.image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c", "while [ ! -f /workspace/.gc-ready ]; do sleep 0.5; done"},
			VolumeMounts:    initVolMounts,
			SecurityContext: agentSecurityContext(""),
		}}
	}

	return pod, nil
}

func validateK8sCapsuleLaunch(cfg runtime.Config) error {
	if cfg.Capsule == nil {
		return nil
	}
	capsule := cfg.Capsule
	if err := capsule.Validate(); err != nil {
		return fmt.Errorf("invalid capsule launch: %w", err)
	}
	if capsule.State.Provider != string(runtime.SecretProviderKubernetes) {
		return fmt.Errorf("invalid capsule launch: state provider must be %q", runtime.SecretProviderKubernetes)
	}
	for kind, value := range map[string]string{
		"state claim":      capsule.State.ResourceID,
		"catalog resource": capsule.CatalogResourceID,
	} {
		if problems := k8svalidation.IsDNS1123Subdomain(value); len(problems) > 0 {
			return fmt.Errorf("invalid capsule launch: %s is not a Kubernetes DNS name", kind)
		}
	}
	podWorkDir := projectedPodWorkDir(cfg)
	for kind, value := range map[string]string{
		"state mount":   capsule.State.MountPath,
		"run root":      capsule.RunRoot,
		"catalog mount": capsule.CatalogMountPath,
	} {
		if pathsOverlap(value, podWorkDir) {
			return fmt.Errorf("invalid capsule launch: %s must be separate from workspace", kind)
		}
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	return left == right || strings.HasPrefix(left, right+string(filepath.Separator)) || strings.HasPrefix(right, left+string(filepath.Separator))
}

func cloneTolerations(in []corev1.Toleration) []corev1.Toleration {
	if len(in) == 0 {
		return nil
	}
	out := append([]corev1.Toleration(nil), in...)
	for i := range out {
		if in[i].TolerationSeconds != nil {
			seconds := *in[i].TolerationSeconds
			out[i].TolerationSeconds = &seconds
		}
	}
	return out
}

// agentSecurityContext returns a container security context.
// When a dynamic linux username is configured, the container starts as root
// (UID 0) so it can create the user at runtime before dropping privileges.
// Otherwise it locks the official agent image to its numeric gcagent UID/GID
// and the Kubernetes Restricted Pod Security profile.
func agentSecurityContext(linuxUsername string) *corev1.SecurityContext {
	if linuxUsername != "" {
		var rootUID int64
		return &corev1.SecurityContext{
			RunAsUser: &rootUID,
		}
	}

	agentUID := agentUserGroupID
	agentGID := agentUserGroupID
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	return &corev1.SecurityContext{
		RunAsNonRoot:             &runAsNonRoot,
		RunAsUser:                &agentUID,
		RunAsGroup:               &agentGID,
		AllowPrivilegeEscalation: &allowPrivilegeEscalation,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// buildPodEnv creates the env var list for the agent container.
// Removes controller-only vars, strips deprecated K8s compatibility inputs,
// and remaps pod-visible ones.
func buildPodEnv(cfgEnv map[string]string, ctrlWorkDir, podWorkDir, managedServiceHost, managedServicePort string) ([]corev1.EnvVar, error) {
	// Start with cfg.Env, removing controller-only vars.
	skip := map[string]bool{
		"GC_BEADS":               true,
		"GC_SESSION":             true,
		"GC_EVENTS":              true,
		"GC_K8S_DOLT_HOST":       true,
		"GC_K8S_DOLT_PORT":       true,
		"GC_K8S_DOLT_SECRET":     true,
		"GC_DOLT_HOST":           true,
		"GC_DOLT_PORT":           true,
		"GC_DOLT_USER":           true,
		"GC_DOLT_PASSWORD":       true,
		"BEADS_DOLT_SERVER_HOST": true,
		"BEADS_DOLT_SERVER_PORT": true,
		"BEADS_DOLT_SERVER_USER": true,
		"BEADS_DOLT_PASSWORD":    true,
		// These credentials are projected below from namespace-local Secrets.
		// Never copy controller literals into the Pod spec.
		"GITHUB_TOKEN":       true,
		"LITELLM_MASTER_KEY": true,
	}

	ctrlCity := controllerCityPath(cfgEnv)
	ctrlRuntimeDir := strings.TrimSpace(cfgEnv["GC_CITY_RUNTIME_DIR"])
	podRuntimeDir := projectedPodRuntimeDir(cfgEnv, ctrlCity)

	var env []corev1.EnvVar
	for k, v := range cfgEnv {
		if skip[k] {
			continue
		}
		val := v
		// Remap city/workdir vars to pod-visible paths.
		switch k {
		case "GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT":
			val = "/workspace"
		case "GC_DIR":
			val = podWorkDir
		case "GC_CITY_RUNTIME_DIR":
			val = podRuntimeDir
		case "GC_CONTROL_DISPATCHER_TRACE_DEFAULT", "GC_PACK_STATE_DIR":
			val = projectControllerRuntimePathToPod(val, ctrlCity, ctrlRuntimeDir, podRuntimeDir)
		case "GC_STORE_ROOT", "GC_RIG_ROOT", "BEADS_DIR", "GT_ROOT", "GC_PACK_DIR":
			val = remapControllerOrWorkDirPathToPod(val, ctrlCity, ctrlWorkDir, podWorkDir)
		}
		env = append(env, corev1.EnvVar{Name: k, Value: val})
	}

	projectedDolt, err := projectedPodDoltEnv(cfgEnv, managedServiceHost, managedServicePort)
	if err != nil {
		return nil, err
	}
	projectedKeys := make([]string, 0, len(projectedDolt))
	for key := range projectedDolt {
		projectedKeys = append(projectedKeys, key)
	}
	sort.Strings(projectedKeys)
	for _, key := range projectedKeys {
		env = append(env, corev1.EnvVar{Name: key, Value: projectedDolt[key]})
	}

	doltSecret := strings.TrimSpace(cfgEnv["GC_K8S_DOLT_SECRET"])
	doltCredentialKeys := map[string]string{
		"GC_DOLT_USER":           "username",
		"GC_DOLT_PASSWORD":       "password",
		"BEADS_DOLT_SERVER_USER": "username",
		"BEADS_DOLT_PASSWORD":    "password",
	}
	if doltSecret == "" {
		for key := range doltCredentialKeys {
			if strings.TrimSpace(cfgEnv[key]) != "" {
				return nil, fmt.Errorf(
					"GC_K8S_DOLT_SECRET is required to project %s without leaking a literal credential",
					key,
				)
			}
		}
	} else {
		keys := make([]string, 0, len(doltCredentialKeys))
		for key := range doltCredentialKeys {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			env = append(env, corev1.EnvVar{
				Name: key,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: doltSecret},
						Key:                  doltCredentialKeys[key],
						Optional:             boolPtr(false),
					},
				},
			})
		}
	}

	// Add tmux session env so agent's tmux provider uses the same session.
	env = append(env, corev1.EnvVar{Name: "GC_TMUX_SESSION", Value: tmuxSession})

	// CLAUDE_CONFIG_DIR: use dynamic username home if LINUX_USERNAME is set,
	// otherwise fall back to the baked-in gcagent user.
	linuxUser := cfgEnv["LINUX_USERNAME"]
	if linuxUser != "" {
		env = append(env, corev1.EnvVar{Name: "CLAUDE_CONFIG_DIR", Value: "/home/" + linuxUser + "/.claude"})
	} else {
		env = append(env, corev1.EnvVar{Name: "CLAUDE_CONFIG_DIR", Value: "/home/gcagent/.claude"})
	}

	// Inject GITHUB_TOKEN from optional K8s secret for git push in pods.
	env = append(env, corev1.EnvVar{
		Name: "GITHUB_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "git-credentials"},
				Key:                  "token",
				Optional:             boolPtr(true),
			},
		},
	})
	// Pi/OpenAI-compatible providers use the namespace-local LiteLLM
	// credential. Keeping this optional preserves non-LiteLLM workers while
	// avoiding secret values in the controller-authored Pod object.
	env = append(env, corev1.EnvVar{
		Name: "LITELLM_MASTER_KEY",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "litellm-credentials"},
				Key:                  "credential",
				Optional:             boolPtr(true),
			},
		},
	})

	return env, nil
}

// needsStaging returns true if the session config requires file staging
// via init container.
func needsStaging(cfg runtime.Config, ctrlCity string) bool {
	if cfg.OverlayDir != "" {
		return true
	}
	if len(cfg.PackOverlayDirs) > 0 {
		return true
	}
	if len(cfg.CopyFiles) > 0 {
		return true
	}
	// Rig agents have a work_dir subdirectory.
	if cfg.WorkDir != "" && cfg.WorkDir != ctrlCity {
		return true
	}
	return false
}

// buildResources creates resource requirements from the provider config.
// Returns an error if any resource quantity string is invalid, instead of
// panicking via MustParse.
func buildResources(p *Provider) (corev1.ResourceRequirements, error) {
	req := corev1.ResourceRequirements{}
	if p.cpuRequest != "" || p.memRequest != "" {
		req.Requests = corev1.ResourceList{}
		if p.cpuRequest != "" {
			q, err := resource.ParseQuantity(p.cpuRequest)
			if err != nil {
				return req, fmt.Errorf("parsing GC_K8S_CPU_REQUEST %q: %w", p.cpuRequest, err)
			}
			req.Requests[corev1.ResourceCPU] = q
		}
		if p.memRequest != "" {
			q, err := resource.ParseQuantity(p.memRequest)
			if err != nil {
				return req, fmt.Errorf("parsing GC_K8S_MEM_REQUEST %q: %w", p.memRequest, err)
			}
			req.Requests[corev1.ResourceMemory] = q
		}
	}
	if p.cpuLimit != "" || p.memLimit != "" {
		req.Limits = corev1.ResourceList{}
		if p.cpuLimit != "" {
			q, err := resource.ParseQuantity(p.cpuLimit)
			if err != nil {
				return req, fmt.Errorf("parsing GC_K8S_CPU_LIMIT %q: %w", p.cpuLimit, err)
			}
			req.Limits[corev1.ResourceCPU] = q
		}
		if p.memLimit != "" {
			q, err := resource.ParseQuantity(p.memLimit)
			if err != nil {
				return req, fmt.Errorf("parsing GC_K8S_MEM_LIMIT %q: %w", p.memLimit, err)
			}
			req.Limits[corev1.ResourceMemory] = q
		}
	}
	return req, nil
}

func boolPtr(b bool) *bool { return &b }
