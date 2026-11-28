package extwire

import (
	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/buswire"
)

// Extension interactive PTY terminal session transport between the hub and manager (#16741).
// Completely separate from native computer machine_id / herdr terminal_subscribe streams so an
// extension terminal channel can never address a host computer pane.
const (
	FeatureExtTerminal             = "ext_terminal_v1"
	MethodExtTerminal              = "extensions.terminal"
	EventExtTerminal               = "extensions.terminal_frame"
	EventExtTerminalConnectionGone = "extensions.terminal_connection_gone"
)

// ExtTerminalMethod forwards one client ext_terminal_* WebSocket message from the hub to the owning manager.
var ExtTerminalMethod = buswire.Method[ExtTerminalCall, buswire.None]{Name: MethodExtTerminal}

// ExtTerminalClientAction identifies a client-to-hub extension PTY WebSocket command.
type ExtTerminalClientAction string

const (
	ExtTerminalActionOpen      ExtTerminalClientAction = "ext_terminal_open"
	ExtTerminalActionSubscribe ExtTerminalClientAction = "ext_terminal_subscribe"
	ExtTerminalActionInput     ExtTerminalClientAction = "ext_terminal_input"
	ExtTerminalActionResize    ExtTerminalClientAction = "ext_terminal_resize"
	ExtTerminalActionClose     ExtTerminalClientAction = "ext_terminal_close"
)

// Valid reports whether a is one of the known client extension terminal actions.
func (a ExtTerminalClientAction) Valid() bool {
	switch a {
	case ExtTerminalActionOpen, ExtTerminalActionSubscribe, ExtTerminalActionInput, ExtTerminalActionResize, ExtTerminalActionClose:
		return true
	}
	return false
}

// ExtTerminalClientEnvelope is the typed client ext_terminal_* WebSocket message relayed from hub to manager.
type ExtTerminalClientEnvelope struct {
	Type      ExtTerminalClientAction `json:"type"`
	Manager   string                  `json:"manager,omitempty"`
	Extension string                  `json:"extension,omitempty"`
	Target    string                  `json:"target,omitempty"`
	SessionID wire.TerminalSessionID  `json:"session_id,omitempty"`
	Cols      int                     `json:"cols,omitempty"`
	Rows      int                     `json:"rows,omitempty"`
	DataB64   string                  `json:"data_b64,omitempty"`
	AfterSeq  int                     `json:"after_seq,omitempty"`
	Confirmed bool                    `json:"confirmed,omitempty"`
	Reason    string                  `json:"reason,omitempty"`
}

// ExtTerminalCall carries an authenticated hub client identity and a typed ext_terminal_* client message.
type ExtTerminalCall struct {
	Who      UIPrincipal               `json:"who"`
	Envelope ExtTerminalClientEnvelope `json:"envelope"`
}

// ExtTerminalEventType identifies a private manager/hub-to-client extension PTY WebSocket event.
type ExtTerminalEventType string

const (
	ExtTerminalEventOpened ExtTerminalEventType = "ext_terminal_opened"
	ExtTerminalEventFrame  ExtTerminalEventType = "ext_terminal_frame"
	ExtTerminalEventClosed ExtTerminalEventType = "ext_terminal_closed"
	ExtTerminalEventError  ExtTerminalEventType = "ext_terminal_error"
)

// Valid reports whether t is one of the known server extension terminal event types.
func (t ExtTerminalEventType) Valid() bool {
	switch t {
	case ExtTerminalEventOpened, ExtTerminalEventFrame, ExtTerminalEventClosed, ExtTerminalEventError:
		return true
	}
	return false
}

// ExtTerminalErrorText carries the human-readable failure message on an ext_terminal_error frame.
type ExtTerminalErrorText string

// ExtTerminalEvent is a typed private WebSocket event delivered to a single full-device connection.
type ExtTerminalEvent struct {
	Type      ExtTerminalEventType   `json:"type"`
	SessionID wire.TerminalSessionID `json:"session_id,omitempty"`
	Extension string                 `json:"extension,omitempty"`
	Target    string                 `json:"target,omitempty"`
	Manager   string                 `json:"manager,omitempty"`
	Title     string                 `json:"title,omitempty"`
	Cols      int                    `json:"cols,omitempty"`
	Rows      int                    `json:"rows,omitempty"`
	Seq       int                    `json:"seq,omitempty"`
	DataB64   string                 `json:"data_b64,omitempty"`
	ExitCode  *int                   `json:"exit_code,omitempty"`
	Reason    string                 `json:"reason,omitempty"`
	Code      string                 `json:"code,omitempty"`
	Error     ExtTerminalErrorText   `json:"error,omitempty"`
}

// ExtTerminalTarget delivers one private ext_terminal_* event (opened/frame/closed/error) to a specific client connection.
// Never broadcast or recorded in the global event replay log.
type ExtTerminalTarget struct {
	Who   UIPrincipal      `json:"who"`
	Event ExtTerminalEvent `json:"event"`
}
