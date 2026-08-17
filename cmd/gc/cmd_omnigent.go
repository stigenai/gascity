package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/omnigent"
	"github.com/spf13/cobra"
)

func newOmnigentCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "omnigent",
		Short: "Run the opt-in local Omnigent compatibility adapter",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(
		newOmnigentServeCmd(stdout, stderr),
		newOmnigentAttachCmd(stdout, stderr),
		newOmnigentExplainCmd(stdout, stderr),
		newOmnigentStatusCmd(stdout, stderr),
		newOmnigentDoctorCmd(stdout, stderr),
	)
	return cmd
}

var omnigentCityClient = func(cityPath string) (*omnigent.APIClient, error) {
	client, reason := maintenanceAPIClient(cityPath)
	if client == nil {
		return nil, fmt.Errorf("gas city controller is unavailable (%s)", reason)
	}
	target, err := client.LocalServiceProxy("omnigent")
	if err != nil {
		return nil, err
	}
	return omnigent.NewAPIClient(target.Endpoint, target.Client)
}

var omnigentLoadSessionKey = func(cityPath, sessionID string) (string, error) {
	front, err := citySessionFrontDoorAt(cityPath)
	if err != nil {
		return "", err
	}
	info, err := front.Get(sessionID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(info.SessionKey), nil
}

var omnigentBindSessionKey = func(cityPath, sessionID, conversationID string) (string, error) {
	front, err := citySessionFrontDoorAt(cityPath)
	if err != nil {
		return "", err
	}
	winner, _, err := front.BindSessionKey(sessionID, conversationID)
	return winner, err
}

var omnigentRecordSessionStatus = func(cityPath, sessionID string, snapshot omnigent.SessionStatusSnapshot) error {
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return err
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	return omnigent.NewSessionStatusStore(beads.SessionStore{Store: cliSessionStore(store, cfg, cityPath)}).Record(sessionID, snapshot)
}

func newOmnigentAttachCmd(stdout, stderr io.Writer) *cobra.Command {
	var profileID, conversationID, title, attachmentMode, socketPath, stateRoot, catalogPath string
	cmd := &cobra.Command{
		Use:          "attach",
		Short:        "Attach this Gas City worker pane to an Omnigent conversation",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			location, err := resolveOmnigentAttachmentLocation(attachmentMode, socketPath, stateRoot, catalogPath)
			if err != nil {
				return fmt.Errorf("gc omnigent attach: %w", err)
			}
			selection, err := omnigent.SelectProfile(profileID, os.LookupEnv)
			if err != nil {
				return fmt.Errorf("gc omnigent attach: %w", err)
			}
			resolved, err := resolveContext()
			if err != nil {
				return fmt.Errorf("gc omnigent attach: resolve local city: %w", err)
			}
			workspace, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("gc omnigent attach: resolve assigned workspace: %w", err)
			}
			identity, err := omnigentIdentityFromLookup(os.LookupEnv)
			if err != nil {
				return fmt.Errorf("gc omnigent attach: %w", err)
			}
			if strings.TrimSpace(title) == "" {
				title = identity.Agent
			}
			var client *omnigent.APIClient
			stopSupervisor := func() error { return nil }
			switch location.Mode {
			case string(omnigent.AttachmentLocationController):
				client, err = omnigentCityClient(resolved.CityPath)
			case string(omnigent.AttachmentLocationCapsule):
				client, stopSupervisor, err = startOmnigentCapsuleSupervisor(cmd.Context(), omnigent.SidecarConfig{
					StateRoot: location.StateRoot, CatalogPath: location.CatalogPath, SocketPath: location.SocketPath,
					ImmutableCatalog: true, Stdout: stdout, Stderr: stderr,
				}, omnigent.ServeSidecar, nil)
			}
			if err != nil {
				return fmt.Errorf("gc omnigent attach: open %s client: %w", location.Mode, err)
			}
			policyBridge, err := newOmnigentPolicyMailBridge(cmd.Context(), client, identity.SessionID, func() (mail.Provider, error) {
				provider, code := openCityMailProvider(stderr, "gc omnigent policy mail")
				if code != 0 || provider == nil {
					return nil, errors.New("open Gas City mail provider for Omnigent policy request")
				}
				return provider, nil
			})
			if err != nil {
				return fmt.Errorf("gc omnigent attach: %w", err)
			}
			storedConversationID, err := omnigentLoadSessionKey(resolved.CityPath, identity.SessionID)
			if err != nil {
				return fmt.Errorf("gc omnigent attach: load durable conversation identity: %w", err)
			}
			requestedConversationID, err := resolveOmnigentRequestedConversation(storedConversationID, conversationID, identity.SessionID)
			if err != nil {
				return fmt.Errorf("gc omnigent attach: %w", err)
			}
			interrupts := make(chan os.Signal, 2)
			signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(interrupts)
			attachErr := runOmnigentAttach(cmd.Context(), client, omnigent.AttachmentOpenInput{
				ProfileID: selection.ID, ConversationID: requestedConversationID,
				Workspace: workspace, Title: title, Identity: identity, Location: omnigent.AttachmentLocation(location.Mode),
			}, func(candidate string) (string, error) {
				return omnigentBindSessionKey(resolved.CityPath, identity.SessionID, candidate)
			}, func(snapshot omnigent.SessionStatusSnapshot) error {
				return omnigentRecordSessionStatus(resolved.CityPath, identity.SessionID, snapshot)
			}, policyBridge, cmd.InOrStdin(), stdout, stderr, interrupts)
			policyBridge.Close()
			stopErr := stopSupervisor()
			if err := errors.Join(attachErr, stopErr); err != nil {
				return fmt.Errorf("gc omnigent attach: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profileID, "profile", "", "opaque local Omnigent execution profile ID")
	cmd.Flags().StringVar(&conversationID, "conversation", "", "exact opaque Omnigent conversation ID to resume")
	cmd.Flags().StringVar(&title, "title", "", "non-secret conversation title")
	cmd.Flags().StringVar(&attachmentMode, "mode", "", "explicit attachment boundary: controller or capsule")
	cmd.Flags().StringVar(&socketPath, "socket", "", "private capsule Unix socket (capsule mode only)")
	cmd.Flags().StringVar(&stateRoot, "state-root", "", "durable capsule Omnigent state root (capsule mode only)")
	cmd.Flags().StringVar(&catalogPath, "catalog", "", "immutable capsule profile catalog (capsule mode only)")
	return cmd
}

type omnigentAttachmentLocation struct {
	Mode        string
	SocketPath  string
	StateRoot   string
	CatalogPath string
}

func resolveOmnigentAttachmentLocation(mode, socketPath, stateRoot, catalogPath string) (omnigentAttachmentLocation, error) {
	mode = strings.TrimSpace(mode)
	socketPath = strings.TrimSpace(socketPath)
	stateRoot = strings.TrimSpace(stateRoot)
	catalogPath = strings.TrimSpace(catalogPath)
	switch mode {
	case string(omnigent.AttachmentLocationController):
		if socketPath != "" || stateRoot != "" || catalogPath != "" {
			return omnigentAttachmentLocation{}, errors.New("controller mode does not accept capsule paths")
		}
		return omnigentAttachmentLocation{Mode: mode}, nil
	case string(omnigent.AttachmentLocationCapsule):
		for kind, value := range map[string]string{"socket": socketPath, "state root": stateRoot, "catalog": catalogPath} {
			if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
				return omnigentAttachmentLocation{}, fmt.Errorf("capsule mode requires a clean absolute %s path", kind)
			}
		}
		return omnigentAttachmentLocation{Mode: mode, SocketPath: socketPath, StateRoot: stateRoot, CatalogPath: catalogPath}, nil
	case "":
		return omnigentAttachmentLocation{}, errors.New("--mode is required; choose controller or capsule explicitly")
	default:
		return omnigentAttachmentLocation{}, fmt.Errorf("unsupported attachment mode %q", mode)
	}
}

type omnigentSidecarRunner func(context.Context, omnigent.SidecarConfig) error

func startOmnigentCapsuleSupervisor(ctx context.Context, cfg omnigent.SidecarConfig, serve omnigentSidecarRunner, readiness func(context.Context, *omnigent.APIClient) error) (*omnigent.APIClient, func() error, error) {
	if serve == nil {
		return nil, nil, errors.New("omnigent capsule supervisor is required")
	}
	supervisorCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serve(supervisorCtx, cfg) }()
	client, err := omnigent.NewUnixAPIClient(cfg.SocketPath)
	if err != nil {
		cancel()
		<-done
		return nil, nil, err
	}
	if readiness == nil {
		readiness = func(probeCtx context.Context, client *omnigent.APIClient) error { return client.Health(probeCtx) }
	}
	timeout := cfg.StartupTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, timeout)
	defer readyCancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		healthCtx, healthCancel := context.WithTimeout(readyCtx, 250*time.Millisecond)
		healthErr := readiness(healthCtx, client)
		healthCancel()
		if healthErr == nil {
			var once sync.Once
			var stopErr error
			stop := func() error {
				once.Do(func() {
					cancel()
					select {
					case stopErr = <-done:
					case <-time.After(5 * time.Second):
						stopErr = errors.New("timed out stopping omnigent capsule supervisor")
					}
				})
				return stopErr
			}
			return client, stop, nil
		}
		select {
		case serveErr := <-done:
			cancel()
			if serveErr == nil {
				serveErr = errors.New("omnigent capsule supervisor exited before readiness")
			}
			return nil, nil, serveErr
		case <-readyCtx.Done():
			cancel()
			<-done
			return nil, nil, fmt.Errorf("wait for omnigent capsule supervisor readiness: %w", readyCtx.Err())
		case <-ticker.C:
		}
	}
}

func resolveOmnigentRequestedConversation(stored, explicit, sessionID string) (string, error) {
	stored = strings.TrimSpace(stored)
	explicit = strings.TrimSpace(explicit)
	if stored != "" && explicit != "" && explicit != stored {
		return "", fmt.Errorf("requested conversation %q conflicts with stored conversation %q for session %q", explicit, stored, sessionID)
	}
	if stored != "" {
		return stored, nil
	}
	return explicit, nil
}

func omnigentIdentityFromLookup(lookup func(string) (string, bool)) (omnigent.GasCityIdentity, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	read := func(name string) string {
		value, _ := lookup(name)
		return strings.TrimSpace(value)
	}
	identity := omnigent.GasCityIdentity{
		SessionID: read("GC_SESSION_ID"), Agent: read("GC_AGENT"),
		Rig: read("GC_RIG"), City: read("GC_CITY"),
	}
	if identity.SessionID == "" {
		return omnigent.GasCityIdentity{}, errors.New("GC_SESSION_ID is required; run through a Gas City-managed worker session")
	}
	if identity.Agent == "" {
		return omnigent.GasCityIdentity{}, errors.New("GC_AGENT is required; run through a Gas City-managed worker session")
	}
	return identity, nil
}

type omnigentInputResult struct {
	text string
	err  error
}

const (
	maxOmnigentInputBytes    = 4 << 20
	omnigentOperationTimeout = 30 * time.Second
)

func runOmnigentAttach(ctx context.Context, client *omnigent.APIClient, input omnigent.AttachmentOpenInput, bindConversation func(string) (string, error), recordStatus func(omnigent.SessionStatusSnapshot) error, policyBridge *omnigentPolicyMailBridge, stdin io.Reader, stdout, stderr io.Writer, interrupts <-chan os.Signal) (returnErr error) {
	if client == nil {
		return errors.New("omnigent client is required")
	}
	if bindConversation == nil {
		return errors.New("omnigent conversation persistence is required")
	}
	if input.Location == "" {
		input.Location = omnigent.AttachmentLocationController
	}
	operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
	rootProfile, err := client.ResolveProfile(operationCtx, input.ProfileID)
	cancel()
	if err != nil {
		return fmt.Errorf("resolve profile %q: %w", input.ProfileID, err)
	}
	operationCtx, cancel = context.WithTimeout(ctx, omnigentOperationTimeout)
	descriptor, err := client.ResolveAttachment(operationCtx, input)
	cancel()
	if err != nil {
		return fmt.Errorf("resolve conversation attachment: %w", err)
	}
	winner, err := bindConversation(descriptor.ConversationID)
	if err != nil {
		if descriptor.Fresh {
			cleanupErr := stopOmnigentConversation(ctx, client, descriptor.ConversationID)
			if cleanupErr != nil {
				return errors.Join(fmt.Errorf("persist fresh Omnigent conversation identity: %w", err), fmt.Errorf("stop unpersisted fresh conversation: %w", cleanupErr))
			}
		}
		return fmt.Errorf("persist Omnigent conversation identity: %w", err)
	}
	if winner != descriptor.ConversationID {
		if descriptor.Fresh {
			if err := stopOmnigentConversation(ctx, client, descriptor.ConversationID); err != nil {
				return fmt.Errorf("stop losing fresh Omnigent conversation %q: %w", descriptor.ConversationID, err)
			}
		}
		input.ConversationID = winner
		operationCtx, cancel = context.WithTimeout(ctx, omnigentOperationTimeout)
		descriptor, err = client.ResolveAttachment(operationCtx, input)
		cancel()
		if err != nil {
			return fmt.Errorf("resolve persisted conversation attachment %q: %w", winner, err)
		}
		if descriptor.ConversationID != winner || descriptor.Fresh {
			return fmt.Errorf("persisted Omnigent conversation %q did not resolve as an exact resume", winner)
		}
	}
	attachment, err := client.OpenResolvedAttachment(ctx, descriptor, input)
	if err != nil {
		return fmt.Errorf("open conversation attachment: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, attachment.Close())
	}()
	activeProfile := rootProfile
	if attachment.State.ActiveProfileID != rootProfile.ID {
		operationCtx, cancel = context.WithTimeout(ctx, omnigentOperationTimeout)
		activeProfile, err = client.ResolveProfile(operationCtx, attachment.State.ActiveProfileID)
		cancel()
		if err != nil {
			return fmt.Errorf("resolve active profile %q: %w", attachment.State.ActiveProfileID, err)
		}
	}
	status := omnigent.NewSessionStatusSnapshot(input.Location, input.ProfileID, attachment.State.ActiveProfileID, attachment.State.ActiveIndex, time.Now())
	if recordStatus != nil {
		if err := recordStatus(status); err != nil {
			return fmt.Errorf("record Omnigent attachment status: %w", err)
		}
	}
	if _, err := fmt.Fprintf(stderr, "[omnigent] conversation=%s profile=%s active_index=%d blurb=%q fallback_chain=%s\n",
		attachment.ConversationID, attachment.State.ActiveProfileID, attachment.State.ActiveIndex,
		activeProfile.Blurb, strings.Join(rootProfile.Chain, ",")); err != nil {
		return err
	}
	seen, err := renderOmnigentSnapshot(attachment.Snapshot, stdout)
	if err != nil {
		return fmt.Errorf("render Omnigent snapshot: %w", err)
	}

	streamDone := make(chan error, 1)
	stream := attachment.Stream
	conversationID := attachment.ConversationID
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	go func() {
		var lastSequence int64
		var hasSequence bool
		activeIndex := attachment.State.ActiveIndex
		streamDone <- stream.Consume(streamCtx, func(event omnigent.StreamEvent) error {
			if event.ConversationID != "" && event.ConversationID != conversationID {
				return fmt.Errorf("omnigent stream event belongs to conversation %q, expected %q", event.ConversationID, conversationID)
			}
			if event.SequenceNumber != nil {
				if hasSequence && *event.SequenceNumber <= lastSequence {
					return fmt.Errorf("omnigent stream sequence moved backward or repeated: %d after %d", *event.SequenceNumber, lastSequence)
				}
				lastSequence = *event.SequenceNumber
				hasSequence = true
			}
			if event.Error != nil {
				operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
				result, err := client.ObserveFailover(operationCtx, conversationID, activeIndex, event)
				cancel()
				if err != nil {
					return fmt.Errorf("observe Omnigent profile failover: %w", err)
				}
				if result.Exhausted {
					status.ActiveProfileID = result.ActiveProfileID
					status.ActiveIndex = result.ActiveIndex
					status.Degradation = omnigent.DegradationExhausted
					status.Exhausted = true
					status.ObservedAt = time.Now().UTC()
					if recordStatus != nil {
						if err := recordStatus(status); err != nil {
							return fmt.Errorf("record exhausted Omnigent attachment status: %w", err)
						}
					}
					return fmt.Errorf("omnigent profile failover exhausted at profile %q", result.ActiveProfileID)
				}
				if result.Transition != nil {
					activeIndex = result.ActiveIndex
					transition := result.Transition
					status.ActiveProfileID = result.ActiveProfileID
					status.ActiveIndex = result.ActiveIndex
					status.Degradation = omnigent.DegradationFromFailoverReason(transition.Reason)
					status.Exhausted = false
					status.ObservedAt = time.Now().UTC()
					if recordStatus != nil {
						if err := recordStatus(status); err != nil {
							return fmt.Errorf("record Omnigent failover status: %w", err)
						}
					}
					if _, err := fmt.Fprintf(stderr,
						"[omnigent] failover from=%s to=%s reason=%s at=%s from_blurb=%q to_blurb=%q\n",
						transition.FromProfileID, transition.ToProfileID, transition.Reason,
						transition.At.UTC().Format(time.RFC3339Nano), transition.FromBlurb, transition.ToBlurb,
					); err != nil {
						return err
					}
				}
			}
			if event.Type == "policy.request" || event.Type == "policy.cancelled" {
				if policyBridge == nil {
					return errors.New("omnigent policy interaction arrived but policy mail is unavailable")
				}
				if err := policyBridge.Observe(streamCtx, conversationID, event); err != nil {
					return err
				}
			}
			return renderOmnigentEvent(event, seen, stdout, stderr)
		})
	}()
	inputEvents := make(chan omnigentInputResult)
	go readOmnigentInput(stdin, inputEvents)

	for {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-interrupts:
			if sig == syscall.SIGTERM {
				stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), omnigentOperationTimeout)
				err := client.PostControl(stopCtx, attachment.ConversationID, "stop_session")
				stopCancel()
				if err != nil {
					return fmt.Errorf("stop Omnigent session: %w", err)
				}
				return nil
			}
			operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
			err := client.PostControl(operationCtx, attachment.ConversationID, "interrupt")
			cancel()
			if err != nil {
				return fmt.Errorf("interrupt Omnigent turn: %w", err)
			}
		case result, ok := <-inputEvents:
			if !ok {
				return nil
			}
			if result.err != nil {
				return result.err
			}
			if result.text == "" {
				continue
			}
			operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
			queued, err := client.PostMessage(operationCtx, attachment.ConversationID, result.text)
			cancel()
			if err != nil {
				return fmt.Errorf("send Omnigent message: %w", err)
			}
			if !queued {
				return errors.New("omnigent rejected message without queueing it")
			}
		case err := <-streamDone:
			if err != nil && ctx.Err() == nil {
				return err
			}
			return nil
		case err := <-policyBridgeErrors(policyBridge):
			if err != nil {
				return err
			}
		}
	}
}

func policyBridgeErrors(bridge *omnigentPolicyMailBridge) <-chan error {
	if bridge == nil {
		return nil
	}
	return bridge.Errors()
}

func stopOmnigentConversation(ctx context.Context, client *omnigent.APIClient, conversationID string) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), omnigentOperationTimeout)
	defer cancel()
	return client.PostControl(stopCtx, conversationID, "stop_session")
}

func readOmnigentInput(input io.Reader, out chan<- omnigentInputResult) {
	defer close(out)
	reader := bufio.NewReaderSize(input, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > maxOmnigentInputBytes {
			out <- omnigentInputResult{err: fmt.Errorf("omnigent input exceeds %d bytes", maxOmnigentInputBytes)}
			return
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line != "" {
			out <- omnigentInputResult{text: line}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				out <- omnigentInputResult{err: fmt.Errorf("read Omnigent input: %w", err)}
			}
			return
		}
	}
}

func renderOmnigentSnapshot(snapshot omnigent.Session, output io.Writer) (map[string]bool, error) {
	seen := make(map[string]bool, len(snapshot.Items))
	for _, item := range snapshot.Items {
		if item.ID != "" && seen[item.ID] {
			continue
		}
		if item.ID != "" {
			seen[item.ID] = true
		}
		if err := renderOmnigentItem(item, output); err != nil {
			return nil, err
		}
	}
	return seen, nil
}

func renderOmnigentEvent(event omnigent.StreamEvent, seen map[string]bool, stdout, stderr io.Writer) error {
	if event.Item != nil {
		if event.Item.ID != "" && seen[event.Item.ID] {
			return nil
		}
		if event.Item.ID != "" {
			seen[event.Item.ID] = true
		}
		return renderOmnigentItem(*event.Item, stdout)
	}
	if event.Delta != "" {
		target := stdout
		if strings.EqualFold(event.Source, "stderr") {
			target = stderr
		}
		_, err := io.WriteString(target, event.Delta)
		return err
	}
	if event.Error != nil {
		_, err := fmt.Fprintf(stderr, "[omnigent] error code=%s\n", event.Error.Code)
		return err
	}
	if event.Status != "" {
		_, err := fmt.Fprintf(stderr, "[omnigent] status=%s\n", event.Status)
		return err
	}
	return nil
}

func renderOmnigentItem(item omnigent.SessionItem, output io.Writer) error {
	prefix := ""
	if strings.EqualFold(item.Role, "user") {
		prefix = "> "
	}
	wrote := false
	for _, block := range item.Content {
		if block.Text == "" {
			continue
		}
		if !wrote && prefix != "" {
			if _, err := io.WriteString(output, prefix); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(output, block.Text); err != nil {
			return err
		}
		wrote = true
	}
	if wrote {
		_, err := io.WriteString(output, "\n")
		return err
	}
	return nil
}

func newOmnigentServeCmd(stdout, stderr io.Writer) *cobra.Command {
	var catalogPath string
	var startupTimeout time.Duration
	var shutdownTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve one Gas City-supervised local Omnigent sidecar",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stateRoot := strings.TrimSpace(os.Getenv("GC_SERVICE_STATE_ROOT"))
			if stateRoot == "" {
				return errors.New("gc omnigent serve: GC_SERVICE_STATE_ROOT is required; run this command through a Gas City proxy_process service")
			}
			if !filepath.IsAbs(stateRoot) {
				return errors.New("gc omnigent serve: GC_SERVICE_STATE_ROOT must be absolute")
			}
			socketPath := strings.TrimSpace(os.Getenv("GC_SERVICE_SOCKET"))
			if socketPath == "" {
				return errors.New("gc omnigent serve: GC_SERVICE_SOCKET is required; run this command through a Gas City proxy_process service")
			}
			if !filepath.IsAbs(socketPath) {
				return errors.New("gc omnigent serve: GC_SERVICE_SOCKET must be absolute")
			}
			if catalogPath != "" && !filepath.IsAbs(catalogPath) {
				return errors.New("gc omnigent serve: --catalog must be absolute")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := omnigent.ServeSidecar(ctx, omnigent.SidecarConfig{
				StateRoot: stateRoot, CatalogPath: catalogPath, SocketPath: socketPath,
				StartupTimeout: startupTimeout, ShutdownTimeout: shutdownTimeout,
				Stdout: stdout, Stderr: stderr,
			}); err != nil {
				if context.Cause(ctx) != nil {
					return nil
				}
				return fmt.Errorf("gc omnigent serve: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&catalogPath, "catalog", "", "absolute profile catalog path (default: <service-root>/config/profiles.yaml)")
	cmd.Flags().DurationVar(&startupTimeout, "startup-timeout", 0, "override bounded child readiness timeout")
	cmd.Flags().DurationVar(&shutdownTimeout, "shutdown-timeout", 0, "override exact-child shutdown grace period")
	return cmd
}
