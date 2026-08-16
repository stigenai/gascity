package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/omnigent"
)

type omnigentMailProviderOpener func() (mail.Provider, error)

type omnigentPolicyMailBridge struct {
	client       *omnigent.APIClient
	sender       string
	openProvider omnigentMailProviderOpener
	pollInterval time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	errors chan error

	providerOnce sync.Once
	provider     mail.Provider
	providerErr  error

	mu      sync.Mutex
	pending map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func newOmnigentPolicyMailBridge(ctx context.Context, client *omnigent.APIClient, sender string, opener omnigentMailProviderOpener) (*omnigentPolicyMailBridge, error) {
	if client == nil || opener == nil {
		return nil, errors.New("omnigent policy mail requires a local client and mail provider")
	}
	sender = strings.TrimSpace(sender)
	if sender == "" {
		return nil, errors.New("omnigent policy mail sender is required")
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	return &omnigentPolicyMailBridge{
		client: client, sender: sender, openProvider: opener,
		pollInterval: omnigent.PolicyReplyPollInterval,
		ctx:          bridgeCtx, cancel: cancel, errors: make(chan error, 1),
		pending: make(map[string]context.CancelFunc),
	}, nil
}

func (b *omnigentPolicyMailBridge) Errors() <-chan error {
	if b == nil {
		return nil
	}
	return b.errors
}

func (b *omnigentPolicyMailBridge) Close() {
	if b == nil {
		return
	}
	b.cancel()
	b.mu.Lock()
	for _, cancel := range b.pending {
		cancel()
	}
	b.mu.Unlock()
	b.wg.Wait()
}

func (b *omnigentPolicyMailBridge) Observe(ctx context.Context, conversationID string, event omnigent.StreamEvent) error {
	if b == nil {
		return errors.New("omnigent policy interaction arrived but policy mail bridge is unavailable")
	}
	if event.Type != "policy.request" && event.Type != "policy.cancelled" {
		return nil
	}
	if event.Policy == nil || strings.TrimSpace(event.Policy.RequestID) == "" {
		return errors.New("omnigent policy event is missing a typed request id")
	}
	if event.Type == "policy.cancelled" {
		operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
		_, err := b.client.CancelPolicyRequest(operationCtx, omnigent.PolicyCancelInput{
			ConversationID: conversationID, RequestID: event.Policy.RequestID,
		})
		cancel()
		b.mu.Lock()
		if stop := b.pending[event.Policy.RequestID]; stop != nil {
			stop()
		}
		b.mu.Unlock()
		if err != nil {
			return fmt.Errorf("cancel Omnigent policy request: %w", err)
		}
		return nil
	}

	operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
	descriptor, err := b.client.OpenPolicyRequest(operationCtx, omnigent.PolicyRequestInput{
		ConversationID: conversationID, Request: *event.Policy,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("open Omnigent policy mail request: %w", err)
	}
	if descriptor.Status == "delivered" || descriptor.Status == "canceled" {
		return nil
	}
	b.mu.Lock()
	if _, exists := b.pending[descriptor.RequestID]; exists {
		b.mu.Unlock()
		return nil
	}
	workerCtx, workerCancel := context.WithCancel(b.ctx)
	b.pending[descriptor.RequestID] = workerCancel
	b.wg.Add(1)
	b.mu.Unlock()
	go b.runRequest(workerCtx, descriptor)
	return nil
}

func (b *omnigentPolicyMailBridge) runRequest(ctx context.Context, descriptor omnigent.PolicyRequestDescriptor) {
	defer b.wg.Done()
	defer func() {
		b.mu.Lock()
		if cancel := b.pending[descriptor.RequestID]; cancel != nil {
			cancel()
			delete(b.pending, descriptor.RequestID)
		}
		b.mu.Unlock()
	}()
	if err := b.deliverRequest(ctx, descriptor); err != nil && ctx.Err() == nil {
		select {
		case b.errors <- fmt.Errorf("omnigent policy mail %q: %w", descriptor.RequestID, err):
		default:
		}
	}
}

func (b *omnigentPolicyMailBridge) deliverRequest(ctx context.Context, descriptor omnigent.PolicyRequestDescriptor) error {
	provider, err := b.mailProvider()
	if err != nil {
		return err
	}
	request, err := findOrCreateOmnigentPolicyMail(provider, b.sender, descriptor)
	if err != nil {
		return err
	}
	if descriptor.MailID == "" {
		operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
		descriptor, err = b.client.BindPolicyMail(operationCtx, omnigent.PolicyMailBinding{
			ConversationID: descriptor.ConversationID, RequestID: descriptor.RequestID, MailID: request.ID,
		})
		cancel()
		if err != nil {
			return fmt.Errorf("bind durable policy mail %q: %w", request.ID, err)
		}
	} else if descriptor.MailID != request.ID {
		return fmt.Errorf("durable policy request is bound to mail %q, found %q", descriptor.MailID, request.ID)
	}
	ticker := time.NewTicker(b.pollInterval)
	defer ticker.Stop()
	for {
		answer, found, err := readOmnigentPolicyReply(provider, request, descriptor)
		if err != nil {
			return err
		}
		if found {
			operationCtx, cancel := context.WithTimeout(ctx, omnigentOperationTimeout)
			result, err := b.client.DeliverPolicyAnswer(operationCtx, omnigent.PolicyAnswerInput{
				ConversationID: descriptor.ConversationID,
				Answer: omnigent.PolicyAnswer{
					RequestID: answer.RequestID, MailID: request.ID,
					Action: answer.Action, Text: answer.Text,
				},
			})
			cancel()
			if err != nil {
				return fmt.Errorf("deliver structured policy reply: %w", err)
			}
			if !result.Delivered {
				return errors.New("sidecar did not confirm policy response delivery")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (b *omnigentPolicyMailBridge) mailProvider() (mail.Provider, error) {
	b.providerOnce.Do(func() { b.provider, b.providerErr = b.openProvider() })
	if b.providerErr != nil {
		return nil, b.providerErr
	}
	if b.provider == nil {
		return nil, errors.New("omnigent policy mail provider is unavailable")
	}
	return b.provider, nil
}

func findOrCreateOmnigentPolicyMail(provider mail.Provider, sender string, descriptor omnigent.PolicyRequestDescriptor) (mail.Message, error) {
	if descriptor.MailID != "" {
		message, err := provider.Get(descriptor.MailID)
		if err != nil {
			return mail.Message{}, fmt.Errorf("load bound policy mail %q: %w", descriptor.MailID, err)
		}
		if !omnigentPolicyMailMatches(message, descriptor) {
			return mail.Message{}, errors.New("bound policy mail does not match durable request")
		}
		return message, nil
	}
	messages, err := provider.All(descriptor.Recipient)
	if err != nil {
		return mail.Message{}, fmt.Errorf("search policy mail idempotency key: %w", err)
	}
	var match *mail.Message
	for i := range messages {
		if !omnigentPolicyMailMatches(messages[i], descriptor) {
			continue
		}
		if match != nil && match.ID != messages[i].ID {
			return mail.Message{}, errors.New("multiple durable policy request messages share one idempotency key")
		}
		item := messages[i]
		match = &item
	}
	if match != nil {
		return *match, nil
	}
	body, err := omnigent.PolicyMailBody(descriptor)
	if err != nil {
		return mail.Message{}, fmt.Errorf("encode policy mail: %w", err)
	}
	message, err := provider.Send(sender, descriptor.Recipient, omnigent.PolicyMailSubject(descriptor), string(body))
	if err != nil {
		return mail.Message{}, fmt.Errorf("send policy mail to %q: %w", descriptor.Recipient, err)
	}
	return message, nil
}

func omnigentPolicyMailMatches(message mail.Message, descriptor omnigent.PolicyRequestDescriptor) bool {
	if message.To != descriptor.Recipient || message.Subject != omnigent.PolicyMailSubject(descriptor) {
		return false
	}
	var envelope omnigent.PolicyMailEnvelope
	if err := decodeOmnigentPolicyJSON(message.Body, &envelope); err != nil {
		return false
	}
	return envelope.SchemaVersion == "1" && envelope.IdempotencyKey == descriptor.IdempotencyKey &&
		envelope.ConversationID == descriptor.ConversationID && envelope.RequestID == descriptor.RequestID
}

func readOmnigentPolicyReply(provider mail.Provider, request mail.Message, descriptor omnigent.PolicyRequestDescriptor) (omnigent.PolicyMailReply, bool, error) {
	thread, err := provider.Thread(request.ID)
	if err != nil {
		return omnigent.PolicyMailReply{}, false, fmt.Errorf("read policy mail thread: %w", err)
	}
	for _, message := range thread {
		if message.ID == request.ID || message.ReplyTo != request.ID {
			continue
		}
		if message.From != descriptor.Recipient {
			return omnigent.PolicyMailReply{}, false, fmt.Errorf("policy reply %q came from %q, expected %q", message.ID, message.From, descriptor.Recipient)
		}
		var reply omnigent.PolicyMailReply
		if err := decodeOmnigentPolicyJSON(message.Body, &reply); err != nil {
			return omnigent.PolicyMailReply{}, false, fmt.Errorf("policy reply %q is malformed structured JSON: %w", message.ID, err)
		}
		if reply.RequestID != descriptor.RequestID {
			return omnigent.PolicyMailReply{}, false, fmt.Errorf("policy reply %q request id does not match %q", message.ID, descriptor.RequestID)
		}
		return reply, true, nil
	}
	return omnigent.PolicyMailReply{}, false, nil
}

func decodeOmnigentPolicyJSON(body string, target any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
