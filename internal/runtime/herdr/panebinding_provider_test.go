package herdr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ── provider-level pane-binding behavior against a fake herdr 0.7.5 ──────────
//
// The fake herdr is a shell script modeling the ≥0.7.5 contract: `agent start`
// launches a supported kind into an existing shell pane and registers the
// name; the name exists only while the agent runs (state file "registered");
// raw commands are typed into the pane and never register anything. State
// files drive the scenario (registered / pane_gone / busy), and calls.log
// records every verb so tests can assert what was — and crucially was NOT —
// issued (the spawn storm was one placement per reconcile tick).

var paneBindSession int64

// newFakeHerdrProvider builds a Provider whose client shells out to a fake
// herdr script. Returns the provider, its session name, and the state dir.
func newFakeHerdrProvider(t *testing.T) (*Provider, string, string) {
	t.Helper()
	session := fmt.Sprintf("gctest-pb-%d-%d", os.Getpid(), atomic.AddInt64(&paneBindSession, 1))
	state := t.TempDir()
	metaDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "herdr")
	fake := `#!/bin/sh
STATE='` + state + `'
METADIR='` + metaDir + `'
shift 2
printf '%s\n' "$*" >> "$STATE/calls.log"
case "$1_$2" in
agent_get)
  if [ -e "$STATE/agent_transport_not_found" ]; then
    printf '%s' 'transport target not found' >&2
    exit 1
  elif [ -e "$STATE/agent_exit_empty" ]; then
    exit 1
  elif [ -e "$STATE/agent_exit_malformed" ]; then
    printf '%s' '{' >&2
    exit 1
  elif [ -e "$STATE/agent_exit_empty_object" ]; then
    printf '%s' '{}' >&2
    exit 1
  elif [ -e "$STATE/agent_exit_null_error" ]; then
    printf '%s' '{"error":null}' >&2
    exit 1
  elif [ -e "$STATE/agent_exit_missing_code" ]; then
    printf '%s' '{"error":{"message":"missing code"}}' >&2
    exit 1
  elif [ -e "$STATE/agent_exit_missing_message" ]; then
    printf '%s' '{"error":{"code":"agent_not_found"}}' >&2
    exit 1
  elif [ -e "$STATE/agent_exit_result_only" ]; then
    printf '%s' '{"result":{"agent":{"name":"worker","pane_id":"%5"}}}' >&2
    exit 1
  elif [ -e "$STATE/agent_exit_mixed" ]; then
    printf '%s' '{"result":{},"error":{"code":"agent_not_found","message":"mixed"}}' >&2
    exit 1
  elif [ -e "$STATE/agent_exit_stdout_result" ]; then
    printf '%s' '{"result":{"agent":{"name":"worker","pane_id":"%5"}}}'
    exit 1
  elif [ -e "$STATE/agent_decode_error" ]; then
    printf '%s' '{"result":"malformed-agent"}'
  elif [ -e "$STATE/agent_get_missing_agent" ]; then
    printf '%s' '{"result":{}}'
  elif [ -e "$STATE/agent_get_null_agent" ]; then
    printf '%s' '{"result":{"agent":null}}'
  elif [ -e "$STATE/agent_get_empty_pane" ]; then
    printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":""}}}'
  elif [ -e "$STATE/registered" ]; then
    printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"idle"}}}'
  else
    printf '%s' '{"error":{"code":"agent_not_found","message":"agent target not found"},"id":"cli:agent:get"}' >&2
    exit 1
  fi ;;
agent_list)
  if [ -e "$STATE/agent_list_missing_agents" ]; then
    printf '%s' '{"result":{}}'
  elif [ -e "$STATE/agent_list_null_agents" ]; then
    printf '%s' '{"result":{"agents":null}}'
  elif [ -e "$STATE/agent_list_empty_name" ]; then
    printf '%s' '{"result":{"agents":[{"name":"","pane_id":"%5"}]}}'
  elif [ -e "$STATE/agent_list_empty_pane" ]; then
    printf '%s' '{"result":{"agents":[{"name":"worker","pane_id":""}]}}'
  else
    printf '%s' '{"result":{"agents":[]}}'
  fi ;;
agent_start)
  if [ -e "$STATE/agent_start_name_taken_once" ] && [ ! -e "$STATE/agent_start_name_taken_seen" ]; then
    : > "$STATE/agent_start_name_taken_seen"
    printf '%s' '{"error":{"code":"agent_name_taken","message":"agent name already used"},"id":"cli:agent:start"}' >&2
    exit 1
  fi
  if [ -e "$STATE/agent_start_missing_agent" ]; then
    printf '%s' '{"result":{}}'
    exit 0
  elif [ -e "$STATE/agent_start_null_agent" ]; then
    printf '%s' '{"result":{"agent":null}}'
    exit 0
  elif [ -e "$STATE/agent_start_empty_pane" ]; then
    printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":""}}}'
    exit 0
  fi
  : > "$STATE/agent_started"
  : > "$STATE/registered"
  : > "$STATE/busy"
  if [ -e "$METADIR/$3/GC_SESSION_ID" ]; then : > "$STATE/meta_seeded_before_launch"; fi
  if [ -e "$METADIR/$3/GC_HERDR_PANE_ID" ]; then : > "$STATE/bound_before_launch"; fi
  printf '%s' '{"result":{"agent":{"name":"'"$3"'","pane_id":"%5","tab_id":"t1","workspace_id":"w1","agent_status":"idle"}}}' ;;
agent_wait)
  printf '%s' '{"result":{"agent":{"name":"'"$3"'","agent_status":"idle"}}}' ;;
agent_prompt)
  if [ -e "$STATE/agent_prompt_transport_not_found" ]; then
    printf '%s' 'transport endpoint not found' >&2
    exit 1
  elif [ -e "$STATE/agent_prompt_empty_envelope" ]; then
    printf '%s' '{}'
  elif [ -e "$STATE/agent_prompt_pane_gone" ]; then
    printf '%s' '{"error":{"code":"pane_not_found","message":"pane disappeared"}}' >&2
    exit 1
  elif [ -e "$STATE/agent_prompt_agent_gone" ]; then
    printf '%s' '{"error":{"code":"agent_not_found","message":"agent disappeared"}}' >&2
    exit 1
  elif [ -e "$STATE/registered" ]; then
    : > "$STATE/prompted"
    printf '%s' '{"result":{"type":"agent_prompted"}}'
  else
    printf '%s' '{"error":{"code":"agent_not_found","message":"agent target not found"}}' >&2
    exit 1
  fi ;;
pane_run)
  if [ -e "$STATE/pane_run_agent_gone" ]; then
    printf '%s' '{"error":{"code":"agent_not_found","message":"agent disappeared"}}' >&2
    exit 1
  else
    : > "$STATE/busy"
    printf '%s' "$4" | sed -e 's|^exec /bin/sh -c ||' -e "s/^'//" -e "s/'\$//" > "$STATE/rawcmd"
    exit 0
  fi ;;
pane_process-info)
  if [ -e "$STATE/pane_transport_not_found" ]; then
    printf '%s' 'transport endpoint not found' >&2
    exit 1
  elif [ -e "$STATE/pane_transport_not_found_once" ] && [ ! -e "$STATE/pane_transport_seen" ]; then
    : > "$STATE/pane_transport_seen"
    printf '%s' 'transport endpoint not found' >&2
    exit 1
  elif [ -e "$STATE/pane_gone" ]; then
    printf '%s' '{"error":{"code":"pane_not_found","message":"pane not found"},"id":"cli:pane:process-info"}' >&2
    exit 1
  elif [ -e "$STATE/pane_decode_error" ]; then
    printf '%s' '{"result":"malformed-process-info"}'
  elif [ -e "$STATE/pane_missing_process_info" ]; then
    printf '%s' '{"result":{}}'
  elif [ -e "$STATE/pane_null_process_info" ]; then
    printf '%s' '{"result":{"process_info":null}}'
  elif [ -e "$STATE/pane_missing_shell_pid" ]; then
    printf '%s' '{"result":{"process_info":{"foreground_processes":[]}}}'
  elif [ -e "$STATE/pane_missing_foreground_processes" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":4242}}}'
  elif [ -e "$STATE/pane_null_foreground_processes" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":null}}}'
  elif [ -e "$STATE/pane_malformed_foreground_process" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{}]}}}'
  elif [ -e "$STATE/pane_empty_foreground_processes" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}'
  elif [ -e "$STATE/pane_zero_shell_pid" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":0,"foreground_processes":[]}}}'
  elif [ -e "$STATE/pane_zero_shell_with_foreground" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":0,"foreground_processes":[{"pid":4243,"name":"claude"}]}}}'
  elif [ -e "$STATE/rawcmd" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4242,"name":"bash","argv":["/bin/sh","-c","'"$(cat "$STATE/rawcmd")"'"]}]}}}'
  elif [ -e "$STATE/busy" ]; then
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4243,"name":"claude"}]}}}'
  else
    printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4242,"name":"zsh"}]}}}'
  fi ;;
pane_close)
  if [ -e "$STATE/pane_close_transport_not_found" ]; then
    printf '%s' 'transport endpoint not found' >&2
    exit 1
  elif [ -e "$STATE/pane_close_gone" ]; then
    printf '%s' '{"error":{"code":"pane_not_found","message":"pane not found"},"id":"cli:pane:close"}' >&2
    exit 1
  else
    : > "$STATE/pane_closed"
    printf '%s' '{"result":{}}'
  fi ;;
workspace_list)
  : > "$STATE/placement_attempted"
  if [ -e "$STATE/workspace_list_missing_workspaces" ]; then
    printf '%s' '{"result":{}}'
  elif [ -e "$STATE/workspace_list_null_workspaces" ]; then
    printf '%s' '{"result":{"workspaces":null}}'
  elif [ -e "$STATE/workspace_list_empty_id" ]; then
    printf '%s' '{"result":{"workspaces":[{"workspace_id":"","label":"rig"}]}}'
  elif [ -e "$STATE/workspace_list_missing_label" ]; then
    printf '%s' '{"result":{"workspaces":[{"workspace_id":"w1"}]}}'
  elif [ -e "$STATE/workspace_list_empty_label" ]; then
    printf '%s' '{"result":{"workspaces":[{"workspace_id":"w1","label":""}]}}'
  elif [ -e "$STATE/workspace_exists" ]; then
    printf '%s' '{"result":{"workspaces":[{"workspace_id":"w1","label":"gastown"}]}}'
  else
    printf '%s' '{"result":{"workspaces":[]}}'
  fi ;;
workspace_create)
  if [ -e "$STATE/workspace_create_incomplete" ]; then
    printf '%s' '{"result":{"workspace":{"workspace_id":"w1"},"tab":null,"root_pane":{"pane_id":""}}}'
  else
    printf '%s' '{"result":{"workspace":{"workspace_id":"w1"},"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"%5"}}}'
  fi ;;
tab_list)
  if [ -e "$STATE/tab_list_missing_tabs" ]; then
    printf '%s' '{"result":{}}'
  elif [ -e "$STATE/tab_list_null_tabs" ]; then
    printf '%s' '{"result":{"tabs":null}}'
  elif [ -e "$STATE/tab_list_empty_id" ]; then
    printf '%s' '{"result":{"tabs":[{"tab_id":"","label":"worker"}]}}'
  elif [ -e "$STATE/tab_list_missing_label" ]; then
    printf '%s' '{"result":{"tabs":[{"tab_id":"t1"}]}}'
  elif [ -e "$STATE/tab_list_empty_label" ]; then
    printf '%s' '{"result":{"tabs":[{"tab_id":"t1","label":""}]}}'
  elif [ -e "$STATE/stale_tabs" ]; then
    printf '%s' '{"result":{"tabs":[{"tab_id":"t-old1","label":"witness"},{"tab_id":"t-old2","label":"witness"},{"tab_id":"t-other","label":"deacon"}]}}'
  else
    printf '%s' '{"result":{"tabs":[]}}'
  fi ;;
tab_create)
  if [ -e "$STATE/tab_create_incomplete" ]; then
    printf '%s' '{"result":{"tab":null,"root_pane":{"pane_id":""}}}'
  else
    printf '%s' '{"result":{"tab":{"tab_id":"t1"},"root_pane":{"pane_id":"%5"}}}'
  fi ;;
*)
  exit 0 ;;
esac
`
	if err := os.WriteFile(script, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New(session, metaDir, t.TempDir(), time.Second, time.Second)
	p.c.bin = script
	return p, session, state
}

// fakeCalls returns the verbs the fake herdr recorded.
func fakeCalls(t *testing.T, state string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(state, "calls.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func setState(t *testing.T, state, flag string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, flag), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// listenHerdrSocket plants a live unix listener at the session's socket path so
// ConfigureServer's serverAlive dial succeeds without launching a real server.
func listenHerdrSocket(t *testing.T, session string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".config", "herdr", "sessions", session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(dir, "herdr.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = l.Close()
		_ = os.RemoveAll(dir)
	})
}

// bindTestPane seeds the sidecar with the binding Start would have persisted
// (the fake herdr always reports pane "%5").
func bindTestPane(t *testing.T, p *Provider, name, mode string) {
	t.Helper()
	if err := p.SetMeta(name, metaBoundPane, "%5"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta(name, metaBoundMode, mode); err != nil {
		t.Fatal(err)
	}
}

func TestRunDecodesTypedErrorEnvelopeFromNonzeroStderr(t *testing.T) {
	t.Run("agent_not_found", func(t *testing.T) {
		p, _, _ := newFakeHerdrProvider(t)
		_, err := p.c.run(context.Background(), "agent", "get", "missing")
		if got := herdrErrorCode(err); got != "agent_not_found" || errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("run error = %v, code %q; want typed agent_not_found only", err, got)
		}
		if _, present, err := p.c.getAgent(context.Background(), "missing"); err != nil || present {
			t.Fatalf("getAgent = present %v, err %v; want typed absence", present, err)
		}
	})

	t.Run("pane_not_found", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "pane_gone")
		_, err := p.c.run(context.Background(), "pane", "process-info", "--pane", "%5")
		if got := herdrErrorCode(err); got != "pane_not_found" || errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("run error = %v, code %q; want typed pane_not_found only", err, got)
		}
		if probe, err := p.probePane(context.Background(), "%5"); err != nil || probe != (paneProbe{}) {
			t.Fatalf("probePane = %+v, %v; want confirmed typed absence", probe, err)
		}
	})

	t.Run("agent_name_taken", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "agent_start_name_taken_once")
		_, err := p.c.startAgentKind(context.Background(), "worker", "claude", "%5", nil)
		if got := herdrErrorCode(err); got != "agent_name_taken" || errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("startAgentKind error = %v, code %q; want typed agent_name_taken only", err, got)
		}
	})

	t.Run("transport prose", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "agent_transport_not_found")
		_, err := p.c.run(context.Background(), "agent", "get", "missing")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || herdrErrorCode(err) != "" {
			t.Fatalf("run transport error = %v; want ErrRuntimeUnavailable without Herdr code", err)
		}
	})

	t.Run("invalid nonzero envelopes", func(t *testing.T) {
		for _, flag := range []string{
			"agent_exit_empty",
			"agent_exit_malformed",
			"agent_exit_empty_object",
			"agent_exit_null_error",
			"agent_exit_missing_code",
			"agent_exit_missing_message",
			"agent_exit_result_only",
			"agent_exit_mixed",
			"agent_exit_stdout_result",
		} {
			t.Run(flag, func(t *testing.T) {
				p, _, state := newFakeHerdrProvider(t)
				setState(t, state, flag)
				_, err := p.c.run(context.Background(), "agent", "get", "missing")
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) || herdrErrorCode(err) != "" {
					t.Fatalf("run invalid exit error = %v; want ErrRuntimeUnavailable without Herdr code", err)
				}
			})
		}
	})
}

// Only Herdr's typed resource error proves absence. Transport, wrapper, and
// decode errors can contain human text such as "not found" while the pane or
// agent is still live; treating that prose as absence is destructive because
// ObserveLiveness reaps a supposedly missing pane.
func TestTypedNotFoundDoesNotCollapseTransportFailures(t *testing.T) {
	t.Run("pane probe", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "pane_transport_not_found")
		bindTestPane(t, p, "gastown__witness", bindModeAgent)

		if _, err := p.probePane(context.Background(), "%5"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("probePane error = %v; want ErrRuntimeUnavailable", err)
		}
		if got := p.ObserveLiveness("gastown__witness", nil); !got.Running || !got.Alive {
			t.Fatalf("ObserveLiveness = %+v on transport failure; want fail-safe live verdict", got)
		}
		if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "%5" {
			t.Fatalf("transport failure cleared live binding: %q", got)
		}
		if calls := fakeCalls(t, state); strings.Contains(calls, "pane close %5") {
			t.Fatalf("transport failure reaped pane:\n%s", calls)
		}
	})

	t.Run("agent get", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "agent_transport_not_found")
		if _, present, err := p.c.getAgent(context.Background(), "witness"); !errors.Is(err, runtime.ErrRuntimeUnavailable) || present {
			t.Fatalf("getAgent = present %v, err %v; want ErrRuntimeUnavailable", present, err)
		}
		if got := p.ObserveLiveness("gastown__witness", nil); !got.Running || !got.Alive {
			t.Fatalf("ObserveLiveness = %+v on agent transport failure; want fail-safe live verdict", got)
		}
	})

	t.Run("decode", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "agent_decode_error")
		if _, _, err := p.c.getAgent(context.Background(), "witness"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("getAgent decode error = %v; want ErrRuntimeUnavailable", err)
		}
		if got := p.ObserveLiveness("gastown__witness", nil); !got.Running || !got.Alive {
			t.Fatalf("ObserveLiveness = %+v on decode failure; want fail-safe live verdict", got)
		}

		p2, _, state2 := newFakeHerdrProvider(t)
		setState(t, state2, "pane_decode_error")
		if _, err := p2.probePane(context.Background(), "%5"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("probePane decode error = %v; want ErrRuntimeUnavailable", err)
		}
	})

	t.Run("well-formed incomplete process info", func(t *testing.T) {
		for _, flag := range []string{
			"pane_missing_process_info",
			"pane_null_process_info",
			"pane_missing_shell_pid",
			"pane_missing_foreground_processes",
			"pane_null_foreground_processes",
			"pane_malformed_foreground_process",
			"pane_zero_shell_with_foreground",
		} {
			t.Run(flag, func(t *testing.T) {
				p, _, state := newFakeHerdrProvider(t)
				setState(t, state, flag)
				bindTestPane(t, p, "gastown__witness", bindModeAgent)
				if _, err := p.probePane(context.Background(), "%5"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("probePane incomplete response error = %v; want ErrRuntimeUnavailable", err)
				}
				if got := p.ObserveLiveness("gastown__witness", nil); !got.Running || !got.Alive {
					t.Fatalf("ObserveLiveness = %+v on incomplete response; want fail-safe live verdict", got)
				}
				if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "%5" {
					t.Fatalf("incomplete response cleared live binding: %q", got)
				}
				if calls := fakeCalls(t, state); strings.Contains(calls, "pane close %5") {
					t.Fatalf("incomplete response reaped pane:\n%s", calls)
				}
			})
		}

		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "pane_zero_shell_pid")
		shellPID, fg, err := p.c.processInfo(context.Background(), "%5")
		if err != nil || shellPID != 0 || len(fg) != 0 {
			t.Fatalf("processInfo explicit zero = shellPID %d, fg %v, err %v; want 0, empty, nil", shellPID, fg, err)
		}

		p2, _, state2 := newFakeHerdrProvider(t)
		setState(t, state2, "pane_empty_foreground_processes")
		shellPID, fg, err = p2.c.processInfo(context.Background(), "%5")
		if err != nil || shellPID != 4242 || len(fg) != 0 {
			t.Fatalf("processInfo explicit empty list = shellPID %d, fg %v, err %v; want 4242, empty, nil", shellPID, fg, err)
		}
	})

	t.Run("agent prompt", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "agent_prompt_transport_not_found")
		if err := p.c.deliverNudge(context.Background(), "%5", "wake"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("deliverNudge error = %v; want ErrRuntimeUnavailable", err)
		}
		if calls := fakeCalls(t, state); strings.Contains(calls, "pane run %5") {
			t.Fatalf("transport failure triggered raw-pane fallback:\n%s", calls)
		}
	})

	t.Run("empty envelope", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "agent_prompt_empty_envelope")
		if err := p.c.deliverNudge(context.Background(), "%5", "wake"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("deliverNudge empty envelope error = %v; want ErrRuntimeUnavailable", err)
		}
		if calls := fakeCalls(t, state); strings.Contains(calls, "pane run %5") {
			t.Fatalf("empty envelope triggered raw-pane fallback:\n%s", calls)
		}
	})
}

func TestRequiredClientResponseShapesFailUnavailable(t *testing.T) {
	t.Run("agent get", func(t *testing.T) {
		for _, flag := range []string{"agent_get_missing_agent", "agent_get_null_agent", "agent_get_empty_pane"} {
			t.Run(flag, func(t *testing.T) {
				p, _, state := newFakeHerdrProvider(t)
				setState(t, state, flag)
				bindTestPane(t, p, "gastown__witness", bindModeAgent)
				if _, present, err := p.c.getAgent(context.Background(), "gastown__witness"); present || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("getAgent = present %v, err %v; want unavailable decode failure", present, err)
				}
				if got := p.ObserveLiveness("gastown__witness", nil); !got.Running || !got.Alive {
					t.Fatalf("ObserveLiveness = %+v on incomplete agent response; want fail-safe live", got)
				}
				if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "%5" {
					t.Fatalf("incomplete agent response cleared binding: %q", got)
				}
				if calls := fakeCalls(t, state); strings.Contains(calls, "pane close") {
					t.Fatalf("incomplete agent response closed pane:\n%s", calls)
				}
			})
		}
	})

	t.Run("agent start", func(t *testing.T) {
		for _, flag := range []string{"agent_start_missing_agent", "agent_start_null_agent", "agent_start_empty_pane"} {
			t.Run(flag, func(t *testing.T) {
				p, _, state := newFakeHerdrProvider(t)
				setState(t, state, flag)
				if _, err := p.c.startAgentKind(context.Background(), "gastown__witness", "claude", "%5", nil); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("startAgentKind incomplete response error = %v; want ErrRuntimeUnavailable", err)
				}
			})
		}
	})

	t.Run("agent list", func(t *testing.T) {
		for _, flag := range []string{"agent_list_missing_agents", "agent_list_null_agents", "agent_list_empty_name", "agent_list_empty_pane"} {
			t.Run(flag, func(t *testing.T) {
				p, _, state := newFakeHerdrProvider(t)
				setState(t, state, flag)
				if _, err := p.ListRunning(""); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("ListRunning incomplete response error = %v; want ErrRuntimeUnavailable", err)
				}
			})
		}
		p, _, _ := newFakeHerdrProvider(t)
		if got, err := p.c.listAgents(context.Background()); err != nil || len(got) != 0 {
			t.Fatalf("listAgents explicit empty list = %v, %v; want empty success", got, err)
		}
	})

	t.Run("placement", func(t *testing.T) {
		for _, flag := range []string{"workspace_list_missing_workspaces", "workspace_list_null_workspaces", "workspace_list_empty_id", "workspace_list_missing_label"} {
			t.Run(flag, func(t *testing.T) {
				p, _, state := newFakeHerdrProvider(t)
				setState(t, state, flag)
				if _, err := p.c.findWorkspace(context.Background(), "rig"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("findWorkspace incomplete response error = %v; want ErrRuntimeUnavailable", err)
				}
			})
		}
		for _, flag := range []string{"tab_list_missing_tabs", "tab_list_null_tabs", "tab_list_empty_id", "tab_list_missing_label"} {
			t.Run(flag, func(t *testing.T) {
				p, _, state := newFakeHerdrProvider(t)
				setState(t, state, flag)
				if _, err := p.c.listTabs(context.Background(), "w1"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("listTabs incomplete response error = %v; want ErrRuntimeUnavailable", err)
				}
			})
		}
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "workspace_create_incomplete")
		if _, _, _, err := p.c.workspaceCreate(context.Background(), "rig", "", nil); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("workspaceCreate incomplete response error = %v; want ErrRuntimeUnavailable", err)
		}
		p2, _, state2 := newFakeHerdrProvider(t)
		setState(t, state2, "tab_create_incomplete")
		if _, _, err := p2.c.tabCreate(context.Background(), "w1", "worker", "", nil); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("tabCreate incomplete response error = %v; want ErrRuntimeUnavailable", err)
		}
		p3, _, _ := newFakeHerdrProvider(t)
		if got, err := p3.c.findWorkspace(context.Background(), "rig"); err != nil || got != "" {
			t.Fatalf("findWorkspace explicit empty list = %q, %v; want empty success", got, err)
		}
		if got, err := p3.c.listTabs(context.Background(), "w1"); err != nil || len(got) != 0 {
			t.Fatalf("listTabs explicit empty list = %v, %v; want empty success", got, err)
		}
		p4, _, state4 := newFakeHerdrProvider(t)
		setState(t, state4, "workspace_list_empty_label")
		if got, err := p4.c.findWorkspace(context.Background(), "rig"); err != nil || got != "" {
			t.Fatalf("findWorkspace explicit empty label = %q, %v; want nonmatch success", got, err)
		}
		p5, _, state5 := newFakeHerdrProvider(t)
		setState(t, state5, "tab_list_empty_label")
		if got, err := p5.c.listTabs(context.Background(), "w1"); err != nil || len(got) != 1 || got[0].Label != "" {
			t.Fatalf("listTabs explicit empty label = %v, %v; want one empty-label tab", got, err)
		}
	})
}

func TestNudgePreservesUnavailableAndTypedAbsence(t *testing.T) {
	t.Run("resolution transport failure", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "agent_transport_not_found")
		bindTestPane(t, p, "gastown__witness", bindModeAgent)

		err := p.Nudge("gastown__witness", runtime.TextContent("wake"))
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Nudge error = %v; want ErrRuntimeUnavailable", err)
		}
		if errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("Nudge collapsed transport failure to ErrSessionNotFound: %v", err)
		}
		if calls := fakeCalls(t, state); strings.Contains(calls, "agent prompt") || strings.Contains(calls, "pane run") {
			t.Fatalf("Nudge attempted delivery after failed resolution:\n%s", calls)
		}
	})

	t.Run("delivery transport failure", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "registered")
		setState(t, state, "agent_prompt_transport_not_found")

		err := p.Nudge("gastown__witness", runtime.TextContent("wake"))
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Nudge error = %v; want ErrRuntimeUnavailable", err)
		}
		if errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("Nudge collapsed delivery failure to ErrSessionNotFound: %v", err)
		}
		if calls := fakeCalls(t, state); strings.Contains(calls, "pane run %5") {
			t.Fatalf("transport failure triggered raw-pane fallback:\n%s", calls)
		}
	})

	t.Run("typed absence", func(t *testing.T) {
		p, _, _ := newFakeHerdrProvider(t)
		if err := p.Nudge("gastown__missing", runtime.TextContent("wake")); !errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("Nudge missing session error = %v; want ErrSessionNotFound", err)
		}
	})

	t.Run("pane disappears during delivery", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "registered")
		setState(t, state, "agent_prompt_pane_gone")

		err := p.Nudge("gastown__witness", runtime.TextContent("wake"))
		if !errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Nudge delivery race error = %v; want ErrSessionNotFound only", err)
		}
		if got := herdrErrorCode(err); got != "pane_not_found" {
			t.Fatalf("Nudge lost Herdr cause code: got %q, want pane_not_found", got)
		}
		if calls := fakeCalls(t, state); strings.Contains(calls, "pane run %5") {
			t.Fatalf("pane disappearance triggered raw-pane fallback:\n%s", calls)
		}
	})

	t.Run("agent disappears during fallback delivery", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		setState(t, state, "registered")
		setState(t, state, "agent_prompt_agent_gone")
		setState(t, state, "pane_run_agent_gone")

		err := p.Nudge("gastown__witness", runtime.TextContent("wake"))
		if !errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Nudge fallback race error = %v; want ErrSessionNotFound only", err)
		}
		if got := herdrErrorCode(err); got != "agent_not_found" {
			t.Fatalf("Nudge lost Herdr cause code: got %q, want agent_not_found", got)
		}
	})
}

func TestStartAgentAdoptingPreservesHolderFailures(t *testing.T) {
	tests := []struct {
		name      string
		flag      string
		wantClose bool
	}{
		{name: "get", flag: "agent_transport_not_found"},
		{name: "probe", flag: "pane_transport_not_found"},
		{name: "close", flag: "pane_close_transport_not_found", wantClose: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _, state := newFakeHerdrProvider(t)
			setState(t, state, "registered")
			setState(t, state, "agent_start_name_taken_once")
			setState(t, state, tt.flag)

			_, adopted, err := p.startAgentAdopting(context.Background(), "gastown__witness", "claude", "%5", nil)
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("startAgentAdopting error = %v; want ErrRuntimeUnavailable", err)
			}
			if adopted {
				t.Error("adopted=true after failed holder observation/cleanup")
			}
			calls := fakeCalls(t, state)
			if got := strings.Count(calls, "agent start "); got != 1 {
				t.Fatalf("agent start calls = %d, want 1 (no retry):\n%s", got, calls)
			}
			if got := strings.Contains(calls, "pane close %5"); got != tt.wantClose {
				t.Fatalf("pane close called = %v, want %v:\n%s", got, tt.wantClose, calls)
			}
		})
	}
}

func TestStartAgentAdoptingRetriesAfterTypedPaneAbsence(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	setState(t, state, "agent_start_name_taken_once")
	setState(t, state, "pane_close_gone")

	got, adopted, err := p.startAgentAdopting(context.Background(), "gastown__witness", "claude", "%5", nil)
	if err != nil || adopted || got.PaneID != "%5" {
		t.Fatalf("startAgentAdopting = %+v, adopted=%v, err=%v; want fresh retry", got, adopted, err)
	}
	if calls := fakeCalls(t, state); strings.Count(calls, "agent start ") != 2 {
		t.Fatalf("typed pane absence did not trigger exactly one retry:\n%s", calls)
	}
}

func TestStopPreservesFailuresAndMetadata(t *testing.T) {
	t.Run("resolution transport failure", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		bindTestPane(t, p, "gastown__witness", bindModeAgent)
		setState(t, state, "agent_transport_not_found")

		err := p.Stop("gastown__witness")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Stop error = %v; want ErrRuntimeUnavailable", err)
		}
		if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "%5" {
			t.Fatalf("Stop cleared metadata after failed resolution: %q", got)
		}
		if calls := fakeCalls(t, state); strings.Contains(calls, "pane close") {
			t.Fatalf("Stop attempted close without resolving a pane:\n%s", calls)
		}
	})

	t.Run("reap close transport failure", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		bindTestPane(t, p, "gastown__witness", bindModeAgent)
		setState(t, state, "pane_close_transport_not_found")

		err := p.Stop("gastown__witness")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Stop error = %v; want ErrRuntimeUnavailable", err)
		}
		if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "%5" {
			t.Fatalf("Stop cleared metadata after failed close: %q", got)
		}
	})

	t.Run("running pane close transport failure", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		bindTestPane(t, p, "gastown__witness", bindModeAgent)
		setState(t, state, "busy")
		setState(t, state, "pane_close_transport_not_found")

		err := p.Stop("gastown__witness")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Stop error = %v; want ErrRuntimeUnavailable", err)
		}
		if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "%5" {
			t.Fatalf("Stop cleared metadata after failed close: %q", got)
		}
	})

	t.Run("typed pane absence is idempotent", func(t *testing.T) {
		p, _, state := newFakeHerdrProvider(t)
		bindTestPane(t, p, "gastown__witness", bindModeAgent)
		setState(t, state, "busy")
		setState(t, state, "pane_close_gone")

		if err := p.Stop("gastown__witness"); err != nil {
			t.Fatalf("Stop typed pane_not_found: %v", err)
		}
		if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "" {
			t.Fatalf("Stop retained metadata after confirmed pane absence: %q", got)
		}
	})
}

func TestWaitPaneLaunchedRetriesTransportTextNotFound(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "pane_transport_not_found_once")
	raw := "python3 worker.py --queue main"
	if err := os.WriteFile(filepath.Join(state, "rawcmd"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	p.waitPaneLaunched(context.Background(), "%5", raw)
	if calls := fakeCalls(t, state); strings.Count(calls, "pane process-info --pane %5") < 2 {
		t.Fatalf("waitPaneLaunched inferred absence from transport prose:\n%s", calls)
	}
}

// The storm-killer: with no registry name but the bound pane busy running the
// agent, IsRunning must stay true so the reconciler never re-issues Start.
func TestIsRunningSurvivesNameClearViaPaneBinding(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "busy")
	bindTestPane(t, p, "gastown__witness", bindModeAgent)
	if !p.IsRunning("gastown__witness") {
		t.Fatal("IsRunning = false for a live agent whose name herdr cleared; this is the spawn-storm trigger")
	}
}

// Without a binding, an unregistered name is a genuinely absent session.
func TestIsRunningFalseWhenNameClearedAndNoBinding(t *testing.T) {
	p, _, _ := newFakeHerdrProvider(t)
	if p.IsRunning("gastown__witness") {
		t.Fatal("IsRunning = true with no live name and no pane binding")
	}
}

func TestIsRunningAndStartFailSafeOnObservationFailure(t *testing.T) {
	for _, flag := range []string{"agent_transport_not_found", "agent_get_missing_agent"} {
		t.Run(flag, func(t *testing.T) {
			p, session, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, session)
			setState(t, state, flag)

			if !p.IsRunning("gastown__witness") {
				t.Fatal("IsRunning = false on observation failure; duplicate spawn would be allowed")
			}
			err := p.Start(context.Background(), "gastown__witness", runtime.Config{Command: "claude"})
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("Start error = %v; want ErrRuntimeUnavailable", err)
			}
			calls := fakeCalls(t, state)
			if strings.Contains(calls, "workspace") || strings.Contains(calls, "tab create") || strings.Contains(calls, "agent start") {
				t.Fatalf("Start placed/spawned after failed existence observation:\n%s", calls)
			}
		})
	}
}

// TestIsRunningCheckedReportsInconclusiveOnObservationFailure covers gcy-h6pa:
// unlike IsRunning's fail-safe-to-true bias (proven above by
// TestIsRunningAndStartFailSafeOnObservationFailure), IsRunningChecked must
// return the honest inconclusive signal — a non-nil error, not a guessed
// bool — so callers doing destructive remediation (e.g. doctor's
// zombie-session check) don't mistake a resolve failure for a confirmed
// negative.
func TestIsRunningCheckedReportsInconclusiveOnObservationFailure(t *testing.T) {
	for _, flag := range []string{"agent_transport_not_found", "agent_get_missing_agent"} {
		t.Run(flag, func(t *testing.T) {
			p, session, state := newFakeHerdrProvider(t)
			listenHerdrSocket(t, session)
			setState(t, state, flag)

			running, err := p.IsRunningChecked("gastown__witness")
			if err == nil {
				t.Fatal("IsRunningChecked = nil error on observation failure; inconclusive probe laundered into a confirmed result")
			}
			if running {
				t.Fatal("IsRunningChecked = running true alongside a non-nil (inconclusive) error")
			}
		})
	}
}

// An exited agent — pane back at its bare shell prompt — is NOT running, so
// the reconciler can restart it; a bare-shell session in the same pane state
// IS running (the shell is the session).
func TestIsRunningModeAwareAtShellPrompt(t *testing.T) {
	p, _, _ := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__witness", bindModeAgent)
	if p.IsRunning("gastown__witness") {
		t.Fatal("IsRunning = true for an exited agent (pane at shell prompt); restarts would never happen")
	}
	bindTestPane(t, p, "gastown__shellsess", bindModeShell)
	if !p.IsRunning("gastown__shellsess") {
		t.Fatal("IsRunning = false for a bare-shell session whose pane exists")
	}
}

// Start on a live-but-unregistered session must return ErrSessionExists
// WITHOUT touching placement: each wrongful placement leaked a pane, which is
// the unbounded shell storm.
func TestStartReturnsSessionExistsWithoutPlacementWhenNameCleared(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	setState(t, state, "busy")
	bindTestPane(t, p, "gastown__witness", bindModeAgent)

	err := p.Start(context.Background(), "gastown__witness", runtime.Config{})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start = %v; want ErrSessionExists", err)
	}
	calls := fakeCalls(t, state)
	if strings.Contains(calls, "workspace") || strings.Contains(calls, "agent start") {
		t.Fatalf("Start touched placement/spawn for a live session (the storm):\n%s", calls)
	}
}

// A clean claude command takes the ≥0.7.5 kind-launch path: placement creates
// the shell pane (with cwd baked in), `agent start --kind claude --pane`
// launches into it, and the binding + agent mode are persisted.
func TestStartKindPathRegistersAndPersistsBinding(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)

	cfg := runtime.Config{
		Command: "claude --dangerously-skip-permissions",
		Env:     map[string]string{"GC_SESSION_ID": "sess-1", "GC_INSTANCE_TOKEN": "tok-1"},
	}
	if err := p.Start(context.Background(), "gastown__witness", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "agent start gastown__witness --kind claude --pane %5") {
		t.Fatalf("Start did not kind-launch into the placed pane:\n%s", calls)
	}
	// The kind launch blocks for seconds (readiness wait + TUI detection), so
	// the identity sidecar AND a provisional pane binding must exist BEFORE
	// the launch: reconcile ticks that fire mid-boot read them, and an
	// unseeded sidecar makes the ownership check roll the fresh runtime back
	// ("live runtime belongs to another session").
	if _, err := os.Stat(filepath.Join(state, "meta_seeded_before_launch")); err != nil {
		t.Error("GC_SESSION_ID was not in the sidecar before the agent launch")
	}
	if _, err := os.Stat(filepath.Join(state, "bound_before_launch")); err != nil {
		t.Error("pane binding was not persisted before the agent launch")
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "%5" {
		t.Fatalf("bound pane after Start = %q; want %%5", got)
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundMode); got != bindModeAgent {
		t.Fatalf("bound mode after Start = %q; want %q", got, bindModeAgent)
	}
}

// A non-kind command is exec'd through the pane shell (raw path): no herdr
// agent registration, shell mode persisted, pane still the session handle.
func TestStartRawPathExecsThroughPaneShell(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)

	cfg := runtime.Config{Command: "python3 worker.py --queue main"}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "pane run %5 exec /bin/sh -c ") {
		t.Fatalf("Start did not exec the raw command through the pane shell:\n%s", calls)
	}
	if strings.Contains(calls, "agent start") {
		t.Fatalf("raw command must not attempt a kind launch:\n%s", calls)
	}
	if got, _ := p.GetMeta("gastown__worker", metaBoundMode); got != bindModeShell {
		t.Fatalf("bound mode after raw Start = %q; want %q", got, bindModeShell)
	}
}

// gc session names carrying uppercase rig names must launch under their
// mapped herdr agent name (herdr ≥0.7.5 rejects them verbatim with
// invalid_agent_name — a hot retry loop found live), while the sidecar keeps
// the exact gc name for enumeration.
func TestStartMapsSessionNameToValidHerdrName(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)

	if err := p.Start(context.Background(), "Indigo--anthony", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	if !strings.Contains(calls, "agent start indigo--anthony --kind claude") {
		t.Fatalf("Start did not use the mapped herdr agent name:\n%s", calls)
	}
	if strings.Contains(calls, "agent start Indigo--anthony") {
		t.Fatalf("Start used the raw gc name herdr rejects:\n%s", calls)
	}
	if got, _ := p.GetMeta("Indigo--anthony", metaBoundName); got != "Indigo--anthony" {
		t.Fatalf("sidecar name = %q; want the exact gc name", got)
	}
	// Liveness and enumeration still key on the gc name.
	if !p.IsRunning("Indigo--anthony") {
		t.Fatal("IsRunning(gc name) = false for the running mapped agent")
	}
	if names, err := p.ListRunning("Indigo"); err != nil || len(names) != 1 || names[0] != "Indigo--anthony" {
		t.Fatalf("ListRunning = %v, %v; want [Indigo--anthony]", names, err)
	}
}

// Placement must recycle EVERY stale tab carrying the session's label, not
// just the first: reconciler churn can leave several behind, and a survivor
// lingers forever (its shell pane with it).
func TestStartRecyclesAllStaleTabs(t *testing.T) {
	p, session, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, session)
	setState(t, state, "stale_tabs")
	setState(t, state, "workspace_exists") // forces the findTab path

	if err := p.Start(context.Background(), "gastown__witness", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	for _, tab := range []string{"tab close t-old1", "tab close t-old2"} {
		if !strings.Contains(calls, tab) {
			t.Errorf("stale duplicate not recycled (%s missing):\n%s", tab, calls)
		}
	}
	if strings.Contains(calls, "tab close t-other") {
		t.Errorf("closed another session's tab:\n%s", calls)
	}
}

// rewriteFake patches the fake herdr script in place.
func rewriteFake(t *testing.T, p *Provider, old, replacement string) {
	t.Helper()
	b, err := os.ReadFile(p.c.bin)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(b), old, replacement, 1)
	if patched == string(b) {
		t.Fatalf("fake script pattern not found:\n%s", old)
	}
	if err := os.WriteFile(p.c.bin, []byte(patched), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Stop must still close the pane via the sidecar binding when no registry
// name exists (the earlier "sleep leak": name lost ⇒ pane never found ⇒
// closePane never issued ⇒ panes piled up), even for an exited agent whose
// pane idles at a prompt — and clear the sidecar.
func TestStopClosesPaneViaBindingWhenNameCleared(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__witness", bindModeAgent)

	if err := p.Stop("gastown__witness"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if calls := fakeCalls(t, state); !strings.Contains(calls, "pane close %5") {
		t.Fatalf("Stop never closed the bound pane:\n%s", calls)
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "" {
		t.Fatalf("binding survived Stop: %q", got)
	}
}

// ObserveLiveness is the fast path every liveness consumer actually reads; it
// must fall back to the bound pane too, or the reconciler still sees
// Running=false each tick and drives Start.
func TestObserveLivenessFallsBackToBoundPane(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "busy")
	bindTestPane(t, p, "gastown__witness", bindModeAgent)

	if got := p.ObserveLiveness("gastown__witness", nil); !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %+v; want Running=true Alive=true via bound pane", got)
	}

	// Pane confirmed gone: liveness zero and the stale binding is cleared so a
	// recycled pane id can never resurrect a dead session.
	setState(t, state, "pane_gone")
	if got := p.ObserveLiveness("gastown__witness", nil); got.Running || got.Alive {
		t.Fatalf("ObserveLiveness = %+v for a gone pane; want zero", got)
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "" {
		t.Fatalf("confirmed-gone binding survived: %q", got)
	}
}

// An exited agent (pane at bare prompt, agent mode) reads as not running so
// the reconciler restarts it.
func TestObserveLivenessExitedAgentReadsDead(t *testing.T) {
	p, _, _ := newFakeHerdrProvider(t)
	bindTestPane(t, p, "gastown__witness", bindModeAgent)
	if got := p.ObserveLiveness("gastown__witness", nil); got.Running || got.Alive {
		t.Fatalf("ObserveLiveness = %+v for an exited agent; want zero", got)
	}
}

// Herdr may retain an idle agent registry entry after a server restart even
// though the referenced pane no longer exists. The registry row is not a
// liveness lease: the pane probe must fence it out so the reconciler can heal
// and replace the session.
func TestObserveLivenessRejectsRegisteredAgentWithMissingPane(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	setState(t, state, "pane_gone")
	bindTestPane(t, p, "gastown__witness", bindModeAgent)

	if got := p.ObserveLiveness("gastown__witness", nil); got.Running || got.Alive {
		t.Fatalf("ObserveLiveness = %+v for stale registry row; want zero", got)
	}
	if calls := fakeCalls(t, state); !strings.Contains(calls, "agent get gastown__witness") ||
		!strings.Contains(calls, "pane process-info --pane %5") ||
		!strings.Contains(calls, "pane close %5") {
		t.Fatalf("ObserveLiveness did not validate and reap the registered pane:\n%s", calls)
	}
	if got, _ := p.GetMeta("gastown__witness", metaBoundPane); got != "" {
		t.Fatalf("stale registry binding survived reap: %q", got)
	}
}

func TestGetMetaFallsBackToRegisteredAgentSessionID(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	rewriteFake(
		t,
		p,
		`"agent_status":"idle"}}}`,
		`"agent_status":"idle","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"6359c25f-aa92-4f83-9329-ab3497b22de7"}}}}`,
	)

	got, err := p.GetMeta("gastown__witness", "GC_PROVIDER_SESSION_ID")
	if err != nil {
		t.Fatalf("GetMeta(GC_PROVIDER_SESSION_ID): %v", err)
	}
	if want := "6359c25f-aa92-4f83-9329-ab3497b22de7"; got != want {
		t.Fatalf("GetMeta(GC_PROVIDER_SESSION_ID) = %q, want %q", got, want)
	}

	if err := p.SetMeta("gastown__witness", "GC_PROVIDER_SESSION_ID", "explicit-sidecar-id"); err != nil {
		t.Fatalf("SetMeta(GC_PROVIDER_SESSION_ID): %v", err)
	}
	got, err = p.GetMeta("gastown__witness", "GC_PROVIDER_SESSION_ID")
	if err != nil {
		t.Fatalf("GetMeta(explicit GC_PROVIDER_SESSION_ID): %v", err)
	}
	if got != "explicit-sidecar-id" {
		t.Fatalf("GetMeta(explicit GC_PROVIDER_SESSION_ID) = %q, want explicit-sidecar-id", got)
	}

	if err := p.RemoveMeta("gastown__witness", "GC_PROVIDER_SESSION_ID"); err != nil {
		t.Fatalf("RemoveMeta(GC_PROVIDER_SESSION_ID): %v", err)
	}
	rewriteFake(t, p, `"kind":"id"`, `"kind":"name"`)
	got, err = p.GetMeta("gastown__witness", "GC_PROVIDER_SESSION_ID")
	if err != nil {
		t.Fatalf("GetMeta(non-ID agent session): %v", err)
	}
	if got != "" {
		t.Fatalf("GetMeta(non-ID agent session) = %q, want empty", got)
	}
}

// ListRunning must see sessions that herdr's registry does not: raw shell
// sessions never register an agent, so listing by registry alone hides them
// from every session-enumeration consumer (orphan detection, gc ls).
func TestListRunningIncludesUnregisteredBoundSessions(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "busy")
	for _, name := range []string{"gastown__worker-1", "gastown__worker-2", "other__worker"} {
		bindTestPane(t, p, name, bindModeShell)
		if err := p.SetMeta(name, metaBoundName, name); err != nil {
			t.Fatal(err)
		}
	}
	got, err := p.ListRunning("gastown__")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	want := map[string]bool{"gastown__worker-1": true, "gastown__worker-2": true}
	if len(got) != len(want) {
		t.Fatalf("ListRunning = %v; want exactly %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("ListRunning = %v; unexpected %q", got, n)
		}
	}
}

// A bound session whose pane is gone must not be listed (and is pruned).
func TestListRunningSkipsGonePanes(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "pane_gone")
	bindTestPane(t, p, "gastown__worker-1", bindModeShell)
	if err := p.SetMeta("gastown__worker-1", metaBoundName, "gastown__worker-1"); err != nil {
		t.Fatal(err)
	}
	got, err := p.ListRunning("gastown__")
	if err != nil || len(got) != 0 {
		t.Fatalf("ListRunning = %v, %v; want empty", got, err)
	}
}

// A persisted binding marks the registry name as provider-owned. Before this
// regression test, ListRunning trusted the matching registry row without the
// pane probe, so the startup adoption barrier never called ObserveLiveness and
// the stale Herdr name lease survived every controller restart.
func TestListRunningReapsRegisteredBoundSessionWithMissingPane(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	setState(t, state, "pane_gone")
	bindTestPane(t, p, "gastown__worker-1", bindModeAgent)
	if err := p.SetMeta("gastown__worker-1", metaBoundName, "gastown__worker-1"); err != nil {
		t.Fatal(err)
	}

	got, err := p.ListRunning("gastown__")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListRunning = %v; want empty for stale registered pane", got)
	}
	if calls := fakeCalls(t, state); !strings.Contains(calls, "pane process-info --pane %5") ||
		!strings.Contains(calls, "pane close %5") {
		t.Fatalf("ListRunning did not validate and reap stale registered pane:\n%s", calls)
	}
	if got, _ := p.GetMeta("gastown__worker-1", metaBoundPane); got != "" {
		t.Fatalf("stale registry binding survived ListRunning: %q", got)
	}
}

// A registered agent whose pane has returned to a bare shell is just as dead
// as one whose pane disappeared. Keeping the shell leaves the registry name
// leased and makes every replacement collide.
func TestObserveLivenessReapsRegisteredAgentAtBareShell(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	bindTestPane(t, p, "gastown__worker-1", bindModeAgent)

	if got := p.ObserveLiveness("gastown__worker-1", nil); got.Running || got.Alive {
		t.Fatalf("ObserveLiveness = %+v for registered bare shell; want zero", got)
	}
	if calls := fakeCalls(t, state); !strings.Contains(calls, "pane close %5") {
		t.Fatalf("ObserveLiveness did not release bare-shell name lease:\n%s", calls)
	}
	if got, _ := p.GetMeta("gastown__worker-1", metaBoundPane); got != "" {
		t.Fatalf("bare-shell registry binding survived reap: %q", got)
	}
}

func TestObserveLivenessRetainsRegisteredBindingWhenReapFails(t *testing.T) {
	p, _, state := newFakeHerdrProvider(t)
	setState(t, state, "registered")
	setState(t, state, "pane_close_transport_not_found")
	bindTestPane(t, p, "gastown__worker-1", bindModeAgent)

	if got := p.ObserveLiveness("gastown__worker-1", nil); !got.Running || !got.Alive {
		t.Fatalf("ObserveLiveness = %+v after close transport failure; want fail-safe live", got)
	}
	if got, _ := p.GetMeta("gastown__worker-1", metaBoundPane); got != "%5" {
		t.Fatalf("close transport failure cleared stable binding: %q", got)
	}
	if calls := fakeCalls(t, state); strings.Count(calls, "pane close %5") != 1 {
		t.Fatalf("first observation close calls != 1:\n%s", calls)
	}

	if err := os.Remove(filepath.Join(state, "pane_close_transport_not_found")); err != nil {
		t.Fatal(err)
	}
	if got := p.ObserveLiveness("gastown__worker-1", nil); got.Running || got.Alive {
		t.Fatalf("ObserveLiveness = %+v after close recovered; want dead", got)
	}
	if got, _ := p.GetMeta("gastown__worker-1", metaBoundPane); got != "" {
		t.Fatalf("successful retry retained stale binding: %q", got)
	}
	if calls := fakeCalls(t, state); strings.Count(calls, "pane close %5") != 2 {
		t.Fatalf("recovery did not retry close exactly once:\n%s", calls)
	}
}
