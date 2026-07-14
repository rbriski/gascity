import { act, cleanup, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import type { SessionStreamStructuredMessageEvent } from 'gas-city-dashboard-shared';
import { reportClientError } from '../lib/clientErrorReporting';
import type * as SessionReads from '../supervisor/sessionReads';
import { useStructuredSessionStream } from './useStructuredSessionStream';

const mockFetchStructuredTranscript = vi.hoisted(() => vi.fn());

vi.mock('../supervisor/sessionReads', async (importOriginal) => {
  const actual = await importOriginal<typeof SessionReads>();
  return {
    ...actual,
    fetchStructuredTranscript: mockFetchStructuredTranscript,
  };
});

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(() => Promise.resolve({ status: 'reported' })),
}));

const eventSources: FakeEventSource[] = [];
const mockReportClientError = reportClientError as Mock;

const envelope: SessionStreamStructuredMessageEvent = {
  id: 'gc-session-1',
  template: 'mayor',
  provider: 'claude',
  format: 'structured',
  schema_version: 'session.structured.v1',
  history: {
    transcript_stream_id: 'stream-1',
    generation: { id: 'gen-1' },
    cursor: {},
    continuity: { status: 'continuous' },
    tail_state: { activity: 'idle' },
  },
  structured_messages: [
    { id: 'm1', role: 'assistant', status: 'final', blocks: [{ type: 'text', text: 'hello' }] },
  ],
};

function structuredFrame(id: string): string {
  return JSON.stringify({
    ...envelope,
    structured_messages: [
      { id, role: 'assistant', status: 'final', blocks: [{ type: 'text', text: id }] },
    ],
  });
}

describe('useStructuredSessionStream', () => {
  beforeEach(() => {
    eventSources.length = 0;
    vi.stubGlobal('EventSource', FakeEventSource);
    mockFetchStructuredTranscript.mockReset();
    mockReportClientError.mockClear();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it('returns explicit idle state when no session is selected', () => {
    const { result } = renderHook(() => useStructuredSessionStream(null, true));
    expect(result.current).toEqual({ status: 'idle', stream: { status: 'idle' } });
  });

  it('seeds from the structured snapshot and opens a format=structured stream', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(envelope);

    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    expect(result.current).toEqual({ status: 'loading', stream: { status: 'connecting' } });

    await flush();
    expect(result.current.status).toBe('ready');
    if (result.current.status !== 'ready') return;
    expect(result.current.result.items).toEqual([{ kind: 'message', message: envelope.structured_messages[0] }]);
    expect(result.current.result.history?.transcript_stream_id).toBe('stream-1');
    expect(result.current.result.activity).toBe('idle');
    expect(eventSources[0]?.url).toContain('/v0/city/test-city/session/gc-session-1/stream');
    expect(eventSources[0]?.url).toContain('format=structured');

    act(() => eventSources[0]?.open());
    expect(result.current.stream).toEqual({ status: 'open' });
  });

  it('appends structured frames in arrival order', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(envelope);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();

    act(() => eventSources[0]?.emit('structured', structuredFrame('m2')));
    expect(result.current.status).toBe('ready');
    if (result.current.status !== 'ready') return;
    expect(result.current.result.items.map((i) => (i.kind === 'message' ? i.message.id : 'p'))).toEqual([
      'm1',
      'm2',
    ]);
  });

  it('does not duplicate the REST snapshot when the stream replays it', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(envelope);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();

    act(() => eventSources[0]?.emit('structured', JSON.stringify(envelope)));

    if (result.current.status !== 'ready') throw new Error('expected ready');
    expect(result.current.result.items).toEqual([
      { kind: 'message', message: envelope.structured_messages[0] },
    ]);
  });

  it('replaces a same-ID partial message with its final form', async () => {
    const partial = {
      ...envelope,
      structured_messages: [
        {
          id: 'm1',
          role: 'assistant',
          status: 'partial',
          blocks: [{ type: 'text', text: 'hel' }],
        },
      ],
    } satisfies SessionStreamStructuredMessageEvent;
    mockFetchStructuredTranscript.mockResolvedValue(partial);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();

    const finalMessage = envelope.structured_messages[0];
    act(() => eventSources[0]?.emit('structured', JSON.stringify(envelope)));

    if (result.current.status !== 'ready') throw new Error('expected ready');
    expect(result.current.result.items).toEqual([{ kind: 'message', message: finalMessage }]);
  });

  it('replaces messages and history when the transcript generation changes', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(envelope);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();
    if (envelope.history === undefined) throw new Error('fixture history is required');

    const replacement = {
      ...envelope,
      history: {
        ...envelope.history,
        transcript_stream_id: 'stream-2',
        generation: { id: 'gen-2' },
        tail_state: { activity: 'in-turn' },
      },
      structured_messages: [
        {
          id: 'm1',
          role: 'assistant',
          status: 'final',
          blocks: [{ type: 'text', text: 'replacement' }],
        },
      ],
    } satisfies SessionStreamStructuredMessageEvent;
    act(() => eventSources[0]?.emit('structured', JSON.stringify(replacement)));

    if (result.current.status !== 'ready') throw new Error('expected ready');
    expect(result.current.result.history?.generation.id).toBe('gen-2');
    expect(result.current.result.activity).toBe('in-turn');
    expect(result.current.result.items).toEqual([
      { kind: 'message', message: replacement.structured_messages[0] },
    ]);
  });

  it('updates activity from activity frames without adding items', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(envelope);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();

    act(() => eventSources[0]?.emit('activity', JSON.stringify({ activity: 'in-turn' })));
    if (result.current.status !== 'ready') throw new Error('expected ready');
    expect(result.current.result.activity).toBe('in-turn');
    expect(result.current.result.items).toHaveLength(1);
  });

  it('appends pending interactions as items', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(envelope);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();

    act(() =>
      eventSources[0]?.emit('pending', JSON.stringify({ request_id: 'req-1', kind: 'tool_approval' })),
    );
    if (result.current.status !== 'ready') throw new Error('expected ready');
    const last = result.current.result.items.at(-1);
    expect(last).toEqual({ kind: 'pending', pending: { request_id: 'req-1', kind: 'tool_approval' } });
  });

  it('treats heartbeat frames as liveness no-ops', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(envelope);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();

    act(() => eventSources[0]?.emit('heartbeat', JSON.stringify({ timestamp: '2026-06-30T00:00:00Z' })));
    if (result.current.status !== 'ready') throw new Error('expected ready');
    expect(result.current.result.items).toHaveLength(1);
    expect(result.current.stream).toEqual({ status: 'open' });
  });

  it('rejects raw message frames and surfaces a degraded stream', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(envelope);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();

    act(() => eventSources[0]?.emit('message', JSON.stringify({ role: 'assistant', text: 'raw' })));
    if (result.current.status !== 'ready') throw new Error('expected ready');
    expect(result.current.result.items).toHaveLength(1);
    expect(result.current.stream).toEqual({ status: 'degraded', error: 'Malformed structured session frame.' });
    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'structured-session-stream',
      operation: 'parse structured frame',
      message: 'gc-session-1: Malformed structured session frame.',
    });
  });

  it('reports unavailable when the server returns a non-structured transcript', async () => {
    mockFetchStructuredTranscript.mockResolvedValue(null);
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();
    expect(result.current).toEqual({ status: 'unavailable', stream: { status: 'idle' } });
    expect(eventSources).toHaveLength(0);
  });

  it('fails when the snapshot fetch rejects', async () => {
    mockFetchStructuredTranscript.mockRejectedValue(new Error('peek failed'));
    const { result } = renderHook(() => useStructuredSessionStream('gc-session-1', true));
    await flush();
    expect(result.current).toEqual({ status: 'failed', error: 'peek failed', stream: { status: 'idle' } });
    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'structured-session-stream',
      operation: 'load structured transcript',
      message: 'gc-session-1: peek failed',
    });
  });
});

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readyState = FakeEventSource.CONNECTING;
  private readonly listeners = new Map<string, Set<EventListener>>();

  constructor(readonly url: string | URL) {
    eventSources.push(this);
  }

  addEventListener(type: string, listener: EventListener): void {
    const listeners = this.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: EventListener): void {
    this.listeners.get(type)?.delete(listener);
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }

  open(): void {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.(new Event('open'));
  }

  emit(type: string, data: string): void {
    const event = new MessageEvent<string>(type, { data });
    this.listeners.get(type)?.forEach((listener) => listener(event));
    if (type === 'message') this.onmessage?.(event);
  }
}
