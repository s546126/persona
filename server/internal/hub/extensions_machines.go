package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
	"github.com/nwverse/persona/server/internal/node"
	"github.com/nwverse/persona/server/internal/nodeproto"
	"github.com/nwverse/persona/server/internal/proto"
	"github.com/nwverse/persona/server/internal/rpc"
)

// hubMachinesPairLink handles extwire.HubMachinesPairLink from an extension granted machines.pair (#12798).
// It reuses the device pairing-code flow (fromDevice = true): newly added computers are always pending until the
// owner confirms "This is my computer", and already-confirmed computers are never given a new code.
func (h *Hub) hubMachinesPairLink(ctx context.Context, from, extID string, raw json.RawMessage) (wire.MachinesPairLinkResult, error) {
	var p wire.MachinesPairLinkParams
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return wire.MachinesPairLinkResult{}, &rpc.Error{Code: "invalid_params", Message: err.Error()}
		}
	}
	if h.isMember() {
		return wire.MachinesPairLinkResult{}, &rpc.Error{Code: "mesh_member", Message: "pairing computers through a member hub is not supported"}
	}
	if h.store == nil {
		return wire.MachinesPairLinkResult{}, &rpc.Error{Code: "unavailable", Message: "no database"}
	}
	if !h.linkPairable() {
		return wire.MachinesPairLinkResult{}, &rpc.Error{Code: "tls_required", Message: "hub TLS or https public_url is required for Persona Link pairing"}
	}
	machineID := strings.TrimSpace(p.MachineID)
	name := strings.TrimSpace(p.Name)
	caller := from
	if caller == "" {
		caller = devicesOwner
	}
	if err := h.addTarget(ctx, from); err != nil {
		return wire.MachinesPairLinkResult{}, mapAddMachineRPCError(err)
	}
	actor := "extension:" + extID
	h.machineAdd.Lock()
	var add *machineAddRequest
	var existingMachine string
	if machineID != "" && h.knownEntry(machineID) != nil && name == "" && p.OS == "" && p.Icon == "" {
		existingMachine = machineID
	} else {
		addName := name
		if addName == "" && machineID != "" && adminMachineID.MatchString(machineID) {
			addName = machineID
		}
		add = &machineAddRequest{
			ID:      machineID,
			Name:    addName,
			OS:      p.OS,
			Icon:    p.Icon,
			Manager: from,
		}
	}
	id, _, err := h.computerForScoped(ctx, existingMachine, add, true, actor, caller)
	h.machineAdd.Unlock()
	if err != nil {
		return wire.MachinesPairLinkResult{}, mapAddMachineRPCError(err)
	}
	code, err := h.newPairCode(ctx, id)
	if err != nil {
		return wire.MachinesPairLinkResult{}, err
	}
	h.notifyComputerCode(principal{ID: actor, Name: extID, Role: "extension", Extension: extID}, id)
	displayName := id
	if mc := h.knownEntry(id); mc != nil && mc.Name != "" {
		displayName = mc.Name
	}
	installLinux := ""
	if inst := h.computerInstall(code.PairURL); inst != nil {
		installLinux = inst["linux"]
	}
	return wire.MachinesPairLinkResult{
		MachineID:    id,
		Name:         displayName,
		Link:         code.PairURL,
		ExpiresAt:    code.ExpiresAt.UTC(),
		Pending:      h.machinePending(id),
		InstallLinux: installLinux,
	}, nil
}

// hubMachinesStatus handles extwire.HubMachinesStatus from an extension granted machines.read or machines.pair (#12798).
func (h *Hub) hubMachinesStatus(from string, raw json.RawMessage) (wire.MachinesStatusResult, error) {
	var p wire.MachinesStatusParams
	if len(bytes.TrimSpace(raw)) == 0 {
		return wire.MachinesStatusResult{}, &rpc.Error{Code: "invalid_params", Message: "machine_id is required"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return wire.MachinesStatusResult{}, &rpc.Error{Code: "invalid_params", Message: err.Error()}
	}
	id := strings.TrimSpace(p.MachineID)
	if id == "" {
		return wire.MachinesStatusResult{}, &rpc.Error{Code: "invalid_params", Message: "machine_id is required"}
	}
	caller := from
	if caller == "" {
		caller = devicesOwner
	}
	if h.knownEntry(id) != nil && h.entryOwner(id) != caller {
		return wire.MachinesStatusResult{}, &rpc.Error{Code: codeOtherManager, Message: errOtherManager.Error()}
	}
	res := wire.MachinesStatusResult{
		MachineID: id,
		Pending:   h.machinePending(id),
	}
	if mc := h.knownEntry(id); mc != nil {
		res.Exists = true
		res.Name = mc.Name
		if res.Name == "" {
			res.Name = mc.ID
		}
	}
	for _, m := range h.machineList() {
		if m.ID != id {
			continue
		}
		if h.entryOwner(m.ID) != caller {
			continue
		}
		res.Exists = true
		if m.Name != "" {
			res.Name = m.Name
		}
		res.Online = m.Online || m.Link != nil
		res.NodeOnline = m.Online
		res.Error = m.Error
		if !m.LastSeen.IsZero() {
			res.LastSeen = m.LastSeen.UTC()
		}
		break
	}
	return res, nil
}

// hubMachinesExec handles extwire.HubMachinesExec from an extension after the manager verified
// ScopeMachinesExec and received explicit owner approval via approveCard (#16741).
func (h *Hub) hubMachinesExec(ctx context.Context, from, extID string, raw json.RawMessage) (wire.MachinesExecResult, error) {
	if extID == "" {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "invalid_params", Message: "extension id is required"}
	}
	var p wire.MachinesExecParams
	if len(bytes.TrimSpace(raw)) == 0 {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "invalid_params", Message: "params are required"}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "invalid_params", Message: err.Error()}
	}
	if err := node.ValidateMachinesExecParams(&p); err != nil {
		return wire.MachinesExecResult{}, err
	}
	caller := from
	if caller == "" {
		caller = devicesOwner
	}
	machineID := string(p.MachineID)
	if h.knownEntry(machineID) != nil && h.entryOwner(machineID) != caller {
		return wire.MachinesExecResult{}, &rpc.Error{Code: codeOtherManager, Message: errOtherManager.Error()}
	}
	if h.machinePending(machineID) {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "machine_pending", Message: "computer is pending owner confirmation"}
	}
	m := h.localMachine(machineID)
	if m == nil {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "not_found", Message: "unknown machine"}
	}
	if h.entryOwner(m.cfg.ID) != caller {
		return wire.MachinesExecResult{}, &rpc.Error{Code: codeOtherManager, Message: errOtherManager.Error()}
	}
	if h.machinePending(m.cfg.ID) {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "machine_pending", Message: "computer is pending owner confirmation"}
	}
	if !m.isOnline() {
		return wire.MachinesExecResult{}, &rpc.Error{Code: "machine_offline", Message: "computer is offline"}
	}
	if !m.hasCapability(nodeproto.MachinesExec) {
		return wire.MachinesExecResult{}, &rpc.Error{Code: proto.CodeNodeOutdated, Message: "node does not support machines.exec; upgrade the node"}
	}
	p.MachineID = wire.HubMachineID(m.cfg.ID)
	timeoutMs := p.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 20000
	}
	// Give the node slight headroom past the command timeout so it can return timed_out=true cleanly.
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond+5*time.Second)
	defer cancel()
	var res wire.MachinesExecResult
	if err := m.call(callCtx, "machines.exec", p, &res); err != nil {
		var re *rpc.Error
		if errors.As(err, &re) && strings.HasPrefix(re.Message, "unknown method") {
			return wire.MachinesExecResult{}, &rpc.Error{Code: proto.CodeNodeOutdated, Message: "node does not support machines.exec; upgrade the node"}
		}
		return wire.MachinesExecResult{}, err
	}
	if res.MachineID == "" {
		res.MachineID = wire.HubMachineID(m.cfg.ID)
	}
	return res, nil
}

func mapAddMachineRPCError(err error) error {
	var ae *addMachineError
	if errors.As(err, &ae) && ae.code != "" {
		return &rpc.Error{Code: ae.code, Message: ae.err.Error()}
	}
	return err
}
