package extwire

import (
	"encoding/json"

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

// ExtTerminalCall carries an authenticated hub client identity and the raw ext_terminal_* client message.
type ExtTerminalCall struct {
	Who      UIPrincipal     `json:"who"`
	Envelope json.RawMessage `json:"envelope"`
}

// ExtTerminalTarget delivers one private ext_terminal_* frame (opened/frame/closed/error) to a specific client connection.
// Never broadcast or recorded in the global event replay log.
type ExtTerminalTarget struct {
	Who      UIPrincipal     `json:"who"`
	Envelope json.RawMessage `json:"envelope"`
}
