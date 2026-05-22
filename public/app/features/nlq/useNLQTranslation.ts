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
//   - Uses only `useState` and `useCallback` from React. No `useEffect`,
//     no `useReducer`, no `useMemo` — the AAP explicitly forbids them to
//     keep the hook minimal and trivially predictable.
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
//   - Single-turn only: each call to `translate()` is independent; the hook
//     does NOT maintain conversation history (AAP §0.7.2.3).

import { useCallback, useState } from 'react';

import type { DataSourceInstanceSettings } from '@grafana/data';

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
   * Initial value is derived from the current `dsSettings.type` prop via a
   * `useState` lazy initializer. The flag is also re-evaluated and re-set
   * on every call to `translate()` so the runtime short-circuit always
   * reflects the current `dsSettings.type` (and re-asserts the flag after a
   * prior `reset()` has cleared it). `reset()` clears the flag back to
   * `false` per the checkpoint contract.
   */
  isUnsupportedDatasource: boolean;
}

/**
 * NLQ feature: custom React hook that owns the natural-language translation
 * lifecycle for a single instance of the NLQ bar.
 *
 * Implementation notes (per AAP §0.6.1.4):
 *   - Uses only `useState` and `useCallback`. No effects, no reducers, no
 *     memos. The simplest pattern that satisfies the request/response shape.
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
  // `isUnsupportedDatasource` is held as state (initialized lazily from the
  // current `dsSettings.type` prop) rather than as a per-render derived
  // value, so that `reset()` can clear it per the checkpoint contract.
  // `translate()` re-evaluates the prop on every invocation and re-sets the
  // flag, so the short-circuit guard remains fully enforced even after a
  // prior `reset()` has cleared the flag.
  const [isUnsupportedDatasource, setIsUnsupportedDatasource] = useState<boolean>(() =>
    !isSupportedDatasourceType(dsSettings.type)
  );

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
        setTranslatedQuery(response.query ?? '');

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
        setWarnings(response.warnings ?? []);
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
