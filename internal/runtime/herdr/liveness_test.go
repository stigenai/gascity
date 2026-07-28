package herdr

import (
	"errors"
	"testing"
)

// TestAgentAliveFromStatus pins the core of the claude-2.1.x singleton
// restart-loop fix: liveness is derived from herdr's own agent_status, so an
// active status (idle/working/done/…) reads alive without any dependency on
// matching a process name or reading GC_SESSION_ID out of the agent's
// environment. Only an explicit terminal status reads dead, and unknown/empty
// statuses fail safe toward alive (a live singleton misread as dead is the
// destructive direction this fix exists to prevent).
func TestAgentAliveFromStatus(t *testing.T) {
	alive := []string{"idle", "working", "done", "running", "Idle", "  WORKING ", "thinking", "busy", "", "  "}
	for _, s := range alive {
		if !agentAliveFromStatus(s) {
			t.Errorf("agentAliveFromStatus(%q) = false; want true (active/unknown status must read alive)", s)
		}
	}
	dead := []string{"exited", "stopped", "dead", "gone", "terminated", "closed", "crashed", "EXITED", " Stopped "}
	for _, s := range dead {
		if agentAliveFromStatus(s) {
			t.Errorf("agentAliveFromStatus(%q) = true; want false (terminal status must read dead)", s)
		}
	}
}

// TestLivenessFromAgentAbsentOrError is the required negative case: a failed
// `agent get` or an agent herdr reports absent must be neither running nor
// alive, so a genuinely-gone session is still eligible for restart.
func TestLivenessFromAgentAbsentOrError(t *testing.T) {
	if got := livenessFromAgent(agentInfo{}, false, nil); got.Running || got.Alive {
		t.Errorf("absent agent: got %+v; want Running=false Alive=false", got)
	}
	if got := livenessFromAgent(agentInfo{AgentStatus: "idle"}, false, errors.New("herdr transport failure")); got.Running || got.Alive {
		t.Errorf("query error: got %+v; want Running=false Alive=false", got)
	}
}

// TestLivenessFromAgentPresent covers the incident case: a present, idle
// singleton mayor must report Running=true and Alive=true (pre-fix it reported
// Alive=false via the process-table walk and looped). A present agent herdr
// reports terminal reads running-but-dead so the reconciler can restart it.
func TestLivenessFromAgentPresent(t *testing.T) {
	live := livenessFromAgent(agentInfo{Name: "mayor", AgentStatus: "idle"}, true, nil)
	if !live.Running || !live.Alive {
		t.Errorf("present idle mayor: got %+v; want Running=true Alive=true", live)
	}
	terminal := livenessFromAgent(agentInfo{Name: "mayor", AgentStatus: "exited"}, true, nil)
	if !terminal.Running || terminal.Alive {
		t.Errorf("present exited agent: got %+v; want Running=true Alive=false", terminal)
	}
}

// TestLivenessFromAgentAndPaneRejectsStaleRegistryEntry reproduces the
// production Herdr restart failure: agent list/get retained an idle registry
// row after the old pane disappeared. Trusting agent_status alone kept the
// missing runtime active forever, consumed the pool's max slot, and prevented
// routed work from starting a replacement.
func TestLivenessFromAgentAndPaneRejectsStaleRegistryEntry(t *testing.T) {
	info := agentInfo{Name: "worker", PaneID: "w2:p3G", AgentStatus: "idle"}

	missing := livenessFromAgentAndPane(info, true, nil, paneProbe{}, nil)
	if missing.Running || missing.Alive {
		t.Errorf("confirmed-missing pane: got %+v; want Running=false Alive=false", missing)
	}

	bareShell := livenessFromAgentAndPane(info, true, nil, paneProbe{Exists: true, Busy: false}, nil)
	if bareShell.Running || bareShell.Alive {
		t.Errorf("bare-shell pane: got %+v; want Running=false Alive=false", bareShell)
	}

	live := livenessFromAgentAndPane(info, true, nil, paneProbe{Exists: true, Busy: true}, nil)
	if !live.Running || !live.Alive {
		t.Errorf("busy pane: got %+v; want Running=true Alive=true", live)
	}

	probeErr := errors.New("herdr transport failure")
	uncertain := livenessFromAgentAndPane(info, true, nil, paneProbe{}, probeErr)
	if !uncertain.Running || !uncertain.Alive {
		t.Errorf("probe uncertainty: got %+v; want fail-safe Running=true Alive=true", uncertain)
	}
}
