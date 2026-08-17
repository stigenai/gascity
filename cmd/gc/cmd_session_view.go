package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/omnigent"
	sessionherdr "github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

const herdrFleetSession = "gascity-fleet"

type herdrFleetRow struct {
	ViewerID     string `json:"viewer_id"`
	City         string `json:"city"`
	Rig          string `json:"rig,omitempty"`
	SessionID    string `json:"session_id"`
	Alias        string `json:"alias,omitempty"`
	State        string `json:"state"`
	Running      bool   `json:"running"`
	Provider     string `json:"provider,omitempty"`
	Transport    string `json:"transport,omitempty"`
	Harness      string `json:"harness,omitempty"`
	Backend      string `json:"backend,omitempty"`
	Profile      string `json:"profile,omitempty"`
	ProfileBlurb string `json:"profile_blurb,omitempty"`
	ViewerOpen   bool   `json:"viewer_open"`

	attachCommand []string
}

type herdrFleetCity struct {
	name          string
	client        *api.Client
	attachCommand func(string) []string
}

var discoverHerdrFleetHook = discoverHerdrFleet

func newSessionViewCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "view",
		Short: "Discover and view local or remote sessions in Herdr",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		newSessionViewListCmd(stdout, stderr),
		newSessionViewOpenCmd(stdout, stderr),
		newSessionViewCloseCmd(stdout, stderr),
		newSessionViewAttachCmd(stdout, stderr),
	)
	return cmd
}

func newSessionViewListCmd(stdout, stderr io.Writer) *cobra.Command {
	var state, transport, provider string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Herdr-viewable sessions across the selected fleet",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows, err := discoverHerdrFleetHook(cmd.Context())
			if err != nil {
				fmt.Fprintf(stderr, "gc session view list: %v\n", err) //nolint:errcheck
				return errExit
			}
			rows = filterHerdrFleetRows(rows, state, transport, provider)
			if jsonOutput {
				if err := json.NewEncoder(stdout).Encode(rows); err != nil {
					fmt.Fprintf(stderr, "gc session view list: encode output: %v\n", err) //nolint:errcheck
					return errExit
				}
				return nil
			}
			writeHerdrFleetTable(stdout, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter by session state")
	cmd.Flags().StringVar(&transport, "transport", "", "filter by runtime transport")
	cmd.Flags().StringVar(&provider, "provider", "", "filter by configured provider")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "JSON output")
	return cmd
}

func newSessionViewOpenCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "open <viewer-id-or-session>",
		Short: "Open a lifecycle-neutral Herdr viewer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, err := discoverHerdrFleetHook(cmd.Context())
			if err != nil {
				fmt.Fprintf(stderr, "gc session view open: %v\n", err) //nolint:errcheck
				return errExit
			}
			row, err := resolveHerdrFleetRow(rows, args[0])
			if err != nil {
				fmt.Fprintf(stderr, "gc session view open: %v\n", err) //nolint:errcheck
				return errExit
			}
			binding, err := herdrFleetProjection().Open(cmd.Context(), sessionherdr.ViewerSpec{
				Session: row.ViewerID, Label: herdrFleetLabel(row), ProfileBlurb: row.ProfileBlurb,
				AttachCommand: row.attachCommand,
			})
			if err != nil {
				fmt.Fprintf(stderr, "gc session view open: %v\n", err) //nolint:errcheck
				return errExit
			}
			fmt.Fprintf(stdout, "Opened %s in Herdr pane %s.\n", row.ViewerID, binding.PaneID) //nolint:errcheck
			return nil
		},
	}
}

func newSessionViewCloseCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "close <viewer-id-or-session>",
		Short: "Close a Herdr viewer without stopping its worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			viewerID, err := resolveHerdrViewerID(cmd.Context(), args[0])
			if err != nil {
				fmt.Fprintf(stderr, "gc session view close: %v\n", err) //nolint:errcheck
				return errExit
			}
			if err := herdrFleetProjection().Close(cmd.Context(), viewerID); err != nil {
				fmt.Fprintf(stderr, "gc session view close: %v\n", err) //nolint:errcheck
				return errExit
			}
			fmt.Fprintf(stdout, "Closed %s viewer; the worker was not stopped.\n", viewerID) //nolint:errcheck
			return nil
		},
	}
}

func newSessionViewAttachCmd(_ io.Writer, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "attach <viewer-id-or-session>",
		Short: "Attach this terminal to an open Herdr viewer",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			viewerID, err := resolveHerdrViewerID(cmd.Context(), args[0])
			if err != nil {
				fmt.Fprintf(stderr, "gc session view attach: %v\n", err) //nolint:errcheck
				return errExit
			}
			if err := herdrFleetProjection().Attach(cmd.Context(), viewerID); err != nil {
				fmt.Fprintf(stderr, "gc session view attach: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
}

func resolveHerdrViewerID(ctx context.Context, selector string) (string, error) {
	selector = strings.TrimSpace(selector)
	if strings.Contains(selector, "/") {
		return selector, nil
	}
	rows, err := discoverHerdrFleetHook(ctx)
	if err != nil {
		return "", err
	}
	row, err := resolveHerdrFleetRow(rows, selector)
	return row.ViewerID, err
}

func discoverHerdrFleet(_ context.Context) ([]herdrFleetRow, error) {
	cities, err := herdrFleetCities()
	if err != nil {
		return nil, err
	}
	projection := herdrFleetProjection()
	var rows []herdrFleetRow
	for _, city := range cities {
		sessions, err := city.client.ListSessions("all", "", false)
		if err != nil {
			return nil, fmt.Errorf("city %q sessions: %w", city.name, err)
		}
		statuses, err := city.client.ListOmnigentStatus()
		if err != nil {
			return nil, fmt.Errorf("city %q Omnigent status: %w", city.name, err)
		}
		cityRows := projectHerdrFleetRows(city.name, sessions.Body, statuses.Body, city.attachCommand)
		for i := range cityRows {
			_, cityRows[i].ViewerOpen, err = projection.Binding(cityRows[i].ViewerID)
			if err != nil {
				return nil, err
			}
		}
		rows = append(rows, cityRows...)
	}
	sortHerdrFleetRows(rows)
	return rows, nil
}

func herdrFleetCities() ([]herdrFleetCity, error) {
	selection, err := resolveContextAllowRemote()
	if err != nil {
		return nil, err
	}
	if selection.Remote != nil {
		client, err := buildRemoteClient(selection.Remote)
		if err != nil {
			return nil, err
		}
		target := selection.Remote
		return []herdrFleetCity{{
			name: target.CityName, client: client,
			attachCommand: func(sessionID string) []string { return remoteViewerAttachCommand(target, sessionID) },
		}}, nil
	}

	baseURL, err := supervisorAPIBaseURL()
	if err == nil {
		cities, listErr := api.NewClient(baseURL).ListCities()
		if listErr == nil {
			out := make([]herdrFleetCity, 0, len(cities))
			for _, city := range cities {
				if !city.Running {
					continue
				}
				city := city
				out = append(out, herdrFleetCity{
					name: city.Name, client: api.NewCityScopedClient(baseURL, city.Name),
					attachCommand: func(sessionID string) []string { return localViewerAttachCommand(city.Path, sessionID) },
				})
			}
			if len(out) > 0 {
				return out, nil
			}
		}
	}
	client := apiClient(selection.CityPath)
	if client == nil {
		return nil, errors.New("the local supervisor is not serving live session state")
	}
	cfg, err := loadCityConfig(selection.CityPath, io.Discard)
	if err != nil {
		return nil, err
	}
	return []herdrFleetCity{{
		name: cfg.Workspace.Name, client: client,
		attachCommand: func(sessionID string) []string { return localViewerAttachCommand(selection.CityPath, sessionID) },
	}}, nil
}

func localViewerAttachCommand(cityPath, sessionID string) []string {
	return []string{"gc", "--city", cityPath, "session", "attach", "--no-resume", "--", sessionID}
}

func remoteViewerAttachCommand(target *remoteTarget, sessionID string) []string {
	if target != nil && target.Ctx != nil && strings.TrimSpace(target.Ctx.Name) != "" {
		return []string{"gc", "--context", target.Ctx.Name, "session", "attach", "--no-resume", "--", sessionID}
	}
	return []string{"gc", "--city-url", target.BaseURL, "--city-name", target.CityName, "session", "attach", "--no-resume", "--", sessionID}
}

func projectHerdrFleetRows(city string, sessions []api.SessionView, statuses []omnigent.RemoteSessionStatus, attach func(string) []string) []herdrFleetRow {
	statusBySession := make(map[string]omnigent.RemoteSessionStatus, len(statuses))
	for _, status := range statuses {
		statusBySession[status.SessionID] = status
	}
	rows := make([]herdrFleetRow, 0, len(sessions))
	for _, session := range sessions {
		row := herdrFleetRow{
			ViewerID: city + "/" + session.ID, City: city, Rig: session.Rig,
			SessionID: session.ID, Alias: session.Alias, State: session.State,
			Running: session.Running, Provider: session.Provider,
		}
		if attach != nil {
			row.attachCommand = append([]string(nil), attach(session.ID)...)
		}
		if status, ok := statusBySession[session.ID]; ok {
			row.Transport = status.Transport
			profile := status.ActiveProfile
			if profile == nil {
				profile = status.ConfiguredProfile
			}
			if profile != nil {
				row.Profile = profile.DisplayName
				row.ProfileBlurb = profile.Blurb
				row.Harness = profile.Harness
				row.Backend = profile.Backend
			}
		}
		rows = append(rows, row)
	}
	sortHerdrFleetRows(rows)
	return rows
}

func sortHerdrFleetRows(rows []herdrFleetRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.City != b.City {
			return a.City < b.City
		}
		if a.Rig != b.Rig {
			return a.Rig < b.Rig
		}
		if a.Alias != b.Alias {
			return a.Alias < b.Alias
		}
		return a.SessionID < b.SessionID
	})
}

func filterHerdrFleetRows(rows []herdrFleetRow, state, transport, provider string) []herdrFleetRow {
	state, transport, provider = strings.TrimSpace(state), strings.TrimSpace(transport), strings.TrimSpace(provider)
	out := make([]herdrFleetRow, 0, len(rows))
	for _, row := range rows {
		if state != "" && row.State != state {
			continue
		}
		if transport != "" && row.Transport != transport {
			continue
		}
		if provider != "" && row.Provider != provider {
			continue
		}
		out = append(out, row)
	}
	return out
}

func resolveHerdrFleetRow(rows []herdrFleetRow, selector string) (herdrFleetRow, error) {
	selector = strings.TrimSpace(selector)
	var matches []herdrFleetRow
	for _, row := range rows {
		if selector == row.ViewerID || selector == row.SessionID || selector == row.Alias || selector == row.City+"/"+row.Alias {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return herdrFleetRow{}, fmt.Errorf("session viewer %q was not found in authorized live state", selector)
	}
	if len(matches) > 1 {
		return herdrFleetRow{}, fmt.Errorf("session viewer %q is ambiguous; use city/session-id", selector)
	}
	return matches[0], nil
}

func writeHerdrFleetTable(w io.Writer, rows []herdrFleetRow) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "VIEWER\tRIG\tSESSION\tSTATE\tRUNTIME\tPROFILE\tOPEN") //nolint:errcheck
	for _, row := range rows {
		name := row.SessionID
		if row.Alias != "" {
			name = row.Alias
		}
		runtimeName := row.Transport
		if runtimeName == "" {
			runtimeName = row.Provider
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%t\n", row.ViewerID, row.Rig, name, row.State, runtimeName, row.Profile, row.ViewerOpen) //nolint:errcheck
	}
	_ = tw.Flush()
}

func herdrFleetLabel(row herdrFleetRow) string {
	name := row.Alias
	if name == "" {
		name = row.SessionID
	}
	if row.Profile != "" {
		return row.City + " · " + name + " · " + row.Profile
	}
	return row.City + " · " + name
}

func herdrFleetProjection() *sessionherdr.ViewerProjection {
	root, err := os.Getwd()
	if err != nil {
		root = supervisor.DefaultHome()
	}
	stateDir := filepath.Join(supervisor.DefaultHome(), "herdr-viewers", "fleet")
	return sessionherdr.NewViewerProjection(herdrFleetSession, stateDir, root)
}
