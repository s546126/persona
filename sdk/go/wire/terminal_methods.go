package wire

// Extension interactive PTY terminal JSON-RPC methods and events (schema/extension/terminal.schema.json, #16741).
const (
	MethodTerminalOpen   = "terminal.open"
	MethodTerminalInput  = "terminal.input"
	MethodTerminalResize = "terminal.resize"
	MethodTerminalClose  = "terminal.close"

	EventTerminalFrame  = "terminal.frame"
	EventTerminalClosed = "terminal.closed"
)
