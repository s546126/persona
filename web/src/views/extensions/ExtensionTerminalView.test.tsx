// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

type HubHandler = (msg: Record<string, unknown>) => void;
const listeners = new Map<string, Set<HubHandler>>();
const sent: Record<string, unknown>[] = [];
const written: Uint8Array[] = [];
let onDataCb: ((data: string) => void) | null = null;

vi.mock("../../lib/store", () => ({
  onHubEvent: (type: string, fn: HubHandler) => {
    let set = listeners.get(type);
    if (!set) {
      set = new Set();
      listeners.set(type, set);
    }
    set.add(fn);
    return () => set?.delete(fn);
  },
  send: (msg: Record<string, unknown>) => {
    sent.push(msg);
  },
}));

vi.mock("../../lib/router", () => ({
  navigate: vi.fn(),
}));

vi.mock("../session/terminal/xterm", () => {
  class MockTerminal {
    cols = 100;
    rows = 30;
    loadAddon() {}
    open() {}
    focus() {}
    write(data: Uint8Array) {
      written.push(data);
    }
    onData(cb: (data: string) => void) {
      onDataCb = cb;
      return { dispose: () => { onDataCb = null; } };
    }
    onBinary() {
      return { dispose: () => {} };
    }
    dispose() {}
  }
  class MockAddon {
    fit() {}
  }
  return {
    Terminal: MockTerminal,
    FitAddon: MockAddon,
    ClipboardAddon: MockAddon,
    WebLinksAddon: MockAddon,
  };
});

import { ExtensionTerminalView } from "./ExtensionTerminalView";

function emit(type: string, msg: Record<string, unknown>) {
  for (const fn of listeners.get(type) ?? []) fn({ type, ...msg });
}

describe("ExtensionTerminalView", () => {
  let host: HTMLDivElement;
  let root: Root;

  beforeEach(() => {
    listeners.clear();
    sent.length = 0;
    written.length = 0;
    onDataCb = null;
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  it("requires explicit owner confirmation before opening, streams bidirectional PTY frames, and closes cleanly", async () => {
    await act(async () => {
      root.render(<ExtensionTerminalView extension="ensemble" target="worker-1" onToggleSidebar={() => {}} />);
    });

    // Must not open silently before owner clicks Connect.
    expect(sent).toHaveLength(0);
    const buttons = [...host.querySelectorAll("button")];
    const connectBtn = buttons.find((b) => /connect|连接|接続/i.test(b.textContent ?? ""));
    expect(connectBtn).toBeTruthy();

    await act(async () => {
      connectBtn!.click();
    });
    await vi.waitFor(() => {
      expect(sent).toContainEqual({
        type: "ext_terminal_open",
        extension: "ensemble",
        target: "worker-1",
        cols: 100,
        rows: 30,
        confirmed: true,
      });
    });

    // Hub answers ext_terminal_opened.
    await act(async () => {
      emit("ext_terminal_opened", {
        extension: "ensemble",
        target: "worker-1",
        session_id: "extterm_abc",
        title: "Worker 1",
        cols: 100,
        rows: 30,
      });
    });

    // Incoming alternate-screen frame (vim) is decoded and written to xterm.
    const vimAnsi = "\x1b[?1049h~ VIM ~\x1b[?1049l";
    await act(async () => {
      emit("ext_terminal_frame", {
        extension: "ensemble",
        session_id: "extterm_abc",
        seq: 1,
        data_b64: btoa(vimAnsi),
      });
    });
    expect(written).toHaveLength(1);
    expect(new TextDecoder().decode(written[0])).toBe(vimAnsi);

    // User keystrokes go out over ext_terminal_input.
    await act(async () => {
      onDataCb?.("ls\r");
    });
    expect(sent).toContainEqual({
      type: "ext_terminal_input",
      extension: "ensemble",
      session_id: "extterm_abc",
      data_b64: btoa("ls\r"),
    });

    // Close session sends ext_terminal_close.
    const closeBtn = [...host.querySelectorAll("button")].find((b) => /close session|关闭会话|セッションを閉じる/i.test(b.textContent ?? ""));
    expect(closeBtn).toBeTruthy();
    await act(async () => {
      closeBtn!.click();
    });
    expect(sent).toContainEqual({
      type: "ext_terminal_close",
      extension: "ensemble",
      session_id: "extterm_abc",
      reason: "client_closed",
    });
  });
});
