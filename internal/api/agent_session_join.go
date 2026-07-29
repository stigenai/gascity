package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/session"
)

// liveAgentSession is the slice of a live session's identity the agent
// handlers need in order to report a pool slot as running: the runtime name to
// probe and the persisted state to fall back on when the runtime probe itself
// is unavailable.
type liveAgentSession struct {
	sessionName string
	state       session.State
	id          string
}

// agentSessionIdentities returns the qualified agent identities a live session
// may be occupying, most authoritative first.
//
// The precedence mirrors statusSessionQualifiedName: AgentName is the identity
// the runtime recorded for the session, and it is only meaningful when it says
// something the template does not already say. Alias is added because that is
// where the pool spawner records the *slot* a bounded-pool session occupies
// (e.g. "rig/pack.archivist-1"), which is exactly the key the agent roster is
// enumerated under. The sanitized-session-name reversal is the last resort and
// matches what discoverUnlimitedPool does for unlimited pools.
func agentSessionIdentities(cityName, sessTmpl string, info session.Info) []string {
	template := strings.TrimSpace(info.Template)

	var keys []string
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		// A key equal to the template is not a slot identity — the template is
		// the *pool*, and attributing a session to it would let one worker
		// masquerade as the whole pool.
		if candidate == "" || candidate == template {
			return
		}
		for _, existing := range keys {
			if existing == candidate {
				return
			}
		}
		keys = append(keys, candidate)
	}

	add(info.AgentName)
	add(info.Alias)

	runtimeName := strings.TrimSpace(info.SessionName)
	if runtimeName == "" {
		runtimeName = strings.TrimSpace(info.SessionNameMetadata)
	}
	if runtimeName != "" {
		qnSanitized := runtimeName
		if templatePrefix := agent.SessionNameFor(cityName, "", sessTmpl); templatePrefix != "" &&
			strings.HasPrefix(qnSanitized, templatePrefix) {
			qnSanitized = qnSanitized[len(templatePrefix):]
		}
		add(agent.UnsanitizeQualifiedNameFromSession(qnSanitized))
	}

	return keys
}

// agentLiveSessionIndex indexes non-terminal sessions by the qualified agent
// identity each one occupies.
//
// This exists because the agent roster and the session runtime disagree on how
// a bounded-pool member is named. The roster enumerates deterministic slots
// ("<pool>-1".."<pool>-N", see expandAgent) and probes the tmux name derived
// from the slot; the runtime names an ephemeral pool session after its session
// id and without the rig prefix ("pack__archivist-de-o44"). The two never
// match, so before this index every bounded-pool slot reported stopped while
// its worker was executing — only singletons whose deterministic name happens
// to equal their runtime name (mayor) ever reported running. See #4703.
//
// Returns nil when there is no session store or the read fails; callers treat a
// nil index as "no fallback available" and keep the deterministic answer, so a
// degraded store can never turn a running agent into a *wrong* claim.
func (s *Server) agentLiveSessionIndex(cityName, sessTmpl string) map[string]liveAgentSession {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil
	}
	infos, _, err := sessionReadModelInfos(session.NewStore(store))
	if err != nil {
		return nil
	}

	index := make(map[string]liveAgentSession, len(infos))
	cfg := s.state.Config()
	for _, info := range infos {
		if info.Closed {
			continue
		}
		state := statusSessionStateInfo(info)
		if state == session.StateArchived {
			continue
		}

		runtimeName := strings.TrimSpace(info.SessionName)
		if runtimeName == "" {
			runtimeName = strings.TrimSpace(info.SessionNameMetadata)
		}
		if runtimeName == "" {
			continue
		}

		live := liveAgentSession{
			sessionName: runtimeName,
			state:       state,
			id:          strings.TrimSpace(info.ID),
		}
		keys := agentSessionIdentities(cityName, sessTmpl, info)
		// A singleton's template is its agent identity. Pool templates are
		// deliberately excluded because one pool session must never
		// masquerade as every slot, but retaining the singleton identity lets
		// the roster use the durable projection without probing Herdr once per
		// configured agent.
		if cfg != nil {
			if configured, ok := findAgent(cfg, info.Template); ok &&
				!isMultiSessionAgent(configured) {
				keys = append(keys, strings.TrimSpace(info.Template))
			}
		}
		for _, key := range keys {
			if key == "" {
				continue
			}
			// The read model is sorted created-desc, so the first session to
			// claim a slot is the newest one holding it. Later (older) sessions
			// for the same slot must not overwrite it.
			if _, taken := index[key]; taken {
				continue
			}
			index[key] = live
		}
	}
	return index
}

// resolveAgentRuntimeProjected resolves an agent roster row from the durable
// session read model, with a single runtime snapshot as a degraded-store
// fallback. Agent-list requests are a dashboard projection, not a liveness
// scan: probing the runtime once per configured agent made a 40-row roster
// issue 80+ synchronous Herdr calls and take 30-40 seconds. The controller
// already owns runtime fencing and continuously projects its answer into
// session state, so the roster consumes that bounded projection while the
// single-agent detail endpoint retains its direct live probe.
func resolveAgentRuntimeProjected(
	deterministicName string,
	qualifiedName string,
	lookup func() map[string]liveAgentSession,
	runtimeLookup func() map[string]struct{},
) (string, bool) {
	if lookup != nil {
		index := lookup()
		if live, ok := index[qualifiedName]; ok {
			return live.sessionName, live.state == session.StateActive
		}
		// A non-nil projection is authoritative. Do not turn a missing
		// projected session into N per-agent runtime probes.
		if index != nil {
			return deterministicName, false
		}
	}
	if runtimeLookup == nil {
		return deterministicName, false
	}
	_, running := runtimeLookup()[deterministicName]
	return deterministicName, running
}

// resolveAgentRuntime returns the runtime session name to use for an agent slot
// and whether that slot is running.
//
// The deterministic name is probed first, so the common singleton path costs
// exactly what it did before and needs no session-store read. Only on a miss is
// the lazily-built index consulted, via lookup, which memoizes for the caller.
func resolveAgentRuntime(
	sp interface{ IsRunning(string) bool },
	deterministicName string,
	qualifiedName string,
	lookup func() map[string]liveAgentSession,
) (string, bool) {
	if sp != nil && sp.IsRunning(deterministicName) {
		return deterministicName, true
	}
	if lookup == nil {
		return deterministicName, false
	}
	live, ok := lookup()[qualifiedName]
	if !ok {
		return deterministicName, false
	}
	// Prefer the runtime probe against the *correct* name; fall back to the
	// persisted state so the roster still reflects a live worker when the tmux
	// probe is unavailable (which is precisely when the roster matters most).
	if sp != nil && sp.IsRunning(live.sessionName) {
		return live.sessionName, true
	}
	return live.sessionName, live.state == session.StateActive
}

// memoizedAgentSessionIndex wraps agentLiveSessionIndex so a single request
// pays for at most one session-store read no matter how many slots miss.
func (s *Server) memoizedAgentSessionIndex(cityName, sessTmpl string) func() map[string]liveAgentSession {
	var (
		built bool
		index map[string]liveAgentSession
	)
	return func() map[string]liveAgentSession {
		if !built {
			index = s.agentLiveSessionIndex(cityName, sessTmpl)
			built = true
		}
		return index
	}
}

// memoizedAgentRuntimeIndex is the degraded-store fallback for the roster. It
// snapshots runtime names with one ListRunning call instead of issuing one
// process-aware IsRunning probe per configured agent. A healthy durable session
// projection remains authoritative and never calls this lookup.
func memoizedAgentRuntimeIndex(sp sessionLister) func() map[string]struct{} {
	var (
		built bool
		index map[string]struct{}
	)
	return func() map[string]struct{} {
		if built {
			return index
		}
		built = true
		if sp == nil {
			return nil
		}
		names, err := sp.ListRunning("")
		if err != nil {
			return nil
		}
		index = make(map[string]struct{}, len(names))
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				index[name] = struct{}{}
			}
		}
		return index
	}
}
