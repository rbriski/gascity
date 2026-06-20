import { expect, test, type Page } from "@playwright/test";
import { createServer, type Server, type ServerResponse } from "node:http";
import { readFile } from "node:fs/promises";
import path from "node:path";

type ProviderID = "claude" | "codex" | "gemini";

type RequestLog = {
  streamFormats: Map<string, string[]>;
  transcriptFormats: Map<string, string[]>;
};

type SessionFixture = {
  activeBead?: string;
  agentKind: string;
  id: string;
  pool?: string;
  provider: ProviderID;
  rig?: string;
  template: string;
};

const sessions: SessionFixture[] = [
  {
    agentKind: "crew",
    id: "s-claude",
    provider: "claude",
    rig: "rig-a/crew",
    template: "claude-crew",
  },
  {
    activeBead: "ga-structured",
    agentKind: "pool",
    id: "s-codex",
    pool: "builders",
    provider: "codex",
    rig: "rig-a",
    template: "codex-rigged",
  },
  {
    agentKind: "pool",
    id: "s-gemini",
    pool: "floaters",
    provider: "gemini",
    template: "gemini-pooled",
  },
];

const distDir = path.resolve(process.cwd(), "dist");
const requestLog: RequestLog = {
  streamFormats: new Map(),
  transcriptFormats: new Map(),
};
let server: Server;
let baseURL = "";
const openStreams = new Set<ServerResponse>();

test.beforeAll(async () => {
  server = createServer(async (req, res) => {
    if (!req.url) {
      sendJSON(res, 400, { error: "missing URL" });
      return;
    }
    const url = new URL(req.url, "http://127.0.0.1");
    try {
      if (await handleStatic(url, res)) return;
      if (handleAPI(url, res)) return;
      sendJSON(res, 404, { error: `unhandled ${url.pathname}` });
    } catch (error) {
      sendJSON(res, 500, { error: String(error) });
    }
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  if (typeof address !== "object" || !address) throw new Error("server did not bind");
  baseURL = `http://127.0.0.1:${address.port}`;
});

test.afterAll(async () => {
  for (const stream of openStreams) stream.end();
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

test.beforeEach(() => {
  requestLog.streamFormats.clear();
  requestLog.transcriptFormats.clear();
});

test("renders structured transcripts and streams across crew, rigged, and pooled provider sessions", async ({ page }, testInfo) => {
  await page.goto(`${baseURL}/?city=test-city`);

  await openSessionLog(page, "claude-crew");
  const drawer = page.locator("#agent-log-drawer");
  await expect(drawer).toBeVisible();
  await expect(drawer).toContainText("claude");
  await expect(drawer).toContainText("claude-sonnet");
  await expect(drawer).toContainText("[thinking]");
  await expect(drawer).toContainText("Read");
  await expect(drawer).toContainText("AGENTS.md");
  await expect(drawer).toContainText("Claude streamed completion.");
  await expect(drawer).toContainText("claude stream stdout");

  await openSessionLog(page, "codex-rigged");
  await expect(drawer).toContainText("codex");
  await expect(drawer).toContainText("codex-gpt");
  await expect(drawer).toContainText("apply_patch");
  await expect(drawer).toContainText("src/app.ts");
  await expect(drawer).toContainText("+ new line");
  await expect(drawer).toContainText("tests passed");
  await expect(drawer).toContainText("exit 0");

  await openSessionLog(page, "gemini-pooled");
  await expect(drawer).toContainText("gemini");
  await expect(drawer).toContainText("gemini-2.5");
  await expect(drawer).toContainText("python");
  await expect(drawer).toContainText("print('hello from gemini')");
  await expect(drawer).toContainText("hello from gemini");
  await expect(drawer).toContainText("src/gemini.ts");
  await expect(drawer).toContainText("streamed Gemini summary");

  for (const session of sessions) {
    expect(requestLog.transcriptFormats.get(session.id)).toContain("structured");
    expect(requestLog.streamFormats.get(session.id)).toContain("structured");
  }

  const screenshot = await drawer.screenshot({ path: "test-results/structured-session-gemini-drawer.png" });
  await testInfo.attach("structured-session-gemini-drawer", {
    body: screenshot,
    contentType: "image/png",
  });
});

async function openSessionLog(page: Page, name: string): Promise<void> {
  const button = page.getByRole("button", { name });
  await expect(button).toBeVisible();
  await button.click();
}

async function handleStatic(url: URL, res: ServerResponse): Promise<boolean> {
  const pathname = url.pathname === "/" ? "/index.html" : url.pathname;
  if (!["/index.html", "/dashboard.js", "/dashboard.css"].includes(pathname)) {
    return false;
  }
  const filePath = path.join(distDir, pathname.slice(1));
  const ext = path.extname(filePath);
  const contentType = ext === ".js"
    ? "text/javascript; charset=utf-8"
    : ext === ".css"
      ? "text/css; charset=utf-8"
      : "text/html; charset=utf-8";
  const body = await readFile(filePath);
  res.writeHead(200, { "content-type": contentType });
  res.end(body);
  return true;
}

function handleAPI(url: URL, res: ServerResponse): boolean {
  if (url.pathname === "/v0/cities") {
    sendJSON(res, 200, {
      items: [{ name: "test-city", path: "/tmp/test-city", phases_completed: [], running: true, status: "running" }],
    });
    return true;
  }
  if (url.pathname === "/v0/city/test-city/status") {
    sendJSON(res, 200, {
      agents: { running: 3, total: 3 },
      mail: { unread: 0 },
      work: { in_progress: 1, open: 1 },
    });
    return true;
  }
  if (url.pathname === "/v0/city/test-city/sessions") {
    sendJSON(res, 200, {
      items: sessions.map((session) => ({
        active_bead: session.activeBead ?? "",
        agent_kind: session.agentKind,
        attached: session.agentKind === "crew",
        id: session.id,
        last_active: "2026-04-18T20:00:00Z",
        last_output: session.provider === "gemini" ? "ready" : "",
        pool: session.pool ?? "",
        rig: session.rig ?? "",
        running: true,
        template: session.template,
      })),
    });
    return true;
  }
  if (url.pathname === "/v0/city/test-city/bead/ga-structured") {
    sendJSON(res, 200, { id: "ga-structured", title: "Patch dashboard structured streams" });
    return true;
  }
  const pendingMatch = url.pathname.match(/^\/v0\/city\/test-city\/session\/([^/]+)\/pending$/);
  if (pendingMatch) {
    sendJSON(res, 200, { pending: false });
    return true;
  }
  const transcriptMatch = url.pathname.match(/^\/v0\/city\/test-city\/session\/([^/]+)\/transcript$/);
  if (transcriptMatch) {
    const sessionID = transcriptMatch[1] ?? "";
    rememberFormat(requestLog.transcriptFormats, sessionID, url.searchParams.get("format") ?? "");
    sendJSON(res, 200, structuredTranscript(sessionID));
    return true;
  }
  const streamMatch = url.pathname.match(/^\/v0\/city\/test-city\/session\/([^/]+)\/stream$/);
  if (streamMatch) {
    const sessionID = streamMatch[1] ?? "";
    rememberFormat(requestLog.streamFormats, sessionID, url.searchParams.get("format") ?? "");
    sendStructuredSessionStream(res, sessionID);
    return true;
  }
  if (url.pathname === "/v0/city/test-city/events/stream") {
    sendKeepaliveStream(res);
    return true;
  }
  if (url.pathname.startsWith("/v0/city/test-city/")) {
    sendJSON(res, 200, { items: [] });
    return true;
  }
  if (url.pathname === "/health") {
    sendJSON(res, 200, { status: "ok" });
    return true;
  }
  return false;
}

function structuredTranscript(sessionID: string): unknown {
  const fixture = fixtureForSession(sessionID);
  return {
    format: "structured",
    id: sessionID,
    provider: fixture.provider,
    schema_version: "session.structured.v1",
    structured_messages: [{
      blocks: transcriptBlocks(fixture.provider),
      id: `${sessionID}-m-1`,
      model: providerModel(fixture.provider),
      provider: fixture.provider,
      role: "assistant",
      status: "final",
      stop_reason: "stop",
      timestamp: "2026-04-18T20:00:00Z",
    }],
    template: fixture.template,
  };
}

function transcriptBlocks(provider: ProviderID): unknown[] {
  switch (provider) {
    case "claude":
      return [
        { text: "Claude inspected the project instructions.", type: "text" },
        { thinking: "checking whether tool output is safe to show", type: "thinking" },
        { id: "claude-tool-1", input: { file_path: "AGENTS.md", kind: "file" }, name: "Read", type: "tool_use" },
        {
          structured: {
            content: "Architecture Best Practices",
            file_path: "AGENTS.md",
            kind: "read",
            num_lines: 12,
            start_line: 1,
          },
          tool_call_id: "claude-tool-1",
          type: "tool_result",
        },
      ];
    case "codex":
      return [
        { text: "Applying the patch now.", type: "text" },
        { id: "codex-tool-1", input: { file_path: "src/app.ts", kind: "patch", patch: "@@\n- old line\n+ new line" }, name: "apply_patch", type: "tool_use" },
        {
          structured: { file_path: "src/app.ts", kind: "edit", patch: "@@\n- old line\n+ new line" },
          tool_call_id: "codex-tool-1",
          type: "tool_result",
        },
      ];
    case "gemini":
      return [
        { text: "Gemini is validating generated output.", type: "text" },
        { id: "gemini-tool-1", input: { code: "print('hello from gemini')", kind: "code" }, name: "python", type: "tool_use" },
        {
          structured: { code: "print('hello from gemini')", exit_code: 0, kind: "python", stdout: "hello from gemini" },
          tool_call_id: "gemini-tool-1",
          type: "tool_result",
        },
      ];
  }
}

function sendStructuredSessionStream(res: ServerResponse, sessionID: string): void {
  writeStreamHeaders(res);
  const fixture = fixtureForSession(sessionID);
  setTimeout(() => {
    res.write("event: structured\n");
    res.write("id: 1\n");
    res.write(`data: ${JSON.stringify({
      format: "structured",
      id: sessionID,
      provider: fixture.provider,
      schema_version: "session.structured.v1",
      structured_messages: [{
        blocks: streamBlocks(fixture.provider),
        id: `${sessionID}-m-2`,
        model: providerModel(fixture.provider),
        provider: fixture.provider,
        role: "assistant",
        status: "final",
        timestamp: "2026-04-18T20:00:01Z",
      }],
      template: fixture.template,
    })}\n\n`);
  }, 100);
}

function streamBlocks(provider: ProviderID): unknown[] {
  switch (provider) {
    case "claude":
      return [
        { text: "Claude streamed completion.", type: "text" },
        { structured: { exit_code: 0, kind: "bash", stdout: "claude stream stdout" }, type: "tool_result" },
      ];
    case "codex":
      return [
        { structured: { exit_code: 0, kind: "bash", stdout: "tests passed" }, type: "tool_result" },
      ];
    case "gemini":
      return [
        { text: "streamed Gemini summary", type: "text" },
        { structured: { filenames: ["src/gemini.ts"], kind: "grep", mode: "ripgrep", text: "src/gemini.ts: hello" }, type: "tool_result" },
      ];
  }
}

function sendKeepaliveStream(res: ServerResponse): void {
  writeStreamHeaders(res);
  res.write(`event: heartbeat\ndata: ${JSON.stringify({ timestamp: "2026-04-18T20:00:00Z" })}\n\n`);
}

function writeStreamHeaders(res: ServerResponse): void {
  openStreams.add(res);
  res.on("close", () => openStreams.delete(res));
  res.writeHead(200, {
    "cache-control": "no-cache",
    "connection": "keep-alive",
    "content-type": "text/event-stream; charset=utf-8",
  });
}

function fixtureForSession(sessionID: string): SessionFixture {
  const fixture = sessions.find((candidate) => candidate.id === sessionID);
  if (!fixture) throw new Error(`unknown session ${sessionID}`);
  return fixture;
}

function providerModel(provider: ProviderID): string {
  switch (provider) {
    case "claude":
      return "claude-sonnet";
    case "codex":
      return "codex-gpt";
    case "gemini":
      return "gemini-2.5";
  }
}

function rememberFormat(target: Map<string, string[]>, sessionID: string, format: string): void {
  target.set(sessionID, [...(target.get(sessionID) ?? []), format]);
}

function sendJSON(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { "content-type": "application/json; charset=utf-8" });
  res.end(JSON.stringify(body));
}
