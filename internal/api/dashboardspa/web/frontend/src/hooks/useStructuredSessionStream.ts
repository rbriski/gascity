import { useEffect, useRef, useState } from 'react';
import {
  errorMessage,
  isSessionActivityEvent,
  isSessionHeartbeatEvent,
  isSessionStructuredEvent,
  parsePendingInteraction,
  structuredMessagesFromEnvelope,
} from 'gas-city-dashboard-shared';
import type {
  PendingInteraction,
  SessionStructuredHistory,
  SessionStructuredMessage,
} from 'gas-city-dashboard-shared';
import { activeCityOrThrow } from '../api/cityBase';
import { reportClientError } from '../lib/clientErrorReporting';
import { supervisorApi } from '../supervisor/client';
import { fetchStructuredTranscript } from '../supervisor/sessionReads';
import type { SessionStreamProgress } from './useSessionStream';

// Live structured-transcript reader, ported from the old dashboard's
// connectAgentOutput (PR #3718). It seeds from the REST structured snapshot,
// then consumes the four structured-mode SSE frames — structured, activity,
// pending, heartbeat — in arrival order with no dedup or reconnect. Raw
// conversation frames are rejected. A non-structured snapshot yields the
// `unavailable` state so the caller can fall back to conversation rendering.

/** One rendered item in arrival order: a structured message or a pending interaction. */
export type StructuredStreamItem =
  | { kind: 'message'; message: SessionStructuredMessage }
  | { kind: 'pending'; pending: PendingInteraction };

export interface StructuredTranscriptResult {
  provider: string;
  template: string;
  history: SessionStructuredHistory | null;
  items: StructuredStreamItem[];
  /** Latest tail activity (`idle` | `in-turn` | `unknown`). */
  activity: string;
}

export type StructuredStreamState =
  | { status: 'idle'; stream: SessionStreamProgress }
  | { status: 'loading'; stream: SessionStreamProgress }
  | { status: 'failed'; error: string; stream: SessionStreamProgress }
  | { status: 'unavailable'; stream: SessionStreamProgress }
  | { status: 'ready'; result: StructuredTranscriptResult; stream: SessionStreamProgress };

const STRUCTURED_FRAME_ERROR = 'Malformed structured session frame.';

export function useStructuredSessionStream(
  sessionId: string | null,
  stream: boolean,
): StructuredStreamState {
  const [state, setState] = useState<StructuredStreamState>({
    status: 'idle',
    stream: { status: 'idle' },
  });
  const malformedReportedRef = useRef(false);

  useEffect(() => {
    malformedReportedRef.current = false;
    if (!sessionId) {
      setState({ status: 'idle', stream: { status: 'idle' } });
      return;
    }
    let cancelled = false;
    let source: EventSource | null = null;
    const canStream = stream && typeof EventSource !== 'undefined';
    setState({ status: 'loading', stream: { status: canStream ? 'connecting' : 'idle' } });

    const degrade = (): void => {
      if (!malformedReportedRef.current) {
        malformedReportedRef.current = true;
        reportStructuredStreamError('parse structured frame', sessionId, STRUCTURED_FRAME_ERROR);
      }
      setState((current) =>
        current.status === 'ready'
          ? { ...current, stream: { status: 'degraded', error: STRUCTURED_FRAME_ERROR } }
          : current,
      );
    };

    const appendItems = (items: StructuredStreamItem[]): void => {
      if (items.length === 0) return;
      setState((current) =>
        current.status === 'ready'
          ? {
              status: 'ready',
              result: { ...current.result, items: [...current.result.items, ...items] },
              stream: { status: 'open' },
            }
          : current,
      );
    };

    const messageItems = (messages: SessionStructuredMessage[]): StructuredStreamItem[] =>
      messages.map((message) => ({ kind: 'message' as const, message }));

    fetchStructuredTranscript(sessionId).then(
      (envelope) => {
        if (cancelled) return;
        if (envelope === null) {
          setState({ status: 'unavailable', stream: { status: 'idle' } });
          return;
        }
        setState({
          status: 'ready',
          result: {
            provider: envelope.provider,
            template: envelope.template,
            history: envelope.history ?? null,
            items: messageItems(structuredMessagesFromEnvelope(envelope)),
            activity: envelope.history?.tail_state.activity ?? 'unknown',
          },
          stream: { status: canStream ? 'connecting' : 'idle' },
        });
        if (!canStream) return;

        source = new EventSource(
          supervisorApi().sessionStreamUrl(
            activeCityOrThrow('open structured session stream'),
            sessionId,
            undefined,
            'structured',
          ),
          { withCredentials: true },
        );
        source.onopen = () => {
          if (cancelled) return;
          setState((current) =>
            current.status === 'ready' ? { ...current, stream: { status: 'open' } } : current,
          );
        };
        source.addEventListener('structured', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          if (parsed === null || !isSessionStructuredEvent(parsed)) return degrade();
          appendItems(messageItems(structuredMessagesFromEnvelope(parsed)));
        });
        source.addEventListener('activity', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          if (parsed === null || !isSessionActivityEvent(parsed)) return degrade();
          const activity = parsed.activity;
          setState((current) =>
            current.status === 'ready'
              ? { status: 'ready', result: { ...current.result, activity }, stream: { status: 'open' } }
              : current,
          );
        });
        source.addEventListener('pending', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          const pending = parsed === null ? null : parsePendingInteraction(parsed);
          if (pending === null) return degrade();
          appendItems([{ kind: 'pending', pending }]);
        });
        source.addEventListener('heartbeat', (event) => {
          if (cancelled) return;
          const parsed = parseFrame((event as MessageEvent<string>).data);
          if (parsed === null || !isSessionHeartbeatEvent(parsed)) return degrade();
          // Liveness only: mark the stream open, leave the transcript untouched.
          setState((current) =>
            current.status === 'ready' && current.stream.status !== 'open'
              ? { ...current, stream: { status: 'open' } }
              : current,
          );
        });
        // Raw / unnamed `message` frames are not valid on a structured stream.
        source.onmessage = () => {
          if (cancelled) return;
          degrade();
        };
        source.onerror = () => {
          if (cancelled) return;
          const streamState = source?.readyState === EventSource.CLOSED ? 'closed' : 'connecting';
          setState((current) =>
            current.status === 'ready' ? { ...current, stream: { status: streamState } } : current,
          );
        };
      },
      (err: unknown) => {
        if (cancelled) return;
        reportStructuredStreamError('load structured transcript', sessionId, err);
        setState({
          status: 'failed',
          error: errorMessage(err) || 'Failed to load session.',
          stream: { status: 'idle' },
        });
      },
    );

    return () => {
      cancelled = true;
      source?.close();
    };
  }, [sessionId, stream]);

  return state;
}

function parseFrame(data: string): unknown {
  try {
    return JSON.parse(data) as unknown;
  } catch {
    return null;
  }
}

function reportStructuredStreamError(operation: string, sessionId: string, err: unknown): void {
  void reportClientError({
    component: 'structured-session-stream',
    operation,
    message: `${sessionId}: ${errorMessage(err)}`,
  });
}
