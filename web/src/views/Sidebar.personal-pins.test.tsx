// @vitest-environment jsdom
// Pinning a personal agent moves it to the top of the sidebar and the Agents page at once, in pin order, and the pin
// stays lit on a pinned row instead of showing only on hover.
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
vi.hoisted(() => {
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver;
  window.matchMedia ??= ((media: string) => ({ matches: false, media, addEventListener() {}, removeEventListener() {} })) as typeof window.matchMedia;
});
// jsdom lays nothing out, so the virtualizer would draw no row: draw every item, in order
vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: ({ count, getItemKey }: { count: number; getItemKey: (i: number) => string }) => ({
    getVirtualItems: () => Array.from({ length: count }, (_, index) => ({ index, key: getItemKey(index), start: index * 32 })),
    getTotalSize: () => count * 32,
    measureElement: () => {},
  }),
}));
import { initI18n, t } from "../i18n";
import { emptyPrefs, pinnedFirst, refreshAgentSessions, resetAgents, setAgentListMode, updateAgentPrefs, type AgentPrefs, type AgentSession, type AgentSourceStatus } from "../lib/agents";
import { SESSIONS_LIST_FEATURE } from "../lib/session-list";
import { getState, reset } from "../lib/store";
import { PersonalAgents } from "./SidebarRows";
import { AgentsView } from "./agents/AgentsView";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
const row = (key: string, rankTimeMs: number, unreadCount = 0): AgentSession => ({ id: `example:${key}`, source: "example", sessionKey: key, provider: "example", title: `Agent ${key}`, canSend: true, unreadCount, rankTimeMs });
const listed = [row("new", 3000), row("mid", 2000, 2), row("old", 1000)];
const ready: AgentSourceStatus = { source: "example", enabled: true, accounts: [{ accountId: "example-account", provider: "example" }] };
const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
let host: HTMLDivElement, root: Root;
let puts: string[][];
let hold: Promise<void> | null;
const flush = async () => { for (let i = 0; i < 15; i++) await act(async () => { await Promise.resolve(); }); };

beforeEach(async () => {
  await initI18n("en");
  localStorage.clear();
  resetAgents();
  puts = [];
  hold = null;
  vi.stubGlobal("fetch", async (url: string, init?: RequestInit) => {
    const path = new URL(url, "https://example.invalid").pathname;
    // the hub's page in its own (time) order: the client must not wait for a re-pull to move a pinned row
    if (path === "/api/v1/agents/sessions") return response({ sessions: listed, remove: [], added: [], has_more: false, revision: "r1", total: listed.length });
    if (path === "/api/v1/agents") return response({ sources: [ready] });
    if (path === "/api/v1/agents/prefs") {
      const body = init?.body ? (JSON.parse(String(init.body)) as AgentPrefs) : emptyPrefs();
      if ((init?.method ?? "GET").toUpperCase() === "PUT") {
        puts.push(body.pins);
        if (hold) await hold;
      }
      return response(body);
    }
    return response({});
  });
  updateAgentPrefs(() => emptyPrefs());
  await flush();
  puts = [];
  getState().features = [SESSIONS_LIST_FEATURE];
  setAgentListMode(true);
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  host.remove();
  reset();
  resetAgents();
  vi.unstubAllGlobals();
});

const titles = () => [...host.querySelectorAll("a")].map((a) => a.textContent?.match(/Agent (new|mid|old)/)?.[0]).filter(Boolean);
const pinOf = (title: string) => {
  const link = [...host.querySelectorAll("a")].find((a) => a.textContent?.includes(title))!;
  return link.parentElement!.querySelector<HTMLButtonElement>(`button[aria-label="${t("sidebar.pin")}"], button[aria-label="${t("sidebar.unpin")}"], button[aria-label="${t("agents-agentsview.pin")}"], button[aria-label="${t("agents-agentsview.unpin")}"]`)!;
};

describe("pinnedFirst", () => {
  it("orders pins first in pin order, the rest newest first", () => {
    const pins = ["example:old", "example:mid"];
    expect([...listed].sort((a, b) => pinnedFirst(pins, a, b)).map((s) => s.sessionKey)).toEqual(["old", "mid", "new"]);
    expect([...listed].sort((a, b) => pinnedFirst([], a, b)).map((s) => s.sessionKey)).toEqual(["new", "mid", "old"]);
  });
});

for (const view of ["sidebar", "agents page"] as const) describe(`${view} pins`, () => {
  const mount = async () => {
    await act(async () => root.render(view === "sidebar"
      ? <PersonalAgents reg={() => () => {}} hover={() => {}} activeKey="" go={() => () => {}} />
      : <AgentsView onToggleSidebar={() => {}} />));
    await act(async () => { await refreshAgentSessions(); });
    await flush();
  };

  it("moves a pinned row to the top at once, keeps pin order, and puts it back on unpin", async () => {
    await mount();
    expect(titles()).toEqual(["Agent new", "Agent mid", "Agent old"]);
    let release!: () => void;
    hold = new Promise((done) => { release = done; });
    await act(async () => pinOf("Agent old").click());
    // before the hub answers the PUT
    expect(titles()).toEqual(["Agent old", "Agent new", "Agent mid"]);
    expect(puts).toEqual([["example:old"]]);
    await act(async () => pinOf("Agent mid").click());
    expect(titles()).toEqual(["Agent mid", "Agent old", "Agent new"]);
    release();
    await flush();
    expect(titles()).toEqual(["Agent mid", "Agent old", "Agent new"]);
    hold = null;
    await act(async () => pinOf("Agent mid").click());
    await act(async () => pinOf("Agent old").click());
    await flush();
    expect(titles()).toEqual(["Agent new", "Agent mid", "Agent old"]);
  });

  it("keeps a pinned row's pin visible and lit", async () => {
    await mount();
    const before = pinOf("Agent old");
    expect(before.getAttribute("aria-pressed")).toBe("false");
    expect(before.className).not.toContain("text-accent");
    await act(async () => before.click());
    await flush();
    const pin = pinOf("Agent old");
    expect(pin.getAttribute("aria-pressed")).toBe("true");
    expect(pin.className).toContain("text-accent");
    expect(pin.className).not.toMatch(/(^|\s)opacity-0(\s|$)/);
    if (view === "sidebar") {
      expect(pinOf("Agent new").className).toMatch(/(^|\s)opacity-0(\s|$)/); // an unpinned row's pin still waits for hover
      // a pinned row with unread messages keeps its badge clear of the pin: room on the right, not hidden on hover
      await act(async () => pinOf("Agent mid").click());
      await flush();
      const link = [...host.querySelectorAll("a")].find((a) => a.textContent?.includes("Agent mid"))!;
      expect(link.className).toContain("pr-7");
      expect(link.querySelector("span.rounded-full")?.className).not.toContain("group-hover/row:invisible");
    }
  });
});

describe("pins across pages", () => {
  it("shows a pinned old row on the first page, and unpinning sends it back past the loaded rows", async () => {
    const many = Array.from({ length: 120 }, (_, i) => row(`r${i}`, 100_000 - i));
    let hubPins: string[] = [];
    let revision = 1;
    // a hub as the server orders it: pinned first, in pin order, then newest first; 50 a page behind a cursor
    vi.stubGlobal("fetch", async (url: string, init?: RequestInit) => {
      const u = new URL(url, "https://example.invalid");
      if (u.pathname === "/api/v1/agents") return response({ sources: [ready] });
      if (u.pathname === "/api/v1/agents/prefs") {
        const body = init?.body ? (JSON.parse(String(init.body)) as AgentPrefs) : { ...emptyPrefs(), pins: hubPins };
        if ((init?.method ?? "GET").toUpperCase() === "PUT") { hubPins = body.pins; revision++; }
        return response(body);
      }
      if (u.pathname === "/api/v1/agents/sessions") {
        const ordered = [...many].sort((a, b) => pinnedFirst(hubPins, a, b));
        const since = u.searchParams.get("since");
        // a delta carries the rows whose pin changed (what the hub's journal holds); a full read pages the list
        if (since) return response({ sessions: since === `r${revision}` ? [] : ordered.filter((s) => hubPins.includes(s.id) || s.id === "example:r119"), remove: [], added: [], has_more: false, revision: `r${revision}`, total: many.length });
        const start = Number(u.searchParams.get("cursor") ?? 0);
        const end = start + 50;
        return response({ sessions: ordered.slice(start, end), remove: [], added: [], has_more: end < ordered.length, ...(end < ordered.length ? { next_cursor: String(end) } : {}), revision: `r${revision}`, total: many.length });
      }
      return response({});
    });
    const names = () => [...host.querySelectorAll("a")].map((a) => a.textContent?.match(/Agent r\d+/)?.[0]).filter(Boolean);
    // already pinned (here or on another device) when the list is first read: the hub's first page leads with it
    await act(async () => updateAgentPrefs((p) => ({ ...p, pins: ["example:r119"] })));
    await flush();
    await act(async () => root.render(<PersonalAgents reg={() => () => {}} hover={() => {}} activeKey="" go={() => () => {}} />));
    await act(async () => { await refreshAgentSessions(); });
    await flush();
    expect(names()).toHaveLength(50);
    expect(names()[0]).toBe("Agent r119");
    expect(names()[1]).toBe("Agent r0");
    // unpinned here: it goes back past the loaded rows, not to the bottom of the first page
    await act(async () => pinOfSidebar("Agent r119").click());
    await flush();
    expect(names()).not.toContain("Agent r119");
    expect(names()[0]).toBe("Agent r0");
    expect(names()).toHaveLength(50);
  });
});

function pinOfSidebar(title: string) {
  const link = [...host.querySelectorAll("a")].find((a) => a.textContent?.includes(title))!;
  return link.parentElement!.querySelector<HTMLButtonElement>("button[aria-pressed]")!;
}
