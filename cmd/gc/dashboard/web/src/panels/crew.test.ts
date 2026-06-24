import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "../api";
import { syncCityScopeFromLocation } from "../state";
import { connectAgentOutput } from "../sse";
import { installCrewInteractions, renderCrew } from "./crew";

vi.mock("../sse", () => ({
  connectAgentOutput: vi.fn(() => ({ close: vi.fn() })),
}));

const nativeScrollIntoView = HTMLElement.prototype.scrollIntoView;

describe("crew empty states", () => {
  beforeEach(() => {
    document.body.innerHTML = `
      <div id="crew-loading">Loading crew...</div>
      <table id="crew-table" style="display:none"><tbody id="crew-tbody"></tbody></table>
      <div id="crew-empty" style="display:none"><p>No crew configured</p></div>
      <div id="rigged-body"></div>
      <div id="pooled-body"></div>
      <span id="crew-count"></span>
      <span id="rigged-count"></span>
      <span id="pooled-count"></span>
      <div id="agent-log-drawer" style="display:none"></div>
    `;
    window.history.pushState({}, "", "/dashboard?city=mc-city");
    syncCityScopeFromLocation();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    restoreScrollIntoView();
    window.history.pushState({}, "", "/dashboard");
    syncCityScopeFromLocation();
  });

  it("shows no crew configured when the city has zero crew sessions", async () => {
    const sessionQueries: Array<Record<string, unknown>> = [];
    vi.spyOn(api, "GET").mockImplementation(async (path: string, options?: unknown) => {
      if (path === "/v0/city/{cityName}/sessions") {
        sessionQueries.push((options as { params?: { query?: Record<string, unknown> } } | undefined)?.params?.query ?? {});
        return { data: { items: [] } } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    await renderCrew();

    expect(sessionQueries[0]).toEqual({ peek: true });
    expect((document.getElementById("crew-empty") as HTMLElement).style.display).toBe("block");
    expect(document.getElementById("crew-empty")?.textContent).toContain("No crew configured");
    expect(document.getElementById("crew-empty")?.textContent).not.toContain("Select a city");
  });

  it("hides agent role sessions from the crew table while keeping crew rows", async () => {
    vi.spyOn(api, "GET").mockImplementation(async (path: string) => {
      if (path === "/v0/city/{cityName}/sessions") {
        return {
          data: {
            items: [
              // Crew member — should appear.
              {
                active_bead: "",
                agent_kind: "crew",
                attached: true,
                id: "s-fontaine",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "",
                rig: "rig-a/crew",
                running: true,
                template: "rig-a/crew/fontaine",
              },
              // Role agents — should NOT appear in the crew table.
              {
                active_bead: "",
                agent_kind: "role",
                attached: false,
                id: "s-role-1",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "",
                running: true,
                template: "rig-a/singleton",
              },
              {
                active_bead: "",
                agent_kind: "role",
                attached: false,
                id: "s-role-2",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "",
                running: true,
                template: "rig-a/another-singleton",
              },
              // Pool/multi-instance agent — also not crew.
              {
                active_bead: "",
                agent_kind: "pool",
                attached: false,
                id: "s-pool-1",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "",
                pool: "scaler",
                rig: "rig-a",
                running: true,
                template: "rig-a/scaler-1",
              },
            ],
          },
        } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/pending") {
        return { data: { pending: false } } as never;
      }
      if (path === "/v0/city/{cityName}/bead/{id}") {
        return { data: null } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    await renderCrew();

    const crewRows = document.querySelectorAll("#crew-tbody tr");
    expect(crewRows.length).toBe(1);
    expect(crewRows[0]?.textContent).toContain("rig-a/crew/fontaine");
    expect(document.getElementById("crew-count")?.textContent).toBe("1");
    expect((document.getElementById("crew-table") as HTMLElement).style.display).toBe("table");
    // Pool agent should still flow through to the rigged panel.
    expect(document.getElementById("rigged-count")?.textContent).toBe("1");
  });

  it("falls back to the crew empty state while still listing non-crew role sessions", async () => {
    vi.spyOn(api, "GET").mockImplementation(async (path: string) => {
      if (path === "/v0/city/{cityName}/sessions") {
        return {
          data: {
            items: [
              {
                active_bead: "",
                agent_kind: "role",
                attached: false,
                id: "s-role",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "",
                running: true,
                template: "rig-a/singleton",
              },
              {
                active_bead: "",
                agent_kind: "role",
                attached: false,
                id: "s-role-rigged",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "",
                rig: "rig-a",
                running: true,
                template: "rig-a/another-singleton",
              },
            ],
          },
        } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/pending") {
        return { data: { pending: false } } as never;
      }
      if (path === "/v0/city/{cityName}/bead/{id}") {
        return { data: null } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    await renderCrew();

    expect(document.querySelectorAll("#crew-tbody tr").length).toBe(0);
    expect((document.getElementById("crew-empty") as HTMLElement).style.display).toBe("block");
    expect(document.getElementById("crew-empty")?.textContent).toContain("No crew configured");
    expect(document.getElementById("crew-count")?.textContent).toBe("0");
    expect(document.getElementById("pooled-count")?.textContent).toBe("2");
    expect(document.getElementById("pooled-body")?.textContent).toContain("rig-a/singleton");
    expect(document.getElementById("pooled-body")?.textContent).toContain("rig-a/another-singleton");
  });

  it("loads older transcript pages without losing the drawer loading sentinel", async () => {
    document.body.innerHTML = `
      <div id="crew-loading">Loading crew...</div>
      <table id="crew-table" style="display:none"><tbody id="crew-tbody"></tbody></table>
      <div id="crew-empty" style="display:none"><p>No crew configured</p></div>
      <div id="rigged-body"></div>
      <div id="pooled-body"></div>
      <span id="crew-count"></span>
      <span id="rigged-count"></span>
      <span id="pooled-count"></span>
      <div id="agent-log-drawer" style="display:none">
        <span id="log-drawer-agent-name"></span>
        <span id="log-drawer-count"></span>
        <button id="log-drawer-older-btn" style="display:none">Load older</button>
        <button id="log-drawer-close-btn">Close</button>
        <div id="log-drawer-body">
          <div id="log-drawer-messages">
            <div id="log-drawer-loading">Loading logs...</div>
          </div>
        </div>
      </div>
    `;
    const transcriptQueries: Array<Record<string, string | undefined>> = [];
    vi.spyOn(api, "GET").mockImplementation(async (path: string, options?: unknown) => {
      if (path === "/v0/city/{cityName}/sessions") {
        return {
          data: {
            items: [{
              active_bead: "",
              agent_kind: "crew",
              attached: true,
              id: "s-reviewer",
              last_active: "2026-04-18T20:00:00Z",
              last_output: "",
              rig: "rig-a/crew",
              running: true,
              template: "reviewer",
            }],
          },
        } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/pending") {
        return { data: { pending: false } } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/transcript") {
        const query = (options as { params?: { query?: Record<string, string | undefined> } } | undefined)?.params?.query ?? {};
        transcriptQueries.push(query);
        if (query.before) {
          return {
            data: {
              turns: [{ role: "assistant", text: "Older transcript turn", timestamp: "2026-04-18T19:00:00Z" }],
              pagination: {
                has_older_messages: false,
                returned_message_count: 1,
                total_compactions: 0,
                total_message_count: 3,
              },
            },
          } as never;
        }
        return {
          data: {
            turns: [{ role: "assistant", text: "Newest transcript turn", timestamp: "2026-04-18T20:00:00Z" }],
            pagination: {
              has_older_messages: true,
              returned_message_count: 1,
              total_compactions: 0,
              total_message_count: 3,
              truncated_before_message: "cursor-1",
            },
          },
        } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    installCrewInteractions();
    await renderCrew();
    document.querySelector<HTMLButtonElement>(".agent-log-link")?.click();
    await waitFor(() => {
      expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Newest transcript turn");
    });

    expect(document.getElementById("log-drawer-loading")).not.toBeNull();
    document.getElementById("log-drawer-older-btn")?.click();
    await waitFor(() => {
      expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Older transcript turn");
    });

    expect(transcriptQueries.map((query) => query.before)).toEqual([undefined, "cursor-1"]);
    expect(document.getElementById("log-drawer-loading")).not.toBeNull();
  });

  it("requests structured transcripts and renders rich tool/diff blocks", async () => {
    const scrollIntoView = mockScrollIntoView();
    document.body.innerHTML = `
      <div id="crew-loading">Loading crew...</div>
      <table id="crew-table" style="display:none"><tbody id="crew-tbody"></tbody></table>
      <div id="crew-empty" style="display:none"><p>No crew configured</p></div>
      <div id="rigged-body"></div>
      <div id="pooled-body"></div>
      <span id="crew-count"></span>
      <span id="rigged-count"></span>
      <span id="pooled-count"></span>
      <div id="agent-log-drawer" style="display:none">
        <span id="log-drawer-agent-name"></span>
        <span id="log-drawer-count"></span>
        <button id="log-drawer-older-btn" style="display:none">Load older</button>
        <button id="log-drawer-close-btn">Close</button>
        <div id="log-drawer-body">
          <div id="log-drawer-messages">
            <div id="log-drawer-loading">Loading logs...</div>
          </div>
        </div>
      </div>
    `;
    const transcriptQueries: Array<Record<string, string | undefined>> = [];
    let streamHandler: ((msg: unknown) => void) | undefined;
    vi.mocked(connectAgentOutput).mockImplementation((_city, _sessionID, onEvent) => {
      streamHandler = onEvent as (msg: unknown) => void;
      return { close: vi.fn() };
    });
    vi.spyOn(api, "GET").mockImplementation(async (path: string, options?: unknown) => {
      if (path === "/v0/city/{cityName}/sessions") {
        return {
          data: {
            items: [{
              active_bead: "",
              agent_kind: "crew",
              attached: true,
              id: "s-codex",
              last_active: "2026-04-18T20:00:00Z",
              last_output: "",
              rig: "rig-a/crew",
              running: true,
              template: "codex-worker",
            }],
          },
        } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/pending") {
        return { data: { pending: false } } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/transcript") {
        const query = (options as { params?: { query?: Record<string, string | undefined> } } | undefined)?.params?.query ?? {};
        transcriptQueries.push(query);
        return {
          data: {
            format: "structured",
            structured_messages: [{
              id: "u-1",
              role: "user",
              timestamp: "2026-04-18T19:59:58Z",
              user_prompt: {
                text: "Please inspect this.",
                opened_files: ["/tmp/project/src/app.ts"],
                uploaded_files: [{
                  original_name: "diagram.png",
                  size: "12 KB",
                  mime_type: "image/png",
                  file_path: "/tmp/uploads/diagram.png",
                }],
                selections: [{ text: "const answer = 42;" }],
              },
              blocks: [{ type: "text", text: "raw prompt with <ide_opened_file>metadata</ide_opened_file>" }],
            }, {
              id: "sys-1",
              role: "system",
              timestamp: "2026-04-18T19:59:59Z",
              system_event: {
                kind: "error",
                category: "usage_limit",
                code: "usage_limit_exceeded",
                message: "You've hit your usage limit.",
              },
              blocks: [{ type: "text", text: "You've hit your usage limit." }],
            }, {
              id: "m-1",
              role: "assistant",
              timestamp: "2026-04-18T20:00:00Z",
              blocks: [
                { type: "text", text: "Applying the patch now." },
                { type: "image", file_path: "screens/shot.png", image_url: "https://example.com/shot.png", mime_type: "image/png" },
                { type: "tool_use", id: "tool-1", name: "apply_patch", input: { kind: "patch", file_path: "src/app.ts", language: "typescript" } },
                {
                  type: "tool_result",
                  tool_call_id: "tool-1",
                  structured: {
                    kind: "edit",
                    file_path: "src/app.ts",
                    language: "typescript",
                    old_string: "old line",
                    new_string: "new line",
                    original_file: "export const message = \"old line\";\n",
                    replace_all: false,
                    user_modified: false,
                    patch_hunks: [{
                      file_path: "src/app.ts",
                      old_start: 1,
                      old_lines: 1,
                      new_start: 1,
                      new_lines: 1,
                      lines: ["- old line", "+ new line"],
                    }],
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-search",
                  structured: {
                    kind: "search",
                    query: "structured tool result formats",
                    mode: "query",
                    filenames: ["https://example.com/provider-format"],
                    num_results: 1,
                    result_items: [{
                      title: "Provider format notes",
                      url: "https://example.com/provider-format",
                      snippet: "Typed provider-neutral search item.",
                    }],
                    content: "https://example.com/provider-format: Provider format notes\n",
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-write",
                  structured: {
                    kind: "write",
                    file_path: "notes.txt",
                    language: "text",
                    content: "wrote notes.txt",
                    num_lines: 1,
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-grep-count",
                  structured: {
                    kind: "grep",
                    mode: "count",
                    filenames: ["README.md", "src/app.ts"],
                    counts: [
                      { name: "README.md", value: "2" },
                      { name: "src/app.ts", value: "5" },
                    ],
                    num_files: 2,
                    num_results: 7,
                    applied_limit: 100,
                    content: "README.md:2\nsrc/app.ts:5\n",
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-glob",
                  structured: {
                    kind: "glob",
                    filenames: ["internal/api/session_structured_types.go"],
                    num_files: 1,
                    duration_ms: 27,
                    truncated: true,
                  },
                },
                {
                  type: "tool_use",
                  id: "tool-fetch",
                  name: "WebFetch",
                  input: {
                    kind: "fetch",
                    url: "https://example.com/spec",
                    prompt: "Extract the structured contract",
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-fetch",
                  structured: {
                    kind: "fetch",
                    url: "https://example.com/spec",
                    status_code: 200,
                    status_text: "OK",
                    bytes: 4096,
                    duration_ms: 83,
                    content: "Fetched structured spec content.",
                  },
                },
                {
                  type: "tool_use",
                  id: "tool-todo",
                  name: "TodoWrite",
                  input: {
                    kind: "todo",
                    todos: [{
                      content: "Normalize typed todos",
                      status: "in_progress",
                      active_form: "Normalizing typed todos",
                      priority: "high",
                    }],
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-todo",
                  structured: {
                    kind: "todo",
                    content: "todos updated",
                    old_todos: [{
                      content: "Normalize typed todos",
                      status: "in_progress",
                      active_form: "Normalizing typed todos",
                    }],
                    new_todos: [{
                      content: "Normalize typed todos",
                      status: "completed",
                      active_form: "Normalizing typed todos",
                    }],
                  },
                },
                {
                  type: "tool_use",
                  id: "tool-plan",
                  name: "ExitPlanMode",
                  input: {
                    kind: "plan",
                    plan: "Expose typed plan data without HTML.",
                    explanation: "Keep clients provider-neutral.",
                    steps: [{
                      step: "Add plan DTO",
                      status: "in_progress",
                    }],
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-plan",
                  structured: {
                    kind: "plan",
                    plan: "Expose typed plan data without HTML.",
                    content: "plan captured",
                  },
                },
                {
                  type: "tool_use",
                  id: "tool-question",
                  name: "AskUserQuestion",
                  input: {
                    kind: "question",
                    question: "Proceed with typed question DTOs?",
                    options: ["Yes", "No"],
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-question",
                  structured: {
                    kind: "question",
                    question: "Select rollout scope",
                    questions: [{
                      question: "Select rollout scope",
                      header: "Scope",
                      multi_select: true,
                      options: [
                        { label: "All providers", description: "Validate first-class and graceful providers" },
                        { label: "Claude only", description: "Narrow smoke test" },
                      ],
                    }],
                    options: ["All providers", "Claude only"],
                    answer: "All providers",
                    answers: [{ name: "Select rollout scope", value: "All providers" }],
                    content: "question answered",
                  },
                },
                {
                  type: "tool_use",
                  id: "tool-stdin",
                  name: "write_stdin",
                  input: {
                    kind: "stdin",
                    task_id: "42",
                    text: "hello\n",
                    linked_command: "claude --resume",
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-stdin",
                  structured: {
                    kind: "stdin",
                    task_id: "42",
                    content: "sent",
                  },
                },
                {
                  type: "tool_use",
                  id: "tool-task",
                  name: "TaskOutput",
                  input: {
                    kind: "task",
                    task_id: "task-123",
                    task_type: "subagent",
                    task_status: "running",
                    description: "Run delegated check",
                  },
                },
                {
                  type: "tool_result",
                  tool_call_id: "tool-task",
                  structured: {
                    kind: "task",
                    task_id: "task-123",
                    task_type: "subagent",
                    task_status: "completed",
                    description: "Run delegated check",
                    total_duration_ms: 1234,
                    total_tokens: 321,
                    total_tool_use_count: 4,
                    output: "delegated check passed",
                    exit_code: 0,
                  },
                },
              ],
            }],
            pagination: {
              has_older_messages: false,
              returned_message_count: 1,
              total_compactions: 0,
              total_message_count: 1,
            },
          },
        } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    installCrewInteractions();
    await renderCrew();
    document.querySelector<HTMLButtonElement>(".agent-log-link")?.click();

    await waitFor(() => {
      expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Applying the patch now.");
    });
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("prompt: Please inspect this.");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("opened files: /tmp/project/src/app.ts");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("uploaded files:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("diagram.png (12 KB, image/png): /tmp/uploads/diagram.png");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("selections:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("const answer = 42;");
    expect(document.getElementById("log-drawer-messages")?.textContent).not.toContain("raw prompt with");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("system event");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("kind: error");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("category: usage_limit");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("code: usage_limit_exceeded");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("message: You've hit your usage limit.");
    expect(transcriptQueries[0]).toMatchObject({ format: "structured" });
    expect(scrollIntoView).toHaveBeenCalledWith({ block: "start" });
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("apply_patch");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("src/app.ts");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("language: typescript");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("old: old line");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("new: new line");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("original file: export const message = \"old line\";");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("replace all: false");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("user modified: false");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("+ new line");
    expect(document.querySelector(".log-msg-diff")).not.toBeNull();
    expect(document.querySelector(".log-msg-diff-file")?.textContent).toBe("*** Update File: src/app.ts");
    expect(document.querySelector(".log-msg-diff-hunk")?.textContent).toBe("@@ -1 +1 @@");
    expect(document.querySelector(".log-msg-diff-del")?.textContent).toBe("- old line");
    expect(document.querySelector(".log-msg-diff-add")?.textContent).toBe("+ new line");
    expect(document.querySelectorAll(".log-msg-diff-del").length).toBe(1);
    expect(document.querySelectorAll(".log-msg-diff-add").length).toBe(1);
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("write result");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("file: notes.txt");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("wrote notes.txt");
    expect(document.querySelector(".log-msg-image-block")?.textContent).toContain("file: screens/shot.png");
    expect(document.querySelector(".log-msg-image-block")?.textContent).toContain("url: https://example.com/shot.png");
    expect(document.querySelector(".log-msg-image-block")?.textContent).toContain("mime: image/png");
    expect(document.querySelector<HTMLImageElement>(".log-msg-image-block img")?.getAttribute("src")).toBe("https://example.com/shot.png");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("query: structured tool result formats");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("result items:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Provider format notes | https://example.com/provider-format | Typed provider-neutral search item.");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("results: 1");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("mode: count");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("counts:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("README.md: 2");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("src/app.ts: 5");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("results: 7");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("applied limit: 100");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("duration ms: 27");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("truncated");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("WebFetch");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("url: https://example.com/spec");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("prompt: Extract the structured contract");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("status: 200");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("status text: OK");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("bytes: 4096");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("duration ms: 83");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Fetched structured spec content.");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("TodoWrite");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("todos:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("[in_progress] Normalize typed todos priority high");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("old todos:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("new todos:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("[completed] Normalize typed todos");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("ExitPlanMode");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("plan: Expose typed plan data without HTML.");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("explanation: Keep clients provider-neutral.");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("steps:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("[in_progress] Add plan DTO");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("plan captured");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("AskUserQuestion");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("question: Select rollout scope");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("questions:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Scope | Select rollout scope | multi-select");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("All providers | Validate first-class and graceful providers");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("options: All providers, Claude only");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("answer: All providers");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("answers:");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Select rollout scope: All providers");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("write_stdin");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("kind: stdin");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("linked command: claude --resume");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("hello");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("TaskOutput");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("task: task-123");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("task type: subagent");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("task status: completed");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("description: Run delegated check");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("total duration ms: 1234");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("total tokens: 321");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("total tool calls: 4");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("output: delegated check passed");

    streamHandler?.({
      type: "structured",
      data: {
        format: "structured",
        structured_messages: [{
          id: "m-2",
          role: "assistant",
          timestamp: "2026-04-18T20:00:01Z",
          blocks: [{
            type: "tool_result",
            structured: {
              kind: "bash",
              command: "npm test",
              task_id: "shell-123",
              task_status: "completed",
              stdout: "tests passed",
              stderr: "warn",
              exit_code: 0,
              stdout_lines: 1,
              stderr_lines: 1,
              timestamp: "2026-06-01T00:00:02Z",
			              error: {
			                category: "command_failure",
			                message: "npm ERR! test failed",
			                user_reason: "stopped by user",
			              },
            },
          }],
        }],
      },
    });

    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("tests passed");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("command: npm test");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("task: shell-123");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("task status: completed");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("stdout lines: 1");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("stderr lines: 1");
	    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("timestamp: 2026-06-01T00:00:02Z");
	    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("error category: command_failure");
	    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("error: npm ERR! test failed");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("user reason: stopped by user");
  });

  it("opens structured session logs from crew, rigged, and pooled panels", async () => {
    document.body.innerHTML = `
      <div id="crew-loading">Loading crew...</div>
      <table id="crew-table" style="display:none"><tbody id="crew-tbody"></tbody></table>
      <div id="crew-empty" style="display:none"><p>No crew configured</p></div>
      <div id="rigged-body"></div>
      <div id="pooled-body"></div>
      <span id="crew-count"></span>
      <span id="rigged-count"></span>
      <span id="pooled-count"></span>
      <div id="agent-log-drawer" style="display:none">
        <span id="log-drawer-agent-name"></span>
        <span id="log-drawer-count"></span>
        <button id="log-drawer-older-btn" style="display:none">Load older</button>
        <button id="log-drawer-close-btn">Close</button>
        <div id="log-drawer-body">
          <div id="log-drawer-messages">
            <div id="log-drawer-loading">Loading logs...</div>
          </div>
        </div>
      </div>
    `;
    const requestedSessions: string[] = [];
    vi.spyOn(api, "GET").mockImplementation(async (path: string, options?: unknown) => {
      if (path === "/v0/city/{cityName}/sessions") {
        return {
          data: {
            items: [
              {
                active_bead: "",
                agent_kind: "crew",
                attached: true,
                id: "s-claude",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "",
                rig: "rig-a/crew",
                running: true,
                template: "claude-crew",
              },
              {
                active_bead: "ga-1",
                agent_kind: "pool",
                attached: false,
                id: "s-codex",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "",
                pool: "builders",
                rig: "rig-a",
                running: true,
                template: "codex-rigged",
              },
              {
                active_bead: "",
                agent_kind: "pool",
                attached: false,
                id: "s-gemini",
                last_active: "2026-04-18T20:00:00Z",
                last_output: "ready",
                pool: "floaters",
                running: true,
                template: "gemini-pooled",
              },
            ],
          },
        } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/pending") {
        return { data: { pending: false } } as never;
      }
      if (path === "/v0/city/{cityName}/bead/{id}") {
        return { data: { id: "ga-1", title: "Patch dashboard" } } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/transcript") {
        const sessionID = (options as { params?: { path?: { id?: string } } } | undefined)?.params?.path?.id ?? "";
        requestedSessions.push(sessionID);
        return {
          data: {
            format: "structured",
            provider: sessionID.replace("s-", ""),
            structured_messages: [{
              id: `m-${sessionID}`,
              model: `${sessionID}-model`,
              provider: sessionID.replace("s-", ""),
              role: "assistant",
              status: "final",
              stop_reason: "stop",
              timestamp: "2026-04-18T20:00:00Z",
              usage: {
                input_tokens: 100,
                output_tokens: 20,
                reasoning_tokens: 7,
                cache_read_tokens: 5,
                cache_creation_tokens: 3,
                context_used_tokens: 108,
                context_window_tokens: 200000,
                context_percent: 1,
              },
              blocks: [
                { type: "text", text: `Transcript for ${sessionID}` },
                {
                  type: "tool_result",
                  structured: {
                    kind: "bash",
                    stdout: `${sessionID} stdout`,
                    stderr: `${sessionID} stderr`,
                    exit_code: 0,
                  },
                },
              ],
            }],
            pagination: {
              has_older_messages: false,
              returned_message_count: 1,
              total_compactions: 0,
              total_message_count: 1,
            },
          },
        } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    installCrewInteractions();
    await renderCrew();

    for (const name of ["claude-crew", "codex-rigged", "gemini-pooled"]) {
      const button = Array.from(document.querySelectorAll<HTMLButtonElement>(".agent-log-link"))
        .find((candidate) => candidate.textContent === name);
      expect(button, `${name} log button`).toBeTruthy();
      button?.click();
      await waitFor(() => {
        expect(document.getElementById("log-drawer-messages")?.textContent).toContain(`Transcript for ${button?.dataset.sessionId}`);
      });
    }

    expect(requestedSessions).toEqual(["s-claude", "s-codex", "s-gemini"]);
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("gemini stderr");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("gemini-model");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("tokens in 100 out 20 reason 7 cache 5 write 3 108/200000 1%");
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("stop");
  });

  it("renders structured history metadata, interactions, subagent lineage, and stream lifecycle frames", async () => {
    document.body.innerHTML = `
      <div id="crew-loading">Loading crew...</div>
      <table id="crew-table" style="display:none"><tbody id="crew-tbody"></tbody></table>
      <div id="crew-empty" style="display:none"><p>No crew configured</p></div>
      <div id="rigged-body"></div>
      <div id="pooled-body"></div>
      <span id="crew-count"></span>
      <span id="rigged-count"></span>
      <span id="pooled-count"></span>
      <div id="agent-log-drawer" style="display:none">
        <span id="log-drawer-agent-name"></span>
        <span id="log-drawer-count"></span>
        <span id="log-drawer-status"></span>
        <button id="log-drawer-older-btn" style="display:none">Load older</button>
        <button id="log-drawer-close-btn">Close</button>
        <div id="log-drawer-body">
          <div id="log-drawer-messages">
            <div id="log-drawer-loading">Loading logs...</div>
          </div>
        </div>
      </div>
    `;
    let streamHandler: ((msg: unknown) => void) | undefined;
    vi.mocked(connectAgentOutput).mockImplementation((_city, _sessionID, onEvent) => {
      streamHandler = onEvent as (msg: unknown) => void;
      return { close: vi.fn() };
    });
    vi.spyOn(api, "GET").mockImplementation(async (path: string) => {
      if (path === "/v0/city/{cityName}/sessions") {
        return {
          data: {
            items: [{
              active_bead: "",
              agent_kind: "crew",
              attached: true,
              id: "s-open-code",
              last_active: "2026-04-18T20:00:00Z",
              last_output: "",
              rig: "rig-a/crew",
              running: true,
              template: "opencode-worker",
            }],
          },
        } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/pending") {
        return { data: { pending: false } } as never;
      }
      if (path === "/v0/city/{cityName}/session/{id}/transcript") {
        return {
          data: {
            format: "structured",
            history: {
              continuity: {
                has_branches: true,
                note: "compacted transcript",
                status: "compacted",
              },
              cursor: { after_entry_id: "entry-42" },
              diagnostics: [{ code: "partial_history", count: 2, message: "older entries compacted" }],
              generation: { id: "generation-1", observed_at: "2026-04-18T20:00:00Z" },
              provider_session_id: "provider-session-99",
              tail_state: {
                activity: "in-turn",
                degraded: true,
                degraded_reason: "reader recovering",
                last_entry_id: "entry-42",
                open_tool_call_ids: ["tool-open"],
                pending_interaction_ids: ["approval-1"],
              },
              transcript_stream_id: "stream-open-code-1",
            },
            provider: "opencode",
            schema_version: "session.structured.v1",
            structured_messages: [{
              blocks: [
                { text: "OpenCode delegated the edit.", type: "text" },
                {
                  interaction: {
                    action: "awaiting_user",
                    kind: "approval",
                    options: ["Approve", "Deny"],
                    prompt: "Allow Edit to modify src/app.ts?",
                    request_id: "approval-1",
                    state: "pending",
                  },
                  type: "interaction",
                },
              ],
              id: "m-open-code",
              is_subagent: true,
              parent_tool_call_id: "parent-tool",
              provider: "opencode",
              role: "assistant",
              status: "partial",
              timestamp: "2026-04-18T20:00:00Z",
            }],
          },
        } as never;
      }
      throw new Error(`unexpected GET ${path}`);
    });

    installCrewInteractions();
    await renderCrew();
    document.querySelector<HTMLButtonElement>(".agent-log-link")?.click();

    await waitFor(() => {
      expect(document.getElementById("log-drawer-messages")?.textContent).toContain("OpenCode delegated the edit.");
    });
    const text = document.getElementById("log-drawer-messages")?.textContent ?? "";
    expect(text).toContain("stream-open-code-1");
    expect(text).toContain("compacted");
    expect(text).toContain("reader recovering");
    expect(text).toContain("partial_history");
    expect(text).toContain("tool-open");
    expect(text).toContain("approval-1");
    expect(text).toContain("subagent");
    expect(text).toContain("parent-tool");
    expect(text).toContain("awaiting_user");
    expect(text).toContain("Approve");
    expect(text).toContain("Deny");

    streamHandler?.({ type: "activity", data: { activity: "idle" } });
    expect(document.getElementById("log-drawer-status")?.textContent).toContain("idle");

    streamHandler?.({
      type: "pending",
      data: {
        kind: "approval",
        options: ["Accept", "Reject"],
        prompt: "Approve streamed write?",
        request_id: "approval-stream",
      },
    });
    await waitFor(() => {
      expect(document.getElementById("log-drawer-messages")?.textContent).toContain("Approve streamed write?");
    });
    expect(document.getElementById("log-drawer-messages")?.textContent).toContain("approval-stream");
    expect(document.getElementById("log-drawer-status")?.textContent).toContain("pending");
  });
});

// Slow Blacksmith CI runs have shown the openLogDrawer + loadTranscript
// chain take ~1.3s while passing runs finish in ~100ms — same VM class,
// same code. The 1s budget here was missing those slow runs by a few
// hundred ms even though the chain ultimately completed (the
// `[crew] Transcript loaded` debug log fires *after* the assertion times
// out). Five seconds keeps the local cost negligible and absorbs the
// observed CI variance.
async function waitFor(assertion: () => void): Promise<void> {
  const started = Date.now();
  let lastError: unknown;
  while (Date.now() - started < 5000) {
    try {
      assertion();
      return;
    } catch (error) {
      lastError = error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  throw lastError;
}

function mockScrollIntoView(): ReturnType<typeof vi.fn> {
  const scrollIntoView = vi.fn();
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
    configurable: true,
    value: scrollIntoView,
  });
  return scrollIntoView;
}

function restoreScrollIntoView(): void {
  if (nativeScrollIntoView) {
    Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
      configurable: true,
      value: nativeScrollIntoView,
    });
    return;
  }
  delete (HTMLElement.prototype as { scrollIntoView?: Element["scrollIntoView"] }).scrollIntoView;
}
