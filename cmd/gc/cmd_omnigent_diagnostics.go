package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/omnigent"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

type omnigentSessionStatus struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	Runtime        string `json:"runtime"`
	RuntimeSession string `json:"runtime_session"`
	Workspace      string `json:"workspace"`
	ConversationID string `json:"conversation_id,omitempty"`
}

type omnigentCLIReport struct {
	SchemaVersion string                      `json:"schema_version"`
	CityPath      string                      `json:"city_path"`
	Selection     *omnigent.ProfileSelection  `json:"selection,omitempty"`
	Selected      *omnigent.ProfileDiagnostic `json:"selected_profile,omitempty"`
	Sidecar       omnigent.LocalStatus        `json:"sidecar"`
	Session       *omnigentSessionStatus      `json:"session,omitempty"`
}

type omnigentDoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type omnigentDoctorReport struct {
	SchemaVersion string                `json:"schema_version"`
	CityPath      string                `json:"city_path"`
	Checks        []omnigentDoctorCheck `json:"checks"`
}

func newOmnigentExplainCmd(stdout, stderr io.Writer) *cobra.Command {
	return newOmnigentReadCommand("explain", "Explain resolved local Omnigent configuration", false, stdout, stderr)
}

func newOmnigentStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return newOmnigentReadCommand("status", "Show local Omnigent sidecar and attachment status", true, stdout, stderr)
}

func newOmnigentReadCommand(use, short string, includeSession bool, stdout, _ io.Writer) *cobra.Command {
	var profileID, sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:          use,
		Short:        short,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveContext()
			if err != nil {
				return fmt.Errorf("gc omnigent %s: resolve local city: %w", use, err)
			}
			if includeSession && strings.TrimSpace(sessionID) == "" {
				sessionID = strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
			}
			report, err := collectOmnigentCLIReport(cmd.Context(), resolved.CityPath, profileID, sessionID)
			if err != nil {
				return fmt.Errorf("gc omnigent %s: %w", use, err)
			}
			if jsonOutput {
				if err := writeCLIJSONLine(stdout, report); err != nil {
					return fmt.Errorf("gc omnigent %s: write JSON: %w", use, err)
				}
				return nil
			}
			return renderOmnigentCLIReport(stdout, report)
		},
	}
	cmd.Flags().StringVar(&profileID, "profile", "", "opaque local Omnigent profile ID to explain")
	if includeSession {
		cmd.Flags().StringVar(&sessionID, "session", "", "Gas City session ID (default: GC_SESSION_ID)")
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON")
	return cmd
}

func collectOmnigentCLIReport(ctx context.Context, cityPath, profileID, sessionID string) (omnigentCLIReport, error) {
	report := omnigentCLIReport{SchemaVersion: "1", CityPath: cityPath}
	conversationID := ""
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
		front, err := citySessionFrontDoorAt(cityPath)
		if err != nil {
			return report, fmt.Errorf("open session store: %w", err)
		}
		info, err := front.Get(sessionID)
		if err != nil {
			return report, fmt.Errorf("load managed session %q: %w", sessionID, err)
		}
		conversationID = strings.TrimSpace(info.SessionKey)
		report.Session = omnigentSessionStatusFromInfo(info)
	}
	client, err := omnigentCityClient(cityPath)
	if err != nil {
		return report, fmt.Errorf("local sidecar unavailable: %w", err)
	}
	operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
	report.Sidecar, err = client.LocalStatus(operationCtx, conversationID)
	cancel()
	if err != nil {
		if conversationID != "" {
			return report, fmt.Errorf("conversation %q attachment status failed: %w", conversationID, err)
		}
		return report, fmt.Errorf("local sidecar status failed: %w", err)
	}
	selection, selected, err := selectOmnigentDiagnostic(profileID, report.Sidecar.Profiles, os.LookupEnv)
	if err != nil {
		return report, err
	}
	report.Selection = selection
	report.Selected = selected
	return report, nil
}

func omnigentSessionStatusFromInfo(info session.Info) *omnigentSessionStatus {
	return &omnigentSessionStatus{
		ID: info.ID, Provider: info.Provider, Runtime: info.Transport,
		RuntimeSession: info.SessionName, Workspace: info.WorkDir,
		ConversationID: info.SessionKey,
	}
}

func selectOmnigentDiagnostic(explicit string, profiles []omnigent.ProfileDiagnostic, lookup func(string) (string, bool)) (*omnigent.ProfileSelection, *omnigent.ProfileDiagnostic, error) {
	if strings.TrimSpace(explicit) == "" {
		if value, ok := lookup(omnigent.ProfileEnvironmentVariable); !ok || strings.TrimSpace(value) == "" {
			return nil, nil, nil
		}
	}
	selection, err := omnigent.SelectProfile(explicit, lookup)
	if err != nil {
		return nil, nil, err
	}
	for i := range profiles {
		if profiles[i].ID == selection.ID {
			selected := profiles[i]
			return &selection, &selected, nil
		}
	}
	return nil, nil, fmt.Errorf("selected Omnigent profile %q is unavailable from the local sidecar", selection.ID)
}

func renderOmnigentCLIReport(w io.Writer, report omnigentCLIReport) error {
	if _, err := fmt.Fprintf(w, "mode: %s\nready: %t\ncity: %s\n", report.Sidecar.Mode, report.Sidecar.Ready, report.CityPath); err != nil {
		return err
	}
	pin := report.Sidecar.Pin
	if _, err := fmt.Fprintf(w, "binary.executable: %s\nbinary.path: %s\nbinary.version: %s\nbinary.commit: %s\nbinary.sha256: %s\n",
		pin.Executable, pin.ResolvedPath, pin.PackageVersion, pin.Commit, pin.SHA256); err != nil {
		return err
	}
	if report.Selected != nil && report.Selection != nil {
		profile := report.Selected
		if _, err := fmt.Fprintf(w, "selected.id: %s\nselected.source: %s\nselected.blurb: %s\nselected.harness: %s\nselected.backend: %s\nselected.chain: %s\nselected.availability: %s\n",
			profile.ID, report.Selection.Source, profile.Blurb, profile.Harness, profile.Backend,
			strings.Join(profile.Chain, ","), profile.Availability); err != nil {
			return err
		}
	}
	for _, profile := range report.Sidecar.Profiles {
		if _, err := fmt.Fprintf(w, "profile.%s: harness=%s backend=%s availability=%s blurb=%q missing_auth=%s\n",
			profile.ID, profile.Harness, profile.Backend, profile.Availability, profile.Blurb,
			strings.Join(profile.MissingEnvironment, ",")); err != nil {
			return err
		}
	}
	if report.Session != nil {
		s := report.Session
		if _, err := fmt.Fprintf(w, "session.id: %s\nsession.provider: %s\nsession.runtime: %s\nsession.runtime_view: %s\nsession.workspace: %s\nsession.conversation: %s\n",
			s.ID, s.Provider, s.Runtime, s.RuntimeSession, s.Workspace, s.ConversationID); err != nil {
			return err
		}
	}
	if conversation := report.Sidecar.Conversation; conversation != nil {
		if _, err := fmt.Fprintf(w, "conversation.status: %s\nconversation.outcome: %s\nconversation.active_profile: %s\nconversation.active_index: %d\nconversation.exhausted: %t\n",
			conversation.Status, conversation.Outcome, conversation.ActiveProfileID, conversation.ActiveIndex, conversation.Exhausted); err != nil {
			return err
		}
		if transition := conversation.LastTransition; transition != nil {
			if _, err := fmt.Fprintf(w, "conversation.failover: %s->%s category=%s at=%s\n",
				transition.FromProfileID, transition.ToProfileID, transition.Reason, transition.At.UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		if policy := conversation.Policy; policy != nil {
			if _, err := fmt.Fprintf(w, "conversation.policy: request=%s kind=%s state=%s pending=%t mail_bound=%t\n",
				policy.RequestID, policy.Kind, policy.State, policy.Pending, policy.MailBound); err != nil {
				return err
			}
		}
	}
	return nil
}

func newOmnigentDoctorCmd(stdout, _ io.Writer) *cobra.Command {
	var profileID, sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Diagnose the pinned local Omnigent integration",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := resolveContext()
			if err != nil {
				return fmt.Errorf("gc omnigent doctor: resolve local city: %w", err)
			}
			if strings.TrimSpace(sessionID) == "" {
				sessionID = strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
			}
			report, healthy := diagnoseOmnigent(cmd.Context(), resolved.CityPath, profileID, sessionID)
			if jsonOutput {
				if err := writeCLIJSONLine(stdout, report); err != nil {
					return err
				}
			} else {
				for _, check := range report.Checks {
					fmt.Fprintf(stdout, "%-4s %-22s %s\n", strings.ToUpper(check.Status), check.Name, check.Message) //nolint:errcheck
				}
			}
			if !healthy {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profileID, "profile", "", "opaque local Omnigent profile ID to diagnose")
	cmd.Flags().StringVar(&sessionID, "session", "", "Gas City session ID (default: GC_SESSION_ID)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit stable JSON")
	return cmd
}

func diagnoseOmnigent(ctx context.Context, cityPath, profileID, sessionID string) (omnigentDoctorReport, bool) {
	report := omnigentDoctorReport{SchemaVersion: "1", CityPath: cityPath}
	add := func(name, status, message string) {
		report.Checks = append(report.Checks, omnigentDoctorCheck{Name: name, Status: status, Message: message})
	}
	healthy := true
	fail := func(name, message string) { healthy = false; add(name, "fail", message) }
	catalogPath := filepath.Join(cityPath, ".gc", "services", "omnigent", "config", "profiles.yaml")
	catalog, err := omnigent.LoadCatalog(catalogPath)
	if err != nil {
		fail("catalog", fmt.Sprintf("%v; install the local profile catalog at %s", err, catalogPath))
	} else {
		add("catalog", "ok", catalogPath)
		verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		verified, verifyErr := omnigent.VerifyExecutable(verifyCtx, catalog.Pin)
		cancel()
		if verifyErr != nil {
			fail("binary-pin", verifyErr.Error())
		} else {
			add("binary-pin", "ok", verified.Path+" "+catalog.Pin.PackageVersion)
		}
	}
	reportView, reportErr := collectOmnigentCLIReport(ctx, cityPath, profileID, sessionID)
	if reportErr != nil {
		fail("local-sidecar", reportErr.Error())
		return report, healthy
	}
	add("local-sidecar", "ok", "ready through the Gas City private local service proxy")
	if reportView.Sidecar.Mode != "local" {
		fail("locality", fmt.Sprintf("forbidden Omnigent mode %q; configure local mode", reportView.Sidecar.Mode))
	} else {
		add("locality", "ok", "local mode; Gas City owns placement")
	}
	if reportView.Selected != nil {
		profile := reportView.Selected
		if len(profile.MissingEnvironment) > 0 || profile.Availability != "available" {
			fail("profile-auth", fmt.Sprintf("profile %s is unavailable; set local auth references: %s", profile.ID, strings.Join(profile.MissingEnvironment, ", ")))
		} else {
			add("profile-auth", "ok", fmt.Sprintf("%s (%s via %s)", profile.ID, profile.Backend, profile.Harness))
		}
	} else {
		for _, profile := range reportView.Sidecar.Profiles {
			name := "profile-auth:" + profile.ID
			if len(profile.MissingEnvironment) > 0 || profile.Availability != "available" {
				fail(name, fmt.Sprintf("unavailable; set local auth references: %s", strings.Join(profile.MissingEnvironment, ", ")))
			} else {
				add(name, "ok", fmt.Sprintf("%s via %s", profile.Backend, profile.Harness))
			}
		}
	}
	if reportView.Session != nil {
		s := reportView.Session
		switch {
		case s.ConversationID == "":
			fail("attachment", fmt.Sprintf("session %s has no persisted Omnigent conversation", s.ID))
		case reportView.Sidecar.Conversation == nil:
			fail("attachment", fmt.Sprintf("conversation %s could not be verified", s.ConversationID))
		default:
			add("attachment", "ok", fmt.Sprintf("%s in %s via %s/%s", s.ConversationID, s.Workspace, s.Runtime, s.RuntimeSession))
		}
	}
	return report, healthy
}
