package omnigentcapsule

import (
	"errors"
	"reflect"
	"testing"
)

func TestFixtureRunsCodexAndDistinctClaudeProfilesWithExactRestart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport Transport
		profile   string
		harness   Harness
		backend   string
	}{
		{"codex on Kubernetes", TransportKubernetes, ProfileCodex, HarnessCodex, "openai-compatible"},
		{"primary Claude on SSH", TransportSSH, ProfileClaudePrimary, HarnessClaudeCode, "anthropic"},
		{"secondary Claude on Kubernetes", TransportKubernetes, ProfileClaudeSecondary, HarnessClaudeCode, "bedrock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fixture, err := New(root, Config{CapsuleID: "ga-capsule", Transport: tc.transport, ProfileID: tc.profile})
			if err != nil {
				t.Fatal(err)
			}
			started, err := fixture.Start()
			if err != nil {
				t.Fatal(err)
			}
			result, err := fixture.Run("implement deterministic work")
			if err != nil {
				t.Fatal(err)
			}
			if result.ConversationID != started.ConversationID || result.Profile.Harness != tc.harness || result.Profile.Backend != tc.backend {
				t.Fatalf("run = %#v, start = %#v", result, started)
			}

			restarted, err := Restart(root)
			if err != nil {
				t.Fatal(err)
			}
			after, err := restarted.Start()
			if err != nil {
				t.Fatal(err)
			}
			if after.ConversationID != started.ConversationID || after.Profile.ID != tc.profile {
				t.Fatalf("restart = %#v, want conversation %q profile %q", after, started.ConversationID, tc.profile)
			}
			if got := restarted.ModelRequests(); len(got) != 1 || got[0].Prompt != "implement deterministic work" {
				t.Fatalf("persisted model requests = %#v", got)
			}
		})
	}
}

func TestFixtureFailoverPolicyMailAndFaultsUseTypedEvents(t *testing.T) {
	fixture, err := New(t.TempDir(), Config{
		CapsuleID: "ga-faults", Transport: TransportSSH, ProfileID: ProfileClaudePrimary,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	fixture.Inject(FaultPrimaryRateLimit)
	result, err := fixture.Run("use fallback")
	if err != nil {
		t.Fatal(err)
	}
	if result.Profile.ID != ProfileClaudeSecondary {
		t.Fatalf("failover profile = %#v", result.Profile)
	}
	if result.Profile.AuthProfile == fixtureProfiles[ProfileClaudePrimary].AuthProfile {
		t.Fatalf("Claude fallback reused primary auth profile: %#v", result.Profile)
	}

	fixture.Inject(FaultPolicyRequired)
	if _, err := fixture.Run("needs an optional policy"); !errors.Is(err, ErrPolicyPending) {
		t.Fatalf("policy run error = %v", err)
	}
	mail := fixture.PendingPolicy()
	if mail == nil || mail.RequestID == "" || mail.ConversationID != result.ConversationID {
		t.Fatalf("pending policy = %#v", mail)
	}
	if err := fixture.AnswerPolicy(mail.RequestID, PolicyApprove, "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Run("after explicit policy answer"); err != nil {
		t.Fatal(err)
	}

	fixture.Inject(FaultTransportLoss)
	if _, err := fixture.Run("during transport loss"); !errors.Is(err, ErrTransportLost) {
		t.Fatalf("transport loss error = %v", err)
	}
	fixture.RestoreTransport()
	if _, err := fixture.Run("after reconnect"); err != nil {
		t.Fatal(err)
	}

	kinds := eventKinds(fixture.Events())
	want := []EventKind{
		EventCapsuleStarted, EventConversationCreated, EventProfileFailedOver,
		EventModelCompleted, EventPolicyRequested, EventPolicyAnswered,
		EventModelCompleted, EventTransportLost, EventTransportRestored, EventModelCompleted,
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("event kinds = %#v, want %#v", kinds, want)
	}
}

func TestFixtureComponentFaultsFailImmediatelyAndRestoreSameConversation(t *testing.T) {
	for _, tc := range []struct {
		fault Fault
		want  error
	}{
		{FaultServerCrash, ErrServerUnavailable},
		{FaultHostCrash, ErrHostUnavailable},
		{FaultHarnessCrash, ErrHarnessUnavailable},
		{FaultModelUnavailable, ErrModelUnavailable},
	} {
		t.Run(string(tc.fault), func(t *testing.T) {
			fixture, err := New(t.TempDir(), Config{CapsuleID: "ga-component", Transport: TransportKubernetes, ProfileID: ProfileCodex})
			if err != nil {
				t.Fatal(err)
			}
			started, err := fixture.Start()
			if err != nil {
				t.Fatal(err)
			}
			fixture.Inject(tc.fault)
			if _, err := fixture.Run("must fail without waiting"); !errors.Is(err, tc.want) {
				t.Fatalf("fault error = %v, want %v", err, tc.want)
			}
			if err := fixture.RestoreComponent(tc.fault); err != nil {
				t.Fatal(err)
			}
			result, err := fixture.Run("after component recovery")
			if err != nil {
				t.Fatal(err)
			}
			if result.ConversationID != started.ConversationID {
				t.Fatalf("component recovery replaced conversation: before=%q after=%q", started.ConversationID, result.ConversationID)
			}
			events := fixture.Events()
			if events[2].Kind != EventComponentFailed || events[2].Component != string(tc.fault) || events[3].Kind != EventComponentRestored {
				t.Fatalf("component events = %#v", events)
			}
		})
	}
}

func TestFixtureVolumeLossFailsClosedAndCleanupCensusIsExact(t *testing.T) {
	for _, transport := range []Transport{TransportKubernetes, TransportSSH} {
		t.Run(string(transport), func(t *testing.T) {
			root := t.TempDir()
			fixture, err := New(root, Config{CapsuleID: "ga-cleanup", Transport: transport, ProfileID: ProfileCodex})
			if err != nil {
				t.Fatal(err)
			}
			started, err := fixture.Start()
			if err != nil {
				t.Fatal(err)
			}
			census := fixture.Census()
			for name, count := range map[string]Count{
				"process group": census.ProcessGroups, "Omnigent server": census.OmnigentServers,
				"capsule host": census.CapsuleHosts, "harness": census.HarnessProcesses,
				"model endpoint": census.ModelEndpoints, "tmux session": census.TmuxSessions,
				"tmux monitor": census.TmuxMonitorProcesses, "Unix socket": census.UnixSockets,
			} {
				if !count.IsOne() {
					t.Fatalf("%s census = %d, want 1: %#v", name, count, census)
				}
			}
			if transport == TransportKubernetes && !census.KubectlProcesses.IsOne() {
				t.Fatalf("kubectl census = %#v", census)
			}
			if transport == TransportSSH && !census.SSHClientProcesses.IsOne() {
				t.Fatalf("SSH client census = %#v", census)
			}
			fixture.OpenHerdrViewer()
			if !fixture.Census().HerdrViewers.IsOne() || !fixture.Census().HerdrProcesses.IsOne() {
				t.Fatalf("viewer census = %#v", fixture.Census())
			}
			fixture.CloseHerdrViewer()
			if fixture.Snapshot().ConversationID != started.ConversationID {
				t.Fatal("closing viewer mutated worker conversation")
			}

			fixture.Inject(FaultVolumeLoss)
			if _, err := fixture.Run("must not run from vanished state"); !errors.Is(err, ErrDurableStateLost) {
				t.Fatalf("run after volume loss = %v", err)
			}
			if _, err := fixture.Start(); !errors.Is(err, ErrDurableStateLost) {
				t.Fatalf("start after volume loss = %v", err)
			}
			if _, err := Restart(root); !errors.Is(err, ErrDurableStateLost) {
				t.Fatalf("restart after volume loss = %v", err)
			}
			if fixture.Snapshot().ConversationID == "" {
				t.Fatal("volume loss silently minted replacement conversation")
			}

			if err := fixture.Cleanup(false); err != nil {
				t.Fatal(err)
			}
			if err := fixture.AssertClean(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFixtureRequiresRestartWhenDurableStateExists(t *testing.T) {
	root := t.TempDir()
	fixture, err := New(root, Config{CapsuleID: "ga-existing", Transport: TransportSSH, ProfileID: ProfileCodex})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root, Config{CapsuleID: "ga-existing", Transport: TransportSSH, ProfileID: ProfileCodex}); err == nil {
		t.Fatal("New silently replaced existing durable fixture state")
	}
	if _, err := Restart(root); err != nil {
		t.Fatalf("Restart existing state: %v", err)
	}
}

func eventKinds(events []Event) []EventKind {
	out := make([]EventKind, 0, len(events))
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}
