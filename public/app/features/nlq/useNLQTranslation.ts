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
//   - Never logs the user's input or the LLM response payload. The LLM API
//     key never reaches the browser (it is read on the server from
//     `GF_NLQ_LLM_API_KEY` — AAP §0.8.5), so there is no secret to leak
//     from this hook; the no-log constraint is enforced here to prevent
//     accidental telemetry of user-typed natural-language content.
//   - Single-turn only: each call to `translate()` is independent; the hook
//     does NOT maintain conversation history (AAP §0.7.2.3).

import { useCallback, useState } from 'react';

import { DataSourceInstanceSettings } from '@grafana/data';

import { postTranslate } from './nlqApi';

/**
 * NLQ feature: string literal union of the datasource types for which the
 * NLQ bar will actually call the backend translation service. The supported
 * set is closed and limited to Prometheus-compatible (`prometheus`) and Loki
 * (`loki`) per AAP §0.6.3.1.
 *
 * Mimir uses the `prometheus` datasource plugin type (Mimir is wire-compatible
 * with the Prometheus query API), so it is supported by inclusion in this
 * literal union — no separate `mimir` entry is required.
 *
 * Declared as a type alias rather than a string array so that the type guard
 * below can use direct literal comparisons (`type === 'prometheus'`) — this
 * matches the pattern used elsewhere in the codebase (e.g.
 * `isEditableVariableType` in
 * `public/app/features/dashboard-scene/settings/variables/utils.ts`) and
 * avoids the type-assertion pattern that the project's
 * `@typescript-eslint/consistent-type-assertions: never` rule forbids.
 */
type SupportedDsType = 'prometheus' | 'loki';

/**
 * NLQ feature: user-defined type guard that determines whether the active
 * datasource type is in the supported set.
 *
 * Implemented as a type predicate (`type is SupportedDsType`) so TypeScript
 * can narrow the input in the call site, even though we do not currently rely
 * on that narrowing — including the predicate keeps the function future-proof
 * if a caller ever needs the narrowed type.
 *
 * The implementation uses direct string equality rather than an
 * `Array.includes` lookup so that the project's no-type-assertions ESLint
 * rule is honored without introducing a separate widened-string-array
 * intermediate.
 */
function isSupportedDatasourceType(type: string): type is SupportedDsType {
  return type === 'prometheus' || type === 'loki';
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
   * Resets `translatedQuery`, `language`, `explanation`, `warnings`, and
   * `error` back to their initial values. Does NOT touch `isLoading` (a
   * caller MUST NOT reset while a translation is in flight; doing so would
   * leave the loading indicator stuck — the hook expects the user to wait
   * for the in-flight call to settle first).
   *
   * Does NOT change `isUnsupportedDatasource` because that flag is derived
   * from the current `dsSettings.type` prop, not from internal state.
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
   * so that consumers can rely on the `.message` property without
   * runtime narrowing.
   */
  error: Error | null;

  /**
   * NLQ feature: `true` when `dsSettings.type` is not in `{prometheus, loki}`.
   *
   * When this is `true`, the hook will refuse to call the backend and
   * `translate()` is a no-op. The parent `NaturalLanguageQueryBar` is
   * expected to read this flag and render an `<Alert severity="warning">`
   * with an "unsupported data source" message rather than the active
   * NL input/preview UI, per AAP §0.6.3.1.
   *
   * Re-evaluated on every render from the current `dsSettings` prop so
   * switching datasources within the same panel editor session flips this
   * flag automatically without any side effect being required.
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

  // Re-evaluated every render from the current dsSettings prop. Switching
  // datasources without a re-mount therefore flips this flag automatically.
  const isUnsupportedDatasource = !isSupportedDatasourceType(dsSettings.type);

  /**
   * NLQ feature: clears all derived translation state.
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
  }, []);

  /**
   * NLQ feature: fires the translation request.
   *
   * Memoized with `[dsSettings.uid, dsSettings.type, isUnsupportedDatasource]`
   * as dependencies so the callback identity is stable across re-renders that
   * do not change the active datasource. We include `isUnsupportedDatasource`
   * in addition to `dsSettings.type` even though one is derived from the
   * other — `react-hooks/exhaustive-deps` requires every component-scope
   * value referenced inside the callback to appear in the dependency list,
   * and the two primitives change in lockstep so the extra dependency does
   * not cause additional callback re-creations.
   *
   * State setters are intentionally omitted from the dependency list per
   * React's stability guarantee for setters returned by `useState`.
   */
  const translate = useCallback(
    async (input: string): Promise<void> => {
      // NLQ feature: short-circuit on unsupported datasource — no HTTP call
      // is made. The parent bar is expected to render an `<Alert>` based on
      // `isUnsupportedDatasource`, but this guard is the authoritative
      // enforcement so a buggy caller cannot bypass the contract.
      if (isUnsupportedDatasource) {
        return;
      }

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
        // Wrap non-Error throws so consumers can rely on `error.message`.
        // The wrapping covers the three realistic shapes of a thrown value:
        //   1. An `Error` instance (the common case from getBackendSrv).
        //   2. A bare string (some legacy throw sites).
        //   3. Anything else (treat as opaque and use a generic message).
        // NOTE: we deliberately do NOT include the user's input or the raw
        // error payload in the message — those may contain sensitive data
        // (per AAP §0.8.5 secret-handling discipline).
        const wrapped =
          err instanceof Error ? err : new Error(typeof err === 'string' ? err : 'NLQ translation failed');
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
    [dsSettings.uid, dsSettings.type, isUnsupportedDatasource]
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
