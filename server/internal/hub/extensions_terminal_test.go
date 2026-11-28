package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/assistantwire"
	"github.com/nwverse/persona/server/internal/db"
	"github.com/nwverse/persona/server/internal/extension"
	"github.com/nwverse/persona/server/internal/extmgr"
	"github.com/nwverse/persona/server/internal/extwire"
	"github.com/nwverse/persona/server/internal/nodeproto"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
)

// TestHubMachinesExecValidationAndCapabilities verifies hub-side ownership, pending,
// offline, node_outdated, and successful machines.exec forwarding.
func TestHubMachinesExecValidationAndCapabilities(t *testing.T) {
	h, store := setupExtensionMachinesHub(t)

	// 1. Create a pending machine owned by m-alpha via hubMachinesPairLink.
	pairRes, err := h.hubMachinesPairLink(context.Background(), "m-alpha", "ext-a",
		json.RawMessage(`{"machine_id":"box-alpha","name":"Box Alpha","os":"linux"}`))
	if err != nil || pairRes.MachineID != "box-alpha" || !pairRes.Pending {
		t.Fatalf("pair_link box-alpha: res=%+v err=%v", pairRes, err)
	}
	mc := h.knownEntry("box-alpha")
	if mc == nil {
		t.Fatal("box-alpha entry not registered on hub")
	}
	mAlpha := newMachine(h, *mc)
	h.mu.Lock()
	h.machines = append(h.machines, mAlpha)
	h.byID["box-alpha"] = mAlpha
	h.mu.Unlock()

	req := json.RawMessage(`{"machine_id":"box-alpha","argv":["/bin/echo","hi"]}`)

	// Wrong manager rejected.
	if _, err := h.hubMachinesExec(context.Background(), "m-beta", "ext-a", req); err == nil {
		t.Fatal("expected other manager rejection")
	} else {
		var re *rpc.Error
		if !errors.As(err, &re) || re.Code != codeOtherManager {
			t.Fatalf("wrong manager: got %v, want %s", err, codeOtherManager)
		}
	}

	// Pending machine rejected.
	if _, err := h.hubMachinesExec(context.Background(), "m-alpha", "ext-a", req); err == nil {
		t.Fatal("expected pending machine rejection")
	} else {
		var re *rpc.Error
		if !errors.As(err, &re) || re.Code != "machine_pending" {
			t.Fatalf("pending machine: got %v, want machine_pending", err)
		}
	}

	// Confirm machine, still offline -> machine_offline.
	_, _ = store.ConfirmMachine(context.Background(), "box-alpha")
	h.setPendingLocal("box-alpha", time.Time{}, false)
	if _, err := h.hubMachinesExec(context.Background(), "m-alpha", "ext-a", req); err == nil {
		t.Fatal("expected offline machine rejection")
	} else {
		var re *rpc.Error
		if !errors.As(err, &re) || re.Code != "machine_offline" {
			t.Fatalf("offline machine: got %v, want machine_offline", err)
		}
	}

	// Bring node online WITHOUT machines_exec capability -> node_outdated.
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	nodePeer := rpc.New("n", serverConn)
	nodePeer.OnRequest = func(_ context.Context, method string, params json.RawMessage) (any, error) {
		if method == "machines.exec" {
			return wire.MachinesExecResult{MachineID: "box-alpha", ExitCode: 0, Output: "hi\n", DurationMs: 5}, nil
		}
		return nil, &rpc.Error{Code: "unknown_method", Message: "unknown method " + method}
	}
	go func() { _ = nodePeer.Serve(t.Context(), serverConn) }()
	hubPeer := rpc.New("h", clientConn)
	go func() { _ = hubPeer.Serve(t.Context(), clientConn) }()

	mAlpha.mu.Lock()
	mAlpha.online = true
	mAlpha.peer = hubPeer
	mAlpha.info.Protocol = 1
	mAlpha.info.Capabilities = []string{"terminal_stream"} // missing nodeproto.MachinesExec
	mAlpha.mu.Unlock()

	if _, err := h.hubMachinesExec(context.Background(), "m-alpha", "ext-a", req); err == nil {
		t.Fatal("expected node_outdated for old node")
	} else {
		var re *rpc.Error
		if !errors.As(err, &re) || re.Code != proto.CodeNodeOutdated {
			t.Fatalf("old node: got %v, want %s", err, proto.CodeNodeOutdated)
		}
	}

	// Declare machines_exec capability -> succeeds.
	mAlpha.mu.Lock()
	mAlpha.info.Capabilities = []string{nodeproto.MachinesExec}
	mAlpha.mu.Unlock()

	res, err := h.hubMachinesExec(context.Background(), "m-alpha", "ext-a", req)
	if err != nil {
		t.Fatalf("capable node hubMachinesExec: %v", err)
	}
	if res.MachineID != "box-alpha" || res.ExitCode != 0 || res.Output != "hi\n" {
		t.Fatalf("unexpected machines.exec result: %+v", res)
	}
}

// fakePTYExtension runs an interactive extension over a real Unix socket using sdk/go/ext + HandleTerminal.
// It simulates a PTY that echoes input, emits alternate-screen ANSI frames (vim-style \x1b[?1049h),
// records resize events, and records terminal.close calls.
type fakePTYExtension struct {
	mu      sync.Mutex
	opened  map[string]wire.TerminalOpenParams
	resizes map[string][]wire.TerminalResizeParams
	closed  map[string]wire.TerminalCloseParams
	seq     map[string]int
	closedC chan wire.TerminalCloseParams
}

func startFakePTYExtension(t *testing.T, socket string, manifest json.RawMessage) *fakePTYExtension {
	t.Helper()
	f := &fakePTYExtension{
		opened:  map[string]wire.TerminalOpenParams{},
		resizes: map[string][]wire.TerminalResizeParams{},
		closed:  map[string]wire.TerminalCloseParams{},
		seq:     map[string]int{},
		closedC: make(chan wire.TerminalCloseParams, 16),
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var hubRef *ext.Hub
				prog := ext.Program{
					ID:       "demo-pty",
					Version:  "1.0.0",
					Manifest: manifest,
					Init: func(_ context.Context, _ *ext.Init, h *ext.Hub) (map[string]any, error) {
						hubRef = h
						return nil, nil
					},
					Namespaces: map[string]ext.Handler{
						ext.TerminalNamespace: ext.HandleTerminal(ext.TerminalFuncs{
							OnOpen: func(_ context.Context, p wire.TerminalOpenParams) (wire.TerminalOpenResult, error) {
								sid := string(p.SessionID)
								f.mu.Lock()
								f.opened[sid] = p
								f.seq[sid] = 0
								f.mu.Unlock()
								return wire.TerminalOpenResult{SessionID: p.SessionID, Title: "PTY " + p.Target}, nil
							},
							OnInput: func(_ context.Context, sid string, data []byte) error {
								f.mu.Lock()
								f.seq[sid]++
								seq := f.seq[sid]
								f.mu.Unlock()
								if string(data) == "vim\r" {
									// Simulate entering + drawing + leaving alternate screen buffer.
									alt := []byte("\x1b[?1049h\x1b[2J\x1b[1;1H~ VIM E2E ~\x1b[?1049l")
									return hubRef.TerminalFrame(sid, seq, alt)
								}
								return hubRef.TerminalFrame(sid, seq, append([]byte("ECHO:"), data...))
							},
							OnResize: func(_ context.Context, p wire.TerminalResizeParams) error {
								sid := string(p.SessionID)
								f.mu.Lock()
								f.resizes[sid] = append(f.resizes[sid], p)
								f.mu.Unlock()
								return nil
							},
							OnClose: func(_ context.Context, p wire.TerminalCloseParams) error {
								sid := string(p.SessionID)
								f.mu.Lock()
								f.closed[sid] = p
								f.mu.Unlock()
								select {
								case f.closedC <- p:
								default:
								}
								return nil
							},
						}),
					},
				}
				_ = ext.Serve(t.Context(), c, c, prog)
			}(conn)
		}
	}()
	return f
}

// memExtTermStore implements extmgr.Store and extension.Store in memory so the real Hub + Manager + PTY
// WebSocket E2E test runs anywhere without requiring an external Postgres instance.
type memExtTermStore struct {
	mu       sync.Mutex
	settings map[string]json.RawMessage
	exts     map[string]extension.Record
}

func newMemExtTermStore() *memExtTermStore {
	return &memExtTermStore{
		settings: map[string]json.RawMessage{},
		exts:     map[string]extension.Record{},
	}
}

func (s *memExtTermStore) Setting(_ context.Context, key string, out any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.settings[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, out)
}

func (s *memExtTermStore) PutSetting(_ context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[key] = raw
	return nil
}

func (s *memExtTermStore) Extensions(context.Context) ([]extension.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]extension.Record, 0, len(s.exts))
	for _, r := range s.exts {
		out = append(out, r)
	}
	return out, nil
}

func (s *memExtTermStore) PutExtension(_ context.Context, r extension.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exts[r.ID] = r
	return nil
}

func (s *memExtTermStore) DeleteExtension(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.exts, id)
	return nil
}

func (s *memExtTermStore) ExtensionSecrets(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (s *memExtTermStore) PutExtensionSecret(context.Context, string, string, string) error {
	return nil
}
func (s *memExtTermStore) DeleteExtensionSecret(context.Context, string, string) error { return nil }
func (s *memExtTermStore) AddExtensionAudit(context.Context, string, string, string, json.RawMessage) error {
	return nil
}

type memBusIDStore struct {
	mu     sync.Mutex
	byID   map[string]db.BusIdentity
	byHash map[string]string
}

func newMemBusIDStore() *memBusIDStore {
	return &memBusIDStore{byID: map[string]db.BusIdentity{}, byHash: map[string]string{}}
}

func (s *memBusIDStore) SaveBusIdentity(_ context.Context, b db.BusIdentity, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, id := range s.byHash {
		if id == b.ID {
			delete(s.byHash, h)
		}
	}
	s.byID[b.ID] = b
	s.byHash[tokenHash] = b.ID
	return nil
}

func (s *memBusIDStore) BusIdentityByHash(_ context.Context, hash string) (*db.BusIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byHash[hash]
	if !ok {
		return nil, nil
	}
	b := s.byID[id]
	return &b, nil
}

func (s *memBusIDStore) BusIdentities(context.Context) ([]db.BusIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]db.BusIdentity, 0, len(s.byID))
	for _, b := range s.byID {
		out = append(out, b)
	}
	return out, nil
}

func (s *memBusIDStore) DeleteBusIdentity(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.byID[id]
	delete(s.byID, id)
	for h, owner := range s.byHash {
		if owner == id {
			delete(s.byHash, h)
		}
	}
	return ok, nil
}

// TestExtensionTerminalE2ERealHubManagerPTYAndWS is a real end-to-end integration test covering:
//   - Real Hub + real extmgr.Manager + real Unix-socket extension hosting HandleTerminal PTY
//   - Non-roleFull client rejection (roleAnswer watch-only)
//   - Mandatory owner confirmation (confirmed: false refused)
//   - Bidirectional input/echo, resize sync, vim/alternate-screen ANSI frame passthrough
//   - Single-frame 64 KiB cap enforcement
//   - Per-extension concurrency limit (4 open succeed, 5th refused with too_many_sessions)
//   - Automatic terminal.close when last subscriber disconnects
func TestExtensionTerminalE2ERealHubManagerPTYAndWS(t *testing.T) {
	h := newTestHub(t)
	h.store = &tokenStore{rows: map[string]db.DeviceToken{}, rev: map[string]bool{}}
	h.bus.idents = newMemBusIDStore()
	h.public.id = "00000000-0000-4000-8000-000000000099"
	srv := httptest.NewServer(h.routes())
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		for _, c := range h.stopBus() {
			_ = c.ws.CloseNow()
		}
	})
	store := newMemExtTermStore()
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "pty.sock")

	manifest := json.RawMessage(`{
		"manifest_version": 1,
		"id": "demo-pty",
		"name": "Demo PTY",
		"version": "1.0.0",
		"engines": {"extension_protocol": 1},
		"runtime": {"mode": "process", "command": "demo-pty", "rpc": "stdio"},
		"activation_events": ["on_call"],
		"contributes": {
			"terminal_sessions": [{"id": "shell", "title": "Interactive Shell"}]
		}
	}`)
	fakeExt := startFakePTYExtension(t, socket, manifest)
	if _, err := extension.Parse(manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if err := store.PutExtension(t.Context(), extension.Record{
		ID:       "demo-pty",
		Source:   extension.SourceInstalled,
		Version:  "1.0.0",
		Manifest: manifest,
		Enabled:  true,
		Remote:   &extension.Remote{RPCSocket: socket},
	}); err != nil {
		t.Fatalf("PutExtension: %v", err)
	}
	t.Cleanup(func() { _ = store.DeleteExtension(context.Background(), "demo-pty") })

	var manager *extmgr.Manager
	raw := dialRawBusWith(t, srv, busToken(t, h, assistantwire.AssistantID, "PTY Manager"), func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		if manager != nil {
			if answer, handled, err := manager.Request(ctx, method, params); handled {
				return answer, err
			}
		}
		return map[string]any{}, nil
	})
	manager = extmgr.New(extmgr.Options{Dir: dir, Config: &extmgr.Config{}, Store: store, Hub: raw.peer, Logf: t.Logf})
	raw.peer.OnEvent = func(event string, params json.RawMessage) { manager.Event(event, params) }

	var hello json.RawMessage
	if err := raw.call(assistantwire.MethodHello, map[string]any{"id": assistantwire.AssistantID}, &hello); err != nil {
		t.Fatalf("manager hello: %v", err)
	}
	manager.Hello(hello)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("manager shutdown timeout")
		}
	})
	select {
	case <-manager.Started():
	case <-time.After(5 * time.Second):
		t.Fatal("manager start timeout")
	}

	dialWS := func(token string) *websocket.Conn {
		t.Helper()
		wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws?token=" + token
		ws, _, err := websocket.Dial(t.Context(), wsURL, nil)
		if err != nil {
			t.Fatalf("dial ws: %v", err)
		}
		t.Cleanup(func() { _ = ws.CloseNow() })
		// Drain until hello so connection is registered.
		hctx, hcancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer hcancel()
		for {
			_, frame, err := ws.Read(hctx)
			if err != nil {
				t.Fatalf("read hello: %v", err)
			}
			var m struct {
				Type     string   `json:"type"`
				Features []string `json:"features"`
			}
			if json.Unmarshal(frame, &m) == nil && m.Type == "hello" {
				if !slices.Contains(m.Features, extwire.FeatureExtTerminal) {
					t.Fatalf("hello missing %s: %v", extwire.FeatureExtTerminal, m.Features)
				}
				return ws
			}
		}
	}

	sendJSON := func(ws *websocket.Conn, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		wctx, wcancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer wcancel()
		if err := ws.Write(wctx, websocket.MessageText, b); err != nil {
			t.Fatalf("ws write: %v", err)
		}
	}

	readExtTerm := func(ws *websocket.Conn) map[string]any {
		t.Helper()
		rctx, rcancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer rcancel()
		for {
			_, frame, err := ws.Read(rctx)
			if err != nil {
				t.Fatalf("ws read ext_terminal_*: %v", err)
			}
			var m map[string]any
			if json.Unmarshal(frame, &m) != nil {
				continue
			}
			typ, _ := m["type"].(string)
			if strings.HasPrefix(typ, "ext_terminal_") {
				return m
			}
		}
	}

	// 1. Non-roleFull devices MUST be rejected:
	//    a) watch-only (roleAnswer) is blocked at HTTP WS upgrade (403) and in onClientMessage (ext_terminal_error: forbidden);
	//    b) read-only (roleReadOnly) WS connection receives ext_terminal_error { code: "forbidden" }.
	watchClient := &client{hub: h, role: roleAnswer, device: "dev_watch_ext", uiConnection: "watch-conn", out: make(chan []byte, 4)}
	h.onClientMessage(t.Context(), watchClient, []byte(`{"type":"ext_terminal_open","extension":"demo-pty","target":"shell","cols":80,"rows":24,"confirmed":true}`))
	var watchResp map[string]any
	select {
	case rawMsg := <-watchClient.out:
		_ = json.Unmarshal(rawMsg, &watchResp)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch-only refusal")
	}
	if watchResp["type"] != "ext_terminal_error" || watchResp["code"] != "forbidden" {
		t.Fatalf("watch-only open got %v, want ext_terminal_error(forbidden)", watchResp)
	}
	scopedClient := &client{hub: h, role: roleScoped, device: "dev_scoped_ext", uiConnection: "scoped-conn", out: make(chan []byte, 4)}
	h.onClientMessage(t.Context(), scopedClient, []byte(`{"type":"ext_terminal_input","extension":"demo-pty","session_id":"s1","data_b64":"aGk="}`))
	var scopedResp map[string]any
	select {
	case rawMsg := <-scopedClient.out:
		_ = json.Unmarshal(rawMsg, &scopedResp)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for scoped refusal")
	}
	if scopedResp["type"] != "ext_terminal_error" || scopedResp["code"] != "forbidden" {
		t.Fatalf("scoped input got %v, want ext_terminal_error(forbidden)", scopedResp)
	}

	// 2. roleFull client: confirmed=false MUST be rejected with confirmation_required.
	fullWS := dialWS(h.cfg.Token)
	sendJSON(fullWS, map[string]any{
		"type":      "ext_terminal_open",
		"extension": "demo-pty",
		"target":    "shell",
		"cols":      80,
		"rows":      24,
		"confirmed": false,
	})
	unconf := readExtTerm(fullWS)
	if unconf["type"] != "ext_terminal_error" || unconf["code"] != "confirmation_required" {
		t.Fatalf("unconfirmed open got %v, want confirmation_required", unconf)
	}

	// 3. Confirmed open succeeds -> receives ext_terminal_opened.
	sendJSON(fullWS, map[string]any{
		"type":      "ext_terminal_open",
		"extension": "demo-pty",
		"target":    "shell",
		"cols":      100,
		"rows":      30,
		"confirmed": true,
	})
	opened := readExtTerm(fullWS)
	if opened["type"] != "ext_terminal_opened" || opened["title"] != "PTY shell" {
		t.Fatalf("confirmed open got %v, want ext_terminal_opened", opened)
	}
	sid, _ := opened["session_id"].(string)
	if sid == "" {
		t.Fatalf("missing session_id in %v", opened)
	}

	// 4. Bidirectional input -> echo frame.
	sendJSON(fullWS, map[string]any{
		"type":       "ext_terminal_input",
		"extension":  "demo-pty",
		"session_id": sid,
		"data_b64":   base64.StdEncoding.EncodeToString([]byte("ls -la\n")),
	})
	echoFrame := readExtTerm(fullWS)
	if echoFrame["type"] != "ext_terminal_frame" || echoFrame["session_id"] != sid {
		t.Fatalf("expected ext_terminal_frame, got %v", echoFrame)
	}
	rawOut, _ := base64.StdEncoding.DecodeString(echoFrame["data_b64"].(string))
	if string(rawOut) != "ECHO:ls -la\n" {
		t.Fatalf("echo output = %q, want %q", string(rawOut), "ECHO:ls -la\n")
	}

	// 5. Alternate screen ANSI (vim) passthrough intact.
	sendJSON(fullWS, map[string]any{
		"type":       "ext_terminal_input",
		"extension":  "demo-pty",
		"session_id": sid,
		"data_b64":   base64.StdEncoding.EncodeToString([]byte("vim\r")),
	})
	vimFrame := readExtTerm(fullWS)
	vimBytes, _ := base64.StdEncoding.DecodeString(vimFrame["data_b64"].(string))
	wantVim := "\x1b[?1049h\x1b[2J\x1b[1;1H~ VIM E2E ~\x1b[?1049l"
	if string(vimBytes) != wantVim {
		t.Fatalf("vim alternate screen frame = %q, want %q", string(vimBytes), wantVim)
	}

	// 6. Resize forwards cols/rows to extension.
	sendJSON(fullWS, map[string]any{
		"type":       "ext_terminal_resize",
		"extension":  "demo-pty",
		"session_id": sid,
		"cols":       132,
		"rows":       42,
	})
	eventually(t, 3*time.Second, "extension received resize", func() bool {
		fakeExt.mu.Lock()
		defer fakeExt.mu.Unlock()
		rs := fakeExt.resizes[sid]
		return len(rs) > 0 && rs[0].Cols == 132 && rs[0].Rows == 42
	})

	// 7. Oversized input (> 64 KiB) rejected with frame_too_large.
	huge := make([]byte, (64<<10)+1)
	sendJSON(fullWS, map[string]any{
		"type":       "ext_terminal_input",
		"extension":  "demo-pty",
		"session_id": sid,
		"data_b64":   base64.StdEncoding.EncodeToString(huge),
	})
	hugeResp := readExtTerm(fullWS)
	if hugeResp["type"] != "ext_terminal_error" || hugeResp["code"] != "frame_too_large" {
		t.Fatalf("oversized frame got %v, want frame_too_large", hugeResp)
	}

	// 8. Per-extension concurrency cap: 3 more sessions succeed (total 4), 5th returns too_many_sessions.
	for i := 2; i <= 4; i++ {
		sendJSON(fullWS, map[string]any{
			"type":      "ext_terminal_open",
			"extension": "demo-pty",
			"target":    "shell",
			"cols":      80,
			"rows":      24,
			"confirmed": true,
		})
		m := readExtTerm(fullWS)
		if m["type"] != "ext_terminal_opened" {
			t.Fatalf("session #%d failed: %v", i, m)
		}
	}
	sendJSON(fullWS, map[string]any{
		"type":      "ext_terminal_open",
		"extension": "demo-pty",
		"target":    "shell",
		"cols":      80,
		"rows":      24,
		"confirmed": true,
	})
	capResp := readExtTerm(fullWS)
	if capResp["type"] != "ext_terminal_error" || capResp["code"] != "too_many_sessions" {
		t.Fatalf("5th session got %v, want too_many_sessions", capResp)
	}

	// 9. Disconnecting last subscriber triggers terminal.close on the extension for all 4 sessions.
	_ = fullWS.Close(websocket.StatusNormalClosure, "bye")
	eventually(t, 4*time.Second, "all 4 sessions closed on disconnect", func() bool {
		fakeExt.mu.Lock()
		defer fakeExt.mu.Unlock()
		return len(fakeExt.closed) == 4 && fakeExt.closed[sid].Reason == "disconnected"
	})
}
