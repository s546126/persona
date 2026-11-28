package extmgr

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/extension"
	"github.com/nwverse/persona/server/internal/extwire"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
)

func TestExtensionMachinesExecPermissionsAndApproval(t *testing.T) {
	h := newTestManager(t)
	execExt := kbExtensionSpec(t, h, "example-exec", extension.Permissions{Hub: []string{extension.ScopeMachinesExec}})
	pairExt := kbExtensionSpec(t, h, "example-pair", extension.Permissions{Hub: []string{extension.ScopeMachinesPair}})
	readExt := kbExtensionSpec(t, h, "example-read", extension.Permissions{Hub: []string{extension.ScopeMachinesRead}})
	fullExt := kbExtensionSpec(t, h, "example-full", extension.Permissions{HubAPI: true})

	var mu sync.Mutex
	var hubCalls []string
	var asked []proto.Interaction
	approveNext := true
	askErr := error(nil)

	h.opts.Ask = func(_ context.Context, ix proto.Interaction) (proto.Resolution, error) {
		mu.Lock()
		asked = append(asked, ix)
		allow := approveNext
		err := askErr
		mu.Unlock()
		if err != nil {
			return proto.Resolution{}, err
		}
		return proto.Resolution{Allow: &allow}, nil
	}

	th := hubOf(h)
	th.mu.Lock()
	th.app = func(method string, _ json.RawMessage) (any, error) {
		mu.Lock()
		hubCalls = append(hubCalls, method)
		mu.Unlock()
		if method == extwire.HubMachinesExec {
			return wire.MachinesExecResult{
				MachineID:  "mac-prod",
				ExitCode:   0,
				Output:     "ok\n",
				DurationMs: 12,
			}, nil
		}
		return map[string]any{}, nil
	}
	th.mu.Unlock()

	params := json.RawMessage(`{"machine_id":"mac-prod","argv":["/usr/bin/uptime"],"cwd":"/srv/app","reason":"check load"}`)

	// 1. Denied without machines.exec scope (including legacy full-hub token!).
	for name, fn := range map[string]func(context.Context, string, json.RawMessage) (any, error){
		"machines.pair": pairExt.Handle,
		"machines.read": readExt.Handle,
		"full-hub":      fullExt.Handle,
	} {
		_, err := fn(context.Background(), "machines.exec", params)
		var re *rpc.Error
		if !errors.As(err, &re) || re.Code != extension.CodePermissionDenied || !strings.Contains(string(re.Data), `"machines.exec"`) {
			t.Fatalf("%s machines.exec: got %v, want permission_denied(machines.exec)", name, err)
		}
	}
	if len(asked) != 0 || len(hubCalls) != 0 {
		t.Fatalf("unauthorized calls reached ask/hub: asked=%d hub=%d", len(asked), len(hubCalls))
	}

	// 2. Invalid params rejected before showing an approval card.
	for _, bad := range []string{
		`{"machine_id":"mac-prod","argv":["echo"],"command":"echo"}`,
		`{"machine_id":"mac-prod","unknown_field":1}`,
		`{"machine_id":"mac-prod"}`,
	} {
		if _, err := execExt.Handle(context.Background(), "machines.exec", json.RawMessage(bad)); err == nil {
			t.Fatalf("expected invalid params error for %s", bad)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("invalid params triggered approveCard: %d", len(asked))
	}

	// 3. Owner declines approval card -> error returned, Hub NEVER called.
	mu.Lock()
	approveNext = false
	mu.Unlock()
	_, err := execExt.Handle(context.Background(), "machines.exec", params)
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != "user_denied" {
		t.Fatalf("declined machines.exec: got %v, want user_denied", err)
	}
	if len(hubCalls) != 0 {
		t.Fatalf("declined call reached hub: %v", hubCalls)
	}
	if len(asked) != 1 {
		t.Fatalf("expected 1 approval card, got %d", len(asked))
	}
	// Verify card contains extension name, machine, command/argv, cwd, and reason.
	cardJSON, _ := json.Marshal(asked[0])
	cardText := string(cardJSON)
	for _, want := range []string{"example-exec", "mac-prod", "/usr/bin/uptime", "/srv/app", "check load"} {
		if !strings.Contains(cardText, want) {
			t.Fatalf("approval card missing %q: %s", want, cardText)
		}
	}

	// 4. Approval card timeout/unanswered -> error returned, Hub NEVER called.
	mu.Lock()
	askErr = context.DeadlineExceeded
	mu.Unlock()
	if _, err := execExt.Handle(context.Background(), "machines.exec", params); !errors.As(err, &re) || re.Code != "approval_not_granted" {
		t.Fatalf("timed out approval: got %v, want approval_not_granted", err)
	}
	if len(hubCalls) != 0 {
		t.Fatalf("timed-out call reached hub: %v", hubCalls)
	}

	// 5. Owner approves -> forwarded to Hub and returns MachinesExecResult.
	mu.Lock()
	askErr = nil
	approveNext = true
	mu.Unlock()
	out, err := execExt.Handle(context.Background(), "machines.exec", params)
	if err != nil {
		t.Fatalf("approved machines.exec: %v", err)
	}
	res, ok := out.(wire.MachinesExecResult)
	if !ok || res.MachineID != "mac-prod" || res.ExitCode != 0 || res.Output != "ok\n" {
		t.Fatalf("unexpected approved result: %#v", out)
	}
	if len(hubCalls) != 1 || hubCalls[0] != extwire.HubMachinesExec {
		t.Fatalf("hubCalls = %v, want [%s]", hubCalls, extwire.HubMachinesExec)
	}
}
