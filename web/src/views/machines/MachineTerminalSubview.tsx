// Native machine terminal subview (/machines/<id>/terminal, #16741):
// Lists existing panes on the machine via GET /api/v1/machines/{mid}/panes, lets the owner create a new terminal
// via POST /api/v1/machines/{mid}/panes, and embeds TerminalPanel (@xterm/xterm over terminal_subscribe/terminal_input).
// Full-role devices only; watch-only or restricted tokens receive 403 from the hub and see an inline notice.
import { useCallback, useEffect, useMemo, useState } from "react";
import { t } from "../../i18n";
import { api, ApiError, enc, errorText } from "../../lib/api";
import { homeShellHost, terminalId, type PaneDTO, type PanesResponse } from "../../lib/terminals";
import type { Machine } from "../../lib/types";
import { Button, cx, Loading, toast } from "../../ui/atoms";
import { Icon } from "../../ui/icons";
import { TerminalPanel } from "../session/TerminalPanel";

interface ListedPane {
  id: string;
  label: string;
  detail: string;
  cwd?: string;
  pane: PaneDTO;
  hostKind: string;
}

function flattenPanes(resp: PanesResponse | null): ListedPane[] {
  if (!resp?.hosts?.length) return [];
  const out: ListedPane[] = [];
  for (const h of resp.hosts) {
    if (!h.stream) continue;
    const wsById = new Map((h.workspaces ?? []).map((w) => [w.id, w]));
    for (const p of h.panes ?? []) {
      const ws = wsById.get(p.workspace_id);
      const label = p.title || p.foreground || (p.agent ? p.agent : p.pane);
      const detail = [ws?.label, p.cwd || ws?.cwd].filter(Boolean).join(" · ");
      out.push({ id: terminalId(p), label, detail, cwd: p.cwd || ws?.cwd, pane: p, hostKind: h.kind });
    }
  }
  return out;
}

export function MachineTerminalSubview({ machine }: { machine: Machine }) {
  const [resp, setResp] = useState<PanesResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [forbidden, setForbidden] = useState(false);
  const [selectedId, setSelectedId] = useState("");
  const [creating, setCreating] = useState(false);

  const load = useCallback(async () => {
    if (!machine.online) {
      setResp(null);
      setLoading(false);
      setError("");
      setForbidden(false);
      return;
    }
    setLoading(true);
    setError("");
    setForbidden(false);
    try {
      const data = await api<PanesResponse>(`/machines/${enc(machine.id)}/panes`);
      setResp(data);
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) {
        setForbidden(true);
      } else {
        setError(errorText(e));
      }
    } finally {
      setLoading(false);
    }
  }, [machine.id, machine.online]);

  useEffect(() => {
    void load();
  }, [load]);

  const panes = useMemo(() => flattenPanes(resp), [resp]);
  const active = useMemo(() => panes.find((p) => p.id === selectedId) ?? panes[0], [panes, selectedId]);

  useEffect(() => {
    if (!active && selectedId) setSelectedId("");
    else if (active && !selectedId) setSelectedId(active.id);
  }, [active, selectedId]);

  const createTerminal = async () => {
    if (!machine.online || creating) return;
    const host = resp?.hosts.find((h) => h.new_tab);
    const kind = homeShellHost(resp ?? undefined) ?? host?.kind;
    if (!kind) {
      toast(t("work-entry.terminal_unavailable"), "error");
      return;
    }
    const ws = host?.workspaces?.[0];
    const body = ws ? { kind, workspace_id: ws.id } : { kind, cwd: "~", new_space: true };
    setCreating(true);
    try {
      const created = await api<{ terminal: string }>(`/machines/${enc(machine.id)}/panes`, { method: "POST", json: body });
      await load();
      if (created.terminal) setSelectedId(created.terminal);
    } catch (e) {
      toast(t("terminal-terminalpage.new_failed", { error: errorText(e) }), "error");
    } finally {
      setCreating(false);
    }
  };

  if (!machine.online) {
    return (
      <section className="rounded-card bg-surface px-4 py-6 text-center text-meta text-ink-3 shadow-card">
        {t("terminal-terminalpage.offline")}
      </section>
    );
  }

  if (forbidden) {
    return (
      <section role="alert" className="rounded-card bg-surface px-4 py-6 text-center text-meta text-red shadow-card">
        {t("settings-extensionview.terminal_full_device_only")}
      </section>
    );
  }

  const canCreate = Boolean(homeShellHost(resp ?? undefined));

  return (
    <section className="flex flex-col gap-3" data-machine-terminal={machine.id}>
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex min-w-0 flex-1 flex-wrap items-center gap-1.5" role="tablist" aria-label={t("machines.view_terminal")}>
          {panes.map((p) => (
            <button
              key={p.id}
              type="button"
              role="tab"
              aria-selected={active?.id === p.id}
              onClick={() => setSelectedId(p.id)}
              title={p.detail || p.id}
              className={cx(
                "flex h-8 max-w-[220px] items-center gap-1.5 rounded-control px-2.5 text-label transition-colors",
                active?.id === p.id
                  ? "bg-surface font-medium text-ink shadow-card"
                  : "bg-inset text-ink-2 hover:bg-hover hover:text-ink",
              )}
            >
              <Icon name="terminal" size={12} className="shrink-0 text-ink-3" />
              <span className="truncate">{p.label}</span>
            </button>
          ))}
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          <Button tone="quiet" icon="retry" onClick={() => void load()} disabled={loading}>
            {t("settings-extensionview.retry")}
          </Button>
          {canCreate && (
            <Button tone="primary" icon="plus" busy={creating} onClick={() => void createTerminal()}>
              {t("terminal-terminalpage.new_terminal")}
            </Button>
          )}
        </div>
      </div>

      {error && (
        <div role="alert" className="rounded-card bg-surface px-4 py-3 text-meta text-red shadow-card">
          {error}
        </div>
      )}

      {loading && !resp ? (
        <div className="rounded-card bg-surface px-4 py-8 shadow-card">
          <Loading />
        </div>
      ) : active ? (
        <div className="flex h-[560px] min-h-[360px] flex-col overflow-hidden rounded-card border border-line bg-surface shadow-card">
          <TerminalPanel
            key={`${machine.id}/${active.id}`}
            mid={machine.id}
            sid={active.id}
            label={active.label}
            context={machine.name || machine.id}
            cwd={active.cwd}
            embedded
            onClose={() => setSelectedId("")}
          />
        </div>
      ) : (
        <div className="flex flex-col items-center gap-3 rounded-card bg-surface px-4 py-8 text-center shadow-card">
          <p className="text-meta text-ink-3">{t("settings-extensionview.no_panes")}</p>
          {canCreate && (
            <Button tone="primary" icon="plus" busy={creating} onClick={() => void createTerminal()}>
              {t("terminal-terminalpage.new_terminal")}
            </Button>
          )}
        </div>
      )}
    </section>
  );
}
