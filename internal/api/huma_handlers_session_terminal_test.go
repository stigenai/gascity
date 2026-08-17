package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type terminalTestProvider struct {
	*runtime.Fake
	mu       sync.Mutex
	data     []byte
	actions  []string
	input    []byte
	keys     []string
	size     runtime.TerminalSize
	err      error
	readGate <-chan struct{}
}

func (p *terminalTestProvider) ReadTerminal(ctx context.Context, _ string, maxBytes int) (runtime.TerminalRead, error) {
	if p.readGate != nil {
		select {
		case <-p.readGate:
		case <-ctx.Done():
			return runtime.TerminalRead{}, ctx.Err()
		}
	}
	if p.err != nil {
		return runtime.TerminalRead{}, p.err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	data := append([]byte(nil), p.data...)
	truncated := len(data) > maxBytes
	if truncated {
		data = data[len(data)-maxBytes:]
	}
	return runtime.TerminalRead{Data: data, Truncated: truncated}, nil
}

func (p *terminalTestProvider) SendTerminalInput(ctx context.Context, _ string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, "input")
	p.input = append(p.input, data...)
	return p.err
}

func (p *terminalTestProvider) SendTerminalKeys(ctx context.Context, _ string, keys ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, "keys")
	p.keys = append(p.keys, keys...)
	return p.err
}

func (p *terminalTestProvider) ResizeTerminal(ctx context.Context, _ string, size runtime.TerminalSize) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, "resize")
	p.size = size
	return p.err
}

func (p *terminalTestProvider) InterruptTerminal(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, "interrupt")
	return p.err
}

func (p *terminalTestProvider) DetachTerminal(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, "detach")
	return p.err
}

func newTerminalFixture(t *testing.T, provider runtime.Provider) (*fakeState, session.Info) {
	t.Helper()
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	state.sessionsBeadStore = store
	state.sessionProvider = provider
	base, ok := provider.(*terminalTestProvider)
	if !ok {
		base = &terminalTestProvider{Fake: runtime.NewFake()}
	}
	info := createTestSession(t, store, base.Fake, "remote terminal")
	return state, info
}

func TestSessionTerminalSnapshotIsBoundedAndReconnectable(t *testing.T) {
	provider := &terminalTestProvider{Fake: runtime.NewFake(), data: []byte("0123456789")}
	state, info := newTerminalFixture(t, provider)
	h := newTestCityHandler(t, state)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, cityURL(state, "/session/"+info.ID+"/terminal?max_bytes=4"), nil))
	if first.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d body=%s", first.Code, first.Body.String())
	}
	var got SessionTerminalSnapshotBody
	if err := json.NewDecoder(first.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte("6789"))
	if string(got.Data) != "6789" || !got.Truncated || got.Unchanged || got.Cursor != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("snapshot = %#v", got)
	}

	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest(http.MethodGet, cityURL(state, "/session/"+info.ID+"/terminal?max_bytes=4&if_snapshot="+got.Cursor), nil))
	var unchanged SessionTerminalSnapshotBody
	if err := json.NewDecoder(second.Body).Decode(&unchanged); err != nil {
		t.Fatal(err)
	}
	if second.Code != http.StatusOK || !unchanged.Unchanged || len(unchanged.Data) != 0 || unchanged.Cursor != got.Cursor {
		t.Fatalf("reconnect status=%d body=%#v", second.Code, unchanged)
	}
}

func TestSessionTerminalActionsForwardExactlyOnceAndDetachCleanly(t *testing.T) {
	provider := &terminalTestProvider{Fake: runtime.NewFake()}
	state, info := newTerminalFixture(t, provider)
	h := newTestCityHandler(t, state)
	post := func(path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := newPostRequest(cityURL(state, "/session/"+info.ID+"/terminal/"+path), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, action := range []struct{ path, body string }{
		{"input", `{"data":"aGVsbG8="}`},
		{"keys", `{"keys":["Enter","C-c"]}`},
		{"resize", `{"rows":40,"columns":120}`},
		{"interrupt", `{}`},
		{"detach", `{}`},
	} {
		if rec := post(action.path, action.body); rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", action.path, rec.Code, rec.Body.String())
		}
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if got, want := strings.Join(provider.actions, ","), "input,keys,resize,interrupt,detach"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	if string(provider.input) != "hello" || strings.Join(provider.keys, ",") != "Enter,C-c" || provider.size != (runtime.TerminalSize{Rows: 40, Columns: 120}) {
		t.Fatalf("input=%q keys=%v size=%+v", provider.input, provider.keys, provider.size)
	}
	if !provider.IsRunning(info.SessionName) {
		t.Fatal("terminal detach stopped the underlying session")
	}
}

func TestSessionTerminalRejectsUnsupportedAndOversizedInput(t *testing.T) {
	state, info := newTerminalFixture(t, runtime.NewFake())
	h := newTestCityHandler(t, state)
	unsupported := httptest.NewRecorder()
	h.ServeHTTP(unsupported, httptest.NewRequest(http.MethodGet, cityURL(state, "/session/"+info.ID+"/terminal"), nil))
	if unsupported.Code != http.StatusNotImplemented || !strings.Contains(unsupported.Body.String(), "terminal-unsupported") {
		t.Fatalf("unsupported status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}

	provider := &terminalTestProvider{Fake: runtime.NewFake()}
	state, info = newTerminalFixture(t, provider)
	tooLarge := bytes.Repeat([]byte{'x'}, maxTerminalInputBytes+1)
	body, _ := json.Marshal(SessionTerminalInputBody{Data: tooLarge})
	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(state, "/session/"+info.ID+"/terminal/input"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	newTestCityHandler(t, state).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oversized status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(provider.actions) != 0 {
		t.Fatalf("oversized input reached provider: %v", provider.actions)
	}
}

func TestSessionTerminalCancellationAndProviderErrorsAreRedacted(t *testing.T) {
	gate := make(chan struct{})
	provider := &terminalTestProvider{Fake: runtime.NewFake(), readGate: gate}
	state, info := newTerminalFixture(t, provider)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(state).humaHandleSessionTerminalSnapshot(ctx, &SessionTerminalSnapshotInput{CityScope: CityScope{CityName: state.CityName()}, ID: info.ID})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v, want context.Canceled", err)
	}

	provider.readGate = nil
	provider.err = errors.New("ssh user:secret@private.example failed")
	rec := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/session/"+info.ID+"/terminal"), nil))
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "private.example") {
		t.Fatalf("provider error status=%d body=%s", rec.Code, rec.Body.String())
	}

	provider.err = errors.New("ambiguous ssh failure containing secret-input")
	body := []byte(`{"data":"c2VjcmV0LWlucHV0"}`)
	failed := httptest.NewRecorder()
	req := newPostRequest(cityURL(state, "/session/"+info.ID+"/terminal/input"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	newTestCityHandler(t, state).ServeHTTP(failed, req)
	if failed.Code != http.StatusServiceUnavailable || strings.Contains(failed.Body.String(), "secret-input") {
		t.Fatalf("ambiguous input status=%d body=%s", failed.Code, failed.Body.String())
	}
	provider.mu.Lock()
	inputCalls := 0
	for _, action := range provider.actions {
		if action == "input" {
			inputCalls++
		}
	}
	provider.mu.Unlock()
	if inputCalls != 1 {
		t.Fatalf("ambiguous input attempts = %d, want exactly one", inputCalls)
	}
}

func TestSessionTerminalInheritsReadWriteAuthHostOriginAndRedactedAudit(t *testing.T) {
	provider := &terminalTestProvider{Fake: runtime.NewFake(), data: []byte("private-output")}
	state, info := newTerminalFixture(t, provider)
	recorder := events.NewFake()
	resolver := &fakeCityResolver{cities: map[string]*fakeState{state.CityName(): state}, supervisorRecorder: recorder}
	now := time.Unix(1_787_000_000, 0)
	readPub, readPriv := mustKeypair(t)
	writePub, writePriv := mustKeypair(t)
	sm := NewSupervisorMux(resolver, nil, false, "test", "", now).
		WithAllowedHosts([]string{"operator.example"}).
		WithAllowedOrigins([]string{"https://operator.example"}).
		WithReadAuth(newTestReadVerifier(t, readPub, now)).
		WithWriteAuth(newTestWriteVerifier(t, writePub, now))

	readPath := cityURL(state, "/session/"+info.ID+"/terminal")
	missing := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://operator.example"+readPath, nil)
	sm.Handler().ServeHTTP(missing, req)
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing read grant status=%d", missing.Code)
	}
	readReq := httptest.NewRequest(http.MethodGet, "http://operator.example"+readPath, nil)
	readReq.Header.Set(readAuthHeader, mintToken(t, readPriv, readGrant(now, state.CityName(), http.MethodGet, readPath, "", "tty-read")))
	readReq.Header.Set("Origin", "https://operator.example")
	readRec := httptest.NewRecorder()
	sm.Handler().ServeHTTP(readRec, readReq)
	if readRec.Code != http.StatusOK || readRec.Header().Get("Access-Control-Allow-Origin") != "https://operator.example" {
		t.Fatalf("authorized read status=%d headers=%v body=%s", readRec.Code, readRec.Header(), readRec.Body.String())
	}

	writePath := cityURL(state, "/session/"+info.ID+"/terminal/input")
	body := []byte(`{"data":"c2VjcmV0LWlucHV0"}`)
	writeReq := httptest.NewRequest(http.MethodPost, "http://operator.example"+writePath, bytes.NewReader(body))
	writeReq.Header.Set("Content-Type", "application/json")
	writeReq.Header.Set(csrfHeaderName, "1")
	writeReq.Header.Set(writeAuthHeader, mintToken(t, writePriv, grantFor(now, state.CityName(), http.MethodPost, writePath, body, "tty-write")))
	writeRec := httptest.NewRecorder()
	sm.Handler().ServeHTTP(writeRec, writeReq)
	if writeRec.Code != http.StatusOK {
		t.Fatalf("authorized write status=%d body=%s", writeRec.Code, writeRec.Body.String())
	}

	hostile := httptest.NewRecorder()
	hostileReq := httptest.NewRequest(http.MethodGet, "http://attacker.invalid"+readPath, nil)
	sm.Handler().ServeHTTP(hostile, hostileReq)
	if hostile.Code != http.StatusMisdirectedRequest {
		t.Fatalf("hostile host status=%d", hostile.Code)
	}
	crossOrigin := httptest.NewRecorder()
	crossOriginReq := httptest.NewRequest(http.MethodOptions, "http://operator.example"+writePath, nil)
	crossOriginReq.Header.Set("Origin", "https://attacker.invalid")
	sm.Handler().ServeHTTP(crossOrigin, crossOriginReq)
	if crossOrigin.Code != http.StatusNoContent || crossOrigin.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("cross-origin preflight status=%d ACAO=%q", crossOrigin.Code, crossOrigin.Header().Get("Access-Control-Allow-Origin"))
	}
	for _, event := range recorder.Events {
		if bytes.Contains(event.Payload, []byte("secret-input")) || bytes.Contains(event.Payload, []byte("private-output")) {
			t.Fatalf("audit event leaked terminal bytes: %s", event.Payload)
		}
	}
}

func TestSessionTerminalGeneratedClientRoundTrip(t *testing.T) {
	client, state := newRoundTripClient(t)
	provider := &terminalTestProvider{Fake: runtime.NewFake(), data: []byte("generated-client-output")}
	state.sessionProvider = provider
	store := beads.NewMemStore()
	state.cityBeadStore = store
	state.sessionsBeadStore = store
	info := createTestSession(t, store, provider.Fake, "generated terminal")

	limit := int64(128)
	read, err := client.GetV0CityByCityNameSessionByIdTerminalWithResponse(
		context.Background(), state.CityName(), info.ID,
		&genclient.GetV0CityByCityNameSessionByIdTerminalParams{MaxBytes: &limit},
	)
	if err != nil {
		t.Fatal(err)
	}
	if read.StatusCode() != http.StatusOK || read.JSON200 == nil || read.JSON200.Data == nil {
		t.Fatalf("read status=%d body=%s", read.StatusCode(), read.Body)
	}
	decoded, err := base64.StdEncoding.DecodeString(*read.JSON200.Data)
	if err != nil || string(decoded) != "generated-client-output" {
		t.Fatalf("decoded data=%q err=%v", decoded, err)
	}

	write, err := client.PostV0CityByCityNameSessionByIdTerminalInputWithResponse(
		context.Background(), state.CityName(), info.ID,
		&genclient.PostV0CityByCityNameSessionByIdTerminalInputParams{XGCRequest: "1"},
		genclient.SessionTerminalInputBody{Data: base64.StdEncoding.EncodeToString([]byte("generated-input"))},
	)
	if err != nil {
		t.Fatal(err)
	}
	if write.StatusCode() != http.StatusOK || write.JSON200 == nil || write.JSON200.Action != genclient.Input {
		t.Fatalf("write status=%d body=%s", write.StatusCode(), write.Body)
	}
	if string(provider.input) != "generated-input" {
		t.Fatalf("provider input = %q", provider.input)
	}
}
