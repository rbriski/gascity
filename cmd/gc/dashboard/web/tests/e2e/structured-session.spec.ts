import { expect, test } from "@playwright/test";
import { createServer, type Server, type ServerResponse } from "node:http";
import { readFile } from "node:fs/promises";
import path from "node:path";

type RequestLog = {
  streamFormats: string[];
  transcriptFormats: string[];
};

const distDir = path.resolve(process.cwd(), "dist");
const requestLog: RequestLog = {
  streamFormats: [],
  transcriptFormats: [],
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
  requestLog.streamFormats = [];
  requestLog.transcriptFormats = [];
});

test("renders provider-neutral structured session transcript and stream frames", async ({ page }) => {
  await page.goto(`${baseURL}/?city=test-city`);

  const workerButton = page.getByRole("button", { name: "codex-worker" });
  await expect(workerButton).toBeVisible();
  await workerButton.click();

  const drawer = page.locator("#agent-log-drawer");
  await expect(drawer).toBeVisible();
  await expect(drawer).toContainText("Applying the patch now.");
  await expect(drawer).toContainText("apply_patch");
  await expect(drawer).toContainText("src/app.ts");
  await expect(drawer).toContainText("+ new line");
  await expect(drawer).toContainText("tests passed");

  expect(requestLog.transcriptFormats).toContain("structured");
  expect(requestLog.streamFormats).toContain("structured");
});

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
      agents: { running: 1, total: 1 },
      mail: { unread: 0 },
      work: { in_progress: 0, open: 0 },
    });
    return true;
  }
  if (url.pathname === "/v0/city/test-city/sessions") {
    sendJSON(res, 200, {
      items: [{
        active_bead: "",
        agent_kind: "crew",
        attached: true,
        id: "s-codex",
        last_active: "2026-04-18T20:00:00Z",
        last_output: "",
        pool: "",
        rig: "rig-a/crew",
        running: true,
        template: "codex-worker",
      }],
    });
    return true;
  }
  if (url.pathname === "/v0/city/test-city/session/s-codex/pending") {
    sendJSON(res, 200, { pending: false });
    return true;
  }
  if (url.pathname === "/v0/city/test-city/session/s-codex/transcript") {
    requestLog.transcriptFormats.push(url.searchParams.get("format") ?? "");
    sendJSON(res, 200, structuredTranscript());
    return true;
  }
  if (url.pathname === "/v0/city/test-city/session/s-codex/stream") {
    requestLog.streamFormats.push(url.searchParams.get("format") ?? "");
    sendStructuredSessionStream(res);
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

function structuredTranscript(): unknown {
  return {
    format: "structured",
    id: "s-codex",
    provider: "codex",
    schema_version: "session.structured.v1",
    structured_messages: [{
      blocks: [
        { text: "Applying the patch now.", type: "text" },
        { id: "tool-1", input: { file_path: "src/app.ts", kind: "patch" }, name: "apply_patch", type: "tool_use" },
        {
          structured: { file_path: "src/app.ts", kind: "edit", patch: "@@\n- old line\n+ new line" },
          tool_call_id: "tool-1",
          type: "tool_result",
        },
      ],
      id: "m-1",
      role: "assistant",
      status: "final",
      timestamp: "2026-04-18T20:00:00Z",
    }],
    template: "codex-worker",
  };
}

function sendStructuredSessionStream(res: ServerResponse): void {
  writeStreamHeaders(res);
  setTimeout(() => {
    res.write("event: structured\n");
    res.write("id: 1\n");
    res.write(`data: ${JSON.stringify({
      format: "structured",
      id: "s-codex",
      provider: "codex",
      schema_version: "session.structured.v1",
      structured_messages: [{
        blocks: [{ structured: { exit_code: 0, kind: "bash", stdout: "tests passed" }, type: "tool_result" }],
        id: "m-2",
        role: "assistant",
        status: "final",
        timestamp: "2026-04-18T20:00:01Z",
      }],
      template: "codex-worker",
    })}\n\n`);
  }, 100);
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

function sendJSON(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { "content-type": "application/json; charset=utf-8" });
  res.end(JSON.stringify(body));
}
