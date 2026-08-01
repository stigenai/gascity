package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

// StatusJSON is the JSON output format for gc status.
type StatusJSON struct {
	SchemaVersion string                 `json:"schema_version"`
	OK            bool                   `json:"ok"`
	CityName      string                 `json:"city_name"`
	Workspace     WorkspaceJSON          `json:"workspace"`
	CityPath      string                 `json:"city_path"`
	Controller    ControllerJSON         `json:"controller"`
	Running       bool                   `json:"running"`
	Suspended     bool                   `json:"suspended"`
	Health        HealthJSON             `json:"health"`
	Beads         *beads.BeadsDiagnostic `json:"beads,omitempty"`
	// ConditionalWrites mirrors the API status block verbatim (§12.5).
	ConditionalWrites *api.StatusConditionalWrites `json:"conditional_writes,omitempty"`
	Agents            []StatusAgentJSON            `json:"agents"`
	Rigs              []StatusRigJSON              `json:"rigs"`
	Summary           StatusSummaryJSON            `json:"summary"`
	Partial           bool                         `json:"partial,omitempty"`
	PartialErrors     []string                     `json:"partial_errors,omitempty"`
}

type WorkspaceJSON struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type HealthJSON struct {
	Usable   bool     `json:"usable"`
	Degraded bool     `json:"degraded"`
	Signals  []string `json:"signals,omitempty"`
}

// ControllerJSON represents controller state in JSON output.
type ControllerJSON struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Status  string `json:"status,omitempty"`
}

// StatusAgentJSON represents an agent in the JSON status output.
type StatusAgentJSON struct {
	Name          string    `json:"name"`
	QualifiedName string    `json:"qualified_name"`
	Scope         string    `json:"scope"`
	Running       bool      `json:"running"`
	Suspended     bool      `json:"suspended"`
	Pool          *PoolJSON `json:"pool,omitempty"`
}

// PoolJSON represents pool configuration in JSON output.
type PoolJSON struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// StatusRigJSON represents a rig in the JSON status output.
type StatusRigJSON struct {
	Name                string   `json:"name"`
	Path                string   `json:"path"`
	Prefix              string   `json:"prefix,omitempty"`
	Suspended           bool     `json:"suspended"`
	DefaultSlingTarget  string   `json:"default_sling_target,omitempty"`
	DefaultSlingTargets []string `json:"default_sling_targets,omitempty"`
}

// StatusSummaryJSON is the agent count summary in JSON output.
type StatusSummaryJSON struct {
	TotalAgents       int          `json:"total_agents"`
	RunningAgents     int          `json:"running_agents"`
	ActiveSessions    int          `json:"active_sessions,omitempty"`
	SuspendedSessions int          `json:"suspended_sessions,omitempty"`
	StoreHealth       *StoreHealth `json:"store_health,omitempty"`
}

// StoreHealth is the JSON shape of the Dolt bead store health block
// surfaced by gc status. See ADR 0002 / bead ga-d5y design D9.
type StoreHealth struct {
	Path         string  `json:"path"`
	SizeBytes    int64   `json:"size_bytes"`
	LiveRows     int     `json:"live_rows"`
	RatioMB      float64 `json:"ratio_mb_per_row"`
	Warning      bool    `json:"warning"`
	ThresholdMB  float64 `json:"threshold_mb_per_row"`
	LastGCAt     string  `json:"last_gc_at,omitempty"`
	LastGCStatus string  `json:"last_gc_status,omitempty"`
}

var (
	observeSessionTargetForStatus = workerObserveSessionTargetWithConfig
	openCityStoreAtForStatus      = openCityStoreResultAt
)

var (
	controllerStatusStandaloneFallbackTimeout = 250 * time.Millisecond
	statusObservationTimeout                  = 750 * time.Millisecond
	statusSessionSnapshotTimeout              = 3 * time.Second
	// The supervisor status endpoint is cache-backed and normally answers in
	// milliseconds. Keep the CLI path bounded so a live socket with a wedged
	// handler cannot hold `gc status` for the API client's 60-second federated
	// read ceiling. A timeout is authoritative and never falls through to the
	// potentially expensive local Dolt/provider path.
	cityStatusAPITimeout = 5 * time.Second
)

// cityStatusRenderLocal is the complete local fallback initialization seam.
// Keeping it lazy is the ordering contract: a healthy API response must not
// load city config, open Dolt, build a session snapshot, or construct a runtime
// provider. Tests replace this seam with counters/blockers to pin that rule.
var cityStatusRenderLocal = renderCityStatusLocal

type cityStatusAPIResolution struct {
	client          *api.Client
	controller      ControllerJSON
	reason          string
	warning         string
	managedCityName string
	managedCityPath string
}

// cityStatusResolveAPI is the full pre-fetch routing seam. Its production
// implementation performs only bounded/local discovery: no ListCities call
// and no controller/supervisor socket probe. The single command context then
// covers both this resolution phase and the authoritative /status request.
var cityStatusResolveAPI = resolveCityStatusAPI

func resolveCityStatusAPI(ctx context.Context, cityPath string) (cityStatusAPIResolution, error) {
	if err := ctx.Err(); err != nil {
		return cityStatusAPIResolution{}, err
	}
	disabled, warning := classifyGCNoAPI(os.Getenv("GC_NO_API"))
	if disabled {
		return cityStatusAPIResolution{reason: "escape-hatch"}, nil
	}

	// A configured standalone endpoint wins. Constructing the client is a
	// local config read only; the bounded request below proves liveness.
	if c := standaloneControllerClient(cityPath); c != nil {
		return cityStatusAPIResolution{
			client:     c,
			controller: ControllerJSON{Running: true, Mode: "standalone"},
			warning:    warning,
		}, nil
	}
	if err := ctx.Err(); err != nil {
		return cityStatusAPIResolution{}, err
	}

	// A supervisor-managed city normally has no [api] port. Resolve its
	// city-scoped URL directly from the local registry/config and let the one
	// bounded /status request establish availability. supervisorCityAPIClient
	// cannot be used here because it first performs an unbounded ListCities.
	entry, registered, registryErr := registeredCityEntry(cityPath)
	if registryErr == nil && registered {
		baseURL, baseErr := supervisorAPIBaseURLHook()
		if baseErr == nil {
			return cityStatusAPIResolution{
				client:          api.NewCityScopedClient(baseURL, entry.EffectiveName()),
				controller:      ControllerJSON{Running: true, Mode: "supervisor", Status: "running"},
				warning:         warning,
				managedCityName: entry.EffectiveName(),
				managedCityPath: cityPath,
			}, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return cityStatusAPIResolution{}, err
	}

	return cityStatusAPIResolution{reason: staticCityStatusFallbackReason(cityPath), warning: warning}, nil
}

// staticCityStatusFallbackReason mirrors the useful apiClient reason tokens
// without probing a controller socket after the shared command deadline has
// started. It is deliberately config-only.
func staticCityStatusFallbackReason(cityPath string) string {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err == nil && cfg.API.Port > 0 {
		bind := cfg.API.BindOrDefault()
		if bind != "127.0.0.1" && bind != "localhost" && bind != "::1" && !cfg.API.AllowMutations {
			return "non-loopback-bind"
		}
	}
	return "controller-down"
}

// newStatusCmd creates the "gc status [path]" command.
func newStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonFlag bool
	var formatFlag string
	cmd := &cobra.Command{
		Use:   "status [path|name]",
		Short: "Show city-wide status overview",
		Long: `Shows a city-wide overview: controller state, suspension,
all agents with running status, rigs, and a summary count.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCityNames,
		RunE: func(_ *cobra.Command, args []string) error {
			format := strings.ToLower(strings.TrimSpace(formatFlag))
			switch format {
			case "", "text", "json":
			default:
				fmt.Fprintf(stderr, "gc status: unsupported format %q\n", formatFlag) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdCityStatus(args, jsonFlag || format == "json", stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	cmd.Flags().StringVar(&formatFlag, "format", "", "Output format: text or json")
	return cmd
}

// cmdCityStatus is the CLI entry point for the city status overview.
// Routes through the supervisor API when a controller is up and falls
// back to the local snapshot builder otherwise.
func cmdCityStatus(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCommandCity(args)
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "city_resolve_failed", fmt.Sprintf("gc status: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Route before constructing any local status dependencies. The old order
	// opened Dolt and enumerated sessions/providers even when the supervisor's
	// cache was live, which made a millisecond API read appear to hang.
	ctx, cancel := context.WithTimeout(context.Background(), cityStatusAPITimeout)
	defer cancel()
	resolution, err := cityStatusResolveAPI(ctx, cityPath)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintf(stderr, "gc status: API resolution timed out after %s\n", cityStatusAPITimeout) //nolint:errcheck // best-effort stderr
		} else {
			fmt.Fprintf(stderr, "gc status: API resolution failed: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return 1
	}
	if resolution.warning != "" {
		fmt.Fprintln(stderr, "warning: "+resolution.warning) //nolint:errcheck // best-effort stderr
	}
	return routeCityStatusRead(ctx, nil, resolution.client, resolution.controller, resolution.managedCityName, resolution.managedCityPath, resolution.reason, jsonOutput, stdout, stderr, func() int {
		return cityStatusRenderLocal(cityPath, jsonOutput, stdout, stderr)
	})
}

func renderCityStatusLocal(cityPath string, jsonOutput bool, stdout, stderr io.Writer) int {
	configStderr := stderr
	if jsonOutput {
		configStderr = io.Discard
	}
	cfg, err := loadCityConfig(cityPath, configStderr)
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "config_load_failed", fmt.Sprintf("gc status: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	storeStderr := stderr
	if jsonOutput {
		storeStderr = io.Discard
	}
	store, diagnostic, code := openCityStatusStore(cityPath, storeStderr)
	if code != 0 {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "store_open_failed", "gc status: opening bead store failed", code)
		}
		return code
	}
	statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr)
	sp, err := newStatusSessionProviderForCityWithSnapshot(cfg, cityPath, statusSnapshot)
	if err != nil {
		message := fmt.Sprintf("gc status: %v", err)
		if jsonOutput {
			return writeJSONError(stdout, stderr, "session_provider_failed", message, 1)
		}
		fmt.Fprintln(stderr, message) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	if jsonOutput {
		return doCityStatusJSONWithDiagnosticAndSnapshot(sp, cfg, cityPath, store, diagnostic, statusSnapshot, stdout, stderr)
	}
	return doCityStatusWithStoreAndSnapshot(sp, dops, cfg, cityPath, store, statusSnapshot, stdout, stderr)
}

// routeCityStatus dispatches `gc status` to the supervisor API when a
// controller is up; otherwise falls back to the local snapshot builder.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
func routeCityStatus(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	dops drainOps,
	c *api.Client,
	nilReason string,
	jsonOutput bool,
	stdout, stderr io.Writer,
) int {
	ctx, cancel := context.WithTimeout(context.Background(), cityStatusAPITimeout)
	defer cancel()
	controller := ControllerJSON{}
	if c != nil {
		// A successful API response is itself the running-authority proof. The
		// legacy test helper has no resolver provenance, so leave mode unset.
		controller.Running = true
	}
	return routeCityStatusRead(ctx, dops, c, controller, "", "", nilReason, jsonOutput, stdout, stderr, func() int {
		store, diagnostic, code := openCityStatusStore(cityPath, stderr)
		if code != 0 {
			return code
		}
		statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr)
		if jsonOutput {
			return doCityStatusJSONWithDiagnosticAndSnapshot(sp, cfg, cityPath, store, diagnostic, statusSnapshot, stdout, stderr)
		}
		return doCityStatusWithStoreAndSnapshot(sp, dops, cfg, cityPath, store, statusSnapshot, stdout, stderr)
	})
}

func routeCityStatusRead(
	ctx context.Context,
	dops drainOps,
	c *api.Client,
	controller ControllerJSON,
	managedCityName string,
	managedCityPath string,
	nilReason string,
	jsonOutput bool,
	stdout, stderr io.Writer,
	localRender func() int,
) int {
	var cr api.CachedRead[api.StatusView]
	return routeRead(c, "status", nilReason, stderr,
		func() error {
			var err error
			cr, err = c.GetStatusContext(ctx)
			if errors.Is(err, context.DeadlineExceeded) {
				return errorAfterFetch{Detail: fmt.Sprintf("status API timed out after %s", cityStatusAPITimeout)}
			}
			if api.IsCityNotRunningError(err) && managedCityName != "" && controller.Mode == "supervisor" {
				return resolveManagedCityNotRunning(ctx, c, managedCityName, managedCityPath, &cr, &controller)
			}
			return err
		},
		func() int { return renderCityStatusFromAPI(cr, controller, dops, jsonOutput, stdout) },
		localRender,
	)
}

// resolveManagedCityNotRunning turns the supervisor's stable city-scoped 404
// into a bounded supervisor-authoritative status. The follow-up /v0/cities
// read shares the original command deadline; it never invokes the legacy
// unbounded ListCities preflight and never opens the local Dolt/provider path.
func resolveManagedCityNotRunning(
	ctx context.Context,
	c *api.Client,
	managedCityName string,
	managedCityPath string,
	cr *api.CachedRead[api.StatusView],
	controller *ControllerJSON,
) error {
	cities, err := c.ListCitiesContext(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return errorAfterFetch{Detail: fmt.Sprintf("status API timed out after %s", cityStatusAPITimeout)}
	}

	partialErrors := []string{"city is not running; workload status is unavailable"}
	status := "not_running"
	name := managedCityName
	path := managedCityPath
	listedRunning := false
	if err != nil {
		partialErrors = append(partialErrors, "supervisor city-state read failed: "+err.Error())
	} else if city, ok := findManagedStatusCity(cities, managedCityName, managedCityPath); ok {
		name = city.Name
		if city.Path != "" {
			path = city.Path
		}
		if city.Running {
			// The city transitioned between the scoped 404 and the list read.
			// Retry exactly once under the same deadline to obtain full truth.
			retry, retryErr := c.GetStatusContext(ctx)
			if retryErr == nil {
				*cr = retry
				controller.Running = true
				controller.Status = "running"
				return nil
			}
			if errors.Is(retryErr, context.DeadlineExceeded) {
				return errorAfterFetch{Detail: fmt.Sprintf("status API timed out after %s", cityStatusAPITimeout)}
			}
			listedRunning = true
			status = "api_unavailable"
			partialErrors = append(partialErrors, "supervisor reports the city running but its status route is not ready")
		} else if city.Status != "" {
			status = city.Status
		}
		if city.Error != "" {
			partialErrors = append(partialErrors, "supervisor city error: "+city.Error)
		}
	} else {
		status = "not_listed"
		partialErrors = append(partialErrors, "registered city is absent from the supervisor city list")
	}

	controller.Running = listedRunning
	controller.Status = status
	*cr = api.CachedRead[api.StatusView]{Body: api.StatusView{
		CityName:      name,
		CityPath:      path,
		Agents:        []api.StatusAgentView{},
		Rigs:          []api.StatusRigView{},
		NamedSessions: []api.StatusNamedSessionView{},
		Partial:       true,
		PartialErrors: partialErrors,
	}}
	return nil
}

func findManagedStatusCity(cities []api.CityInfo, name, path string) (api.CityInfo, bool) {
	for _, city := range cities {
		if city.Name == name {
			return city, true
		}
	}
	cleanPath := filepath.Clean(path)
	for _, city := range cities {
		if filepath.Clean(city.Path) == cleanPath {
			return city, true
		}
	}
	return api.CityInfo{}, false
}

// renderCityStatusFromAPI renders the server's StatusView using the same
// text and JSON formatters as the fallback path. The API path adds
// _cache_age_s on --json output and a staleness banner on human output
// when cache age > 30 s.
//
// Controller authority comes from the resolver provenance. Re-probing it here
// would issue an unbounded ListCities request after the bounded fetch.
func renderCityStatusFromAPI(cr api.CachedRead[api.StatusView], controller ControllerJSON, dops drainOps, jsonOutput bool, stdout io.Writer) int {
	snapshot := snapshotFromStatusView(cr.Body, controller)
	if jsonOutput {
		writeCityStatusJSONWithCache(snapshot, snapshot.Summary, cr.AgeSeconds, stdout)
		return 0
	}
	renderCityStatusText(snapshot, dops, stdout)
	if cr.Body.SessionCounts.Active > 0 || cr.Body.SessionCounts.Suspended > 0 {
		fmt.Fprintln(stdout)                                                                                                      //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Sessions: %d active, %d suspended\n", cr.Body.SessionCounts.Active, cr.Body.SessionCounts.Suspended) //nolint:errcheck // best-effort stdout
	}
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// snapshotFromStatusView builds a cityStatusSnapshot from the API's
// StatusView so the existing renderCityStatusText + cityStatusJSONFromSnapshot
// helpers produce identical output on the API path.
func snapshotFromStatusView(v api.StatusView, controller ControllerJSON) cityStatusSnapshot {
	snapshot := cityStatusSnapshot{
		CityName:          v.CityName,
		CityPath:          v.CityPath,
		Suspended:         v.Suspended,
		Controller:        controller,
		Beads:             v.Beads,
		ConditionalWrites: v.ConditionalWrites,
		Partial:           v.Partial,
		PartialErrors:     append([]string(nil), v.PartialErrors...),
		Summary: StatusSummaryJSON{
			TotalAgents:       v.Summary.TotalAgents,
			RunningAgents:     v.Summary.RunningAgents,
			ActiveSessions:    v.SessionCounts.Active,
			SuspendedSessions: v.SessionCounts.Suspended,
		},
	}
	for _, a := range v.Agents {
		snapshot.Agents = append(snapshot.Agents, cityStatusAgentRow{
			Agent: StatusAgentJSON{
				Name:          a.Name,
				QualifiedName: a.QualifiedName,
				Scope:         a.Scope,
				Running:       a.Running,
				Suspended:     a.Suspended,
			},
			SessionName: a.SessionName,
			GroupName:   a.GroupName,
			ScaleLabel:  a.ScaleLabel,
			Expanded:    a.Expanded,
			Draining:    a.Draining,
		})
	}
	for _, r := range v.Rigs {
		snapshot.Rigs = append(snapshot.Rigs, StatusRigJSON{
			Name:      r.Name,
			Path:      r.Path,
			Suspended: r.Suspended,
		})
	}
	for _, ns := range v.NamedSessions {
		snapshot.NamedSessions = append(snapshot.NamedSessions, cityStatusNamedSession{
			Identity: ns.Identity,
			Status:   ns.Status,
			Mode:     ns.Mode,
		})
	}
	if v.StoreHealth != nil {
		snapshot.Summary.StoreHealth = &StoreHealth{
			Path:         v.StoreHealth.Path,
			SizeBytes:    v.StoreHealth.SizeBytes,
			LiveRows:     v.StoreHealth.LiveRows,
			RatioMB:      v.StoreHealth.RatioMB,
			Warning:      v.StoreHealth.Warning,
			ThresholdMB:  v.StoreHealth.ThresholdMB,
			LastGCAt:     v.StoreHealth.LastGCAt,
			LastGCStatus: v.StoreHealth.LastGCStatus,
		}
	}
	return snapshot
}

// writeCityStatusJSONWithCache writes the snapshot's JSON form with a
// leading _cache_age_s field inserted at the envelope level. Mirrors the
// envelope shape other routed read commands emit on the API path.
func writeCityStatusJSONWithCache(
	snapshot cityStatusSnapshot,
	summary StatusSummaryJSON,
	ageSeconds float64,
	stdout io.Writer,
) {
	status := cityStatusJSONFromSnapshot(snapshot, summary)
	envelope := struct {
		CacheAgeS  float64 `json:"_cache_age_s"`
		StatusJSON         // inline
	}{CacheAgeS: ageSeconds, StatusJSON: status}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		fmt.Fprintf(stdout, "{\"error\": %q}\n", err.Error()) //nolint:errcheck // best-effort stdout
		return
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
}

func observeSessionTargetWithWarning(
	cmdName string,
	cityPath string,
	_ beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	target statusObservationTarget,
	stderr io.Writer,
) worker.LiveObservation {
	// Status already passes a concrete runtime session name. Resolving that
	// string back through the bead store turns stopped pool instances such as
	// "dog-1" into invalid bd show lookups, which can block the overview.
	type observeResult struct {
		observation worker.LiveObservation
		err         error
	}
	done := make(chan observeResult, 1)
	go func() {
		obs, err := observeSessionTargetForStatus(cityPath, nil, sp, cfg, target.runtimeSessionName)
		done <- observeResult{observation: obs, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			markStatusProviderPartial(sp)
			if stderr != nil {
				fmt.Fprintf(stderr, "%s: observing %q: %v\n", cmdName, target.runtimeSessionName, result.err) //nolint:errcheck // best-effort stderr
			}
		}
		return result.observation
	case <-time.After(statusObservationTimeout):
		markStatusProviderPartial(sp)
		if stderr != nil {
			fmt.Fprintf(stderr, "%s: observing %q timed out after %s\n", cmdName, target.runtimeSessionName, statusObservationTimeout) //nolint:errcheck // best-effort stderr
		}
		return worker.LiveObservation{}
	}
}

type statusObservationTarget struct {
	runtimeSessionName string
	sessionID          string
	suspended          bool
}

func loadStatusSessionSnapshot(cityPath string, cfg *config.City, store beads.Store, stderr io.Writer) *sessionBeadSnapshot {
	if store == nil {
		return newSessionBeadSnapshotFromInfos(nil)
	}
	// Callers pass the session coordination-class store (cliSessionStore) so a
	// [beads.classes.sessions] relocation reaches this snapshot; the guard in
	// frontdoor_di_guard_test.go pins that seam at each read-site file.

	// A throwaway, ctx-bound clone of store when it's bd-CLI-backed: on
	// timeout below, canceling reqCtx kills an in-flight bd child instead
	// of abandoning it to run past this function's return (gascity
	// ga-cdmx6x). scopedStoreLike answers (nil, nil) for non-bd-CLI
	// backends, which have no subprocess to leak — those keep reading
	// through store directly, unchanged.
	//
	// SPLIT CAVEAT (must fix in cmd/gc/scoped_store.go before enabling the
	// domain/infra split): scopedStoreLike is CLASS-BLIND — it unwraps to the
	// backing *beads.BdStore and rebuilds a scoped clone from cityPath / the
	// backing Dir(), never re-consulting resolveClassStore. Today that is
	// byte-identical (cliSessionStore is identity, so store's backing Dir() ==
	// cityPath). Once [beads.classes.sessions] relocates to a bd-CLI-backed
	// store, this clone would silently re-point the session read at the WORK
	// store, defeating the cliSessionStore seam above (the DI guard cannot catch
	// it — the cliSessionStore( needle is still present). scopedStoreLike must be
	// made class-preserving (clone what it unwrapped, or refuse to unwrap a
	// relocated store so this keeps reading through the routed store) as part of
	// the split, where a real infra store makes the fix testable.
	reqCtx, cancel := context.WithTimeout(context.Background(), statusSessionSnapshotTimeout)
	defer cancel()
	readStore := store
	if scoped, err := scopedStoreLike(reqCtx, cityPath, cfg, store); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "gc status: loading session snapshot: resolving store: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return newSessionBeadSnapshotWithError(fmt.Errorf("loading session snapshot: resolving store: %w", err))
	} else if scoped != nil {
		readStore = scoped
	}

	type snapshotResult struct {
		snapshot *sessionBeadSnapshot
		err      error
	}
	done := make(chan snapshotResult, 1)
	go func() {
		snapshot, err := loadSessionBeadSnapshot(readStore)
		done <- snapshotResult{snapshot: snapshot, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "gc status: loading session snapshot: %v\n", result.err) //nolint:errcheck // best-effort stderr
			}
			return newSessionBeadSnapshotWithError(fmt.Errorf("loading session snapshot: %w", result.err))
		}
		if result.snapshot == nil {
			return newSessionBeadSnapshotFromInfos(nil)
		}
		return result.snapshot
	case <-time.After(statusSessionSnapshotTimeout):
		if stderr != nil {
			fmt.Fprintf(stderr, "gc status: loading session snapshot timed out after %s; continuing with runtime-only status\n", statusSessionSnapshotTimeout) //nolint:errcheck // best-effort stderr
		}
		return newSessionBeadSnapshotWithError(fmt.Errorf("loading session snapshot timed out after %s", statusSessionSnapshotTimeout))
	}
}

func statusObservationTargetForIdentity(
	snapshot *sessionBeadSnapshot,
	cityName string,
	identity string,
	sessionTemplate string,
) statusObservationTarget {
	if snapshot != nil {
		if info, ok := snapshot.FindInfoByTemplate(identity); ok {
			if sessionName := strings.TrimSpace(info.SessionNameMetadata); sessionName != "" {
				return statusObservationTarget{
					runtimeSessionName: sessionName,
					sessionID:          info.ID,
					suspended:          sessionMetadataStateInfo(info) == string(session.StateSuspended),
				}
			}
		}
		if info, ok := snapshot.FindInfoByNamedIdentity(identity); ok {
			if sessionName := strings.TrimSpace(info.SessionNameMetadata); sessionName != "" {
				return statusObservationTarget{
					runtimeSessionName: sessionName,
					sessionID:          info.ID,
					suspended:          sessionMetadataStateInfo(info) == string(session.StateSuspended),
				}
			}
		}
	}
	return statusObservationTarget{
		runtimeSessionName: sessionName(nil, cityName, identity, sessionTemplate),
	}
}

func namedSessionBlockedBySuspension(cfg *config.City, agentCfg *config.Agent, suspState suspensionstate.State, suspendedRigs map[string]bool) bool {
	if cfg == nil {
		return false
	}
	if citySuspendedWithState(cfg, suspState) {
		return true
	}
	if agentCfg == nil {
		return false
	}
	return agentCfg.Suspended || (agentCfg.Dir != "" && suspendedRigs[agentCfg.Dir])
}

// doCityStatus prints the city-wide status overview. Accepts injected
// runtime.Provider for testability.
func doCityStatus(
	sp runtime.Provider,
	dops drainOps,
	cfg *config.City,
	cityPath string,
	stdout, stderr io.Writer,
) int {
	store, _, code := openCityStatusStore(cityPath, stderr)
	if code != 0 {
		return code
	}
	return doCityStatusWithStoreAndSnapshot(sp, dops, cfg, cityPath, store, loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr), stdout, stderr)
}

func doCityStatusWithStoreAndSnapshot(
	sp runtime.Provider,
	dops drainOps,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	stdout, stderr io.Writer,
) int {
	snapshot := collectCityStatusSnapshotFromStoreSnapshot(sp, cfg, cityPath, store, statusSnapshot, stderr)
	renderCityStatusText(snapshot, dops, stdout)

	// Track session-snapshot degradation so we can render the textual report
	// AND signal the failure via exit code. Restores the pre-#2005 contract
	// that monitoring callers rely on (see #2147).
	snapshotDegraded := statusSnapshot.LoadError() != nil

	if store != nil {
		sessions, err := collectCitySessionCounts(cityPath, store, sp, cfg, statusSnapshot)
		if err != nil {
			fmt.Fprintf(stderr, "gc status: building session catalog: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if sessions.ActiveSessions > 0 || sessions.SuspendedSessions > 0 {
			fmt.Fprintln(stdout)                                                                                            //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stdout, "Sessions: %d active, %d suspended\n", sessions.ActiveSessions, sessions.SuspendedSessions) //nolint:errcheck // best-effort stdout
		}
	}

	if snapshotDegraded {
		return 1
	}
	return 0
}

// doCityStatusJSON outputs city status as JSON. Accepts injected providers
// for testability.
func doCityStatusJSON(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	stdout, stderr io.Writer,
) int {
	store, diagnostic, code := openCityStatusStore(cityPath, stderr)
	if code != 0 {
		return code
	}
	return doCityStatusJSONWithDiagnosticAndSnapshot(sp, cfg, cityPath, store, diagnostic, loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr), stdout, stderr)
}

func doCityStatusJSONWithDiagnosticAndSnapshot(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	diagnostic *beads.BeadsDiagnostic,
	statusSnapshot *sessionBeadSnapshot,
	stdout, stderr io.Writer,
) int {
	snapshot := collectCityStatusSnapshotFromStoreSnapshot(sp, cfg, cityPath, store, statusSnapshot, stderr)
	snapshot.Beads = diagnostic
	// Track session-snapshot degradation so we can emit the JSON payload AND
	// signal the failure via exit code. Restores the pre-#2005 contract that
	// monitoring callers rely on (see #2147).
	snapshotDegraded := statusSnapshot.LoadError() != nil
	if store != nil {
		sessions, err := collectCitySessionCounts(cityPath, store, sp, cfg, statusSnapshot)
		if err != nil {
			fmt.Fprintf(stderr, "gc status: building session catalog: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		snapshot.Summary.ActiveSessions = sessions.ActiveSessions
		snapshot.Summary.SuspendedSessions = sessions.SuspendedSessions
	}

	status := cityStatusJSONFromSnapshot(snapshot, snapshot.Summary)
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
	if snapshotDegraded {
		return 1
	}
	return 0
}

func controllerStatusForCity(cityPath string) ControllerJSON {
	_, registered, err := registeredCityEntry(cityPath)
	supervisorWasAlive := false
	if err == nil && registered {
		ctrl := ControllerJSON{Mode: "supervisor"}
		if pid := supervisorAliveHook(); pid != 0 {
			supervisorWasAlive = true
			ctrl.PID = pid
			if running, status, known := supervisorCityRunningHook(cityPath); known {
				ctrl.Running = running
				ctrl.Status = status
				return ctrl
			}
			if supervisorAliveHook() != 0 {
				ctrl.Status = "unknown"
				return ctrl
			}
		}
	}
	if supervisorWasAlive {
		if pid := controllerAliveWithin(cityPath, controllerStatusStandaloneFallbackTimeout); pid != 0 {
			return ControllerJSON{Running: true, PID: pid, Mode: "supervisor"}
		}
	}
	if pid := controllerAlive(cityPath); pid != 0 {
		return ControllerJSON{Running: true, PID: pid, Mode: "standalone"}
	}
	if err == nil && registered {
		return ControllerJSON{Mode: "supervisor"}
	}
	return ControllerJSON{}
}

func controllerAliveWithin(cityPath string, timeout time.Duration) int {
	if timeout <= 0 {
		return controllerAlive(cityPath)
	}
	deadline := time.Now().Add(timeout)
	for {
		if pid := controllerAlive(cityPath); pid != 0 {
			return pid
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func controllerSupervisorStatusText(status string) string {
	switch status {
	case "":
		return "city stopped"
	case "loading_config":
		return "loading configuration"
	case "starting_bead_store":
		return "starting bead store"
	case "resolving_formulas":
		return "resolving formulas"
	case "adopting_sessions":
		return "adopting sessions"
	case "starting_agents":
		return "starting agents"
	case "init_failed":
		return "init failed"
	default:
		return strings.ReplaceAll(status, "_", " ")
	}
}

func controllerStatusLine(ctrl ControllerJSON) string {
	switch ctrl.Mode {
	case "supervisor":
		if ctrl.Running {
			if ctrl.PID == 0 {
				return "supervisor-managed"
			}
			return fmt.Sprintf("supervisor-managed (PID %d)", ctrl.PID)
		}
		if ctrl.PID == 0 && ctrl.Status != "" {
			return "supervisor-managed (city " + strings.ReplaceAll(ctrl.Status, "_", " ") + ")"
		}
		if ctrl.PID != 0 {
			return fmt.Sprintf("supervisor-managed (PID %d, %s)", ctrl.PID, controllerSupervisorStatusText(ctrl.Status))
		}
		return "supervisor-managed (supervisor not running)"
	case "standalone":
		if ctrl.Running {
			if ctrl.PID == 0 {
				return "standalone-managed"
			}
			return fmt.Sprintf("standalone-managed (PID %d)", ctrl.PID)
		}
	}
	return "stopped"
}

func controllerStatusGuidance(ctrl ControllerJSON, cityPath string) []string {
	quotedPath := shellQuotePath(cityPath)
	startCommand := "gc start " + quotedPath

	switch ctrl.Mode {
	case "standalone":
		if !ctrl.Running {
			return nil
		}
		authority := "Authority: standalone controller"
		if ctrl.PID != 0 {
			authority = fmt.Sprintf("Authority: standalone controller PID %d", ctrl.PID)
		}
		return []string{
			authority,
			"Next: gc stop " + quotedPath + " && " + startCommand + " to hand ownership to the supervisor",
		}
	case "supervisor":
		if ctrl.Running {
			authority := "Authority: supervisor API"
			if ctrl.PID != 0 {
				authority = fmt.Sprintf("Authority: supervisor process PID %d", ctrl.PID)
			}
			return []string{authority}
		}
		if ctrl.PID == 0 && ctrl.Status != "" {
			return []string{
				"Authority: supervisor API; city status: " + ctrl.Status,
				"Next: " + startCommand + " to ask the supervisor to start this city",
			}
		}
		if ctrl.PID == 0 {
			return []string{
				"Authority: supervisor registry; no supervisor process is running",
				"Next: " + startCommand + " to start the supervisor and reconcile this city",
			}
		}
		lines := []string{fmt.Sprintf("Authority: supervisor process PID %d", ctrl.PID)}
		if ctrl.Status == "" || ctrl.Status == "unknown" {
			return append(lines, "Next: "+startCommand+" to ask the supervisor to start this city")
		}
		if ctrl.Status == "init_failed" {
			return append(lines, "Next: gc supervisor logs to see the init failure")
		}
		return append(lines, "Next: gc supervisor logs to inspect startup progress")
	}
	return nil
}
