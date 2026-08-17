package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/omnigent"
)

func TestProjectHerdrFleetRowsJoinsAuthorizedSessionsAndSortsDeterministically(t *testing.T) {
	sessions := []api.SessionView{
		{ID: "ga-ssh", Alias: "worker", Rig: "zeta", State: "failed", Provider: "omnigent", Running: false},
		{ID: "ga-local", Alias: "worker", Rig: "alpha", State: "active", Provider: "omnigent", Running: true},
		{ID: "ga-k8s", Alias: "reviewer", Rig: "alpha", State: "suspended", Provider: "omnigent", Running: false},
		{ID: "ga-terminal", Rig: "alpha", State: "closed", Provider: "codex", Running: false},
	}
	statuses := []omnigent.RemoteSessionStatus{
		{SessionID: "ga-ssh", Transport: "ssh", ActiveProfile: &omnigent.StatusProfile{DisplayName: "Claude work", Blurb: "Work profile", Harness: "claude-code", Backend: "bedrock"}},
		{SessionID: "ga-k8s", Transport: "kubernetes", ConfiguredProfile: &omnigent.StatusProfile{DisplayName: "Claude primary", Blurb: "Primary profile", Harness: "claude-code", Backend: "anthropic"}},
		{SessionID: "ga-local", Transport: "tmux", ActiveProfile: &omnigent.StatusProfile{DisplayName: "Codex", Blurb: "Local Codex", Harness: "codex", Backend: "openai"}},
		// Status without an authorized session row must never be disclosed.
		{SessionID: "ga-unauthorized", Alias: "secret", Transport: "ssh", ActiveProfile: &omnigent.StatusProfile{Blurb: "must not leak"}},
	}
	rows := projectHerdrFleetRows("city-b", sessions, statuses, func(id string) []string {
		return []string{"gc", "--city", "/path with spaces;$(false)", "session", "attach", "--no-resume", "--", id}
	})
	gotIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		gotIDs = append(gotIDs, row.ViewerID)
	}
	wantIDs := []string{"city-b/ga-terminal", "city-b/ga-k8s", "city-b/ga-local", "city-b/ga-ssh"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("viewer IDs = %#v, want %#v", gotIDs, wantIDs)
	}
	if rows[1].Transport != "kubernetes" || rows[1].Profile != "Claude primary" || rows[1].ProfileBlurb != "Primary profile" {
		t.Fatalf("Kubernetes profile join = %#v", rows[1])
	}
	for _, row := range rows {
		if strings.Contains(row.ViewerID, "unauthorized") || strings.Contains(row.ProfileBlurb, "must not leak") {
			t.Fatalf("unauthorized status leaked: %#v", row)
		}
		if got := row.attachCommand[len(row.attachCommand)-1]; got != row.SessionID {
			t.Fatalf("attach command target = %q, want %q", got, row.SessionID)
		}
	}

	filtered := filterHerdrFleetRows(rows, "suspended", "kubernetes", "omnigent")
	if len(filtered) != 1 || filtered[0].SessionID != "ga-k8s" {
		t.Fatalf("filtered rows = %#v", filtered)
	}
}

func TestResolveHerdrFleetRowRequiresQualifiedIDForCrossCityAliasCollision(t *testing.T) {
	rows := []herdrFleetRow{
		{ViewerID: "alpha/ga-1", City: "alpha", SessionID: "ga-1", Alias: "worker"},
		{ViewerID: "beta/ga-2", City: "beta", SessionID: "ga-2", Alias: "worker"},
	}
	if _, err := resolveHerdrFleetRow(rows, "worker"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("collision error = %v", err)
	}
	got, err := resolveHerdrFleetRow(rows, "beta/ga-2")
	if err != nil || got.SessionID != "ga-2" {
		t.Fatalf("qualified resolve = %#v, %v", got, err)
	}
}

func TestRemoteViewerAttachCommandContainsNoCredentialMaterial(t *testing.T) {
	target := &remoteTarget{
		BaseURL: "https://remote.example", CityName: "prod",
		Ctx:   &clientcontext.Context{Name: "prod-context", CredentialCommand: "secret-command", GrantCommand: "grant-command"},
		Token: "literal-secret",
	}
	got := remoteViewerAttachCommand(target, "ga-1")
	if want := []string{"gc", "--context", "prod-context", "session", "attach", "--no-resume", "--", "ga-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
	joined := strings.Join(got, " ")
	for _, forbidden := range []string{"literal-secret", "secret-command", "grant-command", "Bearer"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("command leaked %q: %q", forbidden, joined)
		}
	}
}

func TestSessionCommandRegistersViewWorkflow(t *testing.T) {
	cmd := newSessionCmd(&strings.Builder{}, &strings.Builder{})
	view, _, err := cmd.Find([]string{"view"})
	if err != nil || view == nil || view.Name() != "view" {
		t.Fatalf("session view command = %#v, %v", view, err)
	}
	for _, name := range []string{"list", "open", "close", "attach"} {
		child, _, err := view.Find([]string{name})
		if err != nil || child == nil || child.Name() != name {
			t.Fatalf("session view %s = %#v, %v", name, child, err)
		}
	}
}
