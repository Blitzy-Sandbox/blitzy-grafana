// NLQ feature: Jest + React Testing Library test suite for the
// `NaturalLanguageQueryBar` component.
//
// This file is the test counterpart of `./NaturalLanguageQueryBar.tsx`. It
// renders the bar end-to-end (collapsible section, textarea, translate
// button, preview, and alerts) and exercises the public contract documented
// in the AAP. Backend interactions are intercepted at the network layer by
// Mock Service Worker (MSW v2.10.4), and the Monaco-based `CodeEditor` is
// replaced by a lightweight `<textarea>` so assertions can read the
// translated query content as a plain string value without coupling the
// tests to Monaco internals (per AAP §0.6.1.4 explicit constraint
// "DO NOT test the CodeEditor's Monaco internals — assert by visible text
// content only").
//
// Coverage map (per AAP §0.6.4 validation criteria):
//   - Criterion #5 — Bar does NOT render when `config.featureToggles.nlqEnabled === false`
//   - Criterion #1 — Successful PromQL translation for Prometheus datasource
//   - Criterion #2 — Successful LogQL translation for Loki datasource
//   - Criterion #7 — Backend 500 surfaces an error `<Alert>`; the existing
//                    bar UI remains visible (the existing query editor below
//                    the bar is not unmounted)
//   - Criterion #9 — Unsupported datasource (e.g. MySQL) shows an
//                    "unsupported" `<Alert>` and does NOT issue an HTTP
//                    request
//   - Criterion AAP §0.1.1.1 (Checkpoint 5 QA fix) — switching the
//                    `dsSettings` prop from a supported datasource (e.g.
//                    Loki) to an unsupported datasource (e.g. TestData)
//                    while the bar is OPEN flips the rendered surface to
//                    the warning Alert IMMEDIATELY, without any
//                    Translate-button click.
//   - Criterion #4 — Clicking "Add as Panel" invokes the `onAddPanel`
//                    callback with the translated query
//
// Testing pattern conventions (per AAP §0.6.1.4):
//   - `render` comes from `'test/test-utils'` (NOT directly from
//     `@testing-library/react`), exactly mirroring the established Grafana
//     pattern at `public/app/features/manage-dashboards/components/
//     PublicDashboardListTable/PublicDashboardListTable.test.tsx`.
//   - MSW handlers use the modern v2 API: `http.post(...)` from `'msw'`
//     and `setupServer(...)` from `'msw/node'` (NOT the legacy `rest` API).
//   - `getBackendSrv()` is overridden via `jest.mock('@grafana/runtime', ...)`
//     to return the real `backendSrv` singleton so that the production code
//     path (`postTranslate` → `getBackendSrv().post()` → real `fetch`) is
//     exercised end-to-end, with MSW intercepting only the outbound network
//     request. This matches the convention in
//     `PublicDashboardListTable.test.tsx`.
//   - `reportInteraction` is stubbed with `jest.fn()` through the same
//     `jest.mock` so its underlying `EchoSrv` dependency does not need to be
//     initialized in jsdom.
//   - `config.featureToggles.nlqEnabled` is mutated per test case via direct
//     assignment on the shared `config` singleton (`@grafana/runtime`),
//     mirroring the precedent in
//     `public/app/features/dashboard-scene/panel-edit/PanelDataPane/
//     EmptyTransformationsMessage.test.tsx`. The flag is reset to `false` in
//     `afterEach` so each test starts from a known baseline.

import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import { render } from 'test/test-utils';

import type { DataSourceInstanceSettings } from '@grafana/data';
import { config } from '@grafana/runtime';
import type { SceneObjectRef, VizPanel } from '@grafana/scenes';

// `parseCssLengthToPx` is a pure helper exported alongside `NLQQueryPreview`.
// Its tests live in this file (and not in a standalone `NLQQueryPreview.test.tsx`)
// because the AAP §0.3.2.2 explicitly lists only two frontend test files in
// scope: this file and `useNLQTranslation.test.ts`. Consolidating the helper
// tests here keeps the test inventory aligned with the AAP-approved scope per
// Checkpoint 4 review feedback ("Either add the file to the formal scope or
// remove/consolidate this test coverage into an in-scope test file").
import { parseCssLengthToPx } from './NLQQueryPreview';
import { NaturalLanguageQueryBar } from './NaturalLanguageQueryBar';

// ---------------------------------------------------------------------------
// Module mocks
// ---------------------------------------------------------------------------

// Mock @grafana/runtime to:
//   1. Spread the actual exports so `config`, `getDataSourceSrv`, theming
//      helpers and any other utilities continue to work normally.
//   2. Override `getBackendSrv()` to return the REAL `backendSrv` singleton
//      so MSW intercepts the actual `fetch` call issued by `nlqApi.ts`
//      → `useNLQTranslation` → `NaturalLanguageQueryBar`. We use
//      `jest.requireActual('app/core/services/backend_srv').backendSrv`
//      inside the factory rather than a closed-over top-level import so the
//      reference is resolved lazily at mock invocation time — avoiding any
//      hoisting / temporal-dead-zone hazards that arise when Jest hoists the
//      `jest.mock(...)` registration above the file's import statements.
//   3. Stub `reportInteraction` with `jest.fn()` so the telemetry call from
//      `NaturalLanguageQueryBar` (which fires on bar open, translate click,
//      run click, and add-as-panel click) does not require an initialized
//      `EchoSrv` in the jsdom environment.
jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getBackendSrv: () => jest.requireActual('app/core/services/backend_srv').backendSrv,
  reportInteraction: jest.fn(),
}));

// Mock @grafana/ui to replace the Monaco-based `CodeEditor` with a simple
// `<textarea>` element. This is the canonical pattern used elsewhere in the
// codebase (e.g. `public/app/features/alerting/unified/Templates.test.tsx`)
// because Monaco does not render reliably in jsdom and asserting on Monaco's
// internal DOM is forbidden by AAP §0.6.1.4 ("assert by visible text content
// only"). Every other `@grafana/ui` export remains the real implementation,
// so `Button`, `Alert`, `CollapsableSection`, `TextArea`, etc. all behave
// normally and continue to honor their accessibility primitives.
jest.mock('@grafana/ui', () => {
  const actual = jest.requireActual('@grafana/ui');
  return {
    ...actual,
    // The mock surfaces only the four props the test inspects (`value`,
    // `language`, `onBlur`, `onSave`). The real `CodeEditor` accepts many
    // more props, but they are irrelevant to test-side assertions and Jest
    // will simply pass them through to the underlying textarea (ignored).
    //
    // Both `onBlur` and `onSave` are wired by the real `NLQQueryPreview` to
    // the same parent `onChange` callback; we route the mock textarea's
    // `onChange` event through both so user-driven edits propagate
    // identically regardless of which signal the production code listens to.
    CodeEditor: function MockCodeEditor({
      value,
      language,
      onBlur,
      onSave,
    }: {
      value: string;
      language?: string;
      onBlur?: (newValue: string) => void;
      onSave?: (newValue: string) => void;
    }) {
      return (
        <textarea
          data-testid="nlq-mock-code-editor"
          data-language={language ?? ''}
          value={value ?? ''}
          onChange={(e) => {
            const next = e.currentTarget.value;
            onBlur?.(next);
            onSave?.(next);
          }}
        />
      );
    },
  };
});

// ---------------------------------------------------------------------------
// MSW server lifecycle
// ---------------------------------------------------------------------------

// A bare MSW server is created with no default handlers — each test registers
// its own per-case handler via `server.use(...)`.
//
// `onUnhandledRequest: 'error'` is the FAIL-FAST setting: any HTTP request the
// component issues that is NOT explicitly handled by a registered MSW handler
// will reject loudly and fail the test. This is the safer default because the
// component MUST NOT issue any HTTP request beyond `POST /api/nlq/translate`
// (e.g. the unsupported-datasource test asserts that NO request is made even
// when the user clicks "Translate"). `'bypass'` would silently let any
// regression that bypassed the short-circuit guard through, which is
// precisely the failure mode we want to detect.
const server = setupServer();

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'error' });
});

afterEach(() => {
  // Drop all per-test handlers so leakage from one test cannot pollute the
  // next. Also clear the `reportInteraction` jest.fn() and any other mocks
  // so call counts are isolated per test.
  server.resetHandlers();
  jest.clearAllMocks();
});

afterAll(() => {
  // Tear down the MSW interceptor cleanly so it does not leak into other
  // test files when Jest runs the suite under `--maxWorkers > 1`.
  server.close();
});

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

/**
 * Constructs a minimal `DataSourceInstanceSettings` fixture for the supplied
 * datasource `type`.
 *
 * The `NaturalLanguageQueryBar` (and the `useNLQTranslation` hook it
 * delegates to) only reads `dsSettings.uid` and `dsSettings.type` — the
 * remaining fields are stubbed with the smallest plausible values so the
 * structural interface is satisfied without re-declaring every member.
 *
 * The double cast (`as unknown as DataSourceInstanceSettings`) is required
 * because `DataSourceInstanceSettings` is a large structural interface;
 * enumerating every field would duplicate the production type with no
 * additional test value. This cast is permitted in test files because the
 * project's `@typescript-eslint/consistent-type-assertions: never` rule is
 * disabled for test files via `commonTestIgnores` (see `eslint.config.js`).
 *
 * @param type — The `dsSettings.type` value used both for the supported-
 *   datasource guard (`prometheus`/`loki` → supported, everything else →
 *   unsupported) and as the `datasourceType` field in the request payload.
 * @param uid — Defaults to `'ds-test'`. Sent as `datasourceUid` in the
 *   request payload.
 */
function makeDsSettings(type: string, uid = 'ds-test'): DataSourceInstanceSettings {
  return {
    id: 1,
    uid,
    type,
    name: `${type}-test`,
    // The bar and hook never inspect `meta`, but the field is required on the
    // structural interface, so we stub the minimum shape (`id` + `name`).
    meta: { id: type, name: type },
    jsonData: {},
    readOnly: false,
    access: 'proxy',
  } as unknown as DataSourceInstanceSettings;
}

/**
 * Minimal `panelRef` stub. The `NaturalLanguageQueryBar` component
 * deliberately does NOT invoke any scene APIs on the ref in the
 * translation/preview flow — `panelRef` exists on the prop surface only so
 * call-site consumers can wire scene-side flows around it (see the prop
 * JSDoc in `NaturalLanguageQueryBar.tsx`). A no-op `resolve()` therefore
 * satisfies the type contract without forcing the test to spin up a real
 * `VizPanel` instance.
 */
const panelRefStub = {
  resolve: () => undefined,
} as unknown as SceneObjectRef<VizPanel>;

/**
 * Required-callback stubs. As of Checkpoint 4 review feedback (C3), the
 * `NaturalLanguageQueryBar` declares `onRun` and `onAddPanel` as REQUIRED
 * props — making them optional caused the production call site to silently
 * no-op when callbacks were omitted. Tests that do not exercise the
 * callbacks themselves still need to satisfy the type contract, so each
 * `render()` call passes a fresh `jest.fn()` pair from this helper.
 *
 * Returning fresh mocks per call (instead of module-level constants)
 * guarantees that call counts from prior tests cannot bleed into the
 * current test's assertions if jest module isolation ever weakens.
 */
function makeCallbackStubs() {
  return {
    onRun: jest.fn(),
    onAddPanel: jest.fn(),
  };
}

/**
 * Default MSW handler factory for the happy-path `POST /api/nlq/translate`
 * endpoint. Returns a PromQL response for Prometheus datasources and a
 * LogQL response for Loki datasources, mirroring the production server
 * behavior closely enough that the bar's UI renders correctly.
 *
 * Tests register this handler explicitly via `server.use(...)` rather than
 * having it registered globally because some tests (e.g. the 500-error
 * test) need to register a different handler for the same route.
 */
function registerHappyPathHandler() {
  server.use(
    http.post('/api/nlq/translate', async ({ request }) => {
      const body = (await request.json()) as { datasourceType: string };
      const language = body.datasourceType === 'loki' ? 'logql' : 'promql';
      const query = language === 'logql' ? '{job="syslog"} |= "Failed password"' : 'rate(http_requests_total[5m])';
      return HttpResponse.json({
        query,
        language,
        explanation: 'Translated from natural language',
        warnings: [],
      });
    })
  );
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('NaturalLanguageQueryBar', () => {
  // Default to "enabled" so most tests start from the canonical render state.
  // Individual feature-flag-gating tests override this assignment before
  // their own `render()` call.
  beforeEach(() => {
    config.featureToggles.nlqEnabled = true;
  });

  // Reset to the safe default after every test so a previous test's mutation
  // cannot bleed into the next file's tests if jest module isolation ever
  // weakens.
  afterEach(() => {
    config.featureToggles.nlqEnabled = false;
  });

  // -------------------------------------------------------------------------
  // Group 1 — Feature-toggle gating
  // -------------------------------------------------------------------------
  describe('feature toggle gating', () => {
    // AAP §0.6.4 criterion #5: the bar MUST NOT render when the feature flag
    // is off, even if the component is imported directly into a tree. This
    // mirrors the defense-in-depth `return null` guard at the top of the
    // production component.
    it('renders nothing when config.featureToggles.nlqEnabled is false', () => {
      config.featureToggles.nlqEnabled = false;

      const cb = makeCallbackStubs();
      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('prometheus')}
          panelRef={panelRefStub}
          onRun={cb.onRun}
          onAddPanel={cb.onAddPanel}
        />
      );

      // The component returns `null` when the flag is off, so neither the
      // outer wrapper (`data-testid="nlq-bar"`) nor any of the visible
      // strings appear in the rendered tree.
      expect(screen.queryByTestId('nlq-bar')).not.toBeInTheDocument();
      expect(screen.queryByText('Ask a question')).not.toBeInTheDocument();
    });

    // The complementary case: when the flag is on, the bar renders. We
    // assert both the wrapper `data-testid` and the human-readable title to
    // catch regressions in either the test-id contract or the i18n default
    // string.
    it('renders the collapsible section when nlqEnabled is true', () => {
      const cb = makeCallbackStubs();
      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('prometheus')}
          panelRef={panelRefStub}
          onRun={cb.onRun}
          onAddPanel={cb.onAddPanel}
        />
      );

      expect(screen.getByTestId('nlq-bar')).toBeInTheDocument();
      expect(screen.getByText('Ask a question')).toBeInTheDocument();
    });
  });

  // -------------------------------------------------------------------------
  // Group 2 — Translation happy path
  // -------------------------------------------------------------------------
  describe('translation happy path', () => {
    // AAP §0.6.4 criterion #1: a Prometheus datasource produces a PromQL
    // response and the preview renders with `language="promql"`. The test
    // walks the full user flow: open the collapsible, type a prompt, click
    // Translate, await the preview, and read back the editor's value.
    it('translates and shows the preview with PromQL for prometheus datasource', async () => {
      registerHappyPathHandler();
      const user = userEvent.setup();

      const cb = makeCallbackStubs();
      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('prometheus')}
          panelRef={panelRefStub}
          onRun={cb.onRun}
          onAddPanel={cb.onAddPanel}
        />
      );

      // Open the collapsible — `CollapsableSection` mounts the children
      // lazily, so the input is not in the DOM until the user expands.
      await user.click(screen.getByRole('button', { name: /Ask a question/i }));

      const input = await screen.findByTestId('nlq-bar-input');
      await user.type(input, 'Show me failed login attempts in the last hour grouped by IP');

      await user.click(screen.getByTestId('nlq-bar-translate-button'));

      // Wait for the preview to render with the LLM-generated PromQL. The
      // mocked `CodeEditor` is a controlled `<textarea>`, so `toHaveValue`
      // is the correct matcher.
      const codeEditor = await screen.findByTestId('nlq-mock-code-editor');
      await waitFor(() => {
        expect(codeEditor).toHaveValue('rate(http_requests_total[5m])');
      });

      // The mock CodeEditor records the `language` prop as a data attribute
      // so we can verify the language discriminator without depending on
      // Monaco's internal state.
      expect(codeEditor).toHaveAttribute('data-language', 'promql');

      // The bar should also surface the LLM's explanation text alongside the
      // editor so the user can see how the query was constructed.
      expect(screen.getByText('Translated from natural language')).toBeInTheDocument();
    });

    // AAP §0.6.4 criterion #2: a Loki datasource produces a LogQL response.
    // The CodeEditor's `data-language` attribute MUST switch to `'logql'` so
    // the Monaco editor picks up the LogQL tokenizer in the non-mocked
    // production path.
    it('uses logql language for loki datasource', async () => {
      registerHappyPathHandler();
      const user = userEvent.setup();

      const cb = makeCallbackStubs();
      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('loki')}
          panelRef={panelRefStub}
          onRun={cb.onRun}
          onAddPanel={cb.onAddPanel}
        />
      );

      await user.click(screen.getByRole('button', { name: /Ask a question/i }));
      const input = await screen.findByTestId('nlq-bar-input');
      await user.type(input, 'Show IPs with more than 10 failed SSH login attempts in the last 30 minutes');
      await user.click(screen.getByTestId('nlq-bar-translate-button'));

      const codeEditor = await screen.findByTestId('nlq-mock-code-editor');
      await waitFor(() => {
        expect(codeEditor).toHaveValue('{job="syslog"} |= "Failed password"');
      });
      expect(codeEditor).toHaveAttribute('data-language', 'logql');
    });
  });

  // -------------------------------------------------------------------------
  // Group 3 — Error and unsupported-datasource paths
  // -------------------------------------------------------------------------
  describe('error and unsupported paths', () => {
    // AAP §0.6.4 criterion #7: when the backend returns 5xx, the bar surfaces
    // an error `<Alert>` rather than crashing or unmounting. The bar's input
    // field MUST remain enabled so the user can retry with a different
    // prompt — this is the graceful degradation contract from §0.1.1.1.
    it('shows an error Alert when the backend returns 500', async () => {
      server.use(http.post('/api/nlq/translate', () => HttpResponse.json({ message: 'boom' }, { status: 500 })));

      const user = userEvent.setup();

      const cb = makeCallbackStubs();
      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('prometheus')}
          panelRef={panelRefStub}
          onRun={cb.onRun}
          onAddPanel={cb.onAddPanel}
        />
      );

      await user.click(screen.getByRole('button', { name: /Ask a question/i }));
      const input = await screen.findByTestId('nlq-bar-input');
      await user.type(input, 'A query that will fail');
      await user.click(screen.getByTestId('nlq-bar-translate-button'));

      // The error alert appears once the request settles. We use
      // `findByTestId` (with its built-in waitFor semantics) so the test
      // does not assert prematurely while the request is still in flight.
      expect(await screen.findByTestId('nlq-bar-error-alert')).toBeInTheDocument();

      // The translated-query preview MUST NOT be rendered when an error is
      // active — the hook clears all derived translation state on failure
      // so the UI never shows a stale query alongside the error alert.
      expect(screen.queryByTestId('nlq-mock-code-editor')).not.toBeInTheDocument();

      // The input field stays in the document and remains usable, so the
      // user can refine the prompt and retry without re-opening the bar.
      expect(screen.getByTestId('nlq-bar-input')).toBeInTheDocument();
    });

    // AAP §0.6.4 criterion #9: when the active datasource is outside
    // {prometheus, loki}, the bar shows an "unsupported data source" alert
    // and MUST NOT issue any HTTP request. We register a spy handler that
    // would fire if the bar ever calls the endpoint, and assert it was
    // never invoked.
    it('shows unsupported-datasource Alert and does not call backend for mysql', async () => {
      const handlerSpy = jest.fn();
      server.use(
        http.post('/api/nlq/translate', () => {
          handlerSpy();
          return HttpResponse.json({ query: 'should-not-be-called', language: 'promql' });
        })
      );

      const user = userEvent.setup();

      const cb = makeCallbackStubs();
      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('mysql')}
          panelRef={panelRefStub}
          onRun={cb.onRun}
          onAddPanel={cb.onAddPanel}
        />
      );

      // The bar renders in its collapsed state initially; opening it
      // reveals the unsupported-datasource alert in place of the input UI.
      await user.click(screen.getByRole('button', { name: /Ask a question/i }));

      // The unsupported alert appears, and the input + translate button are
      // BOTH absent — the bar deliberately hides the active UI to make it
      // visually clear that no LLM call is possible for this datasource.
      expect(await screen.findByTestId('nlq-bar-unsupported-alert')).toBeInTheDocument();
      expect(screen.queryByTestId('nlq-bar-input')).not.toBeInTheDocument();
      expect(screen.queryByTestId('nlq-bar-translate-button')).not.toBeInTheDocument();

      // The MSW spy MUST NOT have been called. This is the load-bearing
      // assertion for criterion #9 — the unsupported-datasource guard
      // exists precisely to prevent wasted LLM calls.
      expect(handlerSpy).not.toHaveBeenCalled();
    });

    // AAP §0.1.1.1 (Checkpoint 5 QA fix): When the user switches the
    // active panel datasource from a supported type (e.g. Loki) to an
    // unsupported type (e.g. grafana-testdata-datasource) WHILE the bar
    // is open, the warning Alert MUST replace the input/preview UI on
    // the very next render — without requiring the user to click
    // Translate. The QA report's MAJOR finding (Issue #1) identified that
    // the lazy `useState` initializer in `useNLQTranslation.ts` ran only
    // ONCE at mount, so the unsupported-datasource state was stale after
    // the prop changed. The `useEffect` synchronization added by this fix
    // closes that gap; this test is the regression guard for the visual
    // contract at the component level.
    it('shows unsupported-datasource Alert immediately when dsSettings prop switches from supported to unsupported', async () => {
      const handlerSpy = jest.fn();
      server.use(
        http.post('/api/nlq/translate', () => {
          handlerSpy();
          return HttpResponse.json({ query: 'should-not-be-called', language: 'promql' });
        })
      );

      const user = userEvent.setup();

      const cb = makeCallbackStubs();
      // Start with a SUPPORTED datasource (Loki). The bar opens cleanly
      // and shows the input UI as it would in production when the user
      // first lands on the panel editor with Loki as their last-used DS.
      const { rerender } = render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('loki', 'ds-loki')}
          panelRef={panelRefStub}
          onRun={cb.onRun}
          onAddPanel={cb.onAddPanel}
        />
      );

      // Open the collapsible so the supported-state UI is mounted.
      await user.click(screen.getByRole('button', { name: /Ask a question/i }));

      // Sanity: with the supported datasource, the input + translate
      // button render and the unsupported alert is absent.
      expect(await screen.findByTestId('nlq-bar-input')).toBeInTheDocument();
      expect(screen.getByTestId('nlq-bar-translate-button')).toBeInTheDocument();
      expect(screen.queryByTestId('nlq-bar-unsupported-alert')).not.toBeInTheDocument();

      // Switch the datasource prop to an UNSUPPORTED type while the bar
      // is still open. This is the exact scenario from the QA report:
      // the user changes the active datasource via the picker without
      // discarding the panel.
      rerender(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('grafana-testdata-datasource', 'ds-unsupported')}
          panelRef={panelRefStub}
          onRun={cb.onRun}
          onAddPanel={cb.onAddPanel}
        />
      );

      // The unsupported Alert MUST appear on the very next render. We use
      // `findByTestId` so any micro-task latency from the prop-sync
      // `useEffect` is awaited; the visual contract from AAP §0.1.1.1
      // requires IMMEDIATE feedback, which means within one effect tick
      // — not a long-running async settle.
      expect(await screen.findByTestId('nlq-bar-unsupported-alert')).toBeInTheDocument();

      // The input and Translate button MUST both be removed from the DOM
      // — the bar deliberately hides the active UI for unsupported
      // datasources to make it visually clear that no LLM call is
      // possible.
      expect(screen.queryByTestId('nlq-bar-input')).not.toBeInTheDocument();
      expect(screen.queryByTestId('nlq-bar-translate-button')).not.toBeInTheDocument();

      // The MSW spy MUST NOT have been called. The whole reason the QA
      // bug was MAJOR (not CRITICAL) is that the SECURITY contract was
      // intact — the runtime short-circuit inside `translate()`
      // prevented the HTTP call. This assertion confirms that contract
      // continues to hold across the prop-sync fix.
      expect(handlerSpy).not.toHaveBeenCalled();
    });
  });

  // -------------------------------------------------------------------------
  // Group 4 — Add-as-Panel callback
  // -------------------------------------------------------------------------
  describe('add-as-panel callback', () => {
    // AAP §0.6.4 criterion #4: when the user clicks "Add as Panel" after a
    // successful translation, the `onAddPanel` callback MUST be invoked with
    // the (possibly user-edited) translated query and the resolved language
    // discriminator. This is the integration point with the existing
    // panel-creation flow in `PanelDataQueriesTab.tsx`.
    it('invokes onAddPanel with the translated query and language', async () => {
      registerHappyPathHandler();
      const onAddPanel = jest.fn();
      const onRun = jest.fn();
      const user = userEvent.setup();

      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('prometheus')}
          panelRef={panelRefStub}
          onRun={onRun}
          onAddPanel={onAddPanel}
        />
      );

      await user.click(screen.getByRole('button', { name: /Ask a question/i }));
      const input = await screen.findByTestId('nlq-bar-input');
      await user.type(input, 'Graph total API request rate by endpoint over the last 24 hours');
      await user.click(screen.getByTestId('nlq-bar-translate-button'));

      // Wait for the preview to render before clicking "Add as Panel" —
      // the Add-as-Panel button lives inside `NLQQueryPreview`, which only
      // mounts after a successful translation.
      await screen.findByTestId('nlq-mock-code-editor');

      // Click "Add as Panel". The button label is rendered via `<Trans>`
      // from `@grafana/i18n`, so it resolves to the default English string
      // in the test environment (i18next is initialized with `lng: 'en-US'`
      // in `public/test/setupTests.ts`).
      await user.click(screen.getByRole('button', { name: /Add as Panel/i }));

      // The callback receives the unedited translated query (we did not
      // touch the mocked CodeEditor's textarea) and the PromQL language
      // discriminator. The hook seeds `editedQuery` from `translatedQuery`
      // when a new translation arrives, so the value passed to the callback
      // is the LLM's raw output for this test.
      await waitFor(() => {
        expect(onAddPanel).toHaveBeenCalledWith('rate(http_requests_total[5m])', 'promql');
      });
      expect(onAddPanel).toHaveBeenCalledTimes(1);
    });

    // AAP §0.6.4 criterion #3 (in spirit): when the user clicks "Run" after a
    // successful translation, the `onRun` callback MUST be invoked with the
    // (possibly user-edited) translated query and the resolved language. This
    // is symmetric to the "Add as Panel" callback assertion above and exists
    // because both callbacks are now REQUIRED props per Checkpoint 4 review
    // (C3) — the previous optional-with-no-op contract caused the integrated
    // Run/Add-as-Panel buttons to silently no-op in production.
    it('invokes onRun with the translated query and language', async () => {
      registerHappyPathHandler();
      const onAddPanel = jest.fn();
      const onRun = jest.fn();
      const user = userEvent.setup();

      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('prometheus')}
          panelRef={panelRefStub}
          onRun={onRun}
          onAddPanel={onAddPanel}
        />
      );

      await user.click(screen.getByRole('button', { name: /Ask a question/i }));
      const input = await screen.findByTestId('nlq-bar-input');
      await user.type(input, 'Graph total API request rate by endpoint over the last 24 hours');
      await user.click(screen.getByTestId('nlq-bar-translate-button'));

      // Wait for the preview before clicking Run.
      await screen.findByTestId('nlq-mock-code-editor');

      // The Run button lives in `NLQQueryPreview`. Its accessible name is
      // localised via `<Trans>` and resolves to "Run" in the test env.
      await user.click(screen.getByRole('button', { name: /^Run$/i }));

      // The callback receives the unedited translated query and the PromQL
      // language discriminator. `onAddPanel` MUST NOT be invoked by a Run
      // click — the two actions are independent.
      await waitFor(() => {
        expect(onRun).toHaveBeenCalledWith('rate(http_requests_total[5m])', 'promql');
      });
      expect(onRun).toHaveBeenCalledTimes(1);
      expect(onAddPanel).not.toHaveBeenCalled();
    });
  });
});

// ---------------------------------------------------------------------------
// Helper unit tests: parseCssLengthToPx (from NLQQueryPreview)
// ---------------------------------------------------------------------------
//
// These tests are colocated here (rather than in a separate
// `NLQQueryPreview.test.tsx`) per Checkpoint 4 review feedback — the AAP
// §0.3.2.2 explicitly lists only `NaturalLanguageQueryBar.test.tsx` and
// `useNLQTranslation.test.ts` as in-scope frontend test files. The helper
// is tested directly because Monaco's `CodeEditor` accepts a numeric
// pixel `fontSize` / `lineHeight` / `padding` / `height`, whereas the
// GrafanaTheme2 token values are strings like `"12px"` or `"0.75rem"`. A
// regression in the parser would silently fall back to the default pixel
// constants, defeating the design-system fix that resolves the MINOR
// "hardcoded Monaco values must use GrafanaTheme2 tokens" finding — so
// the parser invariants are pinned with direct unit tests.
describe('parseCssLengthToPx', () => {
  it('parses a pixel string like "12px"', () => {
    expect(parseCssLengthToPx('12px', 99)).toBe(12);
  });

  it('parses a fractional pixel string like "13.5px"', () => {
    expect(parseCssLengthToPx('13.5px', 99)).toBe(13.5);
  });

  it('parses a rem string using the standard 16px-per-rem ratio', () => {
    // 0.75rem * 16 == 12px
    expect(parseCssLengthToPx('0.75rem', 99)).toBe(12);
  });

  it('parses a whitespace-padded value', () => {
    expect(parseCssLengthToPx('  14px  ', 99)).toBe(14);
  });

  it('parses a bare numeric string as pixels', () => {
    expect(parseCssLengthToPx('16', 99)).toBe(16);
  });

  it('passes through a numeric input unchanged', () => {
    expect(parseCssLengthToPx(20, 99)).toBe(20);
  });

  it('falls back to the default when value is undefined', () => {
    expect(parseCssLengthToPx(undefined, 99)).toBe(99);
  });

  it('falls back to the default for an unrecognised unit', () => {
    // "12em" is intentionally NOT supported — em depends on the parent
    // font size which Monaco cannot compute. The fallback prevents the
    // helper from silently producing a nonsense pixel count.
    expect(parseCssLengthToPx('12em', 99)).toBe(99);
  });

  it('falls back to the default for a non-numeric string', () => {
    expect(parseCssLengthToPx('not-a-length', 99)).toBe(99);
  });

  it('falls back to the default for NaN inputs', () => {
    expect(parseCssLengthToPx(NaN, 99)).toBe(99);
  });
});
