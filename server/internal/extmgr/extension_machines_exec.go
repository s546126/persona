package extmgr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/assistantwire"
	"github.com/nwverse/persona/server/internal/extension"
	"github.com/nwverse/persona/server/internal/extwire"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
)

const (
	machinesExecDefaultTimeoutMs   = 20000
	machinesExecMaxTimeoutMs       = 60000
	machinesExecDefaultOutputBytes = 16384
	machinesExecMaxOutputBytes     = 65536
	machinesExecMaxArgvItems       = 64
	machinesExecMaxArgvBytes       = 4096
	machinesExecMaxCommandRunes    = 1024
	machinesExecMaxReasonRunes     = 200
)

func validateMachinesExecParams(p *wire.MachinesExecParams) error {
	p.MachineID = wire.HubMachineID(strings.TrimSpace(string(p.MachineID)))
	if p.MachineID == "" {
		return &rpc.Error{Code: "invalid_params", Message: "machine_id is required"}
	}
	hasArgv := len(p.Argv) > 0
	hasCmd := p.Command != ""
	if hasArgv && hasCmd {
		return &rpc.Error{Code: "invalid_params", Message: "argv and command are mutually exclusive"}
	}
	if !hasArgv && strings.TrimSpace(p.Command) == "" {
		return &rpc.Error{Code: "invalid_params", Message: "either argv or command is required"}
	}
	if hasArgv {
		if len(p.Argv) > machinesExecMaxArgvItems {
			return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("argv has at most %d items", machinesExecMaxArgvItems)}
		}
		if p.Argv[0] == "" {
			return &rpc.Error{Code: "invalid_params", Message: "argv[0] must not be empty"}
		}
		totalBytes := 0
		for _, a := range p.Argv {
			if strings.IndexByte(a, 0) >= 0 {
				return &rpc.Error{Code: "invalid_params", Message: "argv must not contain NUL bytes"}
			}
			totalBytes += len(a)
		}
		if totalBytes > machinesExecMaxArgvBytes {
			return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("argv total length exceeds %d bytes", machinesExecMaxArgvBytes)}
		}
	}
	if hasCmd {
		if strings.IndexByte(p.Command, 0) >= 0 {
			return &rpc.Error{Code: "invalid_params", Message: "command must not contain NUL bytes"}
		}
		if utf8.RuneCountInString(p.Command) > machinesExecMaxCommandRunes {
			return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("command exceeds %d characters", machinesExecMaxCommandRunes)}
		}
	}
	if strings.IndexByte(p.Cwd, 0) >= 0 || len(p.Cwd) > 4096 {
		return &rpc.Error{Code: "invalid_params", Message: "invalid cwd"}
	}
	if p.TimeoutMs < 0 || p.TimeoutMs > machinesExecMaxTimeoutMs {
		return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("timeout_ms must be between 0 and %d", machinesExecMaxTimeoutMs)}
	}
	if p.MaxOutputBytes < 0 || p.MaxOutputBytes > machinesExecMaxOutputBytes {
		return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("max_output_bytes must be between 0 and %d", machinesExecMaxOutputBytes)}
	}
	if strings.IndexByte(p.Reason, 0) >= 0 || utf8.RuneCountInString(p.Reason) > machinesExecMaxReasonRunes {
		return &rpc.Error{Code: "invalid_params", Message: fmt.Sprintf("reason exceeds %d characters", machinesExecMaxReasonRunes)}
	}
	return nil
}

// extensionMachinesExecCall handles machines.exec from extension id (#16741):
//  1. Verifies the explicit named scope machines.exec (never included in legacy all-scope tokens).
//  2. Strictly validates params (mutually exclusive argv/command, bounds, NUL checks).
//  3. Requires explicit owner approval via approveCard displaying extension name, machine name/id,
//     full command or argv, cwd, and reason. Refusal or timeout returns an error and never reaches hub/node.
//  4. Forwards approved calls to Hub via extwire.HubMachinesExec and records a redacted audit entry.
func (ps *extensions) extensionMachinesExecCall(ctx context.Context, id string, params json.RawMessage) (any, error) {
	if ps.reg == nil || !ps.reg.HubScopeAllowed(id, extension.ScopeMachinesExec) {
		data, _ := json.Marshal(map[string]string{"scope": extension.ScopeMachinesExec})
		return nil, &rpc.Error{Code: extension.CodePermissionDenied, Message: "machines.exec: requires " + extension.ScopeMachinesExec, Data: data}
	}
	var p wire.MachinesExecParams
	if len(bytes.TrimSpace(params)) == 0 {
		return nil, &rpc.Error{Code: "invalid_params", Message: "params are required"}
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, &rpc.Error{Code: "invalid_params", Message: err.Error()}
	}
	if err := validateMachinesExecParams(&p); err != nil {
		return nil, err
	}
	if ps.h == nil {
		return nil, &rpc.Error{Code: assistantwire.CodeAssistantOffline, Message: errAssistantOffline.Error()}
	}

	extName := id
	if ext := ps.reg.Get(id); ext != nil && ext.Full != nil && strings.TrimSpace(ext.Full.Name) != "" {
		extName = ext.Full.Name
	}
	machineID := string(p.MachineID)
	machineName := machineID
	if m := ps.h.localMachine(machineID); m != nil && strings.TrimSpace(m.name()) != "" {
		machineName = m.name()
	}

	cmdDisplay := p.Command
	if len(p.Argv) > 0 {
		cmdDisplay = strings.Join(p.Argv, " ")
	}
	var summaryParts []string
	summaryParts = append(summaryParts, fmt.Sprintf("Extension %s (%s) wants to run on %s (%s): %s", extName, id, machineName, machineID, cmdDisplay))
	if p.Cwd != "" {
		summaryParts = append(summaryParts, "cwd: "+p.Cwd)
	}
	if p.Reason != "" {
		summaryParts = append(summaryParts, "reason: "+p.Reason)
	}
	summary := strings.Join(summaryParts, "\n")

	cardInputMap := map[string]any{
		"extension_id":   id,
		"extension_name": extName,
		"machine_id":     machineID,
		"machine_name":   machineName,
	}
	if len(p.Argv) > 0 {
		cardInputMap["argv"] = p.Argv
	} else {
		cardInputMap["command"] = p.Command
	}
	if p.Cwd != "" {
		cardInputMap["cwd"] = p.Cwd
	}
	if p.Reason != "" {
		cardInputMap["reason"] = p.Reason
	}
	if p.TimeoutMs > 0 {
		cardInputMap["timeout_ms"] = p.TimeoutMs
	}
	cardInput, _ := json.Marshal(cardInputMap)

	res, cardID := ps.h.approveCard(ctx, asker{machine: machineID}, "machines.exec", summary, cardInput)
	outcome := res.Outcome()
	if res.Allow == nil || !*res.Allow {
		ps.h.audit(ctx, id, "machines_exec_denied", "extension:"+id, map[string]any{
			"machine_id": machineID,
			"card_id":    cardID,
			"outcome":    outcome,
			"mode":       machinesExecMode(p),
			"cwd":        p.Cwd,
			"reason":     p.Reason,
		})
		if outcome == proto.OutcomeDeclined {
			return nil, &rpc.Error{Code: "user_denied", Message: "machines.exec: declined by the owner"}
		}
		return nil, &rpc.Error{Code: "approval_not_granted", Message: "machines.exec: not approved (" + outcome + ")"}
	}

	// Normalize defaults before forwarding.
	if p.TimeoutMs == 0 {
		p.TimeoutMs = machinesExecDefaultTimeoutMs
	}
	if p.MaxOutputBytes == 0 {
		p.MaxOutputBytes = machinesExecDefaultOutputBytes
	}
	reqBytes, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	hubReq := struct {
		Extension string          `json:"extension"`
		Request   json.RawMessage `json:"request,omitempty"`
	}{Extension: id, Request: reqBytes}

	var out wire.MachinesExecResult
	callErr := ps.h.callHub(ctx, extwire.HubMachinesExec, hubReq, &out)
	auditDetail := map[string]any{
		"machine_id": p.MachineID,
		"card_id":    cardID,
		"mode":       machinesExecMode(p),
		"cwd":        p.Cwd,
		"reason":     p.Reason,
	}
	if callErr != nil {
		auditDetail["error"] = callErr.Error()
		ps.h.audit(ctx, id, "machines_exec_failed", "extension:"+id, auditDetail)
		return nil, callErr
	}
	auditDetail["exit_code"] = out.ExitCode
	auditDetail["duration_ms"] = out.DurationMs
	auditDetail["truncated"] = out.Truncated
	auditDetail["timed_out"] = out.TimedOut
	auditDetail["cancelled"] = out.Cancelled
	ps.h.audit(ctx, id, "machines_exec", "extension:"+id, auditDetail)
	return out, nil
}

func machinesExecMode(p wire.MachinesExecParams) string {
	if len(p.Argv) > 0 {
		return "argv:" + p.Argv[0]
	}
	return "command"
}
