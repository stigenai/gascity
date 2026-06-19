package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestExtMsgReplyCommandUsesRefAndPostsOutbound(t *testing.T) {
	cityPath, _ := setupExtMsgReplyCLI(t, extMsgReplyAPIModeDelivered)
	refJSON := `{"provider":"provider-x","account_id":"acct-1","conversation_id":"conv-1","scope_id":"scope-a","kind":"dm"}`

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "extmsg", "reply", "--ref", refJSON, "hello from gc"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc extmsg reply exit = %d, want 0\nstdout=%q\nstderr=%q", code, stdout.String(), stderr.String())
	}
	if got, want := strings.TrimSpace(stdout.String()), "delivered conversation=provider-x/acct-1/conv-1 sequence=43"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestExtMsgReplyReadsTextFromStdin(t *testing.T) {
	cityPath, server := setupExtMsgReplyCLI(t, extMsgReplyAPIModeDelivered)
	refJSON := `{"provider":"provider-x","account_id":"acct-1","conversation_id":"conv-1","scope_id":"scope-a","kind":"dm"}`
	withExtMsgReplyStdin(t, "stdin response\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "extmsg", "reply", "--ref", refJSON}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc extmsg reply stdin exit = %d, want 0\nstdout=%q\nstderr=%q", code, stdout.String(), stderr.String())
	}
	if got := server.lastText(); got != "stdin response\n" {
		t.Fatalf("posted text = %q, want stdin payload", got)
	}
}

func TestExtMsgReplyNotDeliveredExitOne(t *testing.T) {
	cityPath, _ := setupExtMsgReplyCLI(t, extMsgReplyAPIModeNotDelivered)
	refJSON := `{"provider":"provider-x","account_id":"acct-1","conversation_id":"conv-1","kind":"dm"}`

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "extmsg", "reply", "--ref", refJSON, "hello"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("gc extmsg reply not-delivered exit = %d, want 1\nstdout=%q\nstderr=%q", code, stdout.String(), stderr.String())
	}
	if got, want := strings.TrimSpace(stdout.String()), "not-delivered conversation=provider-x/acct-1/conv-1 reason=no_subscriber"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "gc: extmsg reply: not delivered") {
		t.Fatalf("stderr = %q, want not-delivered diagnostic", stderr.String())
	}
}

func TestExtMsgReplyDryRunDoesNotCallAPI(t *testing.T) {
	cityPath, server := setupExtMsgReplyCLI(t, extMsgReplyAPIModeDelivered)
	refJSON := `{"provider":"provider-x","account_id":"acct-1","conversation_id":"conv-1","scope_id":"","kind":"dm"}`

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "extmsg", "reply", "--session", "worker", "--dry-run", "--ref", refJSON, "preview only"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc extmsg reply --dry-run exit = %d, want 0\nstdout=%q\nstderr=%q", code, stdout.String(), stderr.String())
	}
	if server.outboundCalls.Load() != 0 {
		t.Fatalf("dry-run made %d outbound API calls, want 0", server.outboundCalls.Load())
	}
	out := strings.TrimSpace(stdout.String())
	for _, want := range []string{
		"dry-run ref=",
		`"provider":"provider-x"`,
		`"account_id":"acct-1"`,
		`"conversation_id":"conv-1"`,
		"session=worker",
		`text="preview only"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run stdout missing %q:\n%s", want, out)
		}
	}
}

func TestExtMsgReplyNoContextExitTwo(t *testing.T) {
	cityPath, _ := setupExtMsgReplyCLI(t, extMsgReplyAPIModeDelivered)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityPath, "extmsg", "reply", "--session", "worker", "hello"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("gc extmsg reply without context exit = %d, want 2\nstdout=%q\nstderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc: no active external-origin context; use --ref") {
		t.Fatalf("stderr = %q, want missing-context diagnostic", stderr.String())
	}
}

func TestExtMsgReplySessionUnavailableExitThree(t *testing.T) {
	refJSON := `{"provider":"provider-x","account_id":"acct-1","conversation_id":"conv-1","kind":"dm"}`
	for _, tt := range []struct {
		name    string
		mode    extMsgReplyAPIMode
		session string
	}{
		{name: "unknown", mode: extMsgReplyAPIModeDelivered, session: "unknown"},
		{name: "not-running", mode: extMsgReplyAPIModeSessionStopped, session: "stopped"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cityPath, _ := setupExtMsgReplyCLI(t, tt.mode)

			var stdout, stderr bytes.Buffer
			code := run([]string{"--city", cityPath, "extmsg", "reply", "--session", tt.session, "--ref", refJSON, "hello"}, &stdout, &stderr)
			if code != 3 {
				t.Fatalf("gc extmsg reply unavailable session exit = %d, want 3\nstdout=%q\nstderr=%q", code, stdout.String(), stderr.String())
			}
			want := "gc: session " + tt.session + " not found or not running"
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
		})
	}
}

type extMsgReplyAPIMode int

const (
	extMsgReplyAPIModeDelivered extMsgReplyAPIMode = iota
	extMsgReplyAPIModeNotDelivered
	extMsgReplyAPIModeSessionStopped
)

type extMsgReplyTestServer struct {
	mode          extMsgReplyAPIMode
	outboundCalls atomic.Int64
	lastBody      atomic.Value
}

func setupExtMsgReplyCLI(t *testing.T, mode extMsgReplyAPIMode) (string, *extMsgReplyTestServer) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_SESSION_NAME", "worker")

	cityPath := filepath.Join(t.TempDir(), "reply-city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatalf("mkdir city: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"reply-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "test-city"); err != nil {
		t.Fatalf("register city: %v", err)
	}

	api := &extMsgReplyTestServer{mode: mode}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	writeExtMsgReplySupervisorConfig(t, srv.URL)

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})
	supervisorAliveHook = func() int { return 1234 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return true, "running", true }

	return cityPath, api
}

func writeExtMsgReplySupervisorConfig(t *testing.T, rawURL string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split server host: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(supervisor.ConfigPath()), 0o755); err != nil {
		t.Fatalf("mkdir supervisor config dir: %v", err)
	}
	data := fmt.Sprintf("[supervisor]\nbind = %q\nport = %s\n", host, port)
	if err := os.WriteFile(supervisor.ConfigPath(), []byte(data), 0o644); err != nil {
		t.Fatalf("write supervisor config: %v", err)
	}
}

func (s *extMsgReplyTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/sessions"):
		s.writeSessions(w)
	case strings.Contains(r.URL.Path, "/sessions/"):
		s.writeSession(w, filepath.Base(r.URL.Path))
	case strings.HasSuffix(r.URL.Path, "/extmsg/outbound"):
		s.writeOutbound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *extMsgReplyTestServer) writeSessions(w http.ResponseWriter) {
	running := s.mode != extMsgReplyAPIModeSessionStopped
	state := "active"
	if !running {
		state = "closed"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"items": []map[string]any{
			{
				"id":           "worker",
				"template":     "worker",
				"state":        state,
				"title":        "Worker",
				"session_name": "worker",
				"provider":     "fake",
				"created_at":   "2026-06-19T19:00:00Z",
				"running":      running,
			},
		},
		"total": 1,
	})
}

func (s *extMsgReplyTestServer) writeSession(w http.ResponseWriter, name string) {
	if s.mode == extMsgReplyAPIModeSessionStopped || name == "stopped" || name == "unknown" {
		http.Error(w, `{"detail":"not_found: session stopped"}`, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":           name,
		"template":     "worker",
		"state":        "active",
		"title":        "Worker",
		"session_name": name,
		"provider":     "fake",
		"created_at":   "2026-06-19T19:00:00Z",
		"running":      true,
	})
}

func (s *extMsgReplyTestServer) writeOutbound(w http.ResponseWriter, r *http.Request) {
	s.outboundCalls.Add(1)
	if r.Method != http.MethodPost {
		http.Error(w, "want POST", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-GC-Request") == "" {
		http.Error(w, "missing X-GC-Request", http.StatusForbidden)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.lastBody.Store(body)
	conv, _ := body["conversation"].(map[string]any)
	if got := body["session_id"]; got != "worker" {
		http.Error(w, fmt.Sprintf("session_id = %v, want worker", got), http.StatusBadRequest)
		return
	}
	if got := body["text"]; got == "" {
		http.Error(w, "text is empty", http.StatusBadRequest)
		return
	}
	for key, want := range map[string]string{
		"provider":        "provider-x",
		"account_id":      "acct-1",
		"conversation_id": "conv-1",
	} {
		if got := conv[key]; got != want {
			http.Error(w, fmt.Sprintf("conversation.%s = %v, want %s", key, got, want), http.StatusBadRequest)
			return
		}
	}
	delivered := s.mode != extMsgReplyAPIModeNotDelivered
	if !delivered {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Receipt": map[string]any{
				"Conversation": conv,
				"Delivered":    false,
			},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"Receipt": map[string]any{
			"Conversation": conv,
			"Delivered":    true,
			"MessageID":    "msg-43",
		},
		"TranscriptEntry": map[string]any{
			"Sequence": 43,
		},
	})
}

func (s *extMsgReplyTestServer) lastText() string {
	body, _ := s.lastBody.Load().(map[string]any)
	text, _ := body["text"].(string)
	return text
}

func withExtMsgReplyStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("create stdin temp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write stdin temp: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek stdin temp: %v", err)
	}
	oldStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = f.Close()
	})
}
