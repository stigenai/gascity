package omnigent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPolicyControllerDefaultOffAndOptInDurableExactlyOnceRoundTrip(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	fake := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewPolicyController(client, catalog)
	if err != nil {
		t.Fatal(err)
	}
	input := PolicyRequestInput{ConversationID: "conv_failover", Request: PolicyRequest{
		RequestID: "policy-1", Kind: "tool.approval",
		Prompt:  "Run safe command; token=top-secret and sk-abcdefghijk?",
		Options: []string{"approve", "deny"},
		Context: map[string]string{"tool": "shell", "reason": "API_KEY=also-secret"},
	}}
	if _, err := controller.OpenRequest(context.Background(), input); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("default-off request error = %v", err)
	}
	fake.mu.Lock()
	if fake.labels[policyRequestIDLabel] != "" {
		fake.mu.Unlock()
		t.Fatal("default-off policy request mutated durable state")
	}
	fake.mu.Unlock()

	profile := catalog.profiles["claude-primary"]
	profile.PolicyMailRecipient = "human-review"
	catalog.profiles["claude-primary"] = profile
	descriptor, err := controller.OpenRequest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Recipient != "human-review" || descriptor.Status != "pending" || descriptor.IdempotencyKey == "" {
		t.Fatalf("descriptor = %#v", descriptor)
	}
	encoded, _ := json.Marshal(descriptor)
	for _, secret := range []string{"top-secret", "sk-abcdefghijk", "also-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("descriptor leaked %q: %s", secret, encoded)
		}
	}
	replay, err := controller.OpenRequest(context.Background(), input)
	if err != nil || replay.IdempotencyKey != descriptor.IdempotencyKey {
		t.Fatalf("request replay = %#v error=%v", replay, err)
	}

	binding := PolicyMailBinding{ConversationID: "conv_failover", RequestID: "policy-1", MailID: "mail-1"}
	bound, err := controller.BindMail(context.Background(), binding)
	if err != nil || bound.MailID != "mail-1" {
		t.Fatalf("BindMail = %#v error=%v", bound, err)
	}
	if _, err := controller.BindMail(context.Background(), binding); err != nil {
		t.Fatalf("BindMail replay: %v", err)
	}
	wrongBinding := binding
	wrongBinding.MailID = "mail-2"
	if _, err := controller.BindMail(context.Background(), wrongBinding); err == nil {
		t.Fatal("different duplicate mail binding succeeded")
	}

	wrong := PolicyAnswerInput{ConversationID: "conv_failover", Answer: PolicyAnswer{
		RequestID: "policy-1", MailID: "mail-1", Action: "auto-approve",
	}}
	if _, err := controller.Respond(context.Background(), wrong); err == nil {
		t.Fatal("answer outside explicit options succeeded")
	}
	answer := PolicyAnswerInput{ConversationID: "conv_failover", Answer: PolicyAnswer{
		RequestID: "policy-1", MailID: "mail-1", Action: "approve", Text: "explicit human choice",
	}}
	result, err := controller.Respond(context.Background(), answer)
	if err != nil || !result.Delivered || result.Status != "delivered" {
		t.Fatalf("Respond = %#v error=%v", result, err)
	}
	responseCount, lastResponse := fake.policyResponseSnapshot()
	if responseCount != 1 || lastResponse.Type != "policy_response" || lastResponse.Data.IdempotencyKey != descriptor.IdempotencyKey {
		t.Fatalf("policy responses=%d event=%#v", responseCount, lastResponse)
	}
	if lastResponse.Data.PolicyResponse == nil || lastResponse.Data.PolicyResponse.Action != "approve" {
		t.Fatalf("policy response = %#v", lastResponse.Data.PolicyResponse)
	}
	if _, err := controller.Respond(context.Background(), answer); err != nil {
		t.Fatalf("duplicate response: %v", err)
	}
	if got := fake.policyResponseCount(); got != 1 {
		t.Fatalf("duplicate response delivered %d times", got)
	}

	// Reconstructing the controller models a sidecar restart. Durable labels
	// still suppress a duplicate delivery.
	restarted, err := NewPolicyController(client, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Respond(context.Background(), answer); err != nil {
		t.Fatalf("post-restart response replay: %v", err)
	}
	if got := fake.policyResponseCount(); got != 1 {
		t.Fatalf("post-restart response delivered %d times", got)
	}
}

func TestPolicyControllerCancellationAndLateOrMismatchedRepliesFail(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	profile := catalog.profiles["claude-primary"]
	profile.PolicyMailRecipient = "reviewer"
	catalog.profiles["claude-primary"] = profile
	fake := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
	client, _ := NewAPIClient(fake.server.URL, fake.server.Client())
	controller, _ := NewPolicyController(client, catalog)
	descriptor, err := controller.OpenRequest(context.Background(), PolicyRequestInput{
		ConversationID: "conv_failover",
		Request:        PolicyRequest{RequestID: "policy-cancel", Kind: "question", Prompt: "Choose explicitly", Options: []string{"one", "two"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err = controller.BindMail(context.Background(), PolicyMailBinding{
		ConversationID: "conv_failover", RequestID: descriptor.RequestID, MailID: "mail-cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Cancel(context.Background(), PolicyCancelInput{
		ConversationID: "conv_failover", RequestID: descriptor.RequestID,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = controller.Respond(context.Background(), PolicyAnswerInput{ConversationID: "conv_failover", Answer: PolicyAnswer{
		RequestID: descriptor.RequestID, MailID: descriptor.MailID, Action: "one",
	}})
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("late reply error = %v", err)
	}
	if got := fake.policyResponseCount(); got != 0 {
		t.Fatalf("canceled request delivered %d responses", got)
	}
	if _, err := controller.Cancel(context.Background(), PolicyCancelInput{ConversationID: "conv_failover", RequestID: "other"}); err == nil {
		t.Fatal("mismatched cancellation succeeded")
	}
}

func TestPolicyControllerSerializesConcurrentRequestsPerConversation(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	profile := catalog.profiles["claude-primary"]
	profile.PolicyMailRecipient = "reviewer"
	catalog.profiles["claude-primary"] = profile
	fake := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewPolicyController(client, catalog)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 16
	var wg sync.WaitGroup
	descriptors := make(chan PolicyRequestDescriptor, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			descriptor, openErr := controller.OpenRequest(context.Background(), PolicyRequestInput{
				ConversationID: "conv_failover",
				Request:        PolicyRequest{RequestID: "policy-concurrent", Kind: "question", Prompt: "Same request", Options: []string{"yes", "no"}},
			})
			descriptors <- descriptor
			errs <- openErr
		}()
	}
	wg.Wait()
	close(descriptors)
	close(errs)
	for openErr := range errs {
		if openErr != nil {
			t.Fatal(openErr)
		}
	}
	var idempotencyKey string
	for descriptor := range descriptors {
		if descriptor.RequestID != "policy-concurrent" || descriptor.Status != "pending" {
			t.Fatalf("descriptor = %#v", descriptor)
		}
		if idempotencyKey == "" {
			idempotencyKey = descriptor.IdempotencyKey
		} else if descriptor.IdempotencyKey != idempotencyKey {
			t.Fatalf("idempotency key changed: %q != %q", descriptor.IdempotencyKey, idempotencyKey)
		}
	}
}

func TestPolicyRequestRemainsPendingAndDeliverableAcrossProfileFailover(t *testing.T) {
	catalog := loadFailoverCatalog(t)
	profile := catalog.profiles["claude-primary"]
	profile.PolicyMailRecipient = "reviewer"
	catalog.profiles["claude-primary"] = profile
	fake := newFailoverAPIFake(t, initialFailoverLabels(t, catalog))
	client, err := NewAPIClient(fake.server.URL, fake.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	policyController, err := NewPolicyController(client, catalog)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := policyController.OpenRequest(context.Background(), PolicyRequestInput{
		ConversationID: "conv_failover",
		Request:        PolicyRequest{RequestID: "policy-during-failover", Kind: "tool.approval", Prompt: "Proceed?", Options: []string{"approve", "deny"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policyController.BindMail(context.Background(), PolicyMailBinding{
		ConversationID: "conv_failover", RequestID: descriptor.RequestID, MailID: "mail-during-failover",
	}); err != nil {
		t.Fatal(err)
	}
	failoverController, err := NewFailoverController(client, catalog, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sequence := int64(9)
	result, err := failoverController.Advance(context.Background(), "conv_failover", 0, errorStreamEvent(&sequence, "429", 0))
	if err != nil || result.State.ActiveProfileID != "claude-secondary" {
		t.Fatalf("failover = %#v, %v", result, err)
	}
	replayed, err := policyController.OpenRequest(context.Background(), PolicyRequestInput{
		ConversationID: "conv_failover", Request: PolicyRequest{RequestID: descriptor.RequestID},
	})
	if err != nil || replayed.Status != "pending" || replayed.MailID != "mail-during-failover" {
		t.Fatalf("pending replay = %#v, %v", replayed, err)
	}
	delivery, err := policyController.Respond(context.Background(), PolicyAnswerInput{
		ConversationID: "conv_failover",
		Answer:         PolicyAnswer{RequestID: descriptor.RequestID, MailID: "mail-during-failover", Action: "approve"},
	})
	if err != nil || !delivery.Delivered || fake.policyResponseCount() != 1 {
		t.Fatalf("delivery = %#v responses=%d error=%v", delivery, fake.policyResponseCount(), err)
	}
}

func TestPolicyMailBodyIsStableStructuredAndRedacted(t *testing.T) {
	descriptor := PolicyRequestDescriptor{
		ConversationID: "conv-1", ProfileID: "profile", RequestID: "request-1",
		Kind: "approval", Prompt: "Proceed?", Options: []string{"approve", "deny"},
		Context: map[string]string{"tool": "shell"}, Recipient: "reviewer",
		Status: "pending", IdempotencyKey: "idem-1",
	}
	body, err := PolicyMailBody(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"schema_version":"1"`, `"idempotency_key":"idem-1"`, `"request_id":"request-1"`, `"response_schema"`, `"properties":{"request_id":{"const":"request-1"}`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("mail body missing %s: %s", want, body)
		}
	}
	if got := PolicyMailSubject(descriptor); got != "Omnigent policy approval [request-1]" {
		t.Fatalf("subject = %q", got)
	}
}
