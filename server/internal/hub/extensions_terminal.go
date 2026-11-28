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
	var header struct {
		Type      string `json:"type"`
		Manager   string `json:"manager"`
		Extension string `json:"extension"`
		Target    string `json:"target"`
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(raw, &header)
	if c.role != roleFull || c.uiConnection == "" {
		c.sendJSON(map[string]any{
			"type":       "ext_terminal_error",
			"session_id": header.SessionID,
			"extension":  header.Extension,
			"target":     header.Target,
			"code":       "forbidden",
			"error":      "interactive extension terminal requires a full device",
		})
		return
	}
	if len(raw) > extTerminalMaxClientFrameBytes {
		c.sendJSON(map[string]any{
			"type":       "ext_terminal_error",
			"session_id": header.SessionID,
			"extension":  header.Extension,
			"target":     header.Target,
			"code":       "frame_too_large",
			"error":      "terminal frame exceeds size limit",
		})
		return
	}
	manager := header.Manager
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
			c.sendJSON(map[string]any{
				"type":       "ext_terminal_error",
				"session_id": header.SessionID,
				"extension":  header.Extension,
				"target":     header.Target,
				"code":       "busy",
				"error":      "too many pending terminal requests",
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
		if _, err := buswire.Call(callCtx, h.managerCaller(manager), extwire.ExtTerminalMethod, extwire.ExtTerminalCall{Who: who, Envelope: raw}); err != nil {
			c.sendJSON(map[string]any{
				"type":       "ext_terminal_error",
				"session_id": header.SessionID,
				"extension":  header.Extension,
				"target":     header.Target,
				"manager":    manager,
				"code":       "manager_unavailable",
				"error":      "extension manager unavailable",
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
		if json.Unmarshal(params, &target) != nil || target.Who.Hub != h.hubID() || len(target.Envelope) > extTerminalMaxClientFrameBytes {
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
			var envelope map[string]json.RawMessage
			if json.Unmarshal(target.Envelope, &envelope) != nil {
				return
			}
			delete(envelope, "event_seq")
			envelope["manager"], _ = json.Marshal(bus.managerID())
			raw, err := json.Marshal(envelope)
			if err == nil {
				c.queue(raw)
			}
			return
		}
	})
}
