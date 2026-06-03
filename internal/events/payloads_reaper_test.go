package events

import (
	"reflect"
	"strings"
	"testing"
)

// TestBeadWorktreeReapedEventConstant verifies the wire value of BeadWorktreeReaped.
func TestBeadWorktreeReapedEventConstant(t *testing.T) {
	if got := BeadWorktreeReaped; got != "bead.worktree.reaped" {
		t.Errorf("BeadWorktreeReaped = %q, want %q", got, "bead.worktree.reaped")
	}
}

// TestBeadWorktreeReapSkippedEventConstant verifies the wire value of BeadWorktreeReapSkipped.
func TestBeadWorktreeReapSkippedEventConstant(t *testing.T) {
	if got := BeadWorktreeReapSkipped; got != "bead.worktree.reap_skipped" {
		t.Errorf("BeadWorktreeReapSkipped = %q, want %q", got, "bead.worktree.reap_skipped")
	}
}

// TestBeadWorktreeReapConstantsInKnownEventTypes verifies both reaper constants
// appear in KnownEventTypes.
func TestBeadWorktreeReapConstantsInKnownEventTypes(t *testing.T) {
	index := make(map[string]bool, len(KnownEventTypes))
	for _, et := range KnownEventTypes {
		index[et] = true
	}
	for _, want := range []string{BeadWorktreeReaped, BeadWorktreeReapSkipped} {
		if !index[want] {
			t.Errorf("KnownEventTypes missing %q", want)
		}
	}
}

// TestBeadWorktreeReapedPayloadLookup verifies LookupPayload resolves
// BeadWorktreeReaped to BeadWorktreeReapedPayload.
func TestBeadWorktreeReapedPayloadLookup(t *testing.T) {
	sample, ok := LookupPayload(BeadWorktreeReaped)
	if !ok {
		t.Fatalf("LookupPayload(%q) returned !ok", BeadWorktreeReaped)
	}
	if _, ok := sample.(BeadWorktreeReapedPayload); !ok {
		t.Errorf("LookupPayload(%q) returned %T, want BeadWorktreeReapedPayload", BeadWorktreeReaped, sample)
	}
}

// TestBeadWorktreeReapSkippedPayloadLookup verifies LookupPayload resolves
// BeadWorktreeReapSkipped to BeadWorktreeReapSkippedPayload.
func TestBeadWorktreeReapSkippedPayloadLookup(t *testing.T) {
	sample, ok := LookupPayload(BeadWorktreeReapSkipped)
	if !ok {
		t.Fatalf("LookupPayload(%q) returned !ok", BeadWorktreeReapSkipped)
	}
	if _, ok := sample.(BeadWorktreeReapSkippedPayload); !ok {
		t.Errorf("LookupPayload(%q) returned %T, want BeadWorktreeReapSkippedPayload", BeadWorktreeReapSkipped, sample)
	}
}

// TestBeadWorktreeReapedPayloadJSONFields verifies that BeadWorktreeReapedPayload
// has exactly the mandatory JSON fields from the design: bead_id, path, rig, branch.
// No omitempty is permitted on mandatory fields.
func TestBeadWorktreeReapedPayloadJSONFields(t *testing.T) {
	assertPayloadFields(t, BeadWorktreeReapedPayload{}, map[string]string{
		"BeadID": "bead_id",
		"Path":   "path",
		"Rig":    "rig",
		"Branch": "branch",
	})
}

// TestBeadWorktreeReapSkippedPayloadJSONFields verifies that BeadWorktreeReapSkippedPayload
// has exactly the mandatory JSON fields from the design: bead_id, path, rig, reason.
// No omitempty is permitted on mandatory fields.
func TestBeadWorktreeReapSkippedPayloadJSONFields(t *testing.T) {
	assertPayloadFields(t, BeadWorktreeReapSkippedPayload{}, map[string]string{
		"BeadID": "bead_id",
		"Path":   "path",
		"Rig":    "rig",
		"Reason": "reason",
	})
}

// TestBeadWorktreeReapPayloadsRegisteredInEventsPackage verifies that the registered
// payload types for the reaper events live in the events package, not in
// internal/api/event_payloads.go. The design calls for self-registration in
// the events package init path.
func TestBeadWorktreeReapPayloadsRegisteredInEventsPackage(t *testing.T) {
	for _, et := range []string{BeadWorktreeReaped, BeadWorktreeReapSkipped} {
		sample, ok := LookupPayload(et)
		if !ok {
			t.Errorf("LookupPayload(%q) not registered", et)
			continue
		}
		pkgPath := reflect.TypeOf(sample).PkgPath()
		if !strings.HasSuffix(pkgPath, "/internal/events") {
			t.Errorf("payload for %q registered from package %q, want .../internal/events", et, pkgPath)
		}
	}
}

// assertPayloadFields checks that a payload struct has exactly the expected JSON
// field names and that no mandatory field carries omitempty.
func assertPayloadFields(t *testing.T, payload Payload, wantFields map[string]string) {
	t.Helper()
	typ := reflect.TypeOf(payload)
	seen := make(map[string]bool)
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Errorf("field %s missing json tag", field.Name)
			continue
		}
		parts := strings.SplitN(tag, ",", 2)
		jsonName := parts[0]
		if len(parts) > 1 && strings.Contains(parts[1], "omitempty") {
			t.Errorf("field %s uses omitempty on a mandatory field (json:%q)", field.Name, tag)
		}
		seen[field.Name] = true
		want, known := wantFields[field.Name]
		if !known {
			t.Errorf("unexpected struct field %s (json:%q)", field.Name, jsonName)
			continue
		}
		if jsonName != want {
			t.Errorf("field %s has json name %q, want %q", field.Name, jsonName, want)
		}
	}
	for name := range wantFields {
		if !seen[name] {
			t.Errorf("missing expected field %s", name)
		}
	}
}
