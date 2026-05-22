// NLQ feature: custom React hook that encapsulates the state and HTTP
// lifecycle for natural-language → query translation.
//
// Responsibilities (per AAP §0.6.1.4):
//   - Owns the local UI state that backs the NLQ bar's "Translated" view
//     (translatedQuery, language, explanation, warnings, isLoading, error).
//   - Exposes an async `translate(input)` function that POSTs the user's
//     natural-language question to the backend `POST /api/nlq/translate`
//     endpoint via the thin `nlqApi.postTranslate` wrapper, then commits the
//     response to local state.
//   - Exposes a `reset()` function that clears all derived translation state
//     (used by the bar when the user changes datasources, re-opens the
//     collapsible, or otherwise abandons a previous translation).
//   - Enforces a closed datasource scope guard: when the active datasource
//     `type` is not in {`prometheus`, `loki`}, `translate()` returns
//     immediately without issuing an HTTP request and `isUnsupportedDatasource`
//     resolves to `true` so the parent can render the unsupported alert
//     (AAP §0.6.3.1).
//
// Design constraints (per AAP §0.6.1.4 and §0.8):
//   - Uses `useState`, `useCallback`, and a single `useEffect` from React.
//     The hook stays deliberately minimal: no `useReducer`, no `useMemo`,
//     no caching, no debouncing. The lone `useEffect` (added per the
//     Checkpoint 5 QA finding for AAP §0.1.1.1) synchronizes the
//     `isUnsupportedDatasource` flag with the current `dsSettings.type`
//     prop on every change — without it, the flag would be stale across
//     datasource switches because `useState` lazy initializers fire only
//     once at component mount. The runtime short-circuit inside
//     `translate()` provides defense-in-depth on top of the effect.
//   - Never imports `getBackendSrv` from `@grafana/runtime`. The HTTP call is
//     delegated to `./nlqApi.postTranslate`, which isolates the runtime
//     dependency to a single module. This keeps the hook trivially testable:
//     consumers can stub `postTranslate` directly, avoiding the need to mock
//     `@grafana/runtime`.
//   - Never adds caching, debouncing, retry, polling, or any other
//     resilience pattern. Per AAP §0.2.1 the translate endpoint is
//     request/response with no caching benefit, so RTK Query / SWR / React
//     Query / similar libraries are deliberately NOT used.
//   - Never logs the user's input or the LLM response payload. The LLM
//     credential never reaches the browser — it is held server-side only
//     (see backend service docs for the exact mechanism). The no-log
//     constraint is enforced here to prevent accidental telemetry of
//     user-typed natural-language content.
//   - Emits TWO post-outcome telemetry events via `reportInteraction` from
//     `@grafana/runtime`, per AAP §0.6.3.2:
//       - `grafana_nlq_translate_succeeded` fired exactly once when the
//         backend HTTP call resolves successfully. Payload carries only
//         non-sensitive metadata: `dsType`, `queryLength` (count, not
//         contents), and `hadWarnings` (boolean).
//       - `grafana_nlq_translate_failed` fired exactly once when the
//         backend HTTP call rejects. Payload carries only `dsType` and a
//         coarse `errorKind` classifier — NEVER the underlying error
//         message, the user's input, or any LLM-generated content.
//     This implements the no-leak-in-telemetry discipline applied to user
//     content (mirrors the API key handling on the backend per AAP §0.8.5).
//   - Single-turn only: each call to `translate()` is independent; the hook
//     does NOT maintain conversation history (AAP §0.7.2.3).

import { useCallback, useEffect, useState } from 'react';

import type { DataSourceInstanceSettings } from '@grafana/data';
import { isFetchError, reportInteraction } from '@grafana/runtime';

import { postTranslate } from './nlqApi';

/**
 * NLQ feature: closed set of datasource `type` strings for which the NLQ bar
 * will actually call the backend translation service.
 *
 * Per AAP §0.6.3.1, only Prometheus-compatible backends (`prometheus`) and
 * Loki (`loki`) are in scope for this release. Mimir uses the `prometheus`
 * plugin type (Mimir is wire-compatible with the Prometheus query API), so
 * it is supported by inclusion in this constant — no separate `mimir` entry
 * is required.
 *
 * This constant is the single source of truth for the supported set: the
 * runtime type guard iterates it via `Array.prototype.some` (see
 * {@link isSupportedDatasourceType}), and the `SupportedDsType` type alias is
 * derived from it via `(typeof SUPPORTED_DS_TYPES)[number]`. Adding a new
 * datasource type to the supported set is therefore a single-line change here.
 *
 * The `as const` assertion narrows the array to a readonly tuple of literal
 * types, which is required so that `SupportedDsType` resolves to the precise
 * union `'prometheus' | 'loki'` rather than the widened `string`.
 */
export const SUPPORTED_DS_TYPES = ['prometheus', 'loki'] as const;

/**
 * NLQ feature: string literal union of the datasource types for which the
 * NLQ bar will actually call the backend translation service. Derived from
 * {@link SUPPORTED_DS_TYPES} so the two stay in lockstep.
 */
type SupportedDsType = (typeof SUPPORTED_DS_TYPES)[number];

/**
 * NLQ feature: user-defined type guard that determines whether the active
 * datasource type is in the supported set.
 *
 * Implemented as a type predicate (`type is SupportedDsType`) so TypeScript
 * can narrow the input in the call site, even though we do not currently rely
 * on that narrowing — including the predicate keeps the function future-proof
 * if a caller ever needs the narrowed type.
 *
 * Iterates through {@link SUPPORTED_DS_TYPES} with `Array.prototype.some` so
 * the supported set is read directly from the constant — adding a new
 * supported datasource type requires editing only the constant above.
 *
 * `some` (rather than `includes`) is used because `SUPPORTED_DS_TYPES.includes`
 * requires its argument to be one of the tuple's literal types, which would
 * require a type assertion to call with a `string` input — the project's
 * `@typescript-eslint/consistent-type-assertions: never` rule forbids such
 * assertions. The `some` callback widens each element to `string` naturally
 * via the comparison, avoiding the need for any cast.
 */
function isSupportedDatasourceType(type: string): type is SupportedDsType {
  return SUPPORTED_DS_TYPES.some((supported) => supported === type);
}

/**
 * NLQ feature: discriminator for the Monaco editor language used by
 * `NLQQueryPreview` to render the translated query with syntax highlighting.
 *
 * - `'promql'` — for Prometheus / Mimir responses
 * - `'logql'` — for Loki responses
 * - `''` — initial/cleared state (no translation has been produced yet, or
 *   the previous translation was reset via `reset()` or cleared by an error)
 *
 * The empty-string sentinel is preferred over `undefined` so that downstream
 * consumers (the `CodeEditor` component) can treat `language` as a plain
 * string with no nullish checks.
 */
export type NLQLanguage = 'promql' | 'logql' | '';

/**
 * NLQ feature: shape of the value returned by {@link useNLQTranslation}.
 *
 * This is the full public contract of the hook. Consumers (the
 * `NaturalLanguageQueryBar` component) destructure these members; the names
 * MUST stay aligned with the JSDoc here and with the AAP-declared spec.
 */
export interface UseNLQTranslationResult {
  /**
   * NLQ feature: fires the translation request.
   *
   * Short-circuits (returns without making an HTTP call) when:
   *   - the active datasource type is not in `{prometheus, loki}`
   *     (per `isUnsupportedDatasource`); OR
   *   - the trimmed input string is empty.
   *
   * On success, all translation state fields are populated from the response.
   * On failure, `error` is populated and all translation state fields are
   * cleared so the UI never shows a stale query alongside an error alert.
   *
   * Resolves (does NOT reject) on translation failure — the error is captured
   * in the `error` field instead. This lets consumers stay in JSX rather than
   * wrapping every call site in a try/catch.
   */
  translate: (input: string) => Promise<void>;

  /**
   * NLQ feature: clears all translation state.
   *
   * Resets `translatedQuery`, `language`, `explanation`, `warnings`,
   * `error`, AND `isUnsupportedDatasource` back to their initial values.
   * Does NOT touch `isLoading` (a caller MUST NOT reset while a translation
   * is in flight; doing so would leave the loading indicator stuck — the
   * hook expects the user to wait for the in-flight call to settle first).
   *
   * `isUnsupportedDatasource` is cleared to `false` per the checkpoint
   * contract. A subsequent call to `translate()` re-evaluates the current
   * `dsSettings.type` and re-sets the flag to `true` if the datasource is
   * unsupported, so the short-circuit guard remains fully enforced.
   */
  reset: () => void;

  /**
   * NLQ feature: the query string returned by the LLM (empty until a
   * successful translation has been received). On error, this is cleared
   * back to the empty string so consumers can use truthiness checks
   * (e.g. `translatedQuery && <NLQQueryPreview ... />`) to gate the
   * preview's render.
   */
  translatedQuery: string;

  /**
   * NLQ feature: query-language identifier used by `NLQQueryPreview` to set
   * the Monaco editor's `language` prop for syntax highlighting and
   * validation. See {@link NLQLanguage} for the value space.
   */
  language: NLQLanguage;

  /**
   * NLQ feature: optional human-readable explanation of how the LLM
   * constructed the query. Empty string when the response omitted the
   * `explanation` field or when no translation has been produced yet.
   */
  explanation: string;

  /**
   * NLQ feature: non-fatal warnings emitted by the server (e.g. schema-fetch
   * fallback notices). Empty array when the response omitted the `warnings`
   * field or when no translation has been produced yet.
   *
   * Surfaced in the UI as an `<Alert severity="info">` per AAP §0.2.1.
   */
  warnings: string[];

  /**
   * NLQ feature: `true` while a `translate()` HTTP request is in flight.
   * Used to disable the "Translate" button and render a `<Spinner />` per
   * AAP §0.6.3.
   */
  isLoading: boolean;

  /**
   * NLQ feature: populated on HTTP failure; `null` otherwise.
   *
   * Always an `Error` instance — non-Error throws are wrapped by the hook
   * so that consumers can rely on the `.message` property existing without
   * runtime narrowing.
   *
   * IMPORTANT — i18n contract: consumers MUST NOT render `error.message`
   * directly to the user. The `.message` value can be backend-supplied
   * English text, browser-native fetch error text, or an empty string from
   * a non-Error throw — none of which flow through the `t()`/`<Trans>`
   * localization pipeline. Consumers SHOULD render a localized message
   * (e.g. `t('nlq.error.generic', ...)`) and use `error` only as a boolean
   * "did the translation fail" discriminator.
   */
  error: Error | null;

  /**
   * NLQ feature: `true` when the active datasource `type` is not in
   * `{prometheus, loki}` (i.e. not in {@link SUPPORTED_DS_TYPES}).
   *
   * When this is `true`, the parent `NaturalLanguageQueryBar` is expected to
   * render an `<Alert severity="warning">` with an "unsupported data source"
   * message rather than the active NL input/preview UI, per AAP §0.6.3.1.
   *
   * Synchronization model (three layers, defense-in-depth):
   *   1. Initial value — derived synchronously from the current
   *      `dsSettings.type` prop via a `useState` lazy initializer so the
   *      flag is correct on the very first render (no flash of stale state
   *      for unsupported datasources mounted directly).
   *   2. Prop-change sync — a `useEffect` keyed on `dsSettings.type` keeps
   *      the flag in lockstep with the prop after the initial render. This
   *      is the load-bearing layer for AAP §0.1.1.1 "the NLQ bar MUST
   *      render a clear 'unsupported data source' message" when the user
   *      changes the active datasource mid-session (Checkpoint 5 QA fix
   *      for the stale-state bug observed when switching from a supported
   *      datasource to an unsupported one inside an open panel editor).
   *   3. Runtime short-circuit — `translate()` re-checks the current
   *      `dsSettings.type` on every invocation and re-asserts the flag
   *      if unsupported, so the HTTP call is blocked even if a regression
   *      removed layers (1) or (2). `reset()` clears the flag back to
   *      `false` per the checkpoint contract; a subsequent re-render of
   *      the hook with an unsupported `dsSettings.type` will re-assert the
   *      flag via layer (2) on the next render, so the visual contract is
   *      preserved even after `reset()`.
   */
  isUnsupportedDatasource: boolean;
}

/**
 * NLQ feature: custom React hook that owns the natural-language translation
 * lifecycle for a single instance of the NLQ bar.
 *
 * Implementation notes (per AAP §0.6.1.4):
 *   - Uses `useState`, `useCallback`, and a single prop-synchronization
 *     `useEffect`. No reducers, no memos. The lone effect mirrors
 *     `dsSettings.type` into the `isUnsupportedDatasource` state slice so
 *     the visual contract holds when the user switches the active panel
 *     datasource at runtime (Checkpoint 5 QA fix for AAP §0.1.1.1).
 *   - Delegates the HTTP call to `postTranslate` from `./nlqApi`, which is
 *     the single point of `getBackendSrv` usage in the NLQ frontend module.
 *   - Defensively narrows the response `language` at runtime — even though
 *     the `TranslateResponse` type declares `language: 'promql' | 'logql'`,
 *     a misbehaving server could return something else and we do not want
 *     the editor to crash, so we coerce unknowns to the empty-string
 *     sentinel.
 *   - On failure, clears all translation state so the UI never shows a
 *     stale query alongside an error alert.
 *   - Wraps non-Error throws in `new Error(...)` so consumers always get
 *     an `Error` instance with a `.message`.
 *
 * @param dsSettings - settings for the active panel datasource; the hook
 *   reads `uid` (sent as `datasourceUid` in the request payload) and `type`
 *   (sent as `datasourceType` and used for the supported-datasource guard).
 *   The full settings object is accepted rather than just the two strings
 *   so future fields (e.g. plugin metadata for richer prompts) can be added
 *   without a breaking signature change.
 * @returns the public {@link UseNLQTranslationResult} contract.
 */
export function useNLQTranslation(dsSettings: DataSourceInstanceSettings): UseNLQTranslationResult {
  // State slices. Each derived translation field is held as its own piece of
  // state so that `setX` calls remain trivially independent — there is no
  // shared reducer to coordinate.
  const [translatedQuery, setTranslatedQuery] = useState<string>('');
  const [language, setLanguage] = useState<NLQLanguage>('');
  const [explanation, setExplanation] = useState<string>('');
  const [warnings, setWarnings] = useState<string[]>([]);
  const [isLoading, setIsLoading] = useState<boolean>(false);
  const [error, setError] = useState<Error | null>(null);
  // NLQ feature: `isUnsupportedDatasource` is held as state (not as a pure
  // per-render derived value) so that `reset()` can clear it per the
  // checkpoint contract — see Case 8 of `useNLQTranslation.test.ts`.
  //
  // Synchronization sources (in order of execution):
  //
  //   1. `useState` lazy initializer (below) seeds the flag from the
  //      initial `dsSettings.type` prop. This guarantees the very first
  //      render of the bar is correct even for unsupported datasources
  //      that were active when the panel editor mounted — there is no
  //      flash of stale "input UI" before the effect fires.
  //
  //   2. `useEffect` keyed on `dsSettings.type` (further below) keeps the
  //      flag in sync after the first render. This is the load-bearing
  //      layer for the Checkpoint 5 QA fix (AAP §0.1.1.1 "the NLQ bar
  //      MUST render a clear 'unsupported data source' message"): when
  //      the user switches the active datasource from supported (e.g.
  //      Loki) to unsupported (e.g. TestData) inside an open panel
  //      editor session, the bar MUST visually flip to the warning Alert
  //      immediately — not only after a Translate click.
  //
  //   3. The runtime short-circuit inside `translate()` re-evaluates the
  //      prop on every invocation and re-asserts the flag if unsupported.
  //      This is defense-in-depth: even if a regression removed layer (2),
  //      the HTTP request is still blocked and the flag is still set
  //      before the function returns.
  const [isUnsupportedDatasource, setIsUnsupportedDatasource] = useState<boolean>(
    () => !isSupportedDatasourceType(dsSettings.type)
  );

  // NLQ feature (Checkpoint 5 QA fix — AAP §0.1.1.1):
  //
  // Synchronize `isUnsupportedDatasource` with `dsSettings.type` on every
  // change. Without this effect, the `useState` lazy initializer above only
  // runs ONCE at mount, so switching the active panel datasource from a
  // supported type (e.g. `loki`) to an unsupported one (e.g.
  // `grafana-testdata-datasource`) would leave the flag stale at `false`
  // and the bar would continue to render the input/preview UI instead of
  // the warning Alert. The Checkpoint 5 QA report identified this as a
  // MAJOR UX-contract violation (the SECURITY contract is preserved by the
  // runtime short-circuit inside `translate()`, but the visual contract
  // requires immediate feedback).
  //
  // Dependency is `dsSettings.type` only (not the full `dsSettings`
  // reference) so the effect skips no-op re-renders where only an
  // unrelated field of `dsSettings` changed.
  //
  // The body is intentionally a single `setX` call with no cleanup
  // function: there is nothing to tear down — we are simply mirroring a
  // prop into state. React will skip the re-render entirely if the next
  // value is identical to the current state (`Object.is` bail-out).
  useEffect(() => {
    setIsUnsupportedDatasource(!isSupportedDatasourceType(dsSettings.type));
  }, [dsSettings.type]);

  /**
   * NLQ feature: clears all derived translation state.
   *
   * Clears every state field including `isUnsupportedDatasource` (which is
   * set back to `false`) per the checkpoint contract. A subsequent call to
   * `translate()` re-evaluates the current `dsSettings.type` and re-asserts
   * the flag if the datasource is still unsupported, so this clearing is
   * purely about state hygiene — the runtime short-circuit is preserved.
   *
   * Memoized with an empty dependency list because it references only state
   * setters (which React guarantees stable across renders). This keeps the
   * `reset` callback identity stable so consumers that put it in a
   * dependency list (e.g. an effect cleanup or a child component prop) do
   * not re-run on every parent render.
   */
  const reset = useCallback(() => {
    setTranslatedQuery('');
    setLanguage('');
    setExplanation('');
    setWarnings([]);
    setError(null);
    setIsUnsupportedDatasource(false);
  }, []);

  /**
   * NLQ feature: fires the translation request.
   *
   * Memoized with `[dsSettings.uid, dsSettings.type]` as dependencies so the
   * callback identity is stable across re-renders that do not change the
   * active datasource. The supported-datasource check is re-performed every
   * call against the CURRENT `dsSettings.type` prop, so the short-circuit
   * remains correct even after a prior `reset()` cleared the
   * `isUnsupportedDatasource` state flag.
   *
   * State setters are intentionally omitted from the dependency list per
   * React's stability guarantee for setters returned by `useState`.
   */
  const translate = useCallback(
    async (input: string): Promise<void> => {
      // NLQ feature: short-circuit on unsupported datasource — no HTTP call
      // is made. The check is performed against the current `dsSettings.type`
      // prop on every invocation, not against the state flag, so the guard
      // remains authoritative even after a prior `reset()` cleared the flag.
      // We then re-assert the `isUnsupportedDatasource` state flag here so
      // the parent UI re-renders the "unsupported data source" alert.
      if (!isSupportedDatasourceType(dsSettings.type)) {
        setIsUnsupportedDatasource(true);
        return;
      }

      // Datasource is supported — ensure the state flag reflects that. If a
      // prior `reset()` left it `false` and the prop has not changed, this
      // is a no-op; if the prop has changed from unsupported to supported,
      // this brings the flag in sync.
      setIsUnsupportedDatasource(false);

      // NLQ feature: short-circuit on empty input — no HTTP call is made.
      // The trim is done first so a whitespace-only input is also rejected.
      // The same trimmed value is reused below as the request payload.
      const trimmed = input == null ? '' : input.trim();
      if (trimmed === '') {
        return;
      }

      setIsLoading(true);
      // Clear any prior error so the UI immediately reflects "I'm trying again
      // now" rather than continuing to show the stale failure alert.
      setError(null);

      try {
        const response = await postTranslate({
          input: trimmed,
          datasourceUid: dsSettings.uid,
          datasourceType: dsSettings.type,
        });

        // Defensive coalescing: the over-the-wire `TranslateResponse` declares
        // `query` as a required string, but a misbehaving server could send
        // `null`/`undefined`. Coercing to an empty string keeps the consuming
        // CodeEditor stable.
        const safeQuery = response.query ?? '';
        setTranslatedQuery(safeQuery);

        // Defensive narrowing: although the `TranslateResponse` type declares
        // `language: 'promql' | 'logql'`, a misbehaving server could return an
        // arbitrary string. We narrow at runtime to the supported set; anything
        // else falls back to the empty-string sentinel and the editor will
        // render plain text without syntax highlighting.
        if (response.language === 'promql' || response.language === 'logql') {
          setLanguage(response.language);
        } else {
          setLanguage('');
        }

        // `explanation` and `warnings` are optional (`,omitempty` on the Go
        // side); coalesce missing values to safe empty defaults.
        setExplanation(response.explanation ?? '');
        const safeWarnings = response.warnings ?? [];
        setWarnings(safeWarnings);

        // NLQ feature MAJOR review fix (review feedback — observability):
        // Emit the `grafana_nlq_translate_succeeded` interaction event
        // mandated by AAP §0.6.3.2. The payload carries ONLY
        // non-sensitive metadata that supports SLO/funnel analytics:
        //   - `dsType` (the datasource plugin id, e.g. "prometheus") so
        //     analytics can segment by backend.
        //   - `queryLength` (a count, not the query itself) so analytics
        //     can measure result-shape distributions without ever
        //     transmitting the generated PromQL/LogQL — which can carry
        //     operator metric names that are confidential in some
        //     deployments.
        //   - `hadWarnings` (a boolean) so analytics can correlate the
        //     SOFT-failure schema-fetch path with downstream user
        //     behavior.
        // SECURITY (AAP §0.8.5 and the no-secret-in-telemetry discipline
        // applied to user content as well): the raw natural-language
        // input, the generated query string, the LLM explanation, and
        // the warning messages are ALL deliberately excluded from the
        // event payload. Centralizing this discipline in the hook
        // (rather than at every call site in the component) guarantees
        // the no-leak invariant across future call-site additions.
        reportInteraction('grafana_nlq_translate_succeeded', {
          dsType: dsSettings.type,
          queryLength: safeQuery.length,
          hadWarnings: safeWarnings.length > 0,
        });
      } catch (err) {
        // Wrap non-Error throws so consumers can rely on `.message`.
        // The wrapping covers the three realistic shapes of a thrown value:
        //   1. An `Error` instance (the common case from getBackendSrv).
        //   2. A bare string (some legacy throw sites are still in the
        //      codebase) — wrapped in a new Error so the type is uniform.
        //   3. Anything else — wrapped in a bare Error with an EMPTY message
        //      so consumers do not display an unlocalized fallback string
        //      to the user (component layer is responsible for rendering a
        //      localized message via t()/Trans).
        // NOTE: we deliberately do NOT include the user's input or the raw
        // error payload in the message — those may contain sensitive data
        // (per backend secret-handling discipline).
        let wrapped: Error;
        if (err instanceof Error) {
          wrapped = err;
        } else if (typeof err === 'string') {
          wrapped = new Error(err);
        } else {
          // Empty-message Error: signals "translation failed" purely as a
          // type discriminator. The component renders a localized
          // `t('nlq.error.generic', ...)` string and ignores `error.message`.
          wrapped = new Error();
        }
        setError(wrapped);
        // Clear all derived translation state so the UI never shows a stale
        // query alongside the error alert.
        setTranslatedQuery('');
        setLanguage('');
        setExplanation('');
        setWarnings([]);

        // NLQ feature MAJOR review fix (review feedback — observability):
        // Emit the `grafana_nlq_translate_failed` interaction event
        // mandated by AAP §0.6.3.2. The payload carries ONLY
        // non-sensitive metadata so analytics can monitor failure rates
        // and break them down by backend without ever capturing user
        // input or upstream error details:
        //   - `dsType` for backend-level segmentation.
        //   - `errorKind` — a coarse-grained classifier derived from
        //     `(typeof err)` and the `Error` shape. We intentionally
        //     avoid `err.message` (which may be the unlocalized backend
        //     text, a fetch error URL, or an empty string) so the
        //     analytics event never carries operator detail or
        //     localized user-facing text.
        // SECURITY (AAP §0.8.5): the raw error message and the user's
        // input are deliberately excluded from the event payload to
        // mirror the no-secret-in-telemetry discipline applied
        // throughout the NLQ feature.
        //
        // The classifier order matters:
        //   1. `isFetchError(err)` is checked FIRST because backendSrv
        //      throws a plain object shaped as `FetchError` (interface,
        //      not class) on non-2xx HTTP responses. A FetchError object
        //      is NOT an `Error` instance, so without this branch the
        //      most common failure path (LLM backend returning 4xx/5xx
        //      surfaced as a FetchError) would be labelled 'unknown' —
        //      defeating the analytics value of the event.
        //   2. `err instanceof Error` for legitimate Error subclasses
        //      thrown by application code (e.g. our defensive Error
        //      wrapping in `nlqApi.ts`).
        //   3. `typeof err === 'string'` for the rare bare-string throw.
        //   4. Anything else falls through to 'unknown'.
        let errorKind: 'fetch' | 'error' | 'string' | 'unknown';
        if (isFetchError(err)) {
          errorKind = 'fetch';
        } else if (err instanceof Error) {
          errorKind = 'error';
        } else if (typeof err === 'string') {
          errorKind = 'string';
        } else {
          errorKind = 'unknown';
        }
        reportInteraction('grafana_nlq_translate_failed', {
          dsType: dsSettings.type,
          errorKind,
        });
      } finally {
        setIsLoading(false);
      }
    },
    [dsSettings.uid, dsSettings.type]
  );

  return {
    translate,
    reset,
    translatedQuery,
    language,
    explanation,
    warnings,
    isLoading,
    error,
    isUnsupportedDatasource,
  };
}
