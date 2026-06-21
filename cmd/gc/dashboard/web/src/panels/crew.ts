import type { SessionRecord } from "../api";
import { api, cityScope } from "../api";
import type {
  PendingInteraction,
  SessionActivityEvent,
  SessionStructuredBlock,
  SessionStructuredHistory,
  SessionStructuredMessage,
} from "../generated";
import { byId, clear, el } from "../util/dom";
import { calculateActivity, formatTimestamp, statusBadgeClass, truncate } from "../util/legacy";
import { connectAgentOutput, type AgentOutputMessage, type SSEHandle } from "../sse";
import { popPause, pushPause, showToast } from "../ui";
import { logDebug } from "../logger";

let logHandle: SSEHandle | null = null;
let logSessionID = "";
let logBeforeCursor = "";
let logCount = 0;

export async function renderCrew(): Promise<void> {
  const city = cityScope();
  if (!city) {
    resetCrewNoCity();
    return;
  }

  const crewLoading = byId("crew-loading");
  const crewTable = byId<HTMLTableElement>("crew-table");
  const crewEmpty = byId("crew-empty");
  const crewBody = byId("crew-tbody");
  const riggedBody = byId("rigged-body");
  const pooledBody = byId("pooled-body");
  if (!crewLoading || !crewTable || !crewEmpty || !crewBody || !riggedBody || !pooledBody) return;

  setCrewEmptyMessage("No crew configured");
  crewLoading.style.display = "block";
  crewTable.style.display = "none";
  crewEmpty.style.display = "none";
  clear(crewBody);

  const { data, error } = await api.GET("/v0/city/{cityName}/sessions", {
    params: { path: { cityName: city }, query: { peek: true } },
  });
  if (error || !data?.items) {
    crewLoading.textContent = "Failed to load crew";
    renderSimpleEmpty(riggedBody, "No rigged agents");
    renderSimpleEmpty(pooledBody, "No other sessions");
    return;
  }

  const sessions = data.items;
  // The Crew table is for persistent named workers — sessions whose backing
  // agent is classified server-side as "crew". Other agent kinds (pool,
  // role) belong on the Rigged/Pooled panels (or stay invisible until a
  // dedicated panel exists), so filter them out here.
  const crew = sessions.filter((session) => session.agent_kind === "crew");
  const pending = await Promise.all(
    crew.map(async (session) => {
      const res = await api.GET("/v0/city/{cityName}/session/{id}/pending", {
        params: { path: { cityName: city, id: session.id } },
      });
      return Boolean(res.data?.pending);
    }),
  );

  const beadTitles = new Map<string, string>();
  await Promise.all(
    sessions.map(async (session) => {
      if (!session.active_bead) return;
      if (beadTitles.has(session.active_bead)) return;
      const res = await api.GET("/v0/city/{cityName}/bead/{id}", {
        params: { path: { cityName: city, id: session.active_bead } },
      });
      beadTitles.set(session.active_bead, res.data?.id ? (res.data.title ?? res.data.id) : session.active_bead);
    }),
  );

  crew.forEach((session, index) => {
    const state = classifyCrewState(session, pending[index] ?? false);
    const beadText = session.active_bead ? truncate(beadTitles.get(session.active_bead) ?? session.active_bead, 24) : "—";
    const row = el("tr", {}, [
      el("td", {}, [session.template]),
      el("td", {}, [session.rig ?? "city"]),
      el("td", {}, [el("span", { class: `badge ${statusBadgeClass(state)}` }, [state])]),
      el("td", {}, [beadText]),
      el("td", { class: calculateActivity(session.last_active).colorClass ? `activity-${calculateActivity(session.last_active).colorClass}` : "" }, [
        el("span", { class: "activity-dot" }),
        ` ${calculateActivity(session.last_active).display}`,
      ]),
      el("td", {}, [
        el("span", { class: `badge ${session.attached ? "badge-green" : "badge-muted"}` }, [
          session.attached ? "Attached" : "Detached",
        ]),
      ]),
      el("td", {}, [
        attachButton(session.template),
        " ",
        logButton(session.id, session.template),
      ]),
    ]);
    crewBody.append(row);
  });

  byId("crew-count")!.textContent = String(crew.length);
  crewLoading.style.display = "none";
  if (crew.length > 0) {
    crewTable.style.display = "table";
  } else {
    setCrewEmptyMessage("No crew configured");
    crewEmpty.style.display = "block";
  }

  renderRiggedAgents(sessions, beadTitles);
  renderPooledAgents(sessions);
}

export function resetCrewNoCity(): void {
  const crewLoading = byId("crew-loading");
  const crewTable = byId<HTMLTableElement>("crew-table");
  const crewEmpty = byId("crew-empty");
  const crewBody = byId("crew-tbody");
  const riggedBody = byId("rigged-body");
  const pooledBody = byId("pooled-body");
  if (!crewLoading || !crewTable || !crewEmpty || !crewBody || !riggedBody || !pooledBody) return;

  closeLogDrawer();
  byId("crew-count")!.textContent = "0";
  byId("rigged-count")!.textContent = "0";
  byId("pooled-count")!.textContent = "0";
  crewLoading.style.display = "none";
  crewTable.style.display = "none";
  crewEmpty.style.display = "block";
  setCrewEmptyMessage("Select a city to view crew");
  clear(crewBody);
  renderSimpleEmpty(riggedBody, "Select a city to view rigged agents");
  renderSimpleEmpty(pooledBody, "Select a city to view pooled agents");
}

function setCrewEmptyMessage(message: string): void {
  byId("crew-empty")?.querySelector("p")?.replaceChildren(document.createTextNode(message));
}

function classifyCrewState(session: SessionRecord, hasPending: boolean): string {
  if (hasPending) return "questions";
  if (session.active_bead) return "spinning";
  if (!session.running) return "finished";
  return "idle";
}

function attachButton(template: string): HTMLElement {
  const btn = el("button", { class: "attach-btn", type: "button" }, ["📎 Attach"]);
  btn.addEventListener("click", async () => {
    const command = `gc agent attach ${template}`;
    try {
      await navigator.clipboard.writeText(command);
      showToast("success", "Attach command copied", command);
    } catch {
      showToast("error", "Copy failed", command);
    }
  });
  return btn;
}

function logButton(sessionID: string, label: string): HTMLElement {
  const btn = el("button", { class: "agent-log-link", type: "button", "data-session-id": sessionID }, [label]);
  btn.addEventListener("click", () => {
    void openSessionLogDrawer(sessionID, label);
  });
  return btn;
}

// renderRiggedAgents lists sessions attached to a specific rig. Grouping
// is purely by the API's `rig` + `pool` fields — no role names hardcoded.
function renderRiggedAgents(sessions: SessionRecord[], beadTitles: Map<string, string>): void {
  const body = byId("rigged-body");
  const count = byId("rigged-count");
  if (!body || !count) return;

  const rows = sessions.filter((session) => session.rig && session.pool);
  count.textContent = String(rows.length);
  if (rows.length === 0) {
    renderSimpleEmpty(body, "No rigged agents");
    return;
  }

  const tbody = el("tbody");
  rows.forEach((session) => {
    const activity = calculateActivity(session.last_active);
    const workStatus = !session.active_bead ? "Idle" : activity.colorClass === "red" ? "Stuck" : activity.colorClass === "yellow" ? "Stale" : "Working";
    tbody.append(el("tr", { class: `rigged-${workStatus.toLowerCase()}` }, [
      el("td", {}, [logButton(session.id, session.template)]),
      el("td", {}, [el("span", { class: "badge badge-muted" }, [session.pool ?? "pool"])]),
      el("td", {}, [session.rig ?? "city"]),
      el("td", { class: "rigged-issue" }, [
        session.active_bead
          ? `${session.active_bead} ${beadTitles.get(session.active_bead) ?? ""}`.trim()
          : "—",
      ]),
      el("td", {}, [el("span", { class: `badge ${statusBadgeClass(workStatus)}` }, [workStatus])]),
      el("td", { class: `activity-${activity.colorClass}` }, [el("span", { class: "activity-dot" }), ` ${activity.display}`]),
    ]));
  });

  clear(body);
  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Agent"]),
      el("th", {}, ["Pool"]),
      el("th", {}, ["Rig"]),
      el("th", {}, ["Working On"]),
      el("th", {}, ["Status"]),
      el("th", {}, ["Activity"]),
    ])]),
    tbody,
  ]));
}

// renderPooledAgents lists non-crew sessions that are not already shown in
// the rig-bound pool panel. Grouping is by API fields only — no role names
// hardcoded.
function renderPooledAgents(sessions: SessionRecord[]): void {
  const body = byId("pooled-body");
  const count = byId("pooled-count");
  if (!body || !count) return;
  const rows = sessions.filter((session) => session.agent_kind !== "crew" && !(session.rig && session.pool));
  count.textContent = String(rows.length);
  if (rows.length === 0) {
    renderSimpleEmpty(body, "No other sessions");
    return;
  }

  const tbody = el("tbody");
  rows.forEach((session) => {
    const kind = session.pool || session.agent_kind || "session";
    tbody.append(el("tr", {}, [
      el("td", {}, [logButton(session.id, session.template)]),
      el("td", {}, [el("span", { class: `badge ${session.active_bead ? "badge-yellow" : "badge-green"}` }, [session.active_bead ? "Working" : "Idle"])]),
      el("td", {}, [el("span", { class: "badge badge-muted" }, [kind])]),
      el("td", { class: "status-hint" }, [truncate(session.last_output, 80) || "—"]),
      el("td", {}, [formatTimestamp(session.last_active)]),
    ]));
  });

  clear(body);
  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Agent"]),
      el("th", {}, ["State"]),
      el("th", {}, ["Kind"]),
      el("th", {}, ["Work"]),
      el("th", {}, ["Activity"]),
    ])]),
    tbody,
  ]));
}

function renderSimpleEmpty(container: HTMLElement, message: string): void {
  clear(container);
  container.append(el("div", { class: "empty-state" }, [el("p", {}, [message])]));
}

export function installCrewInteractions(): void {
  byId("log-drawer-close-btn")?.addEventListener("click", () => closeLogDrawer());
  byId("log-drawer-older-btn")?.addEventListener("click", () => {
    logDebug("crew", "Load older transcript clicked", {
      hasCursor: logBeforeCursor !== "",
      sessionID: logSessionID,
    });
    if (!logSessionID || !logBeforeCursor) return;
    void loadTranscript(logSessionID, true);
  });
}

export async function openSessionLogDrawer(sessionID: string, label: string): Promise<void> {
  const drawer = byId("agent-log-drawer");
  const nameEl = byId("log-drawer-agent-name");
  const messagesEl = byId("log-drawer-messages");
  const loadingEl = byId("log-drawer-loading");
  if (!drawer || !nameEl || !messagesEl || !loadingEl) return;

  if (logSessionID === sessionID && drawer.style.display !== "none") {
    closeLogDrawer();
    return;
  }

  closeLogDrawer();
  logSessionID = sessionID;
  logBeforeCursor = "";
  logCount = 0;

  nameEl.textContent = label;
  clear(messagesEl);
  messagesEl.append(loadingEl);
  loadingEl.style.display = "block";
  drawer.style.display = "block";
  pushPause();

  await loadTranscript(sessionID, false);
  const city = cityScope();
  if (!city) return;
  logHandle = connectAgentOutput(city, sessionID, (msg) => appendStreamEvent(msg));
}

function closeLogDrawer(): void {
  logHandle?.close();
  logHandle = null;
  logSessionID = "";
  logBeforeCursor = "";
  const drawer = byId("agent-log-drawer");
  if (drawer && drawer.style.display !== "none") {
    drawer.style.display = "none";
    popPause();
  }
}

// closeLogDrawerExternal is called by main.ts when the dashboard leaves
// city scope, so the transcript stream + its `pushPause()` token get
// torn down along with every other city-scoped panel. Without this, a
// drawer open at scope-change time would keep its session stream alive
// and leave `pauseCount > 0` forever (blocking all refreshes).
export function closeLogDrawerExternal(): void {
  closeLogDrawer();
}

async function loadTranscript(sessionID: string, prepend: boolean): Promise<void> {
  const city = cityScope();
  const messagesEl = byId("log-drawer-messages");
  const loadingEl = byId("log-drawer-loading");
  const olderBtn = byId<HTMLButtonElement>("log-drawer-older-btn");
  const countEl = byId("log-drawer-count");
  if (!city || !messagesEl || !loadingEl || !olderBtn || !countEl) return;

  loadingEl.style.display = "block";
  const res = await api.GET("/v0/city/{cityName}/session/{id}/transcript", {
    params: {
      path: { cityName: city, id: sessionID },
      query: {
        tail: String(prepend ? 50 : 25),
        before: prepend ? logBeforeCursor : undefined,
        format: "structured",
      },
    },
  });
  loadingEl.style.display = "none";
  if (res.error || !res.data) {
    showToast("error", "Transcript failed", res.error?.detail ?? "Could not load transcript");
    return;
  }

  const fragment = document.createDocumentFragment();
  const history = structuredHistoryFromEnvelope(res.data);
  if (!prepend && history) {
    fragment.append(renderStructuredHistory(history));
  }
  const structuredMessages = structuredMessagesFromEnvelope(res.data);
  if (structuredMessages.length > 0) {
    for (const message of structuredMessages) {
      fragment.append(renderStructuredMessage(message));
      logCount += 1;
    }
  } else {
    for (const turn of res.data.turns ?? []) {
      fragment.append(renderTurn(turn.role, turn.text, turn.timestamp));
      logCount += 1;
    }
  }
  if (prepend) {
    messagesEl.prepend(fragment);
  } else {
    clear(messagesEl);
    messagesEl.append(fragment);
  }
  messagesEl.append(loadingEl);
  loadingEl.style.display = "none";
  countEl.textContent = String(logCount);

  logBeforeCursor = res.data.pagination?.truncated_before_message ?? "";
  olderBtn.style.display = res.data.pagination?.has_older_messages && logBeforeCursor ? "inline-flex" : "none";
  logDebug("crew", "Transcript loaded", {
    hasOlderMessages: res.data.pagination?.has_older_messages ?? false,
    nextBeforeCursor: logBeforeCursor,
    prepend,
    sessionID,
    turnCount: res.data.turns?.length ?? 0,
  });
}

function appendStreamEvent(msg: AgentOutputMessage): void {
  const messagesEl = byId("log-drawer-messages");
  if (!messagesEl) return;
  if (msg.type === "activity") {
    const activity = activityFromFrame(msg.data);
    if (activity) setLogDrawerStatus(activity);
    return;
  }
  if (msg.type === "pending") {
    const pending = pendingInteractionFromFrame(msg.data);
    if (!pending) return;
    setLogDrawerStatus(`pending ${pending.kind}`);
    messagesEl.append(renderPendingInteraction(pending));
    logCount += 1;
    byId("log-drawer-count")!.textContent = String(logCount);
    const body = byId("log-drawer-body");
    if (body) body.scrollTop = body.scrollHeight;
    return;
  }
  const structuredMessages = structuredMessagesFromEnvelope(msg.data);
  if (msg.type === "structured" && structuredMessages.length > 0) {
    const history = structuredHistoryFromEnvelope(msg.data);
    if (history) messagesEl.append(renderStructuredHistory(history));
    for (const message of structuredMessages) {
      messagesEl.append(renderStructuredMessage(message));
      logCount += 1;
    }
    byId("log-drawer-count")!.textContent = String(logCount);
    const body = byId("log-drawer-body");
    if (body) body.scrollTop = body.scrollHeight;
    return;
  }
  const payload = msg.data as { data?: { message?: { role?: string; text?: string; timestamp?: string } }; event?: string } | null;
  if (msg.type !== "message" || !payload?.data?.message) return;
  messagesEl.append(renderTurn(payload.data.message.role ?? "agent", payload.data.message.text ?? "", payload.data.message.timestamp));
  logCount += 1;
  byId("log-drawer-count")!.textContent = String(logCount);
  const body = byId("log-drawer-body");
  if (body) body.scrollTop = body.scrollHeight;
}

function structuredMessagesFromEnvelope(value: unknown): SessionStructuredMessage[] {
  if (!isRecord(value)) return [];
  const structured = value.structured_messages;
  if (Array.isArray(structured)) return structured.filter(isStructuredMessage);

  // Accept the final spec shape too: a structured envelope may use
  // `messages` for normalized messages. Raw frames also use `messages`,
  // so only treat objects with block arrays as structured messages.
  const messages = value.messages;
  if (Array.isArray(messages)) return messages.filter(isStructuredMessage);
  return [];
}

function structuredHistoryFromEnvelope(value: unknown): SessionStructuredHistory | null {
  if (!isRecord(value)) return null;
  return isSessionStructuredHistory(value.history) ? value.history : null;
}

function renderTurn(role: string, text: string, timestamp: string | undefined): HTMLElement {
  return el("div", { class: "log-msg" }, [
    el("div", { class: "log-msg-header" }, [
      el("span", { class: `log-msg-type log-msg-type-${roleClass(role)}` }, [role]),
      el("span", { class: "log-msg-time" }, [formatTimestamp(timestamp)]),
    ]),
    el("div", { class: "log-msg-body" }, [text]),
  ]);
}

function renderStructuredMessage(message: SessionStructuredMessage): HTMLElement {
  const role = message.role || "agent";
  const body = el("div", { class: "log-msg-body log-msg-body-structured" });
  for (const block of message.blocks ?? []) {
    const rendered = renderStructuredBlock(block);
    if (rendered) body.append(rendered);
  }
  if (!body.hasChildNodes()) {
    body.append(document.createTextNode(""));
  }
  return el("div", { class: "log-msg log-msg-structured" }, [
    el("div", { class: "log-msg-header" }, [
      el("span", { class: `log-msg-type log-msg-type-${roleClass(role)}` }, [role]),
      message.provider ? el("span", { class: "log-msg-provider" }, [message.provider]) : null,
      el("span", { class: "log-msg-time" }, [formatTimestamp(message.timestamp)]),
      message.model ? el("span", { class: "log-msg-model" }, [message.model]) : null,
      message.status ? el("span", { class: "log-msg-status" }, [message.status]) : null,
      message.stop_reason ? el("span", { class: "log-msg-stop" }, [message.stop_reason]) : null,
      message.is_subagent ? el("span", { class: "log-msg-status" }, ["subagent"]) : null,
      message.parent_tool_call_id ? el("span", { class: "log-msg-status" }, [`parent ${message.parent_tool_call_id}`]) : null,
    ]),
    body,
  ]);
}

function renderStructuredHistory(history: SessionStructuredHistory): HTMLElement {
  const rows: string[] = [];
  appendField(rows, "stream", history.transcript_stream_id);
  appendField(rows, "provider session", history.provider_session_id);
  appendField(rows, "conversation", history.logical_conversation_id);
  appendField(rows, "gc session", history.gc_session_id);

  appendField(rows, "generation", history.generation.id);
  appendField(rows, "observed", history.generation.observed_at);

  appendField(rows, "cursor", history.cursor.after_entry_id);

  appendField(rows, "continuity", history.continuity.status);
  appendNumber(rows, "compactions", history.continuity.compaction_count);
  if (history.continuity.has_branches === true) rows.push("branches: yes");
  appendField(rows, "note", history.continuity.note);

  appendField(rows, "activity", history.tail_state.activity);
  appendField(rows, "last entry", history.tail_state.last_entry_id);
  appendStringList(rows, "open tools", history.tail_state.open_tool_call_ids);
  appendStringList(rows, "pending", history.tail_state.pending_interaction_ids);
  if (history.tail_state.degraded === true) rows.push("degraded: yes");
  appendField(rows, "degraded reason", history.tail_state.degraded_reason);

  for (const diagnostic of history.diagnostics ?? []) {
    const parts: string[] = [];
    appendField(parts, "code", diagnostic.code);
    appendNumber(parts, "count", diagnostic.count);
    appendField(parts, "message", diagnostic.message);
    if (parts.length > 0) rows.push(`diagnostic: ${parts.join(", ")}`);
  }

  return el("div", { class: "log-msg-history" }, [
    el("div", { class: "log-msg-tool-title" }, [
      el("span", { class: "log-msg-tool-kind" }, ["history"]),
      " structured session",
    ]),
    el("pre", { class: "log-msg-tool-pre" }, [rows.length > 0 ? rows.join("\n") : "structured history"]),
  ]);
}

function renderStructuredBlock(block: SessionStructuredBlock): HTMLElement | null {
  switch (block.type) {
    case "text":
      return el("div", { class: "log-msg-text-block" }, [block.text ?? ""]);
    case "thinking":
      return el("div", { class: "log-msg-thinking-block" }, [block.thinking ? "[thinking] " + block.thinking : "[thinking]"]);
    case "tool_use":
      return el("div", { class: "log-msg-tool log-msg-tool-use" }, [
        el("div", { class: "log-msg-tool-title" }, [
          el("span", { class: "log-msg-tool-kind" }, ["tool"]),
          " ",
          block.name ? `${block.name}` : "tool",
        ]),
        renderToolInput(block),
      ]);
    case "tool_result":
      return el("div", { class: `log-msg-tool-result${block.is_error ? " is-error" : ""}` }, [
        ...renderToolResult(block),
      ]);
    case "interaction":
      return el("div", { class: "log-msg-tool" }, [formatInteraction(block)]);
    default:
      return el("div", { class: "log-msg-tool-result" }, [formatInlineValue(block)]);
  }
}

function renderToolInput(block: SessionStructuredBlock): HTMLElement {
  const input = recordOf(block.input);
  if (!input) {
    return el("pre", { class: "log-msg-tool-pre" }, [block.input !== undefined ? formatInlineValue(block.input) : ""]);
  }
  const rows: string[] = [];
  const patch = stringValue(input.patch);
  appendField(rows, "kind", input.kind);
  appendField(rows, "file", input.file_path);
  appendField(rows, "command", input.command);
  appendField(rows, "code", input.code);
  appendField(rows, "query", input.query);
  appendField(rows, "pattern", input.pattern);
  appendField(rows, "text", input.text);
  if (Array.isArray(input.arguments) && input.arguments.length > 0) {
    rows.push(...input.arguments.map((arg) => formatArgument(arg)));
  }
  if (patch === "") {
    if (rows.length === 0) rows.push(formatInlineValue(input));
    return el("pre", { class: "log-msg-tool-pre" }, [rows.join("\n")]);
  }

  return el("div", { class: "log-msg-tool-input" }, [
    rows.length > 0 ? el("pre", { class: "log-msg-tool-pre" }, [rows.join("\n")]) : null,
    renderDiffPre(patch),
  ]);
}

function renderToolResult(block: SessionStructuredBlock): HTMLElement[] {
  const structured = recordOf(block.structured);
  if (structured) {
    const kind = typeof structured.kind === "string" ? structured.kind : "result";
    const lines: string[] = [];
    appendField(lines, "kind", kind);
    appendField(lines, "file", structured.file_path);
    if (kind === "bash") {
      appendField(lines, "stdout", structured.stdout);
      appendField(lines, "stderr", structured.stderr);
      appendExit(lines, structured.exit_code);
      appendFlags(lines, structured);
      return toolResultNodes(kind, lines);
    }
    if (kind === "python") {
      appendField(lines, "code", structured.code);
      appendField(lines, "stdout", structured.stdout);
      appendField(lines, "stderr", structured.stderr);
      appendExit(lines, structured.exit_code);
      appendFlags(lines, structured);
      return toolResultNodes(kind, lines);
    }
    if (kind === "edit") {
      const patch = stringValue(structured.patch);
      appendField(lines, "content", structured.content);
      return toolResultNodes(kind, lines, patch);
    }
    if (kind === "read") {
      appendField(lines, "content", structured.content);
      appendNumber(lines, "start", structured.start_line);
      appendNumber(lines, "lines", structured.num_lines);
      appendNumber(lines, "total", structured.total_lines);
      appendFlags(lines, structured);
      return toolResultNodes(kind, lines);
    }
    if (kind === "grep" || kind === "search") {
      if (Array.isArray(structured.filenames) && structured.filenames.length > 0) {
        appendField(lines, "files", structured.filenames.join(", "));
      }
      appendField(lines, "mode", structured.mode);
      appendField(lines, "content", structured.content);
      appendField(lines, "text", structured.text);
      appendNumber(lines, "files", structured.num_files);
      appendNumber(lines, "lines", structured.num_lines);
      appendFlags(lines, structured);
      return toolResultNodes(kind, lines);
    }
    appendField(lines, "content", structured.content);
    appendField(lines, "text", structured.text);
    appendField(lines, "stdout", structured.stdout);
    appendField(lines, "stderr", structured.stderr);
    appendExit(lines, structured.exit_code);
    if (lines.length === 1) lines.push(formatInlineValue(structured));
    return toolResultNodes(kind, lines);
  }
  if (typeof block.content === "string") return toolResultNodes("result", [block.content]);
  if (block.content !== undefined) return toolResultNodes("result", [formatInlineValue(block.content)]);
  return toolResultNodes("result", [""]);
}

function toolResultNodes(kind: string, lines: string[], diffText = ""): HTMLElement[] {
  const nodes: HTMLElement[] = [
    el("div", { class: "log-msg-tool-title" }, [
      el("span", { class: "log-msg-tool-kind" }, [kind]),
      " result",
    ]),
  ];
  const body = lines.filter(Boolean).join("\n");
  if (body !== "") nodes.push(el("pre", { class: "log-msg-tool-pre" }, [body]));
  if (diffText !== "") nodes.push(renderDiffPre(diffText));
  if (body === "" && diffText === "") nodes.push(el("pre", { class: "log-msg-tool-pre" }, [""]));
  return nodes;
}

function renderDiffPre(diffText: string): HTMLElement {
  const lines = diffText.replace(/\r\n/g, "\n").split("\n");
  const children: Array<HTMLElement | string> = [];
  lines.forEach((line, index) => {
    children.push(el("span", { class: diffLineClass(line) }, [line]));
    if (index < lines.length - 1) children.push("\n");
  });
  return el("pre", { class: "log-msg-tool-pre log-msg-diff" }, children);
}

function diffLineClass(line: string): string {
  if (line.startsWith("@@")) return "log-msg-diff-line log-msg-diff-hunk";
  if (line.startsWith("diff --git") || line.startsWith("index ") || line.startsWith("---") || line.startsWith("+++")) {
    return "log-msg-diff-line log-msg-diff-file";
  }
  if (line.startsWith("+")) return "log-msg-diff-line log-msg-diff-add";
  if (line.startsWith("-")) return "log-msg-diff-line log-msg-diff-del";
  return "log-msg-diff-line";
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function appendField(rows: string[], label: string, value: unknown): void {
  if (typeof value !== "string" || value === "") return;
  rows.push(`${label}: ${value}`);
}

function appendNumber(rows: string[], label: string, value: unknown): void {
  if (typeof value !== "number") return;
  rows.push(`${label}: ${String(value)}`);
}

function appendExit(rows: string[], value: unknown): void {
  if (typeof value !== "number") return;
  rows.push(`exit ${String(value)}`);
}

function appendFlags(rows: string[], structured: Record<string, unknown>): void {
  if (structured.truncated === true) rows.push("truncated");
  if (structured.interrupted === true) rows.push("interrupted");
}

function appendStringList(rows: string[], label: string, value: string[] | null | undefined): void {
  if (!value || value.length === 0) return;
  const parts = value.filter((item) => item !== "");
  if (parts.length === 0) return;
  rows.push(`${label}: ${parts.join(", ")}`);
}

function formatArgument(value: unknown): string {
  const argument = recordOf(value);
  if (!argument) return formatInlineValue(value);
  const name = typeof argument.name === "string" ? argument.name : "argument";
  const argValue = typeof argument.value === "string" ? argument.value : formatInlineValue(argument.value);
  return `${name}: ${argValue}`;
}

function formatInteraction(block: SessionStructuredBlock): string {
  const interaction = block.interaction;
  const kind = interaction?.kind ?? "interaction";
  const state = interaction?.state ?? "";
  const prompt = interaction?.prompt ?? "";
  const requestID = interaction?.request_id ?? "";
  const action = interaction?.action ?? "";
  const options = interaction?.options?.join(", ") ?? "";
  return [kind, state, requestID, action, prompt, options].filter(Boolean).join(" ");
}

function renderPendingInteraction(pending: PendingInteraction): HTMLElement {
  const rows: string[] = [];
  appendField(rows, "kind", pending.kind);
  appendField(rows, "request", pending.request_id);
  appendField(rows, "prompt", pending.prompt);
  appendStringList(rows, "options", pending.options);
  return el("div", { class: "log-msg log-msg-structured log-msg-pending" }, [
    el("div", { class: "log-msg-header" }, [
      el("span", { class: "log-msg-type log-msg-type-system" }, ["pending"]),
    ]),
    el("div", { class: "log-msg-body log-msg-body-structured" }, [
      el("div", { class: "log-msg-tool" }, [
        el("div", { class: "log-msg-tool-title" }, [
          el("span", { class: "log-msg-tool-kind" }, ["interaction"]),
          " pending",
        ]),
        el("pre", { class: "log-msg-tool-pre" }, [rows.join("\n")]),
      ]),
    ]),
  ]);
}

function activityFromFrame(value: unknown): string {
  return isSessionActivityEvent(value) ? value.activity : "";
}

function pendingInteractionFromFrame(value: unknown): PendingInteraction | null {
  return isPendingInteraction(value) ? value : null;
}

function setLogDrawerStatus(status: string): void {
  const statusEl = byId("log-drawer-status");
  if (!statusEl) return;
  statusEl.replaceChildren(document.createTextNode(status));
}

function formatInlineValue(value: unknown): string {
  if (value == null) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function recordOf(value: unknown): Record<string, unknown> | null {
  return isRecord(value) ? value : null;
}

function isSessionStructuredHistory(value: unknown): value is SessionStructuredHistory {
  if (!isRecord(value)) return false;
  if (typeof value.transcript_stream_id !== "string") return false;
  if (!isRecord(value.generation) || typeof value.generation.id !== "string") return false;
  if (!isRecord(value.cursor)) return false;
  if (!isRecord(value.continuity) || typeof value.continuity.status !== "string") return false;
  if (!isRecord(value.tail_state) || typeof value.tail_state.activity !== "string") return false;
  return true;
}

function isSessionActivityEvent(value: unknown): value is SessionActivityEvent {
  return isRecord(value) && typeof value.activity === "string";
}

function isPendingInteraction(value: unknown): value is PendingInteraction {
  return isRecord(value) && typeof value.kind === "string" && typeof value.request_id === "string";
}

function isStructuredMessage(value: unknown): value is SessionStructuredMessage {
  return isRecord(value) && Array.isArray(value.blocks);
}

function roleClass(role: string): string {
  switch ((role ?? "").toLowerCase()) {
    case "assistant":
    case "agent":
      return "assistant";
    case "system":
      return "system";
    case "result":
      return "result";
    default:
      return "user";
  }
}
