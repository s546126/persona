package ext

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/nwverse/persona/sdk/go/wire"
)

// TerminalNamespace is the terminal_sessions extension point's namespace: the manager calls
// terminal.open, terminal.input, terminal.resize and terminal.close, and the program emits
// terminal.frame and terminal.closed events (schema/extension/terminal.schema.json, #16741).
const TerminalNamespace = "terminal"

// TerminalMaxFrameBytes is the maximum decoded byte size of a single PTY frame.
const TerminalMaxFrameBytes = 64 << 10

// TerminalHandler handles interactive PTY terminal session lifecycle calls from the manager.
type TerminalHandler interface {
	Open(ctx context.Context, p wire.TerminalOpenParams) (wire.TerminalOpenResult, error)
	Input(ctx context.Context, sessionID string, data []byte) error
	Resize(ctx context.Context, p wire.TerminalResizeParams) error
	Close(ctx context.Context, p wire.TerminalCloseParams) error
}

// TerminalFuncs is a convenience struct implementing TerminalHandler via callbacks.
type TerminalFuncs struct {
	OnOpen   func(ctx context.Context, p wire.TerminalOpenParams) (wire.TerminalOpenResult, error)
	OnInput  func(ctx context.Context, sessionID string, data []byte) error
	OnResize func(ctx context.Context, p wire.TerminalResizeParams) error
	OnClose  func(ctx context.Context, p wire.TerminalCloseParams) error
}

func (f TerminalFuncs) Open(ctx context.Context, p wire.TerminalOpenParams) (wire.TerminalOpenResult, error) {
	if f.OnOpen == nil {
		return wire.TerminalOpenResult{}, UnknownMethod("open")
	}
	return f.OnOpen(ctx, p)
}

func (f TerminalFuncs) Input(ctx context.Context, sessionID string, data []byte) error {
	if f.OnInput == nil {
		return UnknownMethod("input")
	}
	return f.OnInput(ctx, sessionID, data)
}

func (f TerminalFuncs) Resize(ctx context.Context, p wire.TerminalResizeParams) error {
	if f.OnResize == nil {
		return nil
	}
	return f.OnResize(ctx, p)
}

func (f TerminalFuncs) Close(ctx context.Context, p wire.TerminalCloseParams) error {
	if f.OnClose == nil {
		return nil
	}
	return f.OnClose(ctx, p)
}

// HandleTerminal builds an ext.Handler for TerminalNamespace ("terminal") backed by h.
// Register it on ext.Options.Namespaces[ext.TerminalNamespace].
func HandleTerminal(h TerminalHandler) Handler {
	return Methods{
		"open": Method(func(ctx context.Context, p wire.TerminalOpenParams) (wire.TerminalOpenResult, error) {
			if p.SessionID == "" || p.Target == "" || p.Cols < 1 || p.Rows < 1 {
				return wire.TerminalOpenResult{}, &Error{Code: "invalid_params", Message: "session_id, target, cols and rows are required"}
			}
			return h.Open(ctx, p)
		}),
		"input": Method(func(ctx context.Context, p wire.TerminalInputParams) (Empty, error) {
			if p.SessionID == "" {
				return Empty{}, &Error{Code: "invalid_params", Message: "session_id is required"}
			}
			raw, err := base64.StdEncoding.DecodeString(p.DataB64)
			if err != nil {
				return Empty{}, &Error{Code: "invalid_params", Message: "data_b64: invalid base64"}
			}
			if len(raw) > TerminalMaxFrameBytes {
				return Empty{}, &Error{Code: "invalid_params", Message: "input frame exceeds 64 KiB"}
			}
			return Empty{}, h.Input(ctx, string(p.SessionID), raw)
		}),
		"resize": Method(func(ctx context.Context, p wire.TerminalResizeParams) (Empty, error) {
			if p.SessionID == "" || p.Cols < 1 || p.Rows < 1 {
				return Empty{}, &Error{Code: "invalid_params", Message: "session_id, cols and rows are required"}
			}
			return Empty{}, h.Resize(ctx, p)
		}),
		"close": Method(func(ctx context.Context, p wire.TerminalCloseParams) (Empty, error) {
			if p.SessionID == "" {
				return Empty{}, &Error{Code: "invalid_params", Message: "session_id is required"}
			}
			return Empty{}, h.Close(ctx, p)
		}),
	}.For()
}

// TerminalFrame pushes one ordered chunk of PTY output bytes to the hub for an open session.
// Frames larger than TerminalMaxFrameBytes (64 KiB) are split or refused; this helper refuses them explicitly.
func (h *Hub) TerminalFrame(sessionID string, seq int, data []byte) error {
	if sessionID == "" || seq < 1 {
		return errors.New("terminal.frame: session_id and positive seq are required")
	}
	if len(data) > TerminalMaxFrameBytes {
		return errors.New("terminal.frame: data exceeds 64 KiB")
	}
	return h.Notify(wire.EventTerminalFrame, wire.TerminalFrameEvent{
		SessionID: wire.TerminalSessionID(sessionID),
		Seq:       seq,
		DataB64:   base64.StdEncoding.EncodeToString(data),
	})
}

// TerminalClosed notifies the hub that an interactive PTY session has exited or closed.
func (h *Hub) TerminalClosed(sessionID string, exitCode *int, reason string) error {
	if sessionID == "" {
		return errors.New("terminal.closed: session_id is required")
	}
	return h.Notify(wire.EventTerminalClosed, wire.TerminalClosedEvent{
		SessionID: wire.TerminalSessionID(sessionID),
		ExitCode:  exitCode,
		Reason:    reason,
	})
}

// DecodeTerminalFrameData decodes a TerminalFrameEvent's base64 payload, checking the 64 KiB bound.
func DecodeTerminalFrameData(raw json.RawMessage) ([]byte, error) {
	var ev wire.TerminalFrameEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(ev.DataB64)
	if err != nil {
		return nil, err
	}
	if len(data) > TerminalMaxFrameBytes {
		return nil, errors.New("frame exceeds 64 KiB")
	}
	return data, nil
}
