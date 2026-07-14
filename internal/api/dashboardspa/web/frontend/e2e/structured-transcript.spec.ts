import { expect, test, type Page } from '@playwright/test';
import { appendFile, readFile, rename, writeFile } from 'node:fs/promises';

const CITY_NAME = requiredEnv('DASHPORT_CITY_NAME');
const SESSION_ID = requiredEnv('DASHPORT_SESSION_ID');
const TRANSCRIPT_PATH = requiredEnv('DASHPORT_TRANSCRIPT_PATH');

const INITIAL_PROMPT = 'Inspect transcript enrichment';
const INITIAL_ANSWER = 'Initial structured answer';
const APPENDED_ID = 'transcript-user-2';
const APPENDED_PROMPT = 'Appended structured prompt';
const REPLACEMENT_PROMPT = 'Authoritative replacement prompt';
const REPLACEMENT_ANSWER = 'Authoritative replacement answer';
const PENDING_REQUEST_ID = 'approval-browser';

interface StructuredEnvelope {
  operation?: string;
  reset_reason?: string;
  history?: { cursor?: { resume_token?: string } };
  structured_messages?: Array<{ id?: string }>;
}

interface ObservedSSEEvent {
  data: unknown;
  lastEventId: string;
  type: string;
  url: string;
}

interface ClaudeTranscriptEntry {
  uuid: string;
  parentUuid?: string;
  type: string;
  timestamp: string;
  sessionId?: string;
  cwd?: string;
  message: {
    role: string;
    content: string;
  };
}

test('structured transcript converges through snapshot, inclusive upsert, and reset', async ({
  page,
}) => {
  const consoleProblems: string[] = [];
  const pageErrors: string[] = [];
  const failedRequests: string[] = [];
  const badResponses: string[] = [];
  const clientErrorPosts: string[] = [];
  const streamRequests: string[] = [];
  const transcriptPath = `/v0/city/${CITY_NAME}/session/${SESSION_ID}/transcript`;
  const streamPath = `/v0/city/${CITY_NAME}/session/${SESSION_ID}/stream`;

  await installSSEObserver(page);
  page.on('console', (message) => {
    if (message.type() === 'error' || message.type() === 'warning') {
      consoleProblems.push(`${message.type()}: ${message.text()}`);
    }
  });
  page.on('pageerror', (error) => pageErrors.push(error.message));
  page.on('requestfailed', (request) => {
    failedRequests.push(
      `${request.method()} ${request.url()}: ${request.failure()?.errorText ?? 'failed'}`,
    );
  });
  page.on('response', (response) => {
    if (response.status() >= 400) {
      badResponses.push(`${response.status()} ${response.request().method()} ${response.url()}`);
    }
  });
  page.on('request', (request) => {
    const url = new URL(request.url());
    if (request.method() === 'POST' && url.pathname === '/api/client-errors') {
      clientErrorPosts.push(request.url());
    }
    if (url.pathname === streamPath) streamRequests.push(request.url());
  });

  const snapshotResponsePromise = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return url.pathname === transcriptPath && url.searchParams.get('format') === 'structured';
  });
  await page.goto(`/city/${CITY_NAME}/agents/${encodeURIComponent(SESSION_ID)}`);
  const snapshotResponse = await snapshotResponsePromise;
  expect(snapshotResponse.ok()).toBe(true);
  const snapshot = (await snapshotResponse.json()) as StructuredEnvelope;
  const resumeToken = snapshot.history?.cursor?.resume_token;
  expect(resumeToken, 'REST snapshot must carry the opaque structured resume token').toBeTruthy();
  expect(snapshot.operation).toBe('snapshot');

  await expect(page.getByText(renderedPrompt(INITIAL_PROMPT), { exact: true })).toBeVisible();
  await expect(page.getByText(INITIAL_ANSWER, { exact: true })).toBeVisible();
  await expect(page.getByText('Dashboard view failed.')).toHaveCount(0);

  await expect
    .poll(() => sessionEvents(page, streamPath).then((events) => events.length))
    .toBeGreaterThan(0);
  const initialEvents = await sessionEvents(page, streamPath);
  expect(initialEvents[0]?.type, 'exact REST cursor suppresses the initial structured replay').toBe(
    'activity',
  );
  expect(initialEvents.filter((event) => event.type === 'structured')).toEqual([]);
  expect(streamRequests).toHaveLength(1);
  expect(new URL(requiredFirst(streamRequests)).searchParams.get('after_cursor')).toBe(resumeToken);
  await expect(page.getByText(renderedPrompt(INITIAL_PROMPT), { exact: true })).toHaveCount(1);
  await expect(page.getByText(INITIAL_ANSWER, { exact: true })).toHaveCount(1);

  await expect
    .poll(() => hasPendingRequest(page, streamPath, PENDING_REQUEST_ID), { timeout: 10_000 })
    .toBe(true);
  await expect(page.getByText(`request: ${PENDING_REQUEST_ID}`)).toBeVisible();
  await expect(page.getByText('prompt: Approve browser transcript update?')).toBeVisible();

  const initialTranscript = await readTranscript();
  const appended = appendUserEntry(initialTranscript);
  await appendFile(TRANSCRIPT_PATH, `${JSON.stringify(appended)}\n`, 'utf8');

  await expect.poll(() => hasStructuredOperation(page, streamPath, 'upsert')).toBe(true);
  const upsert = requiredOperation(await sessionEvents(page, streamPath), 'upsert');
  expect(upsert.structured_messages?.map((message) => message.id)).toEqual([
    'transcript-assistant-1',
    APPENDED_ID,
  ]);
  await expect(page.getByText(renderedPrompt(APPENDED_PROMPT), { exact: true })).toBeVisible();
  await expect(page.getByText(INITIAL_ANSWER, { exact: true })).toHaveCount(1);
  await expect(page.getByText(renderedPrompt(APPENDED_PROMPT), { exact: true })).toHaveCount(1);

  const rewritten = (await readTranscript()).map((entry) => structuredClone(entry));
  replaceMessageText(rewritten, 'transcript-user-1', REPLACEMENT_PROMPT);
  replaceMessageText(rewritten, 'transcript-assistant-1', REPLACEMENT_ANSWER);
  await replaceTranscriptAtomically(rewritten);

  await expect.poll(() => hasStructuredOperation(page, streamPath, 'reset')).toBe(true);
  const reset = requiredOperation(await sessionEvents(page, streamPath), 'reset');
  expect(reset.reset_reason).toBe('history_rewritten');
  expect(reset.structured_messages?.map((message) => message.id)).toEqual([
    'transcript-user-1',
    'transcript-assistant-1',
    APPENDED_ID,
  ]);
  await expect(page.getByText(renderedPrompt(REPLACEMENT_PROMPT), { exact: true })).toBeVisible();
  await expect(page.getByText(REPLACEMENT_ANSWER, { exact: true })).toBeVisible();
  await expect(page.getByText(renderedPrompt(INITIAL_PROMPT), { exact: true })).toHaveCount(0);
  await expect(page.getByText(INITIAL_ANSWER, { exact: true })).toHaveCount(0);
  await expect(page.getByText(renderedPrompt(APPENDED_PROMPT), { exact: true })).toHaveCount(1);

  expect(consoleProblems).toEqual([]);
  expect(pageErrors).toEqual([]);
  expect(failedRequests).toEqual([]);
  expect(badResponses).toEqual([]);
  expect(clientErrorPosts).toEqual([]);
  await expect(page.getByText('Dashboard view failed.')).toHaveCount(0);
});

async function installSSEObserver(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const observed: ObservedSSEEvent[] = [];
    Object.defineProperty(window, '__dashportSSEEvents', { value: observed });
    const NativeEventSource = window.EventSource;
    class ObservedEventSource extends NativeEventSource {
      constructor(url: string | URL, init: EventSourceInit = {}) {
        super(url, init);
        for (const type of ['structured', 'activity', 'pending', 'pending_cleared']) {
          this.addEventListener(type, (rawEvent) => {
            const event = rawEvent as MessageEvent<string>;
            let data: unknown = event.data;
            try {
              data = JSON.parse(event.data);
            } catch {
              // Keep malformed payload text as observed data; the application
              // remains responsible for rejecting it.
            }
            observed.push({ data, lastEventId: event.lastEventId, type, url: this.url });
          });
        }
      }
    }
    Object.defineProperty(window, 'EventSource', { value: ObservedEventSource });
  });
}

async function sessionEvents(page: Page, streamPath: string): Promise<ObservedSSEEvent[]> {
  return page.evaluate((path) => {
    const events = (window as Window & { __dashportSSEEvents?: ObservedSSEEvent[] })
      .__dashportSSEEvents;
    return (events ?? []).filter((event) => new URL(event.url).pathname === path);
  }, streamPath);
}

async function hasStructuredOperation(
  page: Page,
  streamPath: string,
  operation: string,
): Promise<boolean> {
  const events = await sessionEvents(page, streamPath);
  return events.some(
    (event) =>
      event.type === 'structured' &&
      typeof event.data === 'object' &&
      event.data !== null &&
      (event.data as StructuredEnvelope).operation === operation,
  );
}

async function hasPendingRequest(
  page: Page,
  streamPath: string,
  requestID: string,
): Promise<boolean> {
  const events = await sessionEvents(page, streamPath);
  return events.some(
    (event) =>
      event.type === 'pending' &&
      typeof event.data === 'object' &&
      event.data !== null &&
      (event.data as { request_id?: string }).request_id === requestID,
  );
}

function requiredOperation(events: ObservedSSEEvent[], operation: string): StructuredEnvelope {
  const event = events.find(
    (candidate) =>
      candidate.type === 'structured' &&
      typeof candidate.data === 'object' &&
      candidate.data !== null &&
      (candidate.data as StructuredEnvelope).operation === operation,
  );
  expect(event, `missing structured ${operation} frame`).toBeDefined();
  return event?.data as StructuredEnvelope;
}

async function readTranscript(): Promise<ClaudeTranscriptEntry[]> {
  const raw = await readFile(TRANSCRIPT_PATH, 'utf8');
  return raw
    .split('\n')
    .filter((line) => line.trim() !== '')
    .map((line) => JSON.parse(line) as ClaudeTranscriptEntry);
}

function appendUserEntry(entries: ClaudeTranscriptEntry[]): ClaudeTranscriptEntry {
  const first = entries[0];
  const previous = entries.at(-1);
  if (first === undefined || previous === undefined) throw new Error('seeded transcript is empty');
  return {
    ...structuredClone(first),
    uuid: APPENDED_ID,
    parentUuid: previous.uuid,
    type: 'user',
    timestamp: '2026-07-14T00:00:02Z',
    message: { role: 'user', content: APPENDED_PROMPT },
  };
}

function replaceMessageText(
  entries: ClaudeTranscriptEntry[],
  id: string,
  replacement: string,
): void {
  const entry = entries.find((candidate) => candidate.uuid === id);
  if (entry === undefined) throw new Error(`seeded transcript is missing ${id}`);
  entry.message.content = replacement;
}

async function replaceTranscriptAtomically(entries: ClaudeTranscriptEntry[]): Promise<void> {
  const replacementPath = `${TRANSCRIPT_PATH}.dashport-${process.pid}`;
  const body = entries.map((entry) => JSON.stringify(entry)).join('\n') + '\n';
  await writeFile(replacementPath, body, 'utf8');
  await rename(replacementPath, TRANSCRIPT_PATH);
}

function requiredFirst(values: string[]): string {
  const first = values[0];
  if (first === undefined) throw new Error('expected one value');
  return first;
}

function renderedPrompt(prompt: string): string {
  return `prompt: ${prompt}`;
}

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (value === undefined || value === '') throw new Error(`${name} is required`);
  return value;
}
