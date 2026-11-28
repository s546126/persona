package hub

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nwverse/persona/server/internal/buswire"
	"github.com/nwverse/persona/server/internal/extwire"
)

// extTerminalMaxClientFrameBytes caps a single ext_terminal_* client WebSocket frame
// (64 KiB raw payload base64-encoded is ~88 KiB plus JSON metadata).
const extTerminalMaxClientFrameBytes = 128 << 10

// forwardExtTerminal relays an interactive extension PTY message (ext_terminal_*) to the owning manager.
// This transport is completely isolated from native machine_id / herdr terminal_subscribe streams:
// only roleFull devices may open or write to an extension terminal, and every message is forwarded
// exclusively to the extension manager over extwire.ExtTerminalMethod.
func (h *Hub) forwardExtTerminal(ctx context.Context, c *client, raw []byte) {
	var env extwire.ExtTerminalClientEnvelope
	unmarshalErr := json.Unmarshal(raw, &env)
	if c.role != roleFull || c.uiConnection == "" {
		c.sendJSON(extwire.ExtTerminalEvent{
			Type:      extwire.ExtTerminalEventError,
			SessionID: env.SessionID,
			Extension: env.Extension,
			Target:    env.Target,
			Code:      "forbidden",
			Error:     "interactive extension terminal requires a full device",
		})
		return
	}
	if len(raw) > extTerminalMaxClientFrameBytes {
		c.sendJSON(extwire.ExtTerminalEvent{
			Type:      extwire.ExtTerminalEventError,
			SessionID: env.SessionID,
			Extension: env.Extension,
			Target:    env.Target,
			Code:      "frame_too_large",
			Error:     "terminal frame exceeds size limit",
		})
		return
	}
	if unmarshalErr != nil {
		c.sendJSON(extwire.ExtTerminalEvent{
			Type:  extwire.ExtTerminalEventError,
			Code:  "invalid_message",
			Error: "invalid terminal message",
		})
		return
	}
	manager := env.Manager
	if manager == "" {
		manager = h.defaultOwner(ctx)
	}
	c.mu.Lock()
	if c.extTermManagers == nil {
		c.extTermManagers = map[string]bool{}
	}
	if len(c.extTermManagers) >= 16 && !c.extTermManagers[manager] {
		c.mu.Unlock()
		return
	}
	c.extTermManagers[manager] = true
	c.mu.Unlock()

	if c.extTermRequests != nil {
		select {
		case c.extTermRequests <- struct{}{}:
		default:
			c.sendJSON(extwire.ExtTerminalEvent{
				Type:      extwire.ExtTerminalEventError,
				SessionID: env.SessionID,
				Extension: env.Extension,
				Target:    env.Target,
				Code:      "busy",
				Error:     "too many pending terminal requests",
			})
			return
		}
	}
	who := h.uiPrincipal(c)
	go func() {
		if c.extTermRequests != nil {
			defer func() { <-c.extTermRequests }()
		}
		callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if _, err := buswire.Call(callCtx, h.managerCaller(manager), extwire.ExtTerminalMethod, extwire.ExtTerminalCall{Who: who, Envelope: env}); err != nil {
			c.sendJSON(extwire.ExtTerminalEvent{
				Type:      extwire.ExtTerminalEventError,
				SessionID: env.SessionID,
				Extension: env.Extension,
				Target:    env.Target,
				Manager:   manager,
				Code:      "manager_unavailable",
				Error:     "extension manager unavailable",
			})
		}
	}()
}

func (h *Hub) extTerminalConnectionGone(c *client) {
	if c.uiConnection == "" {
		return
	}
	who := h.uiPrincipal(c)
	c.mu.Lock()
	var managers []string
	for manager := range c.extTermManagers {
		managers = append(managers, manager)
	}
	c.mu.Unlock()
	for _, manager := range managers {
		_ = h.notifyBus(manager, extwire.EventExtTerminalConnectionGone, who)
	}
}

func init() {
	handleBusEvent(extwire.EventExtTerminal, func(h *Hub, bus *busConn, params json.RawMessage) {
		if len(params) > extTerminalMaxClientFrameBytes+(16<<10) {
			return
		}
		var target extwire.ExtTerminalTarget
		if json.Unmarshal(params, &target) != nil || target.Who.Hub != h.hubID() || !target.Event.Type.Valid() {
			return
		}
		// Private per-connection delivery: never broadcast or add to global event replay log.
		h.mu.Lock()
		defer h.mu.Unlock()
		for c := range h.clients {
			who := h.uiPrincipal(c)
			if who.Connection != target.Who.Connection || who.Principal != target.Who.Principal || c.role != roleFull {
				continue
			}
			c.mu.Lock()
			allowed := c.extTermManagers[bus.managerID()]
			c.mu.Unlock()
			if !allowed {
				return
			}
			ev := target.Event
			ev.Manager = bus.managerID()
			raw, err := json.Marshal(ev)
			if err == nil && len(raw) <= extTerminalMaxClientFrameBytes {
				c.queue(raw)
			}
			return
		}
	})
}
