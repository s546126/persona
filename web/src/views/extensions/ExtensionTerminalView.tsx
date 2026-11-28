// Interactive full-screen PTY WebShell for an extension declaring contributes.terminal_sessions
// (/extension/<ext_id>/terminal/<target_id>, #16741).
//
// Security isolation:
//   - Speaks exclusively ext_terminal_* WebSocket envelopes (never machine_id / terminal_subscribe / herdr).
//   - Requires explicit owner confirmation before sending ext_terminal_open { confirmed: true }.
//   - Reuses @xterm/xterm + FitAddon + ClipboardAddon + WebLinksAddon for full ANSI / 256-color /
//     truecolor / alternate screen (vim, tmux, htop) support with ResizeObserver dimension syncing.
import { useCallback, useEffect, useRef, useState } from "react";
import { t } from "../../i18n";
import { navigate } from "../../lib/router";
import { onHubEvent, send } from "../../lib/store";
import { Button, cx, IconButton, Spinner } from "../../ui/atoms";
import { Icon } from "../../ui/icons";

type SessionPhase = "confirm" | "opening" | "open" | "closed" | "error";

const MAX_INPUT_BYTES = 64 * 1024;

function encodeBase64Bytes(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}

function encodeBase64Text(text: string): string {
  return encodeBase64Bytes(new TextEncoder().encode(text));
}

function decodeBase64Bytes(b64: string): Uint8Array {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export function ExtensionTerminalView({
  extension,
  target,
  onToggleSidebar,
}: {
  extension: string;
  target: string;
  onToggleSidebar: () => void;
}) {
  const [phase, setPhase] = useState<SessionPhase>("confirm");
  const [sessionId, setSessionId] = useState("");
  const [title, setTitle] = useState("");
  const [statusNote, setStatusNote] = useState("");
  const [dims, setDims] = useState<{ cols: number; rows: number }>({ cols: 80, rows: 24 });
  const [attempt, setAttempt] = useState(0);

  const hostRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<import("@xterm/xterm").Terminal | null>(null);
  const fitRef = useRef<import("@xterm/addon-fit").FitAddon | null>(null);
  const sessionIdRef = useRef("");
  const dimsRef = useRef(dims);
  dimsRef.current = dims;
  sessionIdRef.current = sessionId;

  const sendInputBytes = useCallback((bytes: Uint8Array) => {
    const sid = sessionIdRef.current;
    if (!sid || bytes.length === 0) return;
    // Chunk to stay strictly within the 64 KiB single-frame protocol limit.
    for (let offset = 0; offset < bytes.length; offset += MAX_INPUT_BYTES) {
      const slice = bytes.subarray(offset, Math.min(bytes.length, offset + MAX_INPUT_BYTES));
      send({
        type: "ext_terminal_input",
        extension,
        session_id: sid,
        data_b64: encodeBase64Bytes(slice),
      });
    }
  }, [extension]);

  const sendResize = useCallback((cols: number, rows: number) => {
    const c = Math.max(1, Math.min(1000, Math.floor(cols)));
    const r = Math.max(1, Math.min(1000, Math.floor(rows)));
    setDims({ cols: c, rows: r });
    const sid = sessionIdRef.current;
    if (!sid) return;
    send({
      type: "ext_terminal_resize",
      extension,
      session_id: sid,
      cols: c,
      rows: r,
    });
  }, [extension]);

  const closeSession = useCallback((reason = "client_closed") => {
    const sid = sessionIdRef.current;
    if (!sid) return;
    sessionIdRef.current = "";
    setSessionId("");
    send({
      type: "ext_terminal_close",
      extension,
      session_id: sid,
      reason,
    });
    setPhase("closed");
    setStatusNote(t("settings-extensionview.terminal_closed"));
  }, [extension]);

  // Mount xterm once the user confirms opening the session.
  useEffect(() => {
    if (phase === "confirm") return;
    const container = hostRef.current;
    if (!container) return;

    let cancelled = false;
    let resizeObserver: ResizeObserver | null = null;
    let dataDisp: { dispose(): void } | null = null;
    let binDisp: { dispose(): void } | null = null;

    const boot = async () => {
      const { Terminal, FitAddon, ClipboardAddon, WebLinksAddon } = await import("../session/terminal/xterm");
      if (cancelled) return;
      const term = new Terminal({
        cursorBlink: true,
        fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace',
        fontSize: 13,
        scrollback: 5000,
        convertEol: false,
        allowProposedApi: true,
        theme: {
          background: "#0d1117",
          foreground: "#e6edf3",
          cursor: "#e6edf3",
        },
      });
      const fit = new FitAddon();
      term.loadAddon(fit);
      term.loadAddon(new ClipboardAddon());
      term.loadAddon(new WebLinksAddon());
      term.open(container);
      try {
        fit.fit();
      } catch {
        // Container may still have zero size in headless test environments.
      }
      const cols = Math.max(1, term.cols || 80);
      const rows = Math.max(1, term.rows || 24);
      setDims({ cols, rows });
      termRef.current = term;
      fitRef.current = fit;

      dataDisp = term.onData((data) => {
        const sid = sessionIdRef.current;
        if (!sid) return;
        const encoded = new TextEncoder().encode(data);
        if (encoded.length <= MAX_INPUT_BYTES) {
          send({
            type: "ext_terminal_input",
            extension,
            session_id: sid,
            data_b64: encodeBase64Text(data),
          });
        } else {
          sendInputBytes(encoded);
        }
      });
      binDisp = term.onBinary((bin) => {
        const bytes = new Uint8Array(bin.length);
        for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i) & 0xff;
        sendInputBytes(bytes);
      });

      if (typeof ResizeObserver !== "undefined") {
        resizeObserver = new ResizeObserver(() => {
          try {
            fit.fit();
          } catch {
            return;
          }
          if (term.cols > 0 && term.rows > 0 && (term.cols !== dimsRef.current.cols || term.rows !== dimsRef.current.rows)) {
            sendResize(term.cols, term.rows);
          }
        });
        resizeObserver.observe(container);
      }

      send({
        type: "ext_terminal_open",
        extension,
        target,
        cols,
        rows,
        confirmed: true,
      });
      term.focus();
    };

    void boot();

    return () => {
      cancelled = true;
      resizeObserver?.disconnect();
      dataDisp?.dispose();
      binDisp?.dispose();
      const sid = sessionIdRef.current;
      if (sid) {
        sessionIdRef.current = "";
        send({ type: "ext_terminal_close", extension, session_id: sid, reason: "unmounted" });
      }
      termRef.current?.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
  }, [attempt, extension, target, phase === "confirm", sendInputBytes, sendResize]);

  // Listen for private ext_terminal_* server frames.
  useEffect(() => {
    if (phase === "confirm") return;
    const offOpened = onHubEvent("ext_terminal_opened", (msg) => {
      if (msg.extension !== extension) return;
      if (typeof msg.target === "string" && msg.target !== target) return;
      if (typeof msg.session_id === "string" && msg.session_id) {
        sessionIdRef.current = msg.session_id;
        setSessionId(msg.session_id);
      }
      if (typeof msg.title === "string" && msg.title) setTitle(msg.title);
      if (typeof msg.cols === "number" && typeof msg.rows === "number") {
        setDims({ cols: msg.cols, rows: msg.rows });
      }
      setPhase("open");
      setStatusNote("");
    });

    const offFrame = onHubEvent("ext_terminal_frame", (msg) => {
      if (msg.extension !== extension) return;
      if (sessionIdRef.current && msg.session_id !== sessionIdRef.current) return;
      if (typeof msg.data_b64 !== "string" || !msg.data_b64) return;
      try {
        const bytes = decodeBase64Bytes(msg.data_b64);
        termRef.current?.write(bytes);
      } catch {
        // Ignore malformed frame.
      }
    });

    const offClosed = onHubEvent("ext_terminal_closed", (msg) => {
      if (msg.extension !== extension) return;
      if (sessionIdRef.current && msg.session_id !== sessionIdRef.current) return;
      sessionIdRef.current = "";
      setSessionId("");
      setPhase("closed");
      const reason = typeof msg.reason === "string" && msg.reason ? msg.reason : "";
      const code = typeof msg.exit_code === "number" ? ` (exit ${msg.exit_code})` : "";
      setStatusNote(reason ? `${reason}${code}` : `${t("settings-extensionview.terminal_closed")}${code}`);
    });

    const offError = onHubEvent("ext_terminal_error", (msg) => {
      if (typeof msg.extension === "string" && msg.extension && msg.extension !== extension) return;
      if (sessionIdRef.current && typeof msg.session_id === "string" && msg.session_id && msg.session_id !== sessionIdRef.current) return;
      const errText = typeof msg.error === "string" && msg.error ? msg.error : typeof msg.code === "string" ? msg.code : "terminal error";
      setPhase("error");
      setStatusNote(errText);
    });

    return () => {
      offOpened();
      offFrame();
      offClosed();
      offError();
    };
  }, [extension, target, phase === "confirm"]);

  const handleConfirmOpen = () => {
    setStatusNote("");
    setPhase("opening");
    setAttempt((n) => n + 1);
  };

  const handleReconnect = () => {
    setSessionId("");
    sessionIdRef.current = "";
    setStatusNote("");
    setPhase("opening");
    setAttempt((n) => n + 1);
  };

  return (
    <div className="flex h-full min-h-0 flex-1 flex-col bg-page" data-ext-terminal={`${extension}/${target}`}>
      <header className="flex h-11 shrink-0 items-center gap-2 border-b border-line bg-page/90 px-3 backdrop-blur-md">
        <IconButton icon="sidebar" label={t("shellswitch.sidebar")} onClick={onToggleSidebar} className="lg:hidden" />
        <span className="flex size-6 shrink-0 items-center justify-center rounded-[6px] border border-line bg-inset text-ink-2">
          <Icon name="terminal" size={13} />
        </span>
        <div className="flex min-w-0 flex-1 items-baseline gap-1.5 overflow-hidden text-meta">
          <span className="truncate font-medium text-ink">{title || target}</span>
          <span className="min-w-0 shrink truncate text-label text-ink-3">· {extension}</span>
        </div>
        <div className="flex shrink-0 items-center gap-2 text-submeta text-ink-3">
          <span className="hidden font-mono sm:inline">{dims.cols}×{dims.rows}</span>
          <span
            className={cx(
              "inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium",
              phase === "open"
                ? "bg-green/15 text-green"
                : phase === "opening"
                  ? "bg-inset text-ink-2"
                  : phase === "error"
                    ? "bg-red/15 text-red"
                    : "bg-inset text-ink-3",
            )}
            data-ext-terminal-status={phase}
          >
            {phase === "opening" && <Spinner />}
            {phase === "open"
              ? t("machines.online")
              : phase === "opening"
                ? t("settings-extensionview.terminal_connecting")
                : phase === "error"
                  ? t("settings-extensionview.terminal_error")
                  : phase === "closed"
                    ? t("settings-extensionview.terminal_closed")
                    : t("settings-extensionview.confirm_title")}
          </span>
          {phase === "open" && (
            <Button tone="danger" onClick={() => closeSession("client_closed")}>
              {t("settings-extensionview.terminal_close")}
            </Button>
          )}
          {(phase === "closed" || phase === "error") && (
            <Button tone="primary" icon="retry" onClick={handleReconnect}>
              {t("settings-extensionview.terminal_reconnect")}
            </Button>
          )}
        </div>
      </header>

      {phase === "confirm" ? (
        <div className="flex flex-1 items-center justify-center p-6">
          <div className="w-full max-w-md space-y-4 rounded-card bg-surface p-5 shadow-card">
            <div className="flex items-center gap-2 text-card-title font-semibold text-ink">
              <Icon name="terminal" size={16} />
              <span>{t("settings-extensionview.confirm_title")}</span>
            </div>
            <p className="text-meta leading-relaxed text-ink-2">
              {t("settings-extensionview.confirm_open_ext_terminal", { extension, target })}
            </p>
            <div className="flex justify-end gap-2">
              <Button tone="quiet" onClick={() => navigate({ name: "home" })}>
                {t("common.cancel")}
              </Button>
              <Button tone="primary" onClick={handleConfirmOpen}>
                {t("settings-extensionview.terminal_connect")}
              </Button>
            </div>
          </div>
        </div>
      ) : (
        <div className="relative flex min-h-0 flex-1 flex-col bg-[#0d1117]">
          {statusNote && (
            <div
              role={phase === "error" ? "alert" : "status"}
              className={cx(
                "flex items-center justify-between gap-2 border-b border-line px-3 py-1.5 text-label",
                phase === "error" ? "bg-red/10 text-red" : "bg-surface/90 text-ink-2",
              )}
            >
              <span className="truncate">{statusNote}</span>
              <span className="font-mono text-[11px] text-ink-3">{sessionId || target}</span>
            </div>
          )}
          <div ref={hostRef} className="min-h-0 flex-1 p-2" data-ext-terminal-viewport />
        </div>
      )}
    </div>
  );
}
