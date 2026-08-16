package omnigent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// SidecarConfig identifies the service-owned local state and catalog. Empty
// CatalogPath resolves to <state-root>/config/profiles.yaml.
type SidecarConfig struct {
	StateRoot   string
	CatalogPath string
	// ImmutableCatalog permits a provider-staged catalog outside StateRoot.
	// The catalog and every referenced agent file must be regular, non-symlink,
	// and read-only before the pinned executable is verified or started.
	ImmutableCatalog bool
	SocketPath       string
	LoopbackPort     int
	StartupTimeout   time.Duration
	ShutdownTimeout  time.Duration
	Stdout           io.Writer
	Stderr           io.Writer
}

const (
	defaultSidecarStartupTimeout  = 4 * time.Second
	defaultSidecarShutdownTimeout = 2 * time.Second
	sidecarHealthInterval         = 50 * time.Millisecond
)

// SidecarPaths are the owner-only city-scoped paths used by Omnigent.
type SidecarPaths struct {
	Root         string
	ConfigDir    string
	DataDir      string
	RunDir       string
	SecretsDir   string
	LogsDir      string
	ArtifactsDir string
	DatabasePath string
	ConfigPath   string
	CatalogPath  string
}

// SidecarLaunchPlan is the exact verified child argv and restricted process
// environment. Values may contain credentials and must never be logged.
type SidecarLaunchPlan struct {
	Executable string
	Args       []string
	HostArgs   []string
	Env        []string
	Endpoint   string
}

// PreparedSidecar is a validated catalog, state layout, and launch plan ready
// for one immediate foreground start.
type PreparedSidecar struct {
	Catalog    *Catalog
	Executable VerifiedExecutable
	Paths      SidecarPaths
	Plan       SidecarLaunchPlan
}

// PrepareSidecar verifies state containment, permissions, exact executable
// identity, local bind, agent registration paths, and the minimal child env.
func PrepareSidecar(ctx context.Context, cfg SidecarConfig, port int) (*PreparedSidecar, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("omnigent loopback port %d is outside 1-65535", port)
	}
	paths, err := prepareSidecarPaths(cfg)
	if err != nil {
		return nil, err
	}
	catalog, err := LoadCatalog(paths.CatalogPath)
	if err != nil {
		return nil, err
	}
	if cfg.ImmutableCatalog {
		if err := validateImmutableCatalogFiles(catalog, paths.CatalogPath); err != nil {
			return nil, err
		}
	}
	verified, err := VerifyExecutable(ctx, catalog.Pin)
	if err != nil {
		return nil, err
	}
	agentPaths := availableAgentPaths(catalog, os.LookupEnv)
	args := []string{
		"server",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--database-uri", "sqlite:///" + paths.DatabasePath,
		"--conversation-database-uri", "sqlite:///" + paths.DatabasePath,
		"--artifact-location", paths.ArtifactsDir,
		"--config", paths.ConfigPath,
		"--no-open",
	}
	for _, agentPath := range agentPaths {
		args = append(args, "--agent", agentPath)
	}
	return &PreparedSidecar{
		Catalog:    catalog,
		Executable: verified,
		Paths:      paths,
		Plan: SidecarLaunchPlan{
			Executable: verified.Path,
			Args:       args,
			HostArgs: []string{
				"host", "--server", "http://127.0.0.1:" + strconv.Itoa(port), "--non-interactive",
			},
			Env:      sidecarChildEnvironment(catalog, paths, os.LookupEnv),
			Endpoint: "http://127.0.0.1:" + strconv.Itoa(port),
		},
	}, nil
}

func prepareSidecarPaths(cfg SidecarConfig) (SidecarPaths, error) {
	root := strings.TrimSpace(cfg.StateRoot)
	if root == "" || !filepath.IsAbs(root) {
		return SidecarPaths{}, errors.New("omnigent service state root must be absolute")
	}
	root = filepath.Clean(root)
	for _, path := range []string{root, filepath.Join(root, "config"), filepath.Join(root, "data"), filepath.Join(root, "run"), filepath.Join(root, "secrets"), filepath.Join(root, "logs"), filepath.Join(root, "data", "artifacts")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return SidecarPaths{}, fmt.Errorf("create omnigent state directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return SidecarPaths{}, fmt.Errorf("secure omnigent state directory: %w", err)
		}
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return SidecarPaths{}, fmt.Errorf("resolve omnigent state root: %w", err)
	}
	configDir := filepath.Join(resolvedRoot, "config")
	catalogPath := strings.TrimSpace(cfg.CatalogPath)
	if catalogPath == "" {
		catalogPath = filepath.Join(configDir, "profiles.yaml")
	}
	if !filepath.IsAbs(catalogPath) {
		return SidecarPaths{}, errors.New("omnigent catalog path must be absolute")
	}
	resolvedCatalog, err := filepath.EvalSymlinks(filepath.Clean(catalogPath))
	if err != nil {
		return SidecarPaths{}, fmt.Errorf("resolve omnigent catalog: %w", err)
	}
	if cfg.ImmutableCatalog {
		if pathWithin(configDir, resolvedCatalog) {
			return SidecarPaths{}, errors.New("immutable omnigent catalog must be staged outside the writable state root")
		}
		if err := validateReadOnlyRegularFile(catalogPath, "catalog"); err != nil {
			return SidecarPaths{}, err
		}
	} else {
		if !pathWithin(configDir, resolvedCatalog) {
			return SidecarPaths{}, errors.New("omnigent catalog must stay beneath the service config directory")
		}
		if err := os.Chmod(resolvedCatalog, 0o600); err != nil {
			return SidecarPaths{}, fmt.Errorf("secure omnigent catalog: %w", err)
		}
	}
	configPath := filepath.Join(configDir, "config.yaml")
	if err := ensurePrivateEmptyConfig(configPath); err != nil {
		return SidecarPaths{}, err
	}
	if err := validateLocalModeYAML(configPath, "global"); err != nil {
		return SidecarPaths{}, err
	}
	return SidecarPaths{
		Root: resolvedRoot, ConfigDir: configDir,
		DataDir: filepath.Join(resolvedRoot, "data"), RunDir: filepath.Join(resolvedRoot, "run"),
		SecretsDir: filepath.Join(resolvedRoot, "secrets"), LogsDir: filepath.Join(resolvedRoot, "logs"),
		ArtifactsDir: filepath.Join(resolvedRoot, "data", "artifacts"),
		DatabasePath: filepath.Join(resolvedRoot, "data", "chat.db"),
		ConfigPath:   configPath, CatalogPath: resolvedCatalog,
	}, nil
}

func validateImmutableCatalogFiles(catalog *Catalog, catalogPath string) error {
	if err := validateReadOnlyRegularFile(catalogPath, "catalog"); err != nil {
		return err
	}
	for _, profile := range catalog.profiles {
		if err := validateReadOnlyRegularFile(profile.AgentPath, "profile agent"); err != nil {
			return fmt.Errorf("omnigent profile %q: %w", profile.ID, err)
		}
	}
	return nil
}

func validateReadOnlyRegularFile(path, kind string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect immutable omnigent %s: %w", kind, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("immutable omnigent %s must be a regular non-symlink file", kind)
	}
	if info.Mode().Perm()&0o222 != 0 {
		return fmt.Errorf("immutable omnigent %s must be read-only", kind)
	}
	return nil
}

func ensurePrivateEmptyConfig(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("omnigent config path is not a regular file")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("secure omnigent config: %w", err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect omnigent config: %w", err)
	}
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create omnigent config temporary file: %w", err)
	}
	_, writeErr := f.WriteString("{}\n")
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return errors.New("write omnigent config")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install omnigent config: %w", err)
	}
	return nil
}

func availableAgentPaths(catalog *Catalog, lookup func(string) (string, bool)) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, public := range catalog.PublicProfilesWithEnvironment(lookup) {
		if public.Availability != "available" {
			continue
		}
		profile, _ := catalog.Profile(public.ID)
		if !seen[profile.AgentPath] {
			seen[profile.AgentPath] = true
			paths = append(paths, profile.AgentPath)
		}
	}
	sort.Strings(paths)
	return paths
}

func sidecarChildEnvironment(catalog *Catalog, paths SidecarPaths, lookup func(string) (string, bool)) []string {
	values := map[string]string{
		"OMNIGENT_CONFIG_HOME":          paths.ConfigDir,
		"OMNIGENT_DATA_DIR":             paths.DataDir,
		"OMNIGENT_NO_UPDATE_CHECK":      "1",
		"OMNIGENT_DISABLE_TELEMETRY":    "true",
		"OMNIGENT_TELEMETRY_ENABLED":    "0",
		"OMNIGENT_OTEL_CAPTURE_CONTENT": "0",
		"OMNIGENT_ACCOUNTS_AUTO_OPEN":   "0",
		"OMNIGENT_AUTH_ENABLED":         "0",
		"DO_NOT_TRACK":                  "1",
		"NO_PROXY":                      "127.0.0.1,localhost",
		"GC_SERVICE_STATE_ROOT":         paths.Root,
		"GC_SERVICE_SECRETS_DIR":        paths.SecretsDir,
		"GC_SERVICE_RUN_ROOT":           paths.RunDir,
	}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "TMPDIR", "SYSTEMROOT", "WINDIR"} {
		if value, ok := lookup(name); ok && value != "" {
			values[name] = value
		}
	}
	for _, name := range catalog.EnvironmentNames() {
		if value, ok := lookup(name); ok && value != "" {
			values[name] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

// NewSidecarHandler builds the Unix-facing local adapter. Gas City profile
// discovery is handled locally; all Omnigent API paths proxy to one loopback
// child without exposing upstream transport errors.
func NewSidecarHandler(catalog *Catalog, endpoint string, client *http.Client) (http.Handler, error) {
	return newSidecarHandler(catalog, VerifiedExecutable{}, endpoint, client, os.LookupEnv, "")
}

func newSidecarHandler(catalog *Catalog, verified VerifiedExecutable, endpoint string, client *http.Client, lookup func(string) (string, bool), localHostID string) (http.Handler, error) {
	if catalog == nil {
		return nil, errors.New("omnigent catalog is required for sidecar handler")
	}
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "http" || !isLoopbackHost(target.Hostname()) || target.User != nil {
		return nil, errors.New("omnigent sidecar upstream must be an explicit loopback HTTP endpoint")
	}
	if client == nil {
		return nil, errors.New("omnigent sidecar HTTP client is required")
	}
	child, err := NewAPIClient(endpoint, client)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(localHostID) != "" {
		child, err = child.withLocalHost(localHostID)
		if err != nil {
			return nil, err
		}
	}
	failover, err := NewFailoverController(child, catalog, time.Now)
	if err != nil {
		return nil, err
	}
	policy, err := NewPolicyController(child, catalog)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = client.Transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "omnigent child unavailable", http.StatusBadGateway)
	}
	mux := http.NewServeMux()
	status := localStatus(catalog, verified, lookup)
	mux.HandleFunc("/gascity/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		for key := range r.URL.Query() {
			if key != "conversation_id" {
				writeSidecarAPIError(w, http.StatusBadRequest, "invalid_status_query", "invalid status query")
				return
			}
		}
		response := status
		conversationID := strings.TrimSpace(r.URL.Query().Get("conversation_id"))
		if conversationID != "" {
			conversation, err := conversationStatus(r.Context(), child, failover, status, conversationID)
			if err != nil {
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
					writeSidecarAPIError(w, http.StatusNotFound, "conversation_not_found", "requested Omnigent conversation does not exist")
					return
				}
				writeSidecarAPIError(w, http.StatusBadGateway, "status_failed", boundedText(err.Error(), maxErrorMessageBytes))
				return
			}
			response.Conversation = conversation
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("/gascity/v1/profiles", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(catalog.PublicProfiles()); err != nil {
			return
		}
	})
	mux.HandleFunc("/gascity/v1/attachments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		limited := http.MaxBytesReader(w, r.Body, 64<<10)
		decoder := json.NewDecoder(limited)
		decoder.DisallowUnknownFields()
		var input AttachmentOpenInput
		if err := decoder.Decode(&input); err != nil {
			writeSidecarAPIError(w, http.StatusBadRequest, "invalid_attachment", "invalid attachment request")
			return
		}
		attachment, err := OpenAttachment(r.Context(), child, catalog, input)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && strings.TrimSpace(input.ConversationID) != "" {
				writeSidecarAPIError(w, http.StatusNotFound, "conversation_not_found", "requested Omnigent conversation does not exist")
				return
			}
			writeSidecarAPIError(w, http.StatusBadGateway, "attachment_failed", boundedText(err.Error(), maxErrorMessageBytes))
			return
		}
		defer func() { _ = attachment.Close() }()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(AttachmentDescriptor{
			ConversationID: attachment.ConversationID,
			ProfileID:      strings.TrimSpace(input.ProfileID),
			Fresh:          attachment.Fresh,
			ActiveProfile:  attachment.State.ActiveProfileID,
			ActiveIndex:    attachment.State.ActiveIndex,
		})
	})
	mux.HandleFunc("/gascity/v1/failover", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		limited := http.MaxBytesReader(w, r.Body, 16<<10)
		decoder := json.NewDecoder(limited)
		decoder.DisallowUnknownFields()
		var observation FailoverObservation
		if err := decoder.Decode(&observation); err != nil {
			writeSidecarAPIError(w, http.StatusBadRequest, "invalid_failover_observation", "invalid failover observation")
			return
		}
		event := StreamEvent{
			Type: observation.Type, Source: observation.Source, SequenceNumber: observation.Sequence,
			Error: &StreamError{Code: observation.ErrorCode, Detail: &StreamErrorDetail{StatusCode: observation.StatusCode}},
		}
		result, err := failover.Advance(r.Context(), observation.ConversationID, observation.ExpectedActive, event)
		if err != nil {
			writeSidecarAPIError(w, http.StatusBadGateway, "failover_failed", boundedText(err.Error(), maxErrorMessageBytes))
			return
		}
		response := FailoverObservationResult{
			ActiveProfileID: result.State.ActiveProfileID,
			ActiveIndex:     result.State.ActiveIndex,
			Transition:      result.Transition,
			Ignored:         result.Ignored,
			Exhausted:       result.Exhausted,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("/gascity/v1/policy/request", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input PolicyRequestInput
		if !decodeSidecarRequest(w, r, &input) {
			return
		}
		descriptor, err := policy.OpenRequest(r.Context(), input)
		if err != nil {
			writeSidecarAPIError(w, http.StatusConflict, "policy_request_rejected", boundedText(err.Error(), maxErrorMessageBytes))
			return
		}
		writeSidecarJSON(w, descriptor)
	})
	mux.HandleFunc("/gascity/v1/policy/bind", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input PolicyMailBinding
		if !decodeSidecarRequest(w, r, &input) {
			return
		}
		descriptor, err := policy.BindMail(r.Context(), input)
		if err != nil {
			writeSidecarAPIError(w, http.StatusConflict, "policy_mail_rejected", boundedText(err.Error(), maxErrorMessageBytes))
			return
		}
		writeSidecarJSON(w, descriptor)
	})
	mux.HandleFunc("/gascity/v1/policy/respond", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input PolicyAnswerInput
		if !decodeSidecarRequest(w, r, &input) {
			return
		}
		result, err := policy.Respond(r.Context(), input)
		if err != nil {
			writeSidecarAPIError(w, http.StatusConflict, "policy_response_rejected", boundedText(err.Error(), maxErrorMessageBytes))
			return
		}
		writeSidecarJSON(w, result)
	})
	mux.HandleFunc("/gascity/v1/policy/cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var input PolicyCancelInput
		if !decodeSidecarRequest(w, r, &input) {
			return
		}
		result, err := policy.Cancel(r.Context(), input)
		if err != nil {
			writeSidecarAPIError(w, http.StatusConflict, "policy_cancel_rejected", boundedText(err.Error(), maxErrorMessageBytes))
			return
		}
		writeSidecarJSON(w, result)
	})
	mux.Handle("/", proxy)
	return mux, nil
}

func decodeSidecarRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	limited := http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeSidecarAPIError(w, http.StatusBadRequest, "invalid_request", "invalid sidecar request")
		return false
	}
	return true
}

func writeSidecarJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func writeSidecarAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	type sidecarErrorBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body := sidecarErrorBody{}
	body.Error.Code = code
	body.Error.Message = boundedText(redactSensitiveText(message), maxErrorMessageBytes)
	_ = json.NewEncoder(w).Encode(body)
}

// ServeSidecar runs verified foreground Omnigent server and local-host children
// and exposes the server on the assigned Unix socket until cancellation or
// child exit. Existing Gas City proxy_process supervision owns retries and
// convergence around this process group.
func ServeSidecar(ctx context.Context, cfg SidecarConfig) error {
	if ctx == nil {
		return errors.New("omnigent sidecar context is required")
	}
	socketPath := strings.TrimSpace(cfg.SocketPath)
	if socketPath == "" || !filepath.IsAbs(socketPath) {
		return errors.New("omnigent sidecar socket path must be absolute")
	}
	port := cfg.LoopbackPort
	if port == 0 {
		var err error
		port, err = reserveLoopbackPort()
		if err != nil {
			return err
		}
	}
	prepared, err := PrepareSidecar(ctx, cfg, port)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	stdoutTarget := cfg.Stdout
	if stdoutTarget == nil {
		stdoutTarget = os.Stdout
	}
	stderrTarget := cfg.Stderr
	if stderrTarget == nil {
		stderrTarget = os.Stderr
	}
	serverCmd, serverDone, err := startSidecarProcess(prepared, prepared.Plan.Args, stdoutTarget, stderrTarget)
	if err != nil {
		return fmt.Errorf("start verified omnigent child: %w", err)
	}
	startupTimeout := cfg.StartupTimeout
	if startupTimeout <= 0 {
		startupTimeout = defaultSidecarStartupTimeout
	}
	shutdownTimeout := cfg.ShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultSidecarShutdownTimeout
	}
	healthClient := localHTTPClient(250 * time.Millisecond)
	if err := waitSidecarReady(ctx, healthClient, prepared.Plan.Endpoint, serverDone, startupTimeout); err != nil {
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	hostCmd, hostDone, err := startSidecarProcess(prepared, prepared.Plan.HostArgs, stdoutTarget, stderrTarget)
	if err != nil {
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		return fmt.Errorf("start verified omnigent local host: %w", err)
	}
	childClient, err := NewAPIClient(prepared.Plan.Endpoint, healthClient)
	if err != nil {
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		return err
	}
	localHostID, err := waitSidecarHostReady(ctx, childClient, hostDone, startupTimeout)
	if err != nil {
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if err := prepareSidecarSocket(socketPath); err != nil {
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		return fmt.Errorf("listen on omnigent sidecar socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		return fmt.Errorf("secure omnigent sidecar socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	handler, err := newSidecarHandler(prepared.Catalog, prepared.Executable, prepared.Plan.Endpoint, localStreamingHTTPClient(), os.LookupEnv, localHostID)
	if err != nil {
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	shutdownServer := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}
	select {
	case <-ctx.Done():
		shutdownServer()
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		return nil
	case childErr := <-serverDone:
		shutdownServer()
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		if childErr == nil {
			return errors.New("omnigent server exited unexpectedly")
		}
		return fmt.Errorf("omnigent server exited unexpectedly: %w", childErr)
	case hostErr := <-hostDone:
		shutdownServer()
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		if hostErr == nil {
			return errors.New("omnigent local host exited unexpectedly")
		}
		return fmt.Errorf("omnigent local host exited unexpectedly: %w", hostErr)
	case serveErr := <-serveDone:
		stopSidecarChild(hostCmd, hostDone, shutdownTimeout)
		stopSidecarChild(serverCmd, serverDone, shutdownTimeout)
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve omnigent sidecar socket: %w", serveErr)
	}
}

func startSidecarProcess(prepared *PreparedSidecar, args []string, stdoutTarget, stderrTarget io.Writer) (*exec.Cmd, <-chan error, error) {
	cmd := exec.Command(prepared.Plan.Executable, args...)
	cmd.Dir = prepared.Paths.Root
	cmd.Env = prepared.Plan.Env
	environmentReferences := make([]runtime.SecretReference, 0, len(prepared.Catalog.EnvironmentNames()))
	for _, name := range prepared.Catalog.EnvironmentNames() {
		environmentReferences = append(environmentReferences, runtime.SecretReference{Environment: name})
	}
	redactor := newRemoteRedactor(prepared.Plan.Env, environmentReferences,
		prepared.Paths.Root, prepared.Paths.ConfigDir, prepared.Paths.DataDir,
		prepared.Paths.RunDir, prepared.Paths.SecretsDir, prepared.Paths.LogsDir,
		prepared.Paths.ArtifactsDir, prepared.Paths.DatabasePath, prepared.Paths.ConfigPath,
		prepared.Paths.CatalogPath,
	)
	stdout := newRedactingWriterWith(stdoutTarget, redactor)
	stderr := newRedactingWriterWith(stderrTarget, redactor)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- errors.Join(cmd.Wait(), stdout.Flush(), stderr.Flush()) }()
	return cmd, done, nil
}

type sidecarHost struct {
	ID     string `json:"host_id"`
	Owner  string `json:"owner"`
	Status string `json:"status"`
}

func waitSidecarHostReady(ctx context.Context, client *APIClient, hostDone <-chan error, timeout time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(sidecarHealthInterval)
	defer ticker.Stop()
	check := func() (string, error) {
		var response struct {
			Hosts []sidecarHost `json:"hosts"`
		}
		if err := client.doJSON(ctx, http.MethodGet, "/v1/hosts", nil, &response); err != nil {
			return "", nil
		}
		var online []sidecarHost
		for _, host := range response.Hosts {
			if host.Owner == "local" && host.Status == "online" {
				online = append(online, host)
			}
		}
		if len(online) > 1 {
			return "", fmt.Errorf("omnigent local server reported %d online local hosts; expected exactly one supervised host", len(online))
		}
		if len(online) == 0 {
			return "", nil
		}
		if err := validateOpaqueID("host", online[0].ID); err != nil {
			return "", err
		}
		return online[0].ID, nil
	}
	for {
		hostID, err := check()
		if err != nil || hostID != "" {
			return hostID, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-hostDone:
			if err == nil {
				return "", errors.New("omnigent local host exited before readiness")
			}
			return "", fmt.Errorf("omnigent local host exited before readiness: %w", err)
		case <-deadline.C:
			return "", fmt.Errorf("omnigent local host did not become ready within %s", timeout)
		case <-ticker.C:
		}
	}
}

func reserveLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve omnigent loopback port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("release omnigent loopback port reservation: %w", err)
	}
	return port, nil
}

func localHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:              nil,
			DisableCompression: true,
			DisableKeepAlives:  true,
		},
	}
}

func localStreamingHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		ResponseHeaderTimeout: 30 * time.Second,
	}}
}

func waitSidecarReady(ctx context.Context, client *http.Client, endpoint string, childDone <-chan error, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(sidecarHealthInterval)
	defer ticker.Stop()
	check := func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/health", nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	for {
		if check() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-childDone:
			if err == nil {
				return errors.New("omnigent child exited before readiness")
			}
			return fmt.Errorf("omnigent child exited before readiness: %w", err)
		case <-deadline.C:
			return fmt.Errorf("omnigent child did not become ready within %s", timeout)
		case <-ticker.C:
		}
	}
}

func prepareSidecarSocket(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect omnigent sidecar socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("omnigent sidecar socket path is occupied by a non-socket file")
	}
	conn, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return errors.New("omnigent sidecar socket is already active")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale omnigent sidecar socket: %w", err)
	}
	return nil
}

func stopSidecarChild(cmd *exec.Cmd, done <-chan error, timeout time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, 0); errors.Is(err, syscall.ESRCH) {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}
