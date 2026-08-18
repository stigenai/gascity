package omnigent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxJSONResponseBytes = 4 << 20
	maxSSEEventBytes     = 1 << 20
	maxErrorMessageBytes = 512
)

var opaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// APIClient is a typed client for the pinned Omnigent local session API.
type APIClient struct {
	baseURL     string
	http        *http.Client
	localHostID string
}

// EventStream is one established Omnigent session SSE subscription. Opening
// it is separate from consuming events so callers can subscribe before taking
// a session snapshot and avoid a reconnect race.
type EventStream struct {
	mu   sync.Mutex
	body io.ReadCloser
}

// Agent is the non-secret agent identity needed to bind an Omnigent session.
type Agent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Harness string `json:"harness"`
}

// Session is the subset of the Omnigent session snapshot the adapter uses.
type Session struct {
	ID        string            `json:"id"`
	AgentID   string            `json:"agent_id"`
	AgentName string            `json:"agent_name"`
	Status    string            `json:"status"`
	Archived  bool              `json:"archived"`
	Workspace string            `json:"workspace"`
	Labels    map[string]string `json:"labels"`
	Items     []SessionItem     `json:"items"`
}

// SessionItem is a committed conversation item used for reconnect rendering.
type SessionItem struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

type sessionItemWire struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content []ContentBlock  `json:"content"`
	Data    sessionItemData `json:"data"`
}

type sessionItemData struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// UnmarshalJSON accepts the pinned Omnigent item envelope and the earlier
// flat compatibility shape while keeping one typed representation internally.
func (i *SessionItem) UnmarshalJSON(data []byte) error {
	var wire sessionItemWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	i.ID = wire.ID
	i.Type = wire.Type
	i.Role = wire.Role
	i.Content = wire.Content
	if i.Role == "" {
		i.Role = wire.Data.Role
	}
	if len(i.Content) == 0 {
		i.Content = wire.Data.Content
	}
	return nil
}

// ContentBlock is a typed text-bearing item block. Unknown non-text fields are
// ignored at this external wire edge.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// CreateSessionInput is the Gas City-owned placement supplied to Omnigent.
type CreateSessionInput struct {
	AgentID   string
	Workspace string
	Title     string
	Labels    map[string]string
}

type createSessionRequest struct {
	AgentID      string            `json:"agent_id"`
	InitialItems []any             `json:"initial_items"`
	Title        string            `json:"title,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	HostType     string            `json:"host_type"`
	HostID       string            `json:"host_id,omitempty"`
	Workspace    string            `json:"workspace"`
	Git          any               `json:"git,omitempty"`
}

type sessionEventRequest struct {
	Type string           `json:"type"`
	Data sessionEventData `json:"data"`
}

type sessionEventData struct {
	Role           string          `json:"role,omitempty"`
	Content        []ContentBlock  `json:"content,omitempty"`
	PolicyResponse *PolicyResponse `json:"policy_response,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

// PolicyResponse is the typed answer delivered to Omnigent's pending policy
// interaction. It contains no mail body or provider prose.
type PolicyResponse struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	Text      string `json:"text,omitempty"`
}

// StreamError is the stable error classifier carried by Omnigent SSE events.
type StreamError struct {
	Code    string             `json:"code"`
	Message string             `json:"message"`
	Detail  *StreamErrorDetail `json:"detail"`
}

// StreamErrorDetail retains only stable, non-secret classifier fields from
// Omnigent's provider-specific error detail. Raw response bodies are ignored.
type StreamErrorDetail struct {
	StatusCode int `json:"status_code"`
}

// StreamEvent is the typed subset of an Omnigent session-stream event needed
// for status, output rendering, and failover classification.
type StreamEvent struct {
	Type           string         `json:"type"`
	ConversationID string         `json:"conversation_id"`
	SequenceNumber *int64         `json:"sequence_number"`
	Status         string         `json:"status"`
	Delta          string         `json:"delta"`
	Source         string         `json:"source"`
	Error          *StreamError   `json:"error"`
	Item           *SessionItem   `json:"item"`
	Policy         *PolicyRequest `json:"policy,omitempty"`
}

// APIError is a bounded, typed Omnigent HTTP error. It never retains the raw
// response body.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e == nil {
		return "omnigent API error"
	}
	if e.Code == "" {
		return fmt.Sprintf("omnigent API returned HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("omnigent API %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
}

// NewAPIClient builds a client for an explicit loopback HTTP endpoint.
func NewAPIClient(endpoint string, httpClient *http.Client) (*APIClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return nil, fmt.Errorf("parse omnigent endpoint: %w", err)
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("omnigent endpoint must be loopback HTTP without credentials, query, or fragment")
	}
	if parsed.Path != "" && (!strings.HasPrefix(parsed.Path, "/") || pathpkg.Clean(parsed.Path) != parsed.Path) {
		return nil, errors.New("omnigent endpoint path must be absolute and must not contain traversal")
	}
	hostname := parsed.Hostname()
	if hostname == "" || !isLoopbackHost(hostname) {
		return nil, fmt.Errorf("omnigent endpoint host %q is not loopback", hostname)
	}
	if httpClient == nil {
		return nil, errors.New("omnigent HTTP client is required")
	}
	return &APIClient{baseURL: strings.TrimSuffix(parsed.String(), "/"), http: httpClient}, nil
}

// NewUnixAPIClient builds a client that can dial only one configured Unix
// socket. No TCP fallback is installed.
func NewUnixAPIClient(socketPath string) (*APIClient, error) {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" || !filepath.IsAbs(socketPath) {
		return nil, errors.New("omnigent Unix socket path must be absolute")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		DisableCompression: true,
	}
	return &APIClient{
		baseURL: "http://omnigent.local",
		http: &http.Client{
			Transport: transport,
		},
	}, nil
}

// withLocalHost returns a client whose new sessions are bound to the one
// foreground host supervised beside the local Omnigent server. The host is
// transport placement only; Gas City still supplies the exact workspace.
func (c *APIClient) withLocalHost(hostID string) (*APIClient, error) {
	if c == nil {
		return nil, errors.New("omnigent API client is required")
	}
	hostID = strings.TrimSpace(hostID)
	if err := validateOpaqueID("host", hostID); err != nil {
		return nil, err
	}
	bound := *c
	bound.localHostID = hostID
	return &bound, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Health verifies that the local Omnigent server answers its readiness route.
func (c *APIClient) Health(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/health", nil, nil)
}

// ListProfiles returns the sidecar's stable, non-secret profile discovery
// view. Every field is revalidated at the client edge before it can reach a
// pane, status surface, or persisted conversation label.
func (c *APIClient) ListProfiles(ctx context.Context) ([]PublicProfile, error) {
	var profiles []PublicProfile
	if err := c.doJSON(ctx, http.MethodGet, "/gascity/v1/profiles", nil, &profiles); err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		return nil, errors.New("omnigent profile discovery returned no profiles")
	}
	byID := make(map[string]PublicProfile, len(profiles))
	for i := range profiles {
		profile := profiles[i]
		if err := validatePublicProfile(profile); err != nil {
			return nil, fmt.Errorf("omnigent profile discovery item %d: %w", i, err)
		}
		if _, exists := byID[profile.ID]; exists {
			return nil, fmt.Errorf("omnigent profile discovery returned duplicate profile %q", profile.ID)
		}
		profile.Chain = append([]string(nil), profile.Chain...)
		profiles[i] = profile
		byID[profile.ID] = profile
	}
	for _, profile := range profiles {
		for _, chainID := range profile.Chain {
			member, exists := byID[chainID]
			if !exists {
				return nil, fmt.Errorf("omnigent profile %q chain references unknown profile %q", profile.ID, chainID)
			}
			if member.Harness != profile.Harness {
				return nil, fmt.Errorf("omnigent profile %q chain member %q changes harness", profile.ID, chainID)
			}
		}
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return profiles, nil
}

// ResolveProfile returns one available profile by opaque ID. Missing and
// unavailable profiles remain explicit errors and never fall back to another
// profile or a fresh default.
func (c *APIClient) ResolveProfile(ctx context.Context, id string) (PublicProfile, error) {
	if !profileIDPattern.MatchString(id) {
		return PublicProfile{}, fmt.Errorf("invalid omnigent profile id %q", id)
	}
	profiles, err := c.ListProfiles(ctx)
	if err != nil {
		return PublicProfile{}, err
	}
	for _, profile := range profiles {
		if profile.ID != id {
			continue
		}
		if profile.Availability != "available" {
			return PublicProfile{}, fmt.Errorf("omnigent profile %q is unavailable; configure its local authentication environment", id)
		}
		return profile, nil
	}
	return PublicProfile{}, fmt.Errorf("unknown omnigent profile %q", id)
}

func validatePublicProfile(profile PublicProfile) error {
	if !profileIDPattern.MatchString(profile.ID) {
		return fmt.Errorf("profile id %q is invalid", profile.ID)
	}
	if strings.TrimSpace(profile.DisplayName) == "" {
		return fmt.Errorf("profile %q display_name is required", profile.ID)
	}
	if strings.TrimSpace(profile.Blurb) == "" {
		return fmt.Errorf("profile %q blurb is required", profile.ID)
	}
	if len(profile.Blurb) > 240 || secretBlurb.MatchString(profile.Blurb) {
		return fmt.Errorf("profile %q blurb is unsafe", profile.ID)
	}
	if profile.Harness != "claude-sdk" && profile.Harness != "codex" {
		return fmt.Errorf("profile %q harness %q is unsupported", profile.ID, profile.Harness)
	}
	if strings.TrimSpace(profile.Backend) == "" {
		return fmt.Errorf("profile %q backend is required", profile.ID)
	}
	if profile.Network != "offline" && profile.Network != "external-model" {
		return fmt.Errorf("profile %q network %q is unsupported", profile.ID, profile.Network)
	}
	if profile.Availability != "available" && profile.Availability != "unavailable" {
		return fmt.Errorf("profile %q availability %q is unsupported", profile.ID, profile.Availability)
	}
	if len(profile.Chain) == 0 || profile.Chain[0] != profile.ID {
		return fmt.Errorf("profile %q chain must start with its own id", profile.ID)
	}
	seen := make(map[string]bool, len(profile.Chain))
	for _, id := range profile.Chain {
		if !profileIDPattern.MatchString(id) {
			return fmt.Errorf("profile %q chain id %q is invalid", profile.ID, id)
		}
		if seen[id] {
			return fmt.Errorf("profile %q chain contains duplicate id %q", profile.ID, id)
		}
		seen[id] = true
	}
	return nil
}

// ListAgents returns all registered Omnigent agents, following cursor pages.
func (c *APIClient) ListAgents(ctx context.Context) ([]Agent, error) {
	var out []Agent
	after := ""
	for {
		path := "/v1/agents?limit=1000"
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var page struct {
			Data    []Agent `json:"data"`
			LastID  string  `json:"last_id"`
			HasMore bool    `json:"has_more"`
		}
		if err := c.doJSON(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Data...)
		if !page.HasMore {
			return out, nil
		}
		if page.LastID == "" || page.LastID == after {
			return nil, errors.New("omnigent agent pagination did not advance")
		}
		after = page.LastID
	}
}

// CreateSession creates a top-level external-host session in the exact Gas
// City-assigned workspace. Managed hosts and Omnigent git worktrees are never
// requested.
func (c *APIClient) CreateSession(ctx context.Context, input CreateSessionInput) (Session, error) {
	if !opaqueIDPattern.MatchString(input.AgentID) {
		return Session{}, fmt.Errorf("invalid omnigent agent id %q", input.AgentID)
	}
	workspace, err := normalizeWorkspace(input.Workspace)
	if err != nil {
		return Session{}, err
	}
	request := createSessionRequest{
		AgentID:      input.AgentID,
		InitialItems: []any{},
		Title:        strings.TrimSpace(input.Title),
		Labels:       cloneStringMap(input.Labels),
		HostType:     "external",
		HostID:       c.localHostID,
		Workspace:    workspace,
	}
	var session Session
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sessions", request, &session); err != nil {
		return Session{}, err
	}
	if err := validateSessionSnapshot(session, workspace); err != nil {
		return Session{}, err
	}
	return session, nil
}

// GetSession loads one exact conversation. A missing conversation remains a
// typed 404 and is never converted into a fresh session.
func (c *APIClient) GetSession(ctx context.Context, sessionID string) (Session, error) {
	if err := validateOpaqueID("session", sessionID); err != nil {
		return Session{}, err
	}
	var session Session
	if err := c.doJSON(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID), nil, &session); err != nil {
		return Session{}, err
	}
	if session.ID != sessionID {
		return Session{}, fmt.Errorf("omnigent session response id %q does not match requested id %q", session.ID, sessionID)
	}
	return session, nil
}

// PostMessage queues one user input event.
func (c *APIClient) PostMessage(ctx context.Context, sessionID, text string) (bool, error) {
	if err := validateOpaqueID("session", sessionID); err != nil {
		return false, err
	}
	if text == "" {
		return false, errors.New("omnigent message must not be empty")
	}
	request := sessionEventRequest{
		Type: "message",
		Data: sessionEventData{
			Role:    "user",
			Content: []ContentBlock{{Type: "input_text", Text: text}},
		},
	}
	var response struct {
		Queued bool `json:"queued"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/events", request, &response); err != nil {
		return false, err
	}
	return response.Queued, nil
}

// PostControl sends an interrupt or stop_session event.
func (c *APIClient) PostControl(ctx context.Context, sessionID, eventType string) error {
	if err := validateOpaqueID("session", sessionID); err != nil {
		return err
	}
	if eventType != "interrupt" && eventType != "stop_session" {
		return fmt.Errorf("unsupported omnigent control event %q", eventType)
	}
	request := sessionEventRequest{Type: eventType, Data: sessionEventData{}}
	var response struct {
		Queued bool `json:"queued"`
	}
	return c.doJSON(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/events", request, &response)
}

// PostPolicyResponse queues one idempotent structured policy answer.
func (c *APIClient) PostPolicyResponse(ctx context.Context, sessionID string, answer PolicyAnswer, idempotencyKey string) error {
	if err := validateOpaqueID("session", sessionID); err != nil {
		return err
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		return errors.New("omnigent policy response idempotency key is required")
	}
	request := sessionEventRequest{Type: "policy_response", Data: sessionEventData{
		PolicyResponse: &PolicyResponse{RequestID: answer.RequestID, Action: answer.Action, Text: answer.Text},
		IdempotencyKey: idempotencyKey,
	}}
	var response struct {
		Queued bool `json:"queued"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/events", request, &response); err != nil {
		return err
	}
	if !response.Queued {
		return errors.New("omnigent rejected policy response without queueing it")
	}
	return nil
}

// UpdateLabels merges non-secret adapter labels into a conversation.
func (c *APIClient) UpdateLabels(ctx context.Context, sessionID string, labels map[string]string) (Session, error) {
	if err := validateOpaqueID("session", sessionID); err != nil {
		return Session{}, err
	}
	request := struct {
		Labels map[string]string `json:"labels"`
	}{Labels: cloneStringMap(labels)}
	var session Session
	if err := c.doJSON(ctx, http.MethodPatch, "/v1/sessions/"+url.PathEscape(sessionID), request, &session); err != nil {
		return Session{}, err
	}
	return session, nil
}

// SwitchAgent rebinds an idle Omnigent conversation to another registered
// agent while preserving the conversation and transcript.
func (c *APIClient) SwitchAgent(ctx context.Context, sessionID, agentID string) (Session, error) {
	if err := validateOpaqueID("session", sessionID); err != nil {
		return Session{}, err
	}
	if err := validateOpaqueID("agent", agentID); err != nil {
		return Session{}, err
	}
	request := struct {
		AgentID string `json:"agent_id"`
	}{AgentID: agentID}
	var session Session
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sessions/"+url.PathEscape(sessionID)+"/switch-agent", request, &session); err != nil {
		return Session{}, err
	}
	if session.ID != sessionID {
		return Session{}, fmt.Errorf("omnigent switch-agent changed conversation id from %q to %q", sessionID, session.ID)
	}
	return session, nil
}

// Subscribe establishes a live Omnigent SSE stream and validates its media
// type. The caller must close the returned stream.
func (c *APIClient) Subscribe(ctx context.Context, sessionID string) (*EventStream, error) {
	if err := validateOpaqueID("session", sessionID); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/sessions/"+url.PathEscape(sessionID)+"/stream", nil)
	if err != nil {
		return nil, fmt.Errorf("build omnigent stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open omnigent session stream: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := decodeAPIError(resp)
		_ = resp.Body.Close()
		return nil, err
	}
	if mediaType := strings.ToLower(resp.Header.Get("Content-Type")); !strings.HasPrefix(mediaType, "text/event-stream") {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("omnigent stream returned content type %q", mediaType)
	}
	return &EventStream{body: resp.Body}, nil
}

// ConsumeStream reads a live Omnigent SSE stream in publish order until DONE,
// context cancellation, transport failure, or a consumer error.
func (c *APIClient) ConsumeStream(ctx context.Context, sessionID string, consume func(StreamEvent) error) error {
	stream, err := c.Subscribe(ctx, sessionID)
	if err != nil {
		return err
	}
	consumeErr := stream.Consume(ctx, consume)
	return errors.Join(consumeErr, stream.Close())
}

// Consume reads events from an established stream in publish order.
func (s *EventStream) Consume(ctx context.Context, consume func(StreamEvent) error) error {
	if s == nil {
		return errors.New("omnigent event stream is closed")
	}
	s.mu.Lock()
	body := s.body
	s.mu.Unlock()
	if body == nil {
		return errors.New("omnigent event stream is closed")
	}
	if consume == nil {
		return errors.New("omnigent stream consumer is required")
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), maxSSEEventBytes)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := consumeSSEData(data.String(), consume)
			data.Reset()
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read omnigent session stream: %w", err)
	}
	if data.Len() > 0 {
		_, err := consumeSSEData(data.String(), consume)
		return err
	}
	return errors.New("omnigent session stream closed before [DONE]")
}

// Close detaches this consumer without stopping or deleting the conversation.
func (s *EventStream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	body := s.body
	s.body = nil
	s.mu.Unlock()
	if body == nil {
		return nil
	}
	return body.Close()
}

func consumeSSEData(data string, consume func(StreamEvent) error) (bool, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return false, nil
	}
	if data == "[DONE]" {
		return true, nil
	}
	var event StreamEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return false, fmt.Errorf("decode omnigent stream event: %w", err)
	}
	if strings.TrimSpace(event.Type) == "" {
		return false, errors.New("decode omnigent stream event: type is required")
	}
	if event.Error != nil {
		event.Error.Message = boundedText(redactSensitiveText(event.Error.Message), maxErrorMessageBytes)
	}
	return false, consume(event)
}

func (c *APIClient) doJSON(ctx context.Context, method, path string, requestBody, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode omnigent request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build omnigent request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call omnigent API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp)
	}
	if responseBody == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONResponseBytes+1))
		return err
	}
	limited := &io.LimitedReader{R: resp.Body, N: maxJSONResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(responseBody); err != nil {
		return fmt.Errorf("decode omnigent response: %w", err)
	}
	if limited.N <= 0 {
		return errors.New("decode omnigent response: body exceeds size limit")
	}
	return nil
}

func decodeAPIError(resp *http.Response) error {
	limited := io.LimitReader(resp.Body, 64<<10)
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	message := http.StatusText(resp.StatusCode)
	code := ""
	if err := json.NewDecoder(limited).Decode(&envelope); err == nil {
		if envelope.Error != nil {
			code = strings.TrimSpace(envelope.Error.Code)
			if strings.TrimSpace(envelope.Error.Message) != "" {
				message = envelope.Error.Message
			}
		} else if detail, ok := envelope.Detail.(string); ok && strings.TrimSpace(detail) != "" {
			message = detail
		}
	}
	return &APIError{
		StatusCode: resp.StatusCode,
		Code:       boundedText(code, 80),
		Message:    boundedText(redactSensitiveText(strings.TrimSpace(message)), maxErrorMessageBytes),
	}
}

func validateSessionSnapshot(session Session, workspace string) error {
	if err := validateOpaqueID("session response", session.ID); err != nil {
		return err
	}
	snapshotWorkspace, err := normalizeWorkspace(session.Workspace)
	if err != nil {
		return err
	}
	if snapshotWorkspace != workspace {
		return fmt.Errorf("omnigent session workspace %q does not match Gas City assignment %q", session.Workspace, workspace)
	}
	return nil
}

func normalizeWorkspace(value string) (string, error) {
	workspace := filepath.Clean(strings.TrimSpace(value))
	if !filepath.IsAbs(workspace) {
		return "", errors.New("omnigent session workspace must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return workspace, nil
	}
	return "", fmt.Errorf("resolve omnigent session workspace: %w", err)
}

func validateOpaqueID(kind, id string) error {
	if !opaqueIDPattern.MatchString(id) {
		return fmt.Errorf("invalid omnigent %s id %q", kind, id)
	}
	return nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// RetryAfter returns a bounded Retry-After duration from an API response. It
// accepts delta-seconds only; date parsing belongs to Omnigent's own retry
// layer and is not used for Gas City routing.
func RetryAfter(header string) (time.Duration, bool) {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds < 0 || seconds > 3600 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}
