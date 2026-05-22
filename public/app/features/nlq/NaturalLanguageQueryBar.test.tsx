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

import { DataSourceInstanceSettings } from '@grafana/data';
import { config } from '@grafana/runtime';
import { SceneObjectRef, VizPanel } from '@grafana/scenes';

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
// its own per-case handler via `server.use(...)`. The `onUnhandledRequest: 'bypass'`
// option is set in `beforeAll` so that unrelated requests (e.g. internal
// telemetry pings emitted by other modules during render) do not fail the
// suite. Individual tests that need stricter behavior can override on a
// per-test basis.
const server = setupServer();

beforeAll(() => {
  server.listen({ onUnhandledRequest: 'bypass' });
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

      render(<NaturalLanguageQueryBar dsSettings={makeDsSettings('prometheus')} panelRef={panelRefStub} />);

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
      render(<NaturalLanguageQueryBar dsSettings={makeDsSettings('prometheus')} panelRef={panelRefStub} />);

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

      render(<NaturalLanguageQueryBar dsSettings={makeDsSettings('prometheus')} panelRef={panelRefStub} />);

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

      render(<NaturalLanguageQueryBar dsSettings={makeDsSettings('loki')} panelRef={panelRefStub} />);

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

      render(<NaturalLanguageQueryBar dsSettings={makeDsSettings('prometheus')} panelRef={panelRefStub} />);

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

      render(<NaturalLanguageQueryBar dsSettings={makeDsSettings('mysql')} panelRef={panelRefStub} />);

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
      const user = userEvent.setup();

      render(
        <NaturalLanguageQueryBar
          dsSettings={makeDsSettings('prometheus')}
          panelRef={panelRefStub}
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
  });
});
