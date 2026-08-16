package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/omnigent"
)

func TestOmnigentPolicyMailBridgeOptInRoundTripRestartAndDuplicateSafety(t *testing.T) {
	client, upstream := newPolicyMailSidecar(t, "reviewer")
	provider := beadmail.New(beads.NewMemStore())
	var opens atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, err := newOmnigentPolicyMailBridge(ctx, client, "worker-session", func() (mail.Provider, error) {
		opens.Add(1)
		return provider, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge.pollInterval = 10 * time.Millisecond
	defer bridge.Close()

	// Autonomous operation is the default: constructing the bridge alone does
	// not even open the mail backend.
	if opens.Load() != 0 {
		t.Fatal("policy mail provider opened without an explicit request")
	}
	event := omnigent.StreamEvent{Type: "policy.request", Policy: &omnigent.PolicyRequest{
		RequestID: "policy-live", Kind: "tool.approval", Prompt: "Run the tool?",
		Options: []string{"approve", "deny"}, Context: map[string]string{"tool": "shell"},
	}}
	if err := bridge.Observe(ctx, "conv_failover", event); err != nil {
		t.Fatal(err)
	}
	request := waitPolicyMail(t, provider, upstream, "reviewer")
	if opens.Load() != 1 {
		t.Fatalf("mail provider opens=%d", opens.Load())
	}
	replyBody := `{"request_id":"policy-live","action":"approve","text":"reviewed explicitly"}`
	if _, err := provider.Reply(request.ID, "reviewer", "RE: policy", replyBody); err != nil {
		t.Fatal(err)
	}
	waitPolicyResponses(t, upstream, 1)

	if err := bridge.Observe(ctx, "conv_failover", event); err != nil {
		t.Fatalf("duplicate observation: %v", err)
	}
	if got := policyResponseCount(upstream); got != 1 {
		t.Fatalf("duplicate observation delivered %d responses", got)
	}
	messages, err := provider.All("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("duplicate observation created %d request messages", len(messages))
	}

	bridge.Close()
	restarted, err := newOmnigentPolicyMailBridge(ctx, client, "worker-session", func() (mail.Provider, error) {
		return provider, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted.pollInterval = 10 * time.Millisecond
	defer restarted.Close()
	if err := restarted.Observe(ctx, "conv_failover", event); err != nil {
		t.Fatalf("post-restart observation: %v", err)
	}
	if got := policyResponseCount(upstream); got != 1 {
		t.Fatalf("post-restart observation delivered %d responses", got)
	}
}

func TestOmnigentPolicyMailBridgeMalformedReplyStaysVisibleAndPending(t *testing.T) {
	client, upstream := newPolicyMailSidecar(t, "reviewer")
	provider := beadmail.New(beads.NewMemStore())
	bridge, err := newOmnigentPolicyMailBridge(context.Background(), client, "worker", func() (mail.Provider, error) {
		return provider, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge.pollInterval = 10 * time.Millisecond
	defer bridge.Close()
	event := omnigent.StreamEvent{Type: "policy.request", Policy: &omnigent.PolicyRequest{
		RequestID: "policy-malformed", Kind: "question", Prompt: "Choose", Options: []string{"one", "two"},
	}}
	if err := bridge.Observe(context.Background(), "conv_failover", event); err != nil {
		t.Fatal(err)
	}
	request := waitPolicyMail(t, provider, upstream, "reviewer")
	if _, err := provider.Reply(request.ID, "reviewer", "RE", "yes please"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-bridge.Errors():
		if err == nil || !strings.Contains(err.Error(), "malformed structured JSON") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed reply did not surface")
	}
	if got := policyResponseCount(upstream); got != 0 {
		t.Fatalf("malformed reply delivered %d responses", got)
	}
	status := upstream.status()
	if status != "pending" {
		t.Fatalf("durable policy status = %q, want pending", status)
	}
}

func TestOmnigentPolicyMailBridgeMissingRecipientFailsWithoutMail(t *testing.T) {
	client, _ := newPolicyMailSidecar(t, "")
	provider := beadmail.New(beads.NewMemStore())
	bridge, err := newOmnigentPolicyMailBridge(context.Background(), client, "worker", func() (mail.Provider, error) {
		return provider, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	err = bridge.Observe(context.Background(), "conv_failover", omnigent.StreamEvent{
		Type: "policy.request", Policy: &omnigent.PolicyRequest{RequestID: "policy-off", Kind: "approval", Prompt: "Approve?", Options: []string{"yes", "no"}},
	})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error = %v", err)
	}
	messages, listErr := provider.All("reviewer")
	if listErr != nil || len(messages) != 0 {
		t.Fatalf("messages=%v error=%v", messages, listErr)
	}
}

type policySidecarFake struct {
	t          *testing.T
	server     *httptest.Server
	mu         sync.Mutex
	recipient  string
	descriptor omnigent.PolicyRequestDescriptor
	responses  int
	bound      chan struct{}
	responded  chan struct{}
}

func newPolicyMailSidecar(t *testing.T, recipient string) (*omnigent.APIClient, *policySidecarFake) {
	t.Helper()
	fake := &policySidecarFake{
		t: t, recipient: recipient,
		bound: make(chan struct{}, 1), responded: make(chan struct{}, 1),
	}
	fake.server = newOmnigentCLITestServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	client, err := omnigent.NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client, fake
}

func (f *policySidecarFake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/gascity/v1/policy/request":
		var input omnigent.PolicyRequestInput
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			f.t.Errorf("decode request: %v", err)
		}
		if f.recipient == "" {
			writePolicySidecarError(w, http.StatusConflict, "policy_request_rejected", "profile has no policy_mail_recipient; policy mail is disabled")
			return
		}
		if f.descriptor.RequestID == "" {
			f.descriptor = omnigent.PolicyRequestDescriptor{
				ConversationID: input.ConversationID, ProfileID: "claude-primary",
				RequestID: input.Request.RequestID, Kind: input.Request.Kind, Prompt: input.Request.Prompt,
				Options: input.Request.Options, Context: input.Request.Context,
				Recipient: f.recipient, Status: "pending", IdempotencyKey: "idem-policy-live",
			}
		}
		writePolicySidecarJSON(w, f.descriptor)
	case "/gascity/v1/policy/bind":
		var input omnigent.PolicyMailBinding
		_ = json.NewDecoder(r.Body).Decode(&input)
		if input.RequestID != f.descriptor.RequestID {
			writePolicySidecarError(w, http.StatusConflict, "policy_mail_rejected", "request mismatch")
			return
		}
		f.descriptor.MailID = input.MailID
		select {
		case f.bound <- struct{}{}:
		default:
		}
		writePolicySidecarJSON(w, f.descriptor)
	case "/gascity/v1/policy/respond":
		var input omnigent.PolicyAnswerInput
		_ = json.NewDecoder(r.Body).Decode(&input)
		if f.descriptor.Status != "delivered" {
			f.responses++
			f.descriptor.Status = "delivered"
			select {
			case f.responded <- struct{}{}:
			default:
			}
		}
		writePolicySidecarJSON(w, omnigent.PolicyDeliveryResult{RequestID: input.Answer.RequestID, Status: "delivered", Delivered: true})
	case "/gascity/v1/policy/cancel":
		f.descriptor.Status = "canceled"
		writePolicySidecarJSON(w, omnigent.PolicyDeliveryResult{RequestID: f.descriptor.RequestID, Status: "canceled"})
	default:
		http.NotFound(w, r)
	}
}

func (f *policySidecarFake) status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.descriptor.Status
}

func writePolicySidecarJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writePolicySidecarError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
}

func waitPolicyMail(t *testing.T, provider mail.Provider, upstream *policySidecarFake, recipient string) mail.Message {
	t.Helper()
	select {
	case <-upstream.bound:
	case <-time.After(time.Second):
		t.Fatalf("policy mail for %q was not bound", recipient)
	}
	messages, err := provider.All(recipient)
	if err != nil {
		t.Fatalf("list policy mail for %q: %v", recipient, err)
	}
	if len(messages) != 1 {
		t.Fatalf("policy mail for %q = %d messages, want 1", recipient, len(messages))
	}
	return messages[0]
}

func waitPolicyResponses(t *testing.T, upstream *policySidecarFake, want int) {
	t.Helper()
	for range want {
		select {
		case <-upstream.responded:
		case <-time.After(time.Second):
			t.Fatalf("policy responses=%d, want %d", policyResponseCount(upstream), want)
		}
	}
}

func policyResponseCount(upstream *policySidecarFake) int {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return upstream.responses
}
