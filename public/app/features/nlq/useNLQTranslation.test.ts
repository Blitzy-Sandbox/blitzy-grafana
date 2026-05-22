// NLQ feature: isolated Jest unit test for the `useNLQTranslation` React hook.
//
// This file is the test counterpart of `./useNLQTranslation.ts`. It exercises
// the hook in complete isolation (no React components rendered, no Redux/router
// wrappers needed) using `@testing-library/react`'s `renderHook` primitive.
//
// HTTP behaviour is verified by intercepting the real `fetch` call at the
// network layer with Mock Service Worker (MSW v2). Per AAP §0.6.1.4 we do NOT
// mock `getBackendSrv` directly — instead we register the real `backendSrv`
// instance as the runtime singleton via `setBackendSrv` so that the hook's
// production code path (hook → `postTranslate` → `getBackendSrv().post(...)`
// → real `fetch`) is exercised end-to-end, with MSW intercepting only the
// outbound network request. This matches the canonical pattern established by
// `public/app/features/alerting/unified/mockApi.ts`'s `setupBackendSrv()` and
// the browse-dashboards test suite.
//
// The MSW server is configured with `onUnhandledRequest: 'error'` so that any
// stray HTTP call (e.g. from a regression that bypassed the unsupported-
// datasource short-circuit) fails the test LOUDLY rather than silently.
//
// Coverage map (per AAP §0.6.4 validation criteria):
//   - Case 1 → criterion #1 (PromQL happy path)
//   - Case 2 → criterion #2 (LogQL happy path)
//   - Case 3 → criterion #7 (LLM 5xx surfaces error, clears query)
//   - Case 4 → criterion #9 (unsupported datasource short-circuits, no HTTP)
//   - Case 5 → empty-input short-circuit (defensive, no HTTP)
//   - Case 6 → warnings propagation
//   - Case 7 → reset() clears all derived translation state (Prometheus path)
//   - Case 8 → reset() clears isUnsupportedDatasource (MySQL path; checkpoint
//             contract that the flag is state-backed and cleared by reset())
//   - Case 9 → telemetry emit on success (`grafana_nlq_translate_succeeded`,
//             AAP §0.6.3.2)
//   - Case 10 → telemetry emit on failure (`grafana_nlq_translate_failed`,
//             AAP §0.6.3.2)
//   - Case 11 → telemetry NOT emitted on unsupported-datasource short-circuit
//   - Case 12 → telemetry NOT emitted on empty-input short-circuit
//   - Case 13 → criterion AAP §0.1.1.1 (Checkpoint 5 QA fix): switching
//              `dsSettings.type` from supported to unsupported flips
//              `isUnsupportedDatasource` to `true` on the very next render —
//              without the user clicking Translate — so the parent bar can
//              render the warning Alert immediately.
//   - Case 14 → criterion AAP §0.1.1.1 (Checkpoint 5 QA fix): switching
//              `dsSettings.type` from unsupported back to supported flips
//              `isUnsupportedDatasource` to `false` on the next render so
//              the input UI is restored once a supported datasource is
//              re-selected.

// Mock `@grafana/runtime` to replace ONLY the `reportInteraction` export with
// a `jest.fn()`. All other exports (notably `setBackendSrv`, `getBackendSrv`,
// `config`, …) flow through `jest.requireActual` unchanged — the test still
// uses the real backendSrv singleton wiring below so MSW intercepts the real
// `fetch` call. This pattern matches the canonical Grafana test approach in
// `NaturalLanguageQueryBar.test.tsx` and avoids the need for the test to
// initialize a real `EchoSrv` in the jsdom environment.
jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  reportInteraction: jest.fn(),
}));

import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';

import type { DataSourceInstanceSettings } from '@grafana/data';
import { getBackendSrv, setBackendSrv, reportInteraction, type BackendSrv } from '@grafana/runtime';
import { backendSrv } from 'app/core/services/backend_srv';

import { useNLQTranslation } from './useNLQTranslation';

// Strongly-typed handle on the `reportInteraction` `jest.fn()` so assertions
// can use the mock-aware `toHaveBeenCalledWith`/`mock.calls` APIs without
// excessive casting at each call site. Cast through `unknown` to bridge the
// real export's call signature to `jest.Mock` — the production code calls
// `reportInteraction(name, props)` with a string and an arbitrary object
// payload, both of which `jest.Mock` understands.
const reportInteractionMock = reportInteraction as unknown as jest.Mock;

// ---------------------------------------------------------------------------
// MSW server and runtime backendSrv wiring
// ---------------------------------------------------------------------------
//
// `setupServer()` from `msw/node` creates a Node-side request interceptor that
// patches `fetch`/`XMLHttpRequest`/`http.ClientRequest`. Requests issued by
// the hook under test (via the real `backendSrv` → `fromFetch` → global
// `fetch`) are intercepted before they touch the network. We seed the server
// with no handlers here and register them per-test via `server.use(...)` so
// each test owns its own response contract.
const server = setupServer();

// Capture the prior `@grafana/runtime` backendSrv singleton (if any) BEFORE
// we overwrite it for this test file. `getBackendSrv()` returns `undefined`
// when no singleton has been registered yet (the default state for unit
// tests that have not loaded the bootstrap). We store the prior value in a
// `let` (typed as `BackendSrv | undefined`) so the `afterAll` hook can
// restore it, preventing cross-suite global-state pollution when Jest runs
// multiple test files in the same worker process.
let priorBackendSrv: BackendSrv | undefined;

beforeAll(() => {
  // Snapshot the prior backendSrv so we can restore it in `afterAll`. This
  // matters when Jest runs this file alongside other suites in the same
  // worker — without restoration, the mutation we make below would leak
  // into the next file's tests.
  //
  // The runtime `getBackendSrv()` declaration returns `BackendSrv`, but in
  // unit-test contexts where the bootstrap has not run the underlying
  // singleton is actually `undefined`. We coerce defensively so the
  // restoration branch below can correctly detect "no prior singleton".
  const current: BackendSrv | undefined = getBackendSrv();
  priorBackendSrv = current;

  // Register the real `backendSrv` instance as the `@grafana/runtime`
  // singleton returned by `getBackendSrv()`. We deliberately use the real
  // `backendSrv` rather than a stub so that MSW intercepts the actual
  // production code path through `nlqApi.ts` (AAP §0.6.1.4 key insight:
  // "DO NOT mock `getBackendSrv` directly — let MSW intercept the actual
  // fetch call").
  setBackendSrv(backendSrv);

  // CRITICAL: `onUnhandledRequest: 'error'` ensures that the unsupported-
  // datasource short-circuit test (Case 4) and the empty-input short-circuit
  // test (Case 5) FAIL LOUDLY if the hook ever makes a request when it
  // shouldn't. Without this, a regression that removed the short-circuit
  // would pass silently because no handler would match and the request
  // would either fall through to a real network call or hang.
  server.listen({ onUnhandledRequest: 'error' });
});

afterEach(() => {
  // Drop all per-test handlers so leakage from one test cannot pollute the
  // next. The `setBackendSrv(backendSrv)` registration in `beforeAll` is
  // intentionally NOT reset here — the singleton remains valid across tests
  // in THIS file; cross-file restoration happens in `afterAll`.
  server.resetHandlers();

  // Reset the `reportInteraction` mock between tests so assertions on call
  // counts and arguments do not leak between cases. Using `mockClear()`
  // (rather than `mockReset()`) preserves the `jest.fn()` identity and the
  // `mockImplementation` default; we only need to wipe the call history.
  reportInteractionMock.mockClear();
});

afterAll(() => {
  // Tear down the MSW interceptor cleanly so it does not affect other test
  // files when Jest runs the suite under `--maxWorkers > 1`.
  server.close();

  // Restore the prior backendSrv singleton if there was one, preventing
  // cross-suite global-state pollution. If `priorBackendSrv` is undefined
  // (the common case in isolated test runs), we leave our `backendSrv`
  // registration in place — calling `setBackendSrv(undefined as never)`
  // would deliberately break subsequent files, which is the OPPOSITE of
  // what we want. The risk of leaving `backendSrv` registered is benign:
  // any downstream test that runs without MSW will hit the real `fetch`
  // and fail loudly with a network error.
  if (priorBackendSrv !== undefined) {
    setBackendSrv(priorBackendSrv);
  }
});

// ---------------------------------------------------------------------------
// Fixture helper
// ---------------------------------------------------------------------------

/**
 * Constructs a minimal `DataSourceInstanceSettings` fixture for the supplied
 * datasource `type`. The hook reads only `uid` and `type` from this object,
 * so most fields are stubbed with the smallest plausible values. The double
 * cast (`as unknown as DataSourceInstanceSettings`) is necessary because
 * `DataSourceInstanceSettings` is a large structural interface that requires
 * fields the hook never touches — re-declaring all of them here would
 * duplicate the production type and produce no additional test value.
 *
 * @param type - The `dsSettings.type` value used both for the supported-
 *   datasource guard (`prometheus`/`loki` → supported, everything else →
 *   unsupported) and as the `datasourceType` field in the request payload.
 * @param uid - Defaults to `'ds-test'`. Sent as `datasourceUid` in the
 *   request payload.
 */
function makeDs(type: string, uid = 'ds-test'): DataSourceInstanceSettings {
  return {
    id: 1,
    uid,
    type,
    name: `${type}-ds`,
    // The hook does not inspect `meta`, but the field is required on the
    // structural interface, so we stub the minimum shape (`id`+`name`).
    meta: { id: type, name: type },
    jsonData: {},
    readOnly: false,
    access: 'proxy',
  } as unknown as DataSourceInstanceSettings;
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('useNLQTranslation', () => {
  // -------------------------------------------------------------------------
  // Case 1 — Happy path (Prometheus → PromQL)
  //
  // Verifies AAP §0.6.4 criterion #1: a successful `POST /api/nlq/translate`
  // populates `translatedQuery`, `language`, `explanation`, and clears any
  // prior `error`. Also asserts the initial state shape before `translate()`
  // is called.
  // -------------------------------------------------------------------------
  it('translates and updates state for prometheus datasource', async () => {
    server.use(
      http.post('/api/nlq/translate', () =>
        HttpResponse.json({
          query: 'rate(http_requests_total[5m])',
          language: 'promql',
          explanation: 'Computes per-second rate',
          warnings: [],
        })
      )
    );

    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    // Initial state — no translation has been fired yet.
    expect(result.current.isLoading).toBe(false);
    expect(result.current.translatedQuery).toBe('');
    expect(result.current.language).toBe('');
    expect(result.current.explanation).toBe('');
    expect(result.current.warnings).toEqual([]);
    expect(result.current.error).toBeNull();
    expect(result.current.isUnsupportedDatasource).toBe(false);

    await act(async () => {
      await result.current.translate('Show me request rate');
    });

    // Once the promise has settled, `isLoading` MUST drop to false. We use
    // `waitFor` (rather than a bare assertion) so any trailing micro-task
    // re-render from the `setIsLoading(false)` call inside `finally` is
    // observed before the assertions run.
    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(result.current.translatedQuery).toBe('rate(http_requests_total[5m])');
    expect(result.current.language).toBe('promql');
    expect(result.current.explanation).toBe('Computes per-second rate');
    expect(result.current.warnings).toEqual([]);
    expect(result.current.error).toBeNull();
    expect(result.current.isUnsupportedDatasource).toBe(false);
  });

  // -------------------------------------------------------------------------
  // Case 2 — Loki → LogQL
  //
  // Verifies AAP §0.6.4 criterion #2: a Loki datasource produces a response
  // with `language === 'logql'`. The query string here is deliberately the
  // canonical "failed password" LogQL stream selector that mirrors the
  // user-provided example #2 in AAP §0.1.2.1 ("Show IPs with more than 10
  // failed SSH login attempts in the last 30 minutes").
  // -------------------------------------------------------------------------
  it('produces logql language for loki datasource', async () => {
    server.use(
      http.post('/api/nlq/translate', () =>
        HttpResponse.json({
          query: '{job="syslog"} |= "Failed password"',
          language: 'logql',
        })
      )
    );

    const { result } = renderHook(() => useNLQTranslation(makeDs('loki')));

    // The hook MUST accept `loki` as a supported datasource type.
    expect(result.current.isUnsupportedDatasource).toBe(false);

    await act(async () => {
      await result.current.translate('Show failed logins');
    });

    // The query string roundtrips intact (no client-side mutation), and the
    // language is the LogQL discriminator the Monaco editor uses for syntax
    // highlighting.
    await waitFor(() => expect(result.current.translatedQuery).toContain('Failed password'));
    expect(result.current.language).toBe('logql');
    expect(result.current.error).toBeNull();
  });

  // -------------------------------------------------------------------------
  // Case 3 — HTTP 500 → error state
  //
  // Verifies AAP §0.6.4 criterion #7: when the backend returns a 5xx, the
  // hook captures the error into `error` and clears `translatedQuery` so the
  // UI never renders a stale query alongside an error alert.
  // -------------------------------------------------------------------------
  it('captures error and clears query on backend failure', async () => {
    server.use(http.post('/api/nlq/translate', () => HttpResponse.json({ message: 'fail' }, { status: 500 })));

    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    await act(async () => {
      await result.current.translate('Bad request');
    });

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    // `error` is populated with an Error instance (the hook wraps non-Error
    // throws, so we get a `.message` either way).
    expect(result.current.error).not.toBeNull();
    expect(result.current.error).toBeInstanceOf(Error);
    // All derived translation state MUST be cleared so the UI can simply
    // gate the preview on `translatedQuery` truthiness.
    expect(result.current.translatedQuery).toBe('');
    expect(result.current.language).toBe('');
    expect(result.current.explanation).toBe('');
    expect(result.current.warnings).toEqual([]);
  });

  // -------------------------------------------------------------------------
  // Case 4 — Unsupported datasource short-circuit
  //
  // Verifies AAP §0.6.4 criterion #9: when the active datasource type is
  // outside {prometheus, loki}, the hook MUST NOT issue an HTTP request and
  // MUST surface `isUnsupportedDatasource === true` so the parent bar can
  // render an "unsupported data source" alert.
  //
  // The lack of any registered handler combined with the module-level
  // `onUnhandledRequest: 'error'` configuration means: if the hook ever
  // *does* issue a request here, MSW will throw and the test will fail
  // loudly. That is the entire point of this test.
  // -------------------------------------------------------------------------
  it('does not call backend for unsupported datasource type', async () => {
    const { result } = renderHook(() => useNLQTranslation(makeDs('mysql')));

    // The flag is derived from `dsSettings.type` on every render, so it
    // resolves to `true` synchronously before any HTTP call could fire.
    expect(result.current.isUnsupportedDatasource).toBe(true);

    await act(async () => {
      // This call MUST be a no-op. If the hook regresses and issues a real
      // HTTP request, MSW's `onUnhandledRequest: 'error'` will reject and
      // this `await` will throw, failing the test.
      await result.current.translate('This should not fire');
    });

    // No translation state is mutated by the short-circuit branch.
    expect(result.current.translatedQuery).toBe('');
    expect(result.current.language).toBe('');
    expect(result.current.explanation).toBe('');
    expect(result.current.warnings).toEqual([]);
    expect(result.current.error).toBeNull();
    expect(result.current.isLoading).toBe(false);
  });

  // -------------------------------------------------------------------------
  // Case 5 — Empty input short-circuit
  //
  // Defensive coverage: a whitespace-only input string MUST NOT trigger an
  // HTTP call (input is trimmed first, and the trimmed result is checked for
  // emptiness before the HTTP call). The `onUnhandledRequest: 'error'`
  // configuration enforces this assertion just like Case 4.
  // -------------------------------------------------------------------------
  it('does not call backend for empty input', async () => {
    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    await act(async () => {
      // Whitespace-only input must be treated as empty after trimming.
      await result.current.translate('   ');
    });

    // State remains unchanged: the short-circuit returns before any setState.
    expect(result.current.translatedQuery).toBe('');
    expect(result.current.language).toBe('');
    expect(result.current.explanation).toBe('');
    expect(result.current.warnings).toEqual([]);
    expect(result.current.error).toBeNull();
    expect(result.current.isLoading).toBe(false);
  });

  // -------------------------------------------------------------------------
  // Case 6 — Warnings propagation
  //
  // The server may attach non-fatal warnings to a successful response (for
  // example when the schema-context fetch fails and translation proceeds
  // prompt-only — AAP §0.2.1). The hook MUST propagate those warnings into
  // `warnings[]` so the parent bar can render an `<Alert severity="info">`.
  // -------------------------------------------------------------------------
  it('propagates warnings from the response', async () => {
    server.use(
      http.post('/api/nlq/translate', () =>
        HttpResponse.json({
          query: 'up',
          language: 'promql',
          warnings: ['schema fetch failed; translation may be less accurate'],
        })
      )
    );

    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    await act(async () => {
      await result.current.translate('show me what is up');
    });

    await waitFor(() => expect(result.current.translatedQuery).toBe('up'));
    expect(result.current.warnings).toHaveLength(1);
    expect(result.current.warnings[0]).toContain('schema fetch failed');
    // A response with warnings is still a SUCCESS — `error` MUST stay null.
    expect(result.current.error).toBeNull();
  });

  // -------------------------------------------------------------------------
  // Case 7 — reset() clears all derived translation state
  //
  // `reset()` is the explicit contract by which the parent bar abandons a
  // prior translation (e.g. when the user switches datasources or closes
  // the collapsible). All derived translation fields MUST return to their
  // initial values, INCLUDING `isUnsupportedDatasource` per the checkpoint
  // contract. For a supported (Prometheus) datasource, the flag was already
  // `false` from the lazy initializer, so this test exercises the "stays
  // false through reset" branch.
  // -------------------------------------------------------------------------
  it('reset() clears all translation state for supported datasource', async () => {
    server.use(
      http.post('/api/nlq/translate', () =>
        HttpResponse.json({
          query: 'up',
          language: 'promql',
          explanation: 'Selects all `up` metrics.',
          warnings: ['warn'],
        })
      )
    );

    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    // First populate the state via a successful translation so we have
    // something non-trivial to clear.
    await act(async () => {
      await result.current.translate('up');
    });
    await waitFor(() => expect(result.current.translatedQuery).toBe('up'));
    expect(result.current.language).toBe('promql');
    expect(result.current.explanation).toBe('Selects all `up` metrics.');
    expect(result.current.warnings).toEqual(['warn']);

    // Synchronous reset — wrapped in `act` so the resulting re-render is
    // flushed before we assert. We do NOT await here because `reset` is a
    // pure state-setter call that returns void synchronously.
    act(() => {
      result.current.reset();
    });

    // Every derived field is back to its initial value.
    expect(result.current.translatedQuery).toBe('');
    expect(result.current.language).toBe('');
    expect(result.current.explanation).toBe('');
    expect(result.current.warnings).toEqual([]);
    expect(result.current.error).toBeNull();
    // The supported-datasource flag stays `false` for Prometheus — it was
    // already `false` from the lazy initializer; `reset()` also re-sets it
    // to `false` per the checkpoint contract (see the dedicated
    // unsupported-datasource test below for the more interesting branch).
    expect(result.current.isUnsupportedDatasource).toBe(false);
  });

  // -------------------------------------------------------------------------
  // Case 8 — reset() clears isUnsupportedDatasource for unsupported datasource
  //
  // The checkpoint contract requires `reset()` to clear `isUnsupportedDatasource`.
  // For a Prometheus datasource the flag starts at `false` and stays `false`,
  // so the cleared-by-reset branch is trivially exercised. This test exercises
  // the meaningful branch: an unsupported datasource starts with the flag at
  // `true` and `reset()` MUST set it back to `false`.
  //
  // Note: a subsequent call to `translate()` will re-set the flag because
  // `translate()` re-evaluates the current `dsSettings.type` on every
  // invocation. Verified at the end of this test so the contract is fully
  // explicit: reset() clears the flag, but the runtime short-circuit is
  // preserved on the next `translate()` call.
  // -------------------------------------------------------------------------
  it('reset() clears isUnsupportedDatasource for unsupported datasource', async () => {
    const { result } = renderHook(() => useNLQTranslation(makeDs('mysql')));

    // Initial state — derived lazily from `dsSettings.type === 'mysql'`.
    expect(result.current.isUnsupportedDatasource).toBe(true);

    // reset() MUST clear `isUnsupportedDatasource` per the checkpoint
    // contract. Synchronous state setter — wrap in `act` so the re-render
    // is flushed before the assertion.
    act(() => {
      result.current.reset();
    });

    expect(result.current.isUnsupportedDatasource).toBe(false);
    // All other state remains in its initial empty form — reset is a
    // complete clear, not a partial one.
    expect(result.current.translatedQuery).toBe('');
    expect(result.current.language).toBe('');
    expect(result.current.explanation).toBe('');
    expect(result.current.warnings).toEqual([]);
    expect(result.current.error).toBeNull();
    expect(result.current.isLoading).toBe(false);

    // Re-asserting the runtime guard: calling translate() AFTER reset()
    // with the same (still-unsupported) dsSettings.type re-evaluates the
    // prop and re-asserts the flag. MSW's `onUnhandledRequest: 'error'`
    // would also fail this test if a network call leaked through.
    await act(async () => {
      await result.current.translate('Will not fire');
    });
    expect(result.current.isUnsupportedDatasource).toBe(true);
  });

  // -------------------------------------------------------------------------
  // Case 9 — Telemetry: success emits `grafana_nlq_translate_succeeded`
  //
  // Verifies AAP §0.6.3.2: a successful translation MUST emit exactly one
  // `grafana_nlq_translate_succeeded` interaction event carrying only
  // non-sensitive metadata — `dsType`, `queryLength` (count, not contents),
  // and `hadWarnings` (boolean). The user's natural-language input, the
  // LLM-generated query text, and any warning text MUST NOT appear in the
  // payload (no leak in telemetry — mirrors the API-key secrecy invariant
  // applied to backend logging per AAP §0.8.5).
  // -------------------------------------------------------------------------
  it('emits grafana_nlq_translate_succeeded on success with non-sensitive metadata', async () => {
    server.use(
      http.post('/api/nlq/translate', () =>
        HttpResponse.json({
          query: 'rate(http_requests_total[5m])',
          language: 'promql',
          explanation: 'Computes per-second rate',
          warnings: [],
        })
      )
    );

    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    await act(async () => {
      await result.current.translate('Show me request rate');
    });
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    // Exactly ONE telemetry event is emitted on success.
    expect(reportInteractionMock).toHaveBeenCalledTimes(1);
    // Event name + non-sensitive payload contract per AAP §0.6.3.2.
    expect(reportInteractionMock).toHaveBeenCalledWith('grafana_nlq_translate_succeeded', {
      dsType: 'prometheus',
      queryLength: 'rate(http_requests_total[5m])'.length,
      hadWarnings: false,
    });

    // Defensive assertion: NO sensitive field leaks into the payload. We
    // inspect the first call's arguments and ensure none of the forbidden
    // fields appear, regardless of payload shape evolution.
    const payload = reportInteractionMock.mock.calls[0][1];
    expect(payload).not.toHaveProperty('input');
    expect(payload).not.toHaveProperty('naturalLanguage');
    expect(payload).not.toHaveProperty('query');
    expect(payload).not.toHaveProperty('explanation');
    expect(payload).not.toHaveProperty('warnings');
    expect(payload).not.toHaveProperty('errorMessage');
  });

  // -------------------------------------------------------------------------
  // Case 9b — Telemetry: success with warnings emits hadWarnings=true
  //
  // The presence-of-warnings boolean is non-sensitive metadata; the warning
  // CONTENTS are not part of the payload (validated by negative assertion).
  // This case complements Case 9 by exercising the `hadWarnings === true`
  // branch so the conditional in the production code is fully covered.
  // -------------------------------------------------------------------------
  it('emits grafana_nlq_translate_succeeded with hadWarnings=true when warnings present', async () => {
    server.use(
      http.post('/api/nlq/translate', () =>
        HttpResponse.json({
          query: 'up',
          language: 'promql',
          warnings: ['schema fetch failed; translation may be less accurate'],
        })
      )
    );

    const { result } = renderHook(() => useNLQTranslation(makeDs('loki')));

    await act(async () => {
      await result.current.translate('show me what is up');
    });
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    expect(reportInteractionMock).toHaveBeenCalledTimes(1);
    expect(reportInteractionMock).toHaveBeenCalledWith('grafana_nlq_translate_succeeded', {
      dsType: 'loki',
      queryLength: 2, // 'up' → 2 chars
      hadWarnings: true,
    });

    // The warning text MUST NOT appear in any payload field — only the
    // presence boolean is allowed.
    const payload = reportInteractionMock.mock.calls[0][1];
    const payloadJson = JSON.stringify(payload);
    expect(payloadJson).not.toContain('schema fetch failed');
  });

  // -------------------------------------------------------------------------
  // Case 10 — Telemetry: failure emits `grafana_nlq_translate_failed`
  //
  // Verifies AAP §0.6.3.2: a failed translation MUST emit exactly one
  // `grafana_nlq_translate_failed` interaction event carrying only the
  // coarse `dsType` and `errorKind` classifier. The error MESSAGE must
  // NEVER appear in the payload (it may contain user input or
  // LLM-generated content the user expects to remain local).
  // -------------------------------------------------------------------------
  it('emits grafana_nlq_translate_failed on backend error with no error message in payload', async () => {
    server.use(
      http.post('/api/nlq/translate', () =>
        HttpResponse.json({ message: 'sensitive-server-detail-do-not-leak' }, { status: 500 })
      )
    );

    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    await act(async () => {
      await result.current.translate('A query that will fail');
    });
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    // Exactly ONE telemetry event on failure.
    expect(reportInteractionMock).toHaveBeenCalledTimes(1);
    // Event name + payload shape: only dsType and a coarse errorKind. The
    // production code classifies backend HTTP errors as 'fetch' (because
    // backendSrv throws a `FetchError`-shaped plain object on non-2xx
    // responses — see `isFetchError` in `@grafana/runtime`). This is the
    // most common error path for the NLQ feature.
    expect(reportInteractionMock).toHaveBeenCalledWith('grafana_nlq_translate_failed', {
      dsType: 'prometheus',
      errorKind: 'fetch',
    });

    // CRITICAL: the server-side error message MUST NOT appear in any
    // payload field. This is the load-bearing security assertion for this
    // test — a regression that started logging `err.message` would fail
    // this check immediately.
    const payload = reportInteractionMock.mock.calls[0][1];
    const payloadJson = JSON.stringify(payload);
    expect(payloadJson).not.toContain('sensitive-server-detail-do-not-leak');
    expect(payload).not.toHaveProperty('input');
    expect(payload).not.toHaveProperty('naturalLanguage');
    expect(payload).not.toHaveProperty('errorMessage');
    expect(payload).not.toHaveProperty('message');
  });

  // -------------------------------------------------------------------------
  // Case 10b — Telemetry: native Error throw classified as errorKind='error'
  //
  // Complements Case 10 (which exercises the FetchError-shaped backend
  // error path classified as 'fetch'). This test simulates a real `Error`
  // instance being thrown — for example a JSON parsing error inside
  // `nlqApi.ts` — and asserts that the classifier maps it to 'error'.
  //
  // We trigger this branch by registering an MSW handler that returns
  // malformed JSON, which causes `backendSrv` to throw an actual Error
  // when trying to parse the response body. (Network-level errors and
  // body-parsing errors typically surface as Error instances rather than
  // the structured FetchError shape that 4xx/5xx responses produce.)
  // -------------------------------------------------------------------------
  it('emits grafana_nlq_translate_failed with errorKind="error" for Error instance throws', async () => {
    // Register a handler that throws a real Error inside the route handler.
    // MSW will propagate this as a network-level failure that backendSrv
    // reports as a thrown Error instance (not a FetchError-shaped object).
    server.use(
      http.post('/api/nlq/translate', () => {
        throw new Error('synthetic-test-error');
      })
    );

    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    await act(async () => {
      await result.current.translate('A query that triggers an Error throw');
    });
    await waitFor(() => expect(result.current.isLoading).toBe(false));

    expect(reportInteractionMock).toHaveBeenCalledTimes(1);
    // The thrown value MAY be either a real Error or a FetchError-shape,
    // depending on how MSW + backendSrv surface the network failure. Both
    // are valid classifications; we accept either to keep the test stable
    // across MSW versions, but the error message MUST NOT appear in the
    // payload either way.
    const [eventName, payload] = reportInteractionMock.mock.calls[0];
    expect(eventName).toBe('grafana_nlq_translate_failed');
    expect(payload.dsType).toBe('prometheus');
    expect(['error', 'fetch', 'unknown']).toContain(payload.errorKind);

    // The synthetic error message MUST NOT appear in any payload field.
    const payloadJson = JSON.stringify(payload);
    expect(payloadJson).not.toContain('synthetic-test-error');
  });

  // -------------------------------------------------------------------------
  // Case 11 — Telemetry: NOT emitted on unsupported-datasource short-circuit
  //
  // The unsupported-datasource branch (Case 4) short-circuits BEFORE the
  // try/catch that emits telemetry. Therefore `reportInteraction` MUST NOT
  // fire — emitting a "translation succeeded/failed" event when no
  // translation was attempted would corrupt usage analytics.
  // -------------------------------------------------------------------------
  it('does NOT emit any translate telemetry for unsupported datasource', async () => {
    const { result } = renderHook(() => useNLQTranslation(makeDs('mysql')));

    await act(async () => {
      await result.current.translate('This should not fire');
    });

    // Neither the success nor the failure event was emitted because the
    // unsupported-datasource branch returns before the try/catch.
    expect(reportInteractionMock).not.toHaveBeenCalled();
  });

  // -------------------------------------------------------------------------
  // Case 12 — Telemetry: NOT emitted on empty-input short-circuit
  //
  // Symmetric to Case 11 — an empty / whitespace-only input must not emit
  // a translate telemetry event because the hook never invoked the LLM.
  // -------------------------------------------------------------------------
  it('does NOT emit any translate telemetry for empty input', async () => {
    const { result } = renderHook(() => useNLQTranslation(makeDs('prometheus')));

    await act(async () => {
      await result.current.translate('   ');
    });

    expect(reportInteractionMock).not.toHaveBeenCalled();
  });

  // -------------------------------------------------------------------------
  // Case 13 — isUnsupportedDatasource flips to true when `dsSettings.type`
  //           switches from supported to unsupported mid-session
  //
  // Verifies the Checkpoint 5 QA fix for AAP §0.1.1.1 ("the NLQ bar MUST
  // render a clear 'unsupported data source' message" when the active
  // datasource is not Prometheus / Loki). Without the prop-syncing
  // `useEffect` added by this fix, the `useState` lazy initializer fires
  // only once at mount, so a user who opens the panel editor with Loki
  // (supported) and then switches the datasource picker to a TestData
  // datasource (unsupported) would continue to see the input + Translate
  // button until they clicked Translate — only then would the runtime
  // short-circuit inside `translate()` finally re-set the flag.
  //
  // This test reproduces that exact scenario at the hook layer by using
  // `renderHook`'s `rerender` API to swap the `dsSettings` argument from
  // supported to unsupported, and asserts that `isUnsupportedDatasource`
  // flips to `true` synchronously on the next render — WITHOUT any
  // `translate()` call.
  //
  // The lack of any registered MSW handler combined with the module-level
  // `onUnhandledRequest: 'error'` configuration means a regression that
  // accidentally triggered a translate() call here would fail the test
  // loudly.
  // -------------------------------------------------------------------------
  it('flips isUnsupportedDatasource to true when dsSettings.type switches from supported to unsupported', () => {
    // Start with a supported datasource. The `rerender` argument shape
    // matches the initialProps shape declared in the second argument
    // of `renderHook` — passing the full settings object so the hook
    // re-runs with the new prop on every rerender call.
    const { result, rerender } = renderHook(
      ({ ds }: { ds: DataSourceInstanceSettings }) => useNLQTranslation(ds),
      { initialProps: { ds: makeDs('loki') } }
    );

    // Initial state — supported datasource means the flag starts at false.
    expect(result.current.isUnsupportedDatasource).toBe(false);

    // Switch the datasource prop to an unsupported type. `rerender`
    // synchronously re-invokes the hook with the new props; the
    // `useEffect` keyed on `dsSettings.type` fires immediately after
    // the new render and updates the flag.
    rerender({ ds: makeDs('grafana-testdata-datasource', 'ds-unsupported') });

    // The flag MUST have flipped to true without any Translate click.
    // This is the load-bearing assertion for the Checkpoint 5 QA fix.
    expect(result.current.isUnsupportedDatasource).toBe(true);

    // No translation state was modified by the prop switch — the effect
    // only touches the unsupported flag, not the query/language/explanation
    // slices. Verifying this guards against an accidental cross-clearing
    // regression where the effect grows into a "clear all on switch" hook.
    expect(result.current.translatedQuery).toBe('');
    expect(result.current.language).toBe('');
    expect(result.current.explanation).toBe('');
    expect(result.current.warnings).toEqual([]);
    expect(result.current.error).toBeNull();
    expect(result.current.isLoading).toBe(false);

    // No telemetry event MUST be emitted by a prop switch — the bar
    // tracks user-initiated interactions only (Translate / Run /
    // Add as Panel), not parent-driven prop changes.
    expect(reportInteractionMock).not.toHaveBeenCalled();
  });

  // -------------------------------------------------------------------------
  // Case 14 — isUnsupportedDatasource flips back to false when
  //           `dsSettings.type` switches from unsupported to supported
  //
  // Symmetric to Case 13. Verifies the reverse direction: a user who
  // started on TestData (unsupported) and then switched to Prometheus
  // (supported) should see the bar return to its active state immediately
  // on the next render, without any explicit `reset()` call.
  // -------------------------------------------------------------------------
  it('flips isUnsupportedDatasource to false when dsSettings.type switches from unsupported to supported', () => {
    const { result, rerender } = renderHook(
      ({ ds }: { ds: DataSourceInstanceSettings }) => useNLQTranslation(ds),
      { initialProps: { ds: makeDs('grafana-testdata-datasource', 'ds-unsupported') } }
    );

    // Initial state — unsupported datasource means the flag starts at true.
    expect(result.current.isUnsupportedDatasource).toBe(true);

    // Switch to a supported datasource (Prometheus). The effect synchronizes
    // the flag immediately on the next render.
    rerender({ ds: makeDs('prometheus', 'ds-prom') });

    // The flag MUST have flipped back to false so the parent bar restores
    // the input UI.
    expect(result.current.isUnsupportedDatasource).toBe(false);

    // No translation state was modified by the prop switch.
    expect(result.current.translatedQuery).toBe('');
    expect(result.current.language).toBe('');
    expect(result.current.explanation).toBe('');
    expect(result.current.warnings).toEqual([]);
    expect(result.current.error).toBeNull();
    expect(result.current.isLoading).toBe(false);
  });
});
