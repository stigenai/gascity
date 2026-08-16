package omnigent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	policyVersionLabel     = "gascity.omnigent.policy.version"
	policyRequestIDLabel   = "gascity.omnigent.policy.request_id"
	policyKindLabel        = "gascity.omnigent.policy.kind"
	policyPromptLabel      = "gascity.omnigent.policy.prompt"
	policyOptionsLabel     = "gascity.omnigent.policy.options"
	policyContextLabel     = "gascity.omnigent.policy.context"
	policyRecipientLabel   = "gascity.omnigent.policy.recipient"
	policyProfileLabel     = "gascity.omnigent.policy.profile"
	policyStatusLabel      = "gascity.omnigent.policy.status"
	policyMailIDLabel      = "gascity.omnigent.policy.mail_id"
	policyAnswerHashLabel  = "gascity.omnigent.policy.answer_hash"
	policyIdempotencyLabel = "gascity.omnigent.policy.idempotency_key"
	policyStateVersion     = "1"
)

var (
	policyKindPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,47}$`)
	policyKeyPattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,47}$`)
)

// PolicyRequest is one typed Omnigent interaction eligible for opt-in mail.
type PolicyRequest struct {
	RequestID string            `json:"request_id"`
	Kind      string            `json:"kind"`
	Prompt    string            `json:"prompt"`
	Options   []string          `json:"options,omitempty"`
	Context   map[string]string `json:"context,omitempty"`
}

// PolicyRequestInput binds a request to one exact conversation.
type PolicyRequestInput struct {
	ConversationID string        `json:"conversation_id"`
	Request        PolicyRequest `json:"request"`
}

// PolicyRequestDescriptor is the durable, sanitized mail request projection.
type PolicyRequestDescriptor struct {
	ConversationID string            `json:"conversation_id"`
	ProfileID      string            `json:"profile_id"`
	RequestID      string            `json:"request_id"`
	Kind           string            `json:"kind"`
	Prompt         string            `json:"prompt"`
	Options        []string          `json:"options,omitempty"`
	Context        map[string]string `json:"context,omitempty"`
	Recipient      string            `json:"recipient"`
	Status         string            `json:"status"`
	MailID         string            `json:"mail_id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
}

// PolicyMailBinding durably connects a Gas City message to a policy request.
type PolicyMailBinding struct {
	ConversationID string `json:"conversation_id"`
	RequestID      string `json:"request_id"`
	MailID         string `json:"mail_id"`
}

// PolicyAnswer is a structured human response. Action is never inferred.
type PolicyAnswer struct {
	RequestID string `json:"request_id"`
	MailID    string `json:"mail_id"`
	Action    string `json:"action"`
	Text      string `json:"text,omitempty"`
}

// PolicyAnswerInput binds an answer to its exact conversation.
type PolicyAnswerInput struct {
	ConversationID string       `json:"conversation_id"`
	Answer         PolicyAnswer `json:"answer"`
}

// PolicyDeliveryResult reports idempotent response delivery.
type PolicyDeliveryResult struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
	Delivered bool   `json:"delivered"`
}

// PolicyCancelInput cancels one exact pending request without a verdict.
type PolicyCancelInput struct {
	ConversationID string `json:"conversation_id"`
	RequestID      string `json:"request_id"`
}

// OpenPolicyRequest asks the private local sidecar to validate and persist one
// typed interaction. A profile without an explicit recipient fails closed.
func (c *APIClient) OpenPolicyRequest(ctx context.Context, input PolicyRequestInput) (PolicyRequestDescriptor, error) {
	var descriptor PolicyRequestDescriptor
	if err := c.doJSON(ctx, http.MethodPost, "/gascity/v1/policy/request", input, &descriptor); err != nil {
		return PolicyRequestDescriptor{}, err
	}
	if descriptor.ConversationID != input.ConversationID || descriptor.RequestID != input.Request.RequestID {
		return PolicyRequestDescriptor{}, errors.New("omnigent policy descriptor changed request identity")
	}
	return descriptor, nil
}

// BindPolicyMail records the exact Gas City mail request ID.
func (c *APIClient) BindPolicyMail(ctx context.Context, input PolicyMailBinding) (PolicyRequestDescriptor, error) {
	var descriptor PolicyRequestDescriptor
	if err := c.doJSON(ctx, http.MethodPost, "/gascity/v1/policy/bind", input, &descriptor); err != nil {
		return PolicyRequestDescriptor{}, err
	}
	return descriptor, nil
}

// DeliverPolicyAnswer validates and routes a structured reply through the
// private sidecar to the same Omnigent conversation.
func (c *APIClient) DeliverPolicyAnswer(ctx context.Context, input PolicyAnswerInput) (PolicyDeliveryResult, error) {
	var result PolicyDeliveryResult
	if err := c.doJSON(ctx, http.MethodPost, "/gascity/v1/policy/respond", input, &result); err != nil {
		return PolicyDeliveryResult{}, err
	}
	return result, nil
}

// CancelPolicyRequest records an explicit Omnigent cancellation without a
// verdict.
func (c *APIClient) CancelPolicyRequest(ctx context.Context, input PolicyCancelInput) (PolicyDeliveryResult, error) {
	var result PolicyDeliveryResult
	if err := c.doJSON(ctx, http.MethodPost, "/gascity/v1/policy/cancel", input, &result); err != nil {
		return PolicyDeliveryResult{}, err
	}
	return result, nil
}

// PolicyController owns durable policy interaction state in Omnigent labels.
type PolicyController struct {
	client  *APIClient
	catalog *Catalog
	locks   conversationLocks
}

// NewPolicyController constructs a local policy controller.
func NewPolicyController(client *APIClient, catalog *Catalog) (*PolicyController, error) {
	if client == nil || catalog == nil {
		return nil, errors.New("omnigent client and catalog are required for policy mail")
	}
	return &PolicyController{client: client, catalog: catalog, locks: conversationLocks{locks: make(map[string]*conversationLock)}}, nil
}

// OpenRequest validates and durably records an explicitly configured request.
func (c *PolicyController) OpenRequest(ctx context.Context, input PolicyRequestInput) (PolicyRequestDescriptor, error) {
	if err := validateOpaqueID("conversation", input.ConversationID); err != nil {
		return PolicyRequestDescriptor{}, err
	}
	release := c.locks.acquire(input.ConversationID)
	defer release()
	session, err := c.client.GetSession(ctx, input.ConversationID)
	if err != nil {
		return PolicyRequestDescriptor{}, redactedClientError("load omnigent conversation for policy request", err)
	}
	if existingID := strings.TrimSpace(session.Labels[policyRequestIDLabel]); existingID != "" {
		descriptor, parseErr := policyDescriptorFromLabels(input.ConversationID, session.Labels)
		if parseErr != nil {
			return PolicyRequestDescriptor{}, parseErr
		}
		if descriptor.RequestID == strings.TrimSpace(input.Request.RequestID) {
			return descriptor, nil
		}
		if descriptor.Status == "pending" || descriptor.Status == "delivering" {
			return PolicyRequestDescriptor{}, fmt.Errorf("conversation %q already has policy request %q pending", input.ConversationID, descriptor.RequestID)
		}
	}
	request, err := sanitizePolicyRequest(input.Request)
	if err != nil {
		return PolicyRequestDescriptor{}, err
	}
	chain, active, _, err := parseFailoverLabels(session.Labels)
	if err != nil {
		return PolicyRequestDescriptor{}, fmt.Errorf("conversation %q has invalid profile state: %w", input.ConversationID, err)
	}
	profileID := chain[active]
	profile, ok := c.catalog.Profile(profileID)
	if !ok {
		return PolicyRequestDescriptor{}, fmt.Errorf("conversation %q active profile %q is unavailable", input.ConversationID, profileID)
	}
	if profile.PolicyMailRecipient == "" {
		return PolicyRequestDescriptor{}, fmt.Errorf("profile %q has no policy_mail_recipient; policy mail is disabled", profileID)
	}
	optionsJSON, _ := json.Marshal(request.Options)
	contextJSON, _ := json.Marshal(request.Context)
	idempotencyKey := policyIdempotencyKey(input.ConversationID, request.RequestID)
	labels := map[string]string{
		policyVersionLabel: policyStateVersion, policyRequestIDLabel: request.RequestID,
		policyKindLabel: request.Kind, policyPromptLabel: request.Prompt,
		policyOptionsLabel: string(optionsJSON), policyContextLabel: string(contextJSON),
		policyRecipientLabel: profile.PolicyMailRecipient, policyProfileLabel: profileID,
		policyStatusLabel: "pending", policyMailIDLabel: "", policyAnswerHashLabel: "",
		policyIdempotencyLabel: idempotencyKey,
	}
	if _, err := c.client.UpdateLabels(ctx, input.ConversationID, labels); err != nil {
		return PolicyRequestDescriptor{}, redactedClientError("persist omnigent policy request", err)
	}
	return policyDescriptorFromLabels(input.ConversationID, labels)
}

// BindMail records the exact durable Gas City request message.
func (c *PolicyController) BindMail(ctx context.Context, binding PolicyMailBinding) (PolicyRequestDescriptor, error) {
	if err := validateOpaqueID("conversation", binding.ConversationID); err != nil {
		return PolicyRequestDescriptor{}, err
	}
	if err := validateOpaqueID("policy request", binding.RequestID); err != nil {
		return PolicyRequestDescriptor{}, err
	}
	if err := validateOpaqueID("mail", binding.MailID); err != nil {
		return PolicyRequestDescriptor{}, err
	}
	release := c.locks.acquire(binding.ConversationID)
	defer release()
	session, err := c.client.GetSession(ctx, binding.ConversationID)
	if err != nil {
		return PolicyRequestDescriptor{}, redactedClientError("load omnigent policy request", err)
	}
	descriptor, err := policyDescriptorFromLabels(binding.ConversationID, session.Labels)
	if err != nil {
		return PolicyRequestDescriptor{}, err
	}
	if descriptor.RequestID != binding.RequestID {
		return PolicyRequestDescriptor{}, errors.New("policy mail request id does not match pending request")
	}
	if descriptor.Status != "pending" {
		return PolicyRequestDescriptor{}, fmt.Errorf("policy request %q is %s", binding.RequestID, descriptor.Status)
	}
	if descriptor.MailID != "" {
		if descriptor.MailID != binding.MailID {
			return PolicyRequestDescriptor{}, errors.New("policy request is already bound to different mail")
		}
		return descriptor, nil
	}
	if _, err := c.client.UpdateLabels(ctx, binding.ConversationID, map[string]string{policyMailIDLabel: binding.MailID}); err != nil {
		return PolicyRequestDescriptor{}, redactedClientError("bind omnigent policy mail", err)
	}
	descriptor.MailID = binding.MailID
	return descriptor, nil
}

// Respond validates and delivers one explicit answer with a stable idempotency
// key. A crash after posting leaves status=delivering so replay uses the same key.
func (c *PolicyController) Respond(ctx context.Context, input PolicyAnswerInput) (PolicyDeliveryResult, error) {
	if err := validateOpaqueID("conversation", input.ConversationID); err != nil {
		return PolicyDeliveryResult{}, err
	}
	release := c.locks.acquire(input.ConversationID)
	defer release()
	session, err := c.client.GetSession(ctx, input.ConversationID)
	if err != nil {
		return PolicyDeliveryResult{}, redactedClientError("load omnigent policy response", err)
	}
	descriptor, err := policyDescriptorFromLabels(input.ConversationID, session.Labels)
	if err != nil {
		return PolicyDeliveryResult{}, err
	}
	answer, answerHash, err := validatePolicyAnswer(descriptor, input.Answer)
	if err != nil {
		return PolicyDeliveryResult{}, err
	}
	if descriptor.Status == "canceled" {
		return PolicyDeliveryResult{}, fmt.Errorf("policy request %q is canceled", descriptor.RequestID)
	}
	if descriptor.Status == "delivered" {
		if session.Labels[policyAnswerHashLabel] != answerHash {
			return PolicyDeliveryResult{}, errors.New("policy request was already delivered with a different answer")
		}
		return PolicyDeliveryResult{RequestID: descriptor.RequestID, Status: "delivered", Delivered: true}, nil
	}
	if descriptor.Status == "delivering" && session.Labels[policyAnswerHashLabel] != answerHash {
		return PolicyDeliveryResult{}, errors.New("policy request is delivering a different answer")
	}
	if descriptor.Status == "pending" {
		if _, err := c.client.UpdateLabels(ctx, input.ConversationID, map[string]string{
			policyStatusLabel: "delivering", policyAnswerHashLabel: answerHash,
		}); err != nil {
			return PolicyDeliveryResult{}, redactedClientError("arm omnigent policy response delivery", err)
		}
	}
	if err := c.client.PostPolicyResponse(ctx, input.ConversationID, answer, descriptor.IdempotencyKey); err != nil {
		return PolicyDeliveryResult{}, redactedClientError("deliver omnigent policy response", err)
	}
	if _, err := c.client.UpdateLabels(ctx, input.ConversationID, map[string]string{policyStatusLabel: "delivered"}); err != nil {
		return PolicyDeliveryResult{}, redactedClientError("record delivered omnigent policy response", err)
	}
	return PolicyDeliveryResult{RequestID: descriptor.RequestID, Status: "delivered", Delivered: true}, nil
}

// Cancel records cancellation without approving, denying, or expiring.
func (c *PolicyController) Cancel(ctx context.Context, input PolicyCancelInput) (PolicyDeliveryResult, error) {
	if err := validateOpaqueID("conversation", input.ConversationID); err != nil {
		return PolicyDeliveryResult{}, err
	}
	release := c.locks.acquire(input.ConversationID)
	defer release()
	session, err := c.client.GetSession(ctx, input.ConversationID)
	if err != nil {
		return PolicyDeliveryResult{}, redactedClientError("load omnigent policy cancellation", err)
	}
	descriptor, err := policyDescriptorFromLabels(input.ConversationID, session.Labels)
	if err != nil {
		return PolicyDeliveryResult{}, err
	}
	if descriptor.RequestID != strings.TrimSpace(input.RequestID) {
		return PolicyDeliveryResult{}, errors.New("policy cancellation does not match pending request")
	}
	if descriptor.Status == "delivered" {
		return PolicyDeliveryResult{}, errors.New("delivered policy request cannot be canceled")
	}
	if descriptor.Status != "canceled" {
		if _, err := c.client.UpdateLabels(ctx, input.ConversationID, map[string]string{policyStatusLabel: "canceled"}); err != nil {
			return PolicyDeliveryResult{}, redactedClientError("record omnigent policy cancellation", err)
		}
	}
	return PolicyDeliveryResult{RequestID: descriptor.RequestID, Status: "canceled"}, nil
}

func sanitizePolicyRequest(request PolicyRequest) (PolicyRequest, error) {
	request.RequestID = strings.TrimSpace(request.RequestID)
	request.Kind = strings.TrimSpace(request.Kind)
	if err := validateOpaqueID("policy request", request.RequestID); err != nil {
		return PolicyRequest{}, err
	}
	if !policyKindPattern.MatchString(request.Kind) {
		return PolicyRequest{}, errors.New("omnigent policy request kind is invalid")
	}
	prompt, err := sanitizePolicyText(request.Prompt, 2000, true)
	if err != nil {
		return PolicyRequest{}, fmt.Errorf("omnigent policy prompt: %w", err)
	}
	request.Prompt = prompt
	if len(request.Options) > 16 {
		return PolicyRequest{}, errors.New("omnigent policy request has too many options")
	}
	seen := make(map[string]bool, len(request.Options))
	for i, option := range request.Options {
		option, err = sanitizePolicyText(option, 160, true)
		if err != nil || seen[option] {
			return PolicyRequest{}, errors.New("omnigent policy request has invalid or duplicate options")
		}
		seen[option] = true
		request.Options[i] = option
	}
	if len(request.Context) > 8 {
		return PolicyRequest{}, errors.New("omnigent policy request has too many context fields")
	}
	context := make(map[string]string, len(request.Context))
	for key, value := range request.Context {
		key = strings.TrimSpace(key)
		if !policyKeyPattern.MatchString(key) {
			return PolicyRequest{}, errors.New("omnigent policy request context key is invalid")
		}
		value, err = sanitizePolicyText(value, 500, false)
		if err != nil {
			return PolicyRequest{}, fmt.Errorf("omnigent policy context %q: %w", key, err)
		}
		context[key] = value
	}
	request.Context = context
	return request, nil
}

func sanitizePolicyText(value string, limit int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", errors.New("value is required")
	}
	for _, char := range value {
		if (char < 0x20 && char != '\n' && char != '\t') || char == 0x7f {
			return "", errors.New("value contains control characters")
		}
	}
	value = redactSensitiveText(value)
	if len(value) > limit {
		value = value[:limit] + "..."
	}
	return value, nil
}

func policyDescriptorFromLabels(conversationID string, labels map[string]string) (PolicyRequestDescriptor, error) {
	if labels[policyVersionLabel] != policyStateVersion {
		return PolicyRequestDescriptor{}, errors.New("omnigent conversation has no valid pending policy request")
	}
	descriptor := PolicyRequestDescriptor{
		ConversationID: conversationID, ProfileID: labels[policyProfileLabel],
		RequestID: labels[policyRequestIDLabel], Kind: labels[policyKindLabel], Prompt: labels[policyPromptLabel],
		Recipient: labels[policyRecipientLabel], Status: labels[policyStatusLabel],
		MailID: labels[policyMailIDLabel], IdempotencyKey: labels[policyIdempotencyLabel],
	}
	if !profileIDPattern.MatchString(descriptor.ProfileID) || descriptor.Recipient == "" || descriptor.IdempotencyKey == "" {
		return PolicyRequestDescriptor{}, errors.New("omnigent policy request identity is invalid")
	}
	if err := validateOpaqueID("policy request", descriptor.RequestID); err != nil {
		return PolicyRequestDescriptor{}, err
	}
	switch descriptor.Status {
	case "pending", "delivering", "delivered", "canceled":
	default:
		return PolicyRequestDescriptor{}, errors.New("omnigent policy request status is invalid")
	}
	if err := json.Unmarshal([]byte(labels[policyOptionsLabel]), &descriptor.Options); err != nil {
		return PolicyRequestDescriptor{}, errors.New("omnigent policy request options are invalid")
	}
	if err := json.Unmarshal([]byte(labels[policyContextLabel]), &descriptor.Context); err != nil {
		return PolicyRequestDescriptor{}, errors.New("omnigent policy request context is invalid")
	}
	return descriptor, nil
}

func validatePolicyAnswer(descriptor PolicyRequestDescriptor, answer PolicyAnswer) (PolicyAnswer, string, error) {
	answer.RequestID = strings.TrimSpace(answer.RequestID)
	answer.MailID = strings.TrimSpace(answer.MailID)
	answer.Action = strings.TrimSpace(answer.Action)
	if answer.RequestID != descriptor.RequestID || answer.MailID == "" || answer.MailID != descriptor.MailID {
		return PolicyAnswer{}, "", errors.New("policy answer does not match pending request and mail")
	}
	if answer.Action == "" {
		return PolicyAnswer{}, "", errors.New("policy answer action is required")
	}
	if len(descriptor.Options) > 0 {
		allowed := false
		for _, option := range descriptor.Options {
			if answer.Action == option {
				allowed = true
				break
			}
		}
		if !allowed {
			return PolicyAnswer{}, "", errors.New("policy answer action is not one of the allowed options")
		}
	}
	text, err := sanitizePolicyText(answer.Text, 2000, len(descriptor.Options) == 0)
	if err != nil {
		return PolicyAnswer{}, "", fmt.Errorf("policy answer text: %w", err)
	}
	answer.Text = text
	encoded, _ := json.Marshal(answer)
	sum := sha256.Sum256(encoded)
	return answer, hex.EncodeToString(sum[:]), nil
}

func policyIdempotencyKey(conversationID, requestID string) string {
	sum := sha256.Sum256([]byte(conversationID + "\x00" + requestID))
	return "omnigent-policy-" + hex.EncodeToString(sum[:16])
}

// PolicyMailBody returns stable structured JSON for one durable request.
func PolicyMailBody(descriptor PolicyRequestDescriptor) ([]byte, error) {
	return json.Marshal(PolicyMailEnvelope{
		SchemaVersion: "1", IdempotencyKey: descriptor.IdempotencyKey,
		ConversationID: descriptor.ConversationID, ProfileID: descriptor.ProfileID,
		RequestID: descriptor.RequestID, Kind: descriptor.Kind, Prompt: descriptor.Prompt,
		Options: descriptor.Options, Context: descriptor.Context,
		ResponseSchema: PolicyResponseSchema{
			Type: "object", Required: []string{"request_id", "action"},
			Properties: PolicyResponseProperties{
				RequestID: PolicyResponseRequestIDSchema{Const: descriptor.RequestID},
				Action:    PolicyResponseActionSchema{Enum: descriptor.Options},
				Text:      PolicyResponseTextSchema{Type: "string"},
			},
		},
	})
}

// PolicyMailEnvelope is the stable durable request body stored in Gas City
// mail. It contains only sanitized fields and an explicit reply schema.
type PolicyMailEnvelope struct {
	SchemaVersion  string               `json:"schema_version"`
	IdempotencyKey string               `json:"idempotency_key"`
	ConversationID string               `json:"conversation_id"`
	ProfileID      string               `json:"profile_id"`
	RequestID      string               `json:"request_id"`
	Kind           string               `json:"kind"`
	Prompt         string               `json:"prompt"`
	Options        []string             `json:"options,omitempty"`
	Context        map[string]string    `json:"context,omitempty"`
	ResponseSchema PolicyResponseSchema `json:"response_schema"`
}

// PolicyResponseSchema is the typed JSON-schema-shaped reply contract.
type PolicyResponseSchema struct {
	Type       string                   `json:"type"`
	Required   []string                 `json:"required"`
	Properties PolicyResponseProperties `json:"properties"`
}

// PolicyResponseProperties defines the exact structured reply fields.
type PolicyResponseProperties struct {
	RequestID PolicyResponseRequestIDSchema `json:"request_id"`
	Action    PolicyResponseActionSchema    `json:"action"`
	Text      PolicyResponseTextSchema      `json:"text"`
}

// PolicyResponseRequestIDSchema pins reply correlation.
type PolicyResponseRequestIDSchema struct {
	Const string `json:"const"`
}

// PolicyResponseActionSchema enumerates explicit actions.
type PolicyResponseActionSchema struct {
	Enum []string `json:"enum"`
}

// PolicyResponseTextSchema describes optional answer text.
type PolicyResponseTextSchema struct {
	Type string `json:"type"`
}

// PolicyMailReply is the only accepted structured mail reply body.
type PolicyMailReply struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	Text      string `json:"text,omitempty"`
}

// PolicyMailSubject is stable and contains no prompt or credential data.
func PolicyMailSubject(descriptor PolicyRequestDescriptor) string {
	return fmt.Sprintf("Omnigent policy %s [%s]", descriptor.Kind, descriptor.RequestID)
}

// PolicyReplyPollInterval is the bounded asynchronous mail observation cadence.
const PolicyReplyPollInterval = time.Second
