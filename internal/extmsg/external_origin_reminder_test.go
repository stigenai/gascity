package extmsg

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderExternalOriginSystemReminderOmittedWithoutMetadata(t *testing.T) {
	for _, metadata := range []map[string]string{
		nil,
		{},
		{"gc.extmsg.origin": ""},
	} {
		got, ok := RenderExternalOriginSystemReminder(metadata)
		if ok {
			t.Fatalf("RenderExternalOriginSystemReminder(%v) ok = true, want false", metadata)
		}
		if got != "" {
			t.Fatalf("RenderExternalOriginSystemReminder(%v) = %q, want empty", metadata, got)
		}
	}
}

func TestRenderExternalOriginSystemReminderIncludesReplyContext(t *testing.T) {
	raw, err := json.Marshal(ExternalOriginEnvelope{
		Conversation: ConversationRef{
			ScopeID:        "scope-a",
			Provider:       "provider-x",
			AccountID:      "acct-1",
			ConversationID: "conv-1",
			Kind:           ConversationDM,
		},
		BindingID:         "bind-1",
		BindingGeneration: 7,
	})
	if err != nil {
		t.Fatalf("marshal ExternalOriginEnvelope: %v", err)
	}

	got, ok := RenderExternalOriginSystemReminder(map[string]string{
		"gc.extmsg.origin": string(raw),
	})
	if !ok {
		t.Fatalf("RenderExternalOriginSystemReminder ok = false, want true")
	}

	for _, want := range []string{
		"<external-origin>",
		"provider: provider-x",
		"account_id: acct-1",
		"conversation_id: conv-1",
		"binding_id: bind-1",
		"Reply to this conversation using:",
		`gc extmsg reply "your response here"`,
		"Or, to include the ref explicitly:",
		"gc extmsg reply --ref",
		`"provider":"provider-x"`,
		`"account_id":"acct-1"`,
		`"conversation_id":"conv-1"`,
		`"scope_id":"scope-a"`,
		`"kind":"dm"`,
		"</external-origin>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("reminder missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "llm-client") {
		t.Fatalf("reminder should be provider-neutral, got hardcoded llm-client:\n%s", got)
	}
	if strings.Contains(got, "<system-reminder>") || strings.Contains(got, "</system-reminder>") {
		t.Fatalf("external-origin renderer should not wrap itself in system-reminder tags:\n%s", got)
	}
}
