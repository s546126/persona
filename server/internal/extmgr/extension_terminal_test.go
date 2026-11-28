package extmgr

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/extwire"
)

type stubTermProc struct {
	mu     sync.Mutex
	closed []wire.TerminalCloseParams
}

func (s *stubTermProc) Call(_ context.Context, method string, params, _ any) error {
	if method == wire.MethodTerminalClose {
		if p, ok := params.(wire.TerminalCloseParams); ok {
			s.mu.Lock()
			s.closed = append(s.closed, p)
			s.mu.Unlock()
		}
	}
	return nil
}

type stubTermHost struct {
	mu      sync.Mutex
	emitted []extwire.ExtTerminalTarget
}

func (s *stubTermHost) Call(context.Context, string, any, any) error { return nil }
func (s *stubTermHost) Notify(event string, params any) error {
	if event == extwire.EventExtTerminal {
		if tgt, ok := params.(extwire.ExtTerminalTarget); ok {
			s.mu.Lock()
			s.emitted = append(s.emitted, tgt)
			s.mu.Unlock()
		}
	}
	return nil
}

func TestExtTerminalIdleTimeoutReapsSession(t *testing.T) {
	h := newTestManager(t)
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	h.extTerms.idleTimeout = 30 * time.Minute
	h.extTerms.now = func() time.Time { return now }

	proc := &stubTermProc{}
	who := extwire.UIPrincipal{Hub: "hub-1", Principal: "dev-1", Connection: "conn-1", Role: roleFull}

	stubHost := &stubTermHost{}
	h.opts.Hub = stubHost

	h.extTerms.mu.Lock()
	h.extTerms.sessions = map[string]*extTerminalSession{
		"extterm_idle1": {
			id:         "extterm_idle1",
			extension:  "demo-pty",
			target:     "shell",
			cols:       80,
			rows:       24,
			proc:       proc,
			subs:       map[string]extwire.UIPrincipal{extTermSubKey(who): who},
			lastActive: now,
		},
	}
	h.extTerms.mu.Unlock()

	// Not idle yet at +29m.
	h.extTerminalSweep(now.Add(29 * time.Minute))
	h.extTerms.mu.Lock()
	if len(h.extTerms.sessions) != 1 {
		h.extTerms.mu.Unlock()
		t.Fatal("session reaped prematurely before idle timeout")
	}
	h.extTerms.mu.Unlock()

	// Idle at +31m -> reaped, client notified ext_terminal_closed(idle_timeout), extension receives terminal.close(idle_timeout).
	h.extTerminalSweep(now.Add(31 * time.Minute))
	h.extTerms.mu.Lock()
	remaining := len(h.extTerms.sessions)
	h.extTerms.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected 0 sessions after idle sweep, got %d", remaining)
	}

	stubHost.mu.Lock()
	gotEmit := len(stubHost.emitted)
	var ev extwire.ExtTerminalEvent
	if gotEmit > 0 {
		ev = stubHost.emitted[0].Event
	}
	stubHost.mu.Unlock()
	if gotEmit != 1 || ev.Type != extwire.ExtTerminalEventClosed || ev.Reason != "idle_timeout" {
		t.Fatalf("unexpected idle close notification: count=%d ev=%+v", gotEmit, ev)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		proc.mu.Lock()
		n := len(proc.closed)
		var first wire.TerminalCloseParams
		if n > 0 {
			first = proc.closed[0]
		}
		proc.mu.Unlock()
		if n == 1 && first.SessionID == "extterm_idle1" && first.Reason == "idle_timeout" {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("extension did not receive terminal.close(idle_timeout)")
}
