package extmgr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/buswire"
	"github.com/nwverse/persona/server/internal/extension"
	"github.com/nwverse/persona/server/internal/extwire"
	"github.com/nwverse/persona/server/internal/rpc"
)

const (
	extTerminalMaxPerExt          = 4
	extTerminalMaxFrameBytes      = 64 << 10 // 64 KiB decoded payload per frame
	extTerminalDefaultIdleTimeout = 30 * time.Minute
	extTerminalMaxBufferFrames    = 256
	extTerminalMaxBufferBytes     = 512 << 10
	extTerminalCallTimeout        = 15 * time.Second
)

type extTerminalManager struct {
	mu          sync.Mutex
	sessions    map[string]*extTerminalSession // session_id -> session
	idleTimeout time.Duration                  // 0 uses extTerminalDefaultIdleTimeout
	sweepEvery  time.Duration                  // 0 uses time.Minute
	now         func() time.Time
}

type extTerminalBufferedFrame struct {
	seq     int
	dataB64 string
	bytes   int
}

type extTerminalSession struct {
	id         string
	extension  string
	target     string
	title      string
	cols       int
	rows       int
	proc       extTerminalProc
	subs       map[string]extwire.UIPrincipal // connection key -> principal
	lastActive time.Time
	frames     []extTerminalBufferedFrame
	bufBytes   int
	closed     bool
}

type extTerminalProc interface {
	Call(ctx context.Context, method string, params, out any) error
}

type extTerminalClientMsg struct {
	Type      string `json:"type"`
	Manager   string `json:"manager,omitempty"`
	Extension string `json:"extension,omitempty"`
	Target    string `json:"target,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
	DataB64   string `json:"data_b64,omitempty"`
	AfterSeq  int    `json:"after_seq,omitempty"`
	Confirmed bool   `json:"confirmed,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (m *extTerminalManager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *extTerminalManager) idleLimit() time.Duration {
	if m.idleTimeout > 0 {
		return m.idleTimeout
	}
	return extTerminalDefaultIdleTimeout
}

func extTermSubKey(who extwire.UIPrincipal) string {
	return who.Hub + "\x00" + who.Principal + "\x00" + who.Connection
}

func validExtTerminalTarget(s string) bool {
	if s == "" || len(s) > 128 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c == 0x7f || c == '/' || c == '\\' || c == '?' || c == '#' {
			return false
		}
	}
	return true
}

func validTerminalSize(cols, rows int) bool {
	return cols >= 1 && cols <= 1000 && rows >= 1 && rows <= 1000
}

func (h *Manager) extTerminalEmit(who extwire.UIPrincipal, payload any) {
	if h.opts.Hub == nil || who.Connection == "" {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = h.opts.Hub.Notify(extwire.EventExtTerminal, extwire.ExtTerminalTarget{Who: who, Envelope: raw})
}

func (h *Manager) extTerminalError(who extwire.UIPrincipal, sessionID, extID, target, code, msg string) {
	out := map[string]any{
		"type":  "ext_terminal_error",
		"code":  code,
		"error": msg,
	}
	if sessionID != "" {
		out["session_id"] = sessionID
	}
	if extID != "" {
		out["extension"] = extID
	}
	if target != "" {
		out["target"] = target
	}
	h.extTerminalEmit(who, out)
}

func (h *Manager) requestExtTerminal(ctx context.Context, call extwire.ExtTerminalCall) (buswire.None, error) {
	if !h.ready(ctx) {
		return buswire.None{}, errAssistantOffline
	}
	var msg extTerminalClientMsg
	if err := json.Unmarshal(call.Envelope, &msg); err != nil {
		h.extTerminalError(call.Who, "", "", "", "invalid_message", "invalid terminal message")
		return buswire.None{}, nil
	}
	if call.Who.Role != roleFull || call.Who.Connection == "" {
		h.extTerminalError(call.Who, msg.SessionID, msg.Extension, msg.Target, "forbidden", "interactive extension terminal requires a full device")
		return buswire.None{}, nil
	}
	switch msg.Type {
	case "ext_terminal_open":
		h.extTerminalOpen(ctx, call.Who, msg)
	case "ext_terminal_subscribe":
		h.extTerminalSubscribe(call.Who, msg)
	case "ext_terminal_input":
		h.extTerminalInput(ctx, call.Who, msg)
	case "ext_terminal_resize":
		h.extTerminalResize(ctx, call.Who, msg)
	case "ext_terminal_close":
		h.extTerminalClose(call.Who, msg)
	default:
		h.extTerminalError(call.Who, msg.SessionID, msg.Extension, msg.Target, "unknown_type", "unknown extension terminal command")
	}
	return buswire.None{}, nil
}

func (h *Manager) resolveTerminalExtension(extID string) (string, *extension.Extension, extTerminalProc, string, string) {
	if h.extensions == nil || h.extensions.reg == nil {
		return "", nil, nil, "unavailable", "extensions are not ready"
	}
	if !extension.ValidID(extID) {
		return "", nil, nil, "invalid_extension", "invalid extension id"
	}
	canon := h.extensions.reg.CanonicalID(extID)
	p := h.extensions.reg.GetExact(canon)
	if p == nil || p.Manifest == nil {
		return "", nil, nil, "not_found", "unknown extension"
	}
	if !p.Record.Enabled {
		return "", nil, nil, "extension_disabled", "extension is disabled"
	}
	if len(p.Manifest.TerminalSessions()) == 0 {
		return "", nil, nil, "unsupported", "extension does not declare terminal_sessions"
	}
	hp := h.extensions.get(canon)
	if hp == nil || hp.proc == nil {
		return "", nil, nil, "extension_offline", "extension is not running"
	}
	return canon, p, hp.proc, "", ""
}

func (h *Manager) extTerminalOpen(ctx context.Context, who extwire.UIPrincipal, msg extTerminalClientMsg) {
	if !msg.Confirmed {
		h.extTerminalError(who, "", msg.Extension, msg.Target, "confirmation_required", "explicit owner confirmation is required to open an extension terminal")
		return
	}
	if !validExtTerminalTarget(msg.Target) {
		h.extTerminalError(who, "", msg.Extension, msg.Target, "invalid_target", "invalid terminal target")
		return
	}
	cols, rows := msg.Cols, msg.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	if !validTerminalSize(cols, rows) {
		h.extTerminalError(who, "", msg.Extension, msg.Target, "invalid_size", "cols and rows must be between 1 and 1000")
		return
	}
	extID, _, proc, errCode, errMsg := h.resolveTerminalExtension(msg.Extension)
	if errCode != "" {
		h.extTerminalError(who, "", msg.Extension, msg.Target, errCode, errMsg)
		return
	}

	tm := &h.extTerms
	tm.mu.Lock()
	if tm.sessions == nil {
		tm.sessions = map[string]*extTerminalSession{}
	}
	activeForExt := 0
	for _, s := range tm.sessions {
		if s.extension == extID && !s.closed {
			activeForExt++
		}
	}
	if activeForExt >= extTerminalMaxPerExt {
		tm.mu.Unlock()
		h.extTerminalError(who, "", extID, msg.Target, "too_many_sessions", "extension reached maximum concurrent terminal sessions")
		return
	}
	sessionID := "extterm_" + randomHex(12)
	sess := &extTerminalSession{
		id:         sessionID,
		extension:  extID,
		target:     msg.Target,
		cols:       cols,
		rows:       rows,
		proc:       proc,
		subs:       map[string]extwire.UIPrincipal{extTermSubKey(who): who},
		lastActive: tm.clock(),
	}
	// Reserve the slot before calling the extension so concurrent opens cannot bypass the cap
	// and early terminal.frame events sent during terminal.open are never dropped.
	tm.sessions[sessionID] = sess
	tm.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, extTerminalCallTimeout)
	defer cancel()
	var res wire.TerminalOpenResult
	err := proc.Call(callCtx, "terminal.open", wire.TerminalOpenParams{
		SessionID: sessionID,
		Target:    msg.Target,
		Cols:      cols,
		Rows:      rows,
	}, &res)
	if err != nil {
		tm.mu.Lock()
		delete(tm.sessions, sessionID)
		sess.closed = true
		tm.mu.Unlock()
		code, message := extTerminalRPCError(err)
		h.extTerminalError(who, sessionID, extID, msg.Target, code, message)
		return
	}

	tm.mu.Lock()
	if res.Title != "" {
		sess.title = res.Title
	}
	sess.lastActive = tm.clock()
	title, finalCols, finalRows := sess.title, sess.cols, sess.rows
	tm.mu.Unlock()

	h.extTerminalEmit(who, map[string]any{
		"type":       "ext_terminal_opened",
		"session_id": sessionID,
		"extension":  extID,
		"target":     msg.Target,
		"title":      title,
		"cols":       finalCols,
		"rows":       finalRows,
	})
}

func (h *Manager) extTerminalSubscribe(who extwire.UIPrincipal, msg extTerminalClientMsg) {
	if msg.SessionID == "" {
		h.extTerminalError(who, "", msg.Extension, msg.Target, "invalid_session", "session_id is required")
		return
	}
	tm := &h.extTerms
	tm.mu.Lock()
	sess := tm.sessions[msg.SessionID]
	if sess == nil || sess.closed {
		tm.mu.Unlock()
		h.extTerminalError(who, msg.SessionID, msg.Extension, msg.Target, "not_found", "terminal session not found")
		return
	}
	if msg.Extension != "" && h.extensions != nil && h.extensions.reg != nil {
		if h.extensions.reg.CanonicalID(msg.Extension) != sess.extension {
			tm.mu.Unlock()
			h.extTerminalError(who, msg.SessionID, msg.Extension, msg.Target, "not_found", "terminal session not found")
			return
		}
	}
	sess.subs[extTermSubKey(who)] = who
	sess.lastActive = tm.clock()
	title, cols, rows, extID, target := sess.title, sess.cols, sess.rows, sess.extension, sess.target
	var replay []extTerminalBufferedFrame
	for _, f := range sess.frames {
		if f.seq > msg.AfterSeq {
			replay = append(replay, f)
		}
	}
	tm.mu.Unlock()

	h.extTerminalEmit(who, map[string]any{
		"type":       "ext_terminal_opened",
		"session_id": msg.SessionID,
		"extension":  extID,
		"target":     target,
		"title":      title,
		"cols":       cols,
		"rows":       rows,
	})
	for _, f := range replay {
		h.extTerminalEmit(who, map[string]any{
			"type":       "ext_terminal_frame",
			"session_id": msg.SessionID,
			"extension":  extID,
			"seq":        f.seq,
			"data_b64":   f.dataB64,
		})
	}
}

func (h *Manager) extTerminalInput(ctx context.Context, who extwire.UIPrincipal, msg extTerminalClientMsg) {
	if msg.SessionID == "" || msg.DataB64 == "" {
		h.extTerminalError(who, msg.SessionID, msg.Extension, msg.Target, "invalid_params", "session_id and data_b64 are required")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(msg.DataB64)
	if err != nil {
		h.extTerminalError(who, msg.SessionID, msg.Extension, msg.Target, "invalid_params", "invalid base64 input")
		return
	}
	if len(raw) > extTerminalMaxFrameBytes {
		h.extTerminalError(who, msg.SessionID, msg.Extension, msg.Target, "frame_too_large", "terminal input exceeds 64 KiB frame limit")
		return
	}
	tm := &h.extTerms
	tm.mu.Lock()
	sess := tm.sessions[msg.SessionID]
	if sess == nil || sess.closed {
		tm.mu.Unlock()
		h.extTerminalError(who, msg.SessionID, msg.Extension, msg.Target, "not_found", "terminal session not found")
		return
	}
	if _, subscribed := sess.subs[extTermSubKey(who)]; !subscribed {
		tm.mu.Unlock()
		h.extTerminalError(who, msg.SessionID, sess.extension, sess.target, "forbidden", "connection is not subscribed to this terminal session")
		return
	}
	sess.lastActive = tm.clock()
	proc, extID, target := sess.proc, sess.extension, sess.target
	tm.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, extTerminalCallTimeout)
	defer cancel()
	if err := proc.Call(callCtx, "terminal.input", wire.TerminalInputParams{
		SessionID: msg.SessionID,
		DataB64:   msg.DataB64,
	}, nil); err != nil {
		code, message := extTerminalRPCError(err)
		h.extTerminalError(who, msg.SessionID, extID, target, code, message)
	}
}

func (h *Manager) extTerminalResize(ctx context.Context, who extwire.UIPrincipal, msg extTerminalClientMsg) {
	if msg.SessionID == "" || !validTerminalSize(msg.Cols, msg.Rows) {
		h.extTerminalError(who, msg.SessionID, msg.Extension, msg.Target, "invalid_params", "valid session_id, cols, and rows are required")
		return
	}
	tm := &h.extTerms
	tm.mu.Lock()
	sess := tm.sessions[msg.SessionID]
	if sess == nil || sess.closed {
		tm.mu.Unlock()
		h.extTerminalError(who, msg.SessionID, msg.Extension, msg.Target, "not_found", "terminal session not found")
		return
	}
	if _, subscribed := sess.subs[extTermSubKey(who)]; !subscribed {
		tm.mu.Unlock()
		h.extTerminalError(who, msg.SessionID, sess.extension, sess.target, "forbidden", "connection is not subscribed to this terminal session")
		return
	}
	sess.cols, sess.rows = msg.Cols, msg.Rows
	sess.lastActive = tm.clock()
	proc, extID, target := sess.proc, sess.extension, sess.target
	tm.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, extTerminalCallTimeout)
	defer cancel()
	if err := proc.Call(callCtx, "terminal.resize", wire.TerminalResizeParams{
		SessionID: msg.SessionID,
		Cols:      msg.Cols,
		Rows:      msg.Rows,
	}, nil); err != nil {
		code, message := extTerminalRPCError(err)
		h.extTerminalError(who, msg.SessionID, extID, target, code, message)
	}
}

func (h *Manager) extTerminalClose(who extwire.UIPrincipal, msg extTerminalClientMsg) {
	if msg.SessionID == "" {
		return
	}
	reason := strings.TrimSpace(msg.Reason)
	if reason == "" {
		reason = "client_closed"
	}
	tm := &h.extTerms
	tm.mu.Lock()
	sess := tm.sessions[msg.SessionID]
	if sess == nil || sess.closed {
		tm.mu.Unlock()
		return
	}
	if _, subscribed := sess.subs[extTermSubKey(who)]; !subscribed {
		tm.mu.Unlock()
		h.extTerminalError(who, msg.SessionID, sess.extension, sess.target, "forbidden", "connection is not subscribed to this terminal session")
		return
	}
	delete(tm.sessions, msg.SessionID)
	sess.closed = true
	subs := make([]extwire.UIPrincipal, 0, len(sess.subs))
	for _, sub := range sess.subs {
		subs = append(subs, sub)
	}
	proc, extID := sess.proc, sess.extension
	tm.mu.Unlock()

	go func() {
		callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = proc.Call(callCtx, "terminal.close", wire.TerminalCloseParams{
			SessionID: msg.SessionID,
			Reason:    reason,
		}, nil)
	}()
	for _, sub := range subs {
		h.extTerminalEmit(sub, map[string]any{
			"type":       "ext_terminal_closed",
			"session_id": msg.SessionID,
			"extension":  extID,
			"reason":     reason,
		})
	}
}

// extTerminalFrame handles the extension -> hub "terminal.frame" event.
func (h *Manager) extTerminalFrame(extID string, params json.RawMessage) {
	var ev wire.TerminalFrameEvent
	if err := json.Unmarshal(params, &ev); err != nil || ev.SessionID == "" || ev.Seq < 1 || ev.DataB64 == "" {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(ev.DataB64)
	if err != nil || len(raw) > extTerminalMaxFrameBytes {
		// Enforce 64 KiB single-frame cap; drop oversized or malformed frames.
		h.logf("extension %s: terminal.frame dropped for session %s (invalid or > %d bytes)", extID, ev.SessionID, extTerminalMaxFrameBytes)
		return
	}

	tm := &h.extTerms
	tm.mu.Lock()
	sess := tm.sessions[ev.SessionID]
	if sess == nil || sess.closed || sess.extension != extID {
		tm.mu.Unlock()
		return
	}
	sess.lastActive = tm.clock()
	sess.frames = append(sess.frames, extTerminalBufferedFrame{seq: ev.Seq, dataB64: ev.DataB64, bytes: len(raw)})
	sess.bufBytes += len(raw)
	for len(sess.frames) > extTerminalMaxBufferFrames || sess.bufBytes > extTerminalMaxBufferBytes {
		sess.bufBytes -= sess.frames[0].bytes
		sess.frames = sess.frames[1:]
	}
	subs := make([]extwire.UIPrincipal, 0, len(sess.subs))
	for _, sub := range sess.subs {
		subs = append(subs, sub)
	}
	tm.mu.Unlock()

	frame := map[string]any{
		"type":       "ext_terminal_frame",
		"session_id": ev.SessionID,
		"extension":  extID,
		"seq":        ev.Seq,
		"data_b64":   ev.DataB64,
	}
	for _, sub := range subs {
		h.extTerminalEmit(sub, frame)
	}
}

// extTerminalClosed handles the extension -> hub "terminal.closed" event.
func (h *Manager) extTerminalClosed(extID string, params json.RawMessage) {
	var ev wire.TerminalClosedEvent
	if err := json.Unmarshal(params, &ev); err != nil || ev.SessionID == "" {
		return
	}
	tm := &h.extTerms
	tm.mu.Lock()
	sess := tm.sessions[ev.SessionID]
	if sess == nil || sess.closed || sess.extension != extID {
		tm.mu.Unlock()
		return
	}
	delete(tm.sessions, ev.SessionID)
	sess.closed = true
	subs := make([]extwire.UIPrincipal, 0, len(sess.subs))
	for _, sub := range sess.subs {
		subs = append(subs, sub)
	}
	tm.mu.Unlock()

	payload := map[string]any{
		"type":       "ext_terminal_closed",
		"session_id": ev.SessionID,
		"extension":  extID,
		"reason":     ev.Reason,
	}
	if ev.ExitCode != nil {
		payload["exit_code"] = *ev.ExitCode
	}
	for _, sub := range subs {
		h.extTerminalEmit(sub, payload)
	}
}

// extTerminalDisconnect detaches a disconnected WS client; any session left without
// subscribers is closed immediately and the extension is notified with terminal.close.
func (h *Manager) extTerminalDisconnect(who extwire.UIPrincipal) {
	key := extTermSubKey(who)
	tm := &h.extTerms
	tm.mu.Lock()
	var orphaned []*extTerminalSession
	for id, sess := range tm.sessions {
		if _, ok := sess.subs[key]; !ok {
			continue
		}
		delete(sess.subs, key)
		if len(sess.subs) == 0 && !sess.closed {
			delete(tm.sessions, id)
			sess.closed = true
			orphaned = append(orphaned, sess)
		}
	}
	tm.mu.Unlock()

	for _, sess := range orphaned {
		callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sess.proc.Call(callCtx, "terminal.close", wire.TerminalCloseParams{
			SessionID: sess.id,
			Reason:    "disconnected",
		}, nil)
		cancel()
	}
}

// extTerminalDropExtension closes all terminal sessions belonging to extID (or all if extID == "").
func (h *Manager) extTerminalDropExtension(extID string) {
	tm := &h.extTerms
	tm.mu.Lock()
	var gone []*extTerminalSession
	for id, sess := range tm.sessions {
		if extID == "" || sess.extension == extID {
			delete(tm.sessions, id)
			sess.closed = true
			gone = append(gone, sess)
		}
	}
	tm.mu.Unlock()

	for _, sess := range gone {
		for _, sub := range sess.subs {
			h.extTerminalEmit(sub, map[string]any{
				"type":       "ext_terminal_closed",
				"session_id": sess.id,
				"extension":  sess.extension,
				"reason":     "extension_stopped",
			})
		}
		go func(s *extTerminalSession) {
			callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.proc.Call(callCtx, "terminal.close", wire.TerminalCloseParams{
				SessionID: s.id,
				Reason:    "extension_stopped",
			}, nil)
		}(sess)
	}
}

// extTerminalSweep reaps sessions idle longer than idleLimit.
func (h *Manager) extTerminalSweep(now time.Time) {
	tm := &h.extTerms
	limit := tm.idleLimit()
	tm.mu.Lock()
	var expired []*extTerminalSession
	for id, sess := range tm.sessions {
		if !sess.closed && now.Sub(sess.lastActive) >= limit {
			delete(tm.sessions, id)
			sess.closed = true
			expired = append(expired, sess)
		}
	}
	tm.mu.Unlock()

	for _, sess := range expired {
		for _, sub := range sess.subs {
			h.extTerminalEmit(sub, map[string]any{
				"type":       "ext_terminal_closed",
				"session_id": sess.id,
				"extension":  sess.extension,
				"reason":     "idle_timeout",
			})
		}
		go func(s *extTerminalSession) {
			callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.proc.Call(callCtx, "terminal.close", wire.TerminalCloseParams{
				SessionID: s.id,
				Reason:    "idle_timeout",
			}, nil)
		}(sess)
	}
}

func (h *Manager) extTerminalLoop(ctx context.Context) {
	interval := h.extTerms.sweepEvery
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.extTerminalDropExtension("")
			return
		case now := <-ticker.C:
			h.extTerminalSweep(now)
		}
	}
}

func extTerminalRPCError(err error) (string, string) {
	var re *rpc.Error
	if errors.As(err, &re) {
		code := re.Code
		if code == "" {
			code = "extension_error"
		}
		return code, re.Message
	}
	return "extension_error", err.Error()
}
