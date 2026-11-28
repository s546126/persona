// @vitest-environment jsdom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const apiMock = vi.fn();

vi.mock("../../lib/api", () => ({
  api: (...args: unknown[]) => apiMock(...args),
  ApiError: class ApiError extends Error {
    status: number;
    body: Record<string, unknown>;
    constructor(status: number, message: string, body: Record<string, unknown> = {}) {
      super(message);
      this.status = status;
      this.body = body;
    }
  },
  enc: (s: string) => encodeURIComponent(s),
  errorText: (e: unknown) => (e instanceof Error ? e.message : String(e)),
}));

vi.mock("../session/TerminalPanel", () => ({
  TerminalPanel: ({ mid, sid, label }: { mid: string; sid: string; label?: string }) => (
    <div data-testid="mock-terminal-panel" data-mid={mid} data-sid={sid}>
      {label}
    </div>
  ),
}));

import { t } from "../../i18n";
import { MachineTerminalSubview } from "./MachineTerminalSubview";

const machine = {
  id: "mac-studio",
  name: "Mac Studio",
  online: true,
} as never;

async function flush() {
  for (let i = 0; i < 4; i++) {
    await act(async () => {
      await Promise.resolve();
    });
  }
}

describe("MachineTerminalSubview", () => {
  let host: HTMLDivElement;
  let root: Root;

  beforeEach(() => {
    apiMock.mockReset();
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
  });

  it("lists panes via GET /machines/{mid}/panes, embeds TerminalPanel, and creates a new terminal via POST", async () => {
    apiMock
      .mockResolvedValueOnce({
        hosts: [
          {
            kind: "herdr",
            stream: true,
            control: true,
            new_tab: true,
            workspaces: [{ id: "ws1", label: "main", cwd: "/srv/app" }],
            tabs: [{ id: "t1", workspace_id: "ws1", label: "zsh" }],
            panes: [{ pane: "p1", terminal: "pane:p1", tab_id: "t1", workspace_id: "ws1", title: "zsh" }],
          },
        ],
      })
      .mockResolvedValueOnce({ terminal: "pane:p2" })
      .mockResolvedValueOnce({
        hosts: [
          {
            kind: "herdr",
            stream: true,
            control: true,
            new_tab: true,
            workspaces: [{ id: "ws1", label: "main", cwd: "/srv/app" }],
            tabs: [{ id: "t1", workspace_id: "ws1", label: "zsh" }],
            panes: [
              { pane: "p1", terminal: "pane:p1", tab_id: "t1", workspace_id: "ws1", title: "zsh" },
              { pane: "p2", terminal: "pane:p2", tab_id: "t1", workspace_id: "ws1", title: "bash" },
            ],
          },
        ],
      });

    await act(async () => {
      root.render(<MachineTerminalSubview machine={machine} />);
    });
    await flush();

    const panel = host.querySelector('[data-testid="mock-terminal-panel"]');
    expect(panel?.getAttribute("data-mid")).toBe("mac-studio");
    expect(panel?.getAttribute("data-sid")).toBe("pane:p1");
    expect(apiMock).toHaveBeenCalledWith("/machines/mac-studio/panes");

    const newLabel = t("terminal-terminalpage.new_terminal");
    const newBtn = [...host.querySelectorAll("button")].find((b) => (b.textContent ?? "").includes(newLabel));
    expect(newBtn).toBeTruthy();

    await act(async () => {
      newBtn!.click();
    });
    await flush();

    expect(apiMock).toHaveBeenCalledWith("/machines/mac-studio/panes", {
      method: "POST",
      json: { kind: "herdr", workspace_id: "ws1" },
    });
    expect(host.querySelector('[data-testid="mock-terminal-panel"]')?.getAttribute("data-sid")).toBe("pane:p2");
  });

  it("shows a full-device-only alert on HTTP 403", async () => {
    const { ApiError } = await import("../../lib/api");
    apiMock.mockRejectedValueOnce(new ApiError(403, "forbidden", {}));

    await act(async () => {
      root.render(<MachineTerminalSubview machine={machine} />);
    });
    await flush();

    expect(host.querySelector('[role="alert"]')).toBeTruthy();
  });
});
