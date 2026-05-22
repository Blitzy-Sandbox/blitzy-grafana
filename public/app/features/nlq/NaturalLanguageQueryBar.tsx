// NLQ feature: top-level collapsible UI for Natural Language Query (NLQ).
//
// Rendered by `PanelDataQueriesTab.tsx` above the existing
// `QueryGroupTopSection` / `QueryEditorRows` stack whenever
// `config.featureToggles.nlqEnabled` is truthy. Provides four UI states
// (per AAP §0.6.3):
//
//   1. Idle        — collapsible closed, or open with an empty input.
//   2. Loading     — Translate button disabled, inline Spinner displayed.
//   3. Translated  — `<NLQQueryPreview>` renders the LLM-generated PromQL /
//                    LogQL inside an editable Monaco-based editor, with Run /
//                    Add-as-Panel actions.
//   4. Error /
//      Unsupported /
//      Warning     — corresponding `<Alert>` is rendered; the existing query
//                    editor below the bar remains fully functional.
//
// Design constraints (per AAP §0.5 / §0.8):
//   - Every UI primitive comes from `@grafana/ui` exclusively (Alert, Button,
//     CollapsableSection, Field, Spinner, Stack, TextArea, useStyles2). NO
//     external React UI libraries are introduced.
//   - Every user-visible string flows through `t()` / `<Trans>` from
//     `@grafana/i18n` so the feature participates in the Crowdin localization
//     pipeline.
//   - Every visual value resolves to a `GrafanaTheme2` design token — no
//     hardcoded colors, spacings, radii, or typography.
//   - The bar is purely presentational: it owns ZERO state that touches the
//     dashboard JSON model (per AAP §0.7.2.4). All state — input text, edited
//     query, open/closed — is local React state that disappears across
//     remounts.
//   - Defense-in-depth: the bar re-checks `config.featureToggles.nlqEnabled`
//     at the top and renders `null` when disabled, so direct imports of this
//     component from tests or future call sites still respect the flag
//     (per AAP §0.1.1.2).
//
// Telemetry events emitted via `reportInteraction` (per AAP §0.6.3.2):
//   - `grafana_nlq_bar_opened`        — when the user opens the collapsible.
//   - `grafana_nlq_translate_clicked` — when the user clicks Translate.
//   - `grafana_nlq_run_clicked`       — when the user clicks Run on the
//                                       preview, with an `edited` boolean
//                                       indicating whether the displayed query
//                                       differs from the LLM output.
//   - `grafana_nlq_add_panel_clicked` — when the user clicks Add as Panel.
//
// The bar deliberately does NOT import any dashboard-scene orchestration code
// beyond the `SceneObjectRef<VizPanel>` TYPE for the `panelRef` prop — the
// actual run / add-as-panel side effects are delegated up to the parent via
// the optional `onRun` / `onAddPanel` callbacks. This keeps the component
// trivially testable (tests inject `jest.fn()` for the callbacks) and
// preserves the strict folder isolation declared in AAP §0.1.2 ("NLQ
// frontend code lives under `public/app/features/nlq/`").

import { css } from '@emotion/css';
import { useCallback, useEffect, useState } from 'react';

import { DataSourceInstanceSettings, GrafanaTheme2 } from '@grafana/data';
import { Trans, t } from '@grafana/i18n';
import { config, reportInteraction } from '@grafana/runtime';
import { SceneObjectRef, VizPanel } from '@grafana/scenes';
import { Alert, Button, CollapsableSection, Field, Spinner, Stack, TextArea, useStyles2 } from '@grafana/ui';

import { NLQQueryPreview } from './NLQQueryPreview';
import { useNLQTranslation } from './useNLQTranslation';

/**
 * NLQ feature: props consumed by {@link NaturalLanguageQueryBar}.
 *
 * The four members are the AAP-declared public surface
 * (schema `members_exposed`: `dsSettings`, `panelRef`, `onAddPanel`, `onRun`).
 */
export interface NaturalLanguageQueryBarProps {
  /**
   * Settings for the active panel datasource. The bar reads:
   *   - `dsSettings.type` — used to gate the supported-datasource check
   *     inside `useNLQTranslation`, to populate the Monaco editor language,
   *     and as the `dsType` telemetry property on every emitted event.
   *   - `dsSettings.uid`  — passed through to `useNLQTranslation` -> backend
   *     so the server can resolve the datasource and authorize the caller.
   *
   * Supplied by `PanelDataQueriesTab.tsx` from the active scene state.
   */
  dsSettings: DataSourceInstanceSettings;

  /**
   * Reference to the active VizPanel for the panel editor session. Carried so
   * the parent integration can — at the consumer level — delegate the
   * "Add as Panel" action to the dashboard-scene's existing panel-creation
   * flow without the bar itself needing to import scene-orchestration code
   * (per AAP §0.4.3.5).
   *
   * Required (non-optional) because the consumer always has a `panelRef` in
   * scope; making it optional would invite the bar to grow scene-handling
   * logic, which is explicitly out of scope for this minimal-change feature.
   */
  panelRef: SceneObjectRef<VizPanel>;

  /**
   * Optional callback fired when the user clicks "Add as Panel" inside the
   * `NLQQueryPreview`. Receives the (possibly user-edited) translated query
   * string and the resolved query-language identifier.
   *
   * If not provided, the bar simply emits the telemetry event and otherwise
   * no-ops — the consumer is expected to wire the actual scene-side panel
   * creation flow as needed. This keeps the component trivially testable:
   * tests inject a `jest.fn()` and assert the call.
   */
  onAddPanel?: (query: string, language: 'promql' | 'logql') => void;

  /**
   * Optional callback fired when the user clicks "Run" inside the
   * `NLQQueryPreview`. Same shape and semantics as `onAddPanel`.
   */
  onRun?: (query: string, language: 'promql' | 'logql') => void;
}

/**
 * NLQ feature: the top-level Natural Language Query bar rendered by
 * `PanelDataQueriesTab.tsx` above the standard query editor stack.
 *
 * Visual composition (top-to-bottom inside a `CollapsableSection`):
 *   1. If the active datasource is unsupported → a single
 *      `<Alert severity="warning">` and nothing else (the input/translate UI
 *      is hidden — issuing an LLM call for an unsupported datasource would
 *      be wasted work and confusing for the user).
 *   2. Otherwise:
 *      a. A `<Field>`-wrapped `<TextArea>` for the natural-language prompt.
 *      b. A horizontal `<Stack>` of action buttons: Translate (primary)
 *         + optional inline `<Spinner>` (while loading) + Clear (when the
 *         user has typed or a translation is on screen).
 *      c. An `<Alert severity="error">` when the most recent translate
 *         attempt threw.
 *      d. An `<Alert severity="info">` when the server returned non-fatal
 *         warnings (e.g. schema-fetch fallback notices).
 *      e. The `<NLQQueryPreview>` once a translation has been produced.
 *
 * Defense-in-depth flag check: the parent already gates on
 * `config.featureToggles.nlqEnabled`, but we re-check here so a direct
 * import (from another part of the codebase, from a test, or from a future
 * call site) still respects the flag.
 */
export function NaturalLanguageQueryBar({
  dsSettings,
  // NOTE: `panelRef` is intentionally unused INSIDE this component — see the
  // prop JSDoc above. It is part of the public contract so the consumer can
  // wire scene-side flows around it without restructuring this bar. The
  // ESLint `no-unused-vars` rule is configured project-wide to ignore named
  // arguments that begin with `_`; here we keep the original name so that
  // the component type is self-documenting at the call site, and rely on the
  // explicit `// eslint-disable-next-line` directive below if needed by the
  // local lint config.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  panelRef,
  onAddPanel,
  onRun,
}: NaturalLanguageQueryBarProps) {
  // NLQ feature: defense-in-depth feature-flag check. The early return MUST
  // occur before any hook calls so React's Rules of Hooks remain satisfied —
  // when the flag flips at runtime (very unusual but possible) the bar will
  // remount cleanly. The parent (`PanelDataQueriesTab.tsx`) gates on the same
  // flag, so this check is purely belt-and-suspenders.
  if (!config.featureToggles.nlqEnabled) {
    return null;
  }

  return <NaturalLanguageQueryBarInner dsSettings={dsSettings} onAddPanel={onAddPanel} onRun={onRun} />;
}

/**
 * NLQ feature: inner implementation rendered only when the feature flag is
 * enabled.
 *
 * Split into a separate component so the hook calls below execute under a
 * stable component identity — i.e., hooks are always called in the same
 * order across renders of this inner component, regardless of how many
 * times the outer flag check toggles. This is the canonical React pattern
 * for "early-return-then-hooks": the outer component owns the conditional,
 * the inner component owns the hook lifecycle.
 *
 * `panelRef` is intentionally omitted from the inner props — the bar's
 * actual behavior is driven entirely by `dsSettings` plus the two optional
 * callbacks. The outer component carries `panelRef` purely for typing /
 * call-site self-documentation.
 */
interface NaturalLanguageQueryBarInnerProps {
  dsSettings: DataSourceInstanceSettings;
  onAddPanel?: (query: string, language: 'promql' | 'logql') => void;
  onRun?: (query: string, language: 'promql' | 'logql') => void;
}

function NaturalLanguageQueryBarInner({ dsSettings, onAddPanel, onRun }: NaturalLanguageQueryBarInnerProps) {
  const styles = useStyles2(getStyles);

  // Local-only UI state. Per AAP §0.7.2.4 none of these are persisted to the
  // dashboard JSON model; they disappear when the bar is unmounted.
  const [isOpen, setIsOpen] = useState<boolean>(false);
  const [input, setInput] = useState<string>('');
  // The user-edited copy of the LLM's translated query. Seeded from
  // `translatedQuery` whenever a new translation arrives (see useEffect
  // below); mutated by `NLQQueryPreview`'s `onChange` as the user refines.
  const [editedQuery, setEditedQuery] = useState<string>('');

  // Delegate the translation lifecycle to the dedicated hook. The hook owns
  // the HTTP call, the supported-datasource guard, and all derived state.
  const {
    translate,
    reset,
    translatedQuery,
    language,
    explanation,
    warnings,
    isLoading,
    error,
    isUnsupportedDatasource,
  } = useNLQTranslation(dsSettings);

  // Seed `editedQuery` from `translatedQuery` whenever a new translation
  // arrives. This makes the `<NLQQueryPreview>` editable: the initial value
  // shown in the Monaco editor is the LLM output, and subsequent user edits
  // accumulate into `editedQuery` via the preview's `onChange` callback.
  //
  // We include only `translatedQuery` in the dep list (not `editedQuery`) so
  // that the effect fires exclusively when the SERVER produces a new query —
  // not on every keystroke the user makes inside the editor (which would
  // overwrite their edits on every render).
  useEffect(() => {
    setEditedQuery(translatedQuery);
  }, [translatedQuery]);

  // ─── Handler callbacks ─────────────────────────────────────────────────

  /**
   * Fired when the user clicks the CollapsableSection header to open or
   * close the bar. Emits a telemetry event ONLY on open (not on close) so
   * we capture "the user expressed intent to use NLQ" without doubling
   * every event into open/close pairs.
   */
  const handleToggle = useCallback(
    (open: boolean) => {
      setIsOpen(open);
      if (open) {
        reportInteraction('grafana_nlq_bar_opened', { dsType: dsSettings.type });
      }
    },
    [dsSettings.type]
  );

  /**
   * Fired when the user clicks Translate. The translation itself is
   * delegated to the hook; we just emit the click event and await the
   * result. We do not surface the awaited result here — the hook publishes
   * the new state via its return values, and React re-renders this component
   * with the new `translatedQuery` / `error` / `warnings`.
   *
   * Note: `inputLength` is intentionally a count, not the input itself — we
   * never log or transmit the user's raw natural-language text as
   * telemetry (per AAP §0.8.5 secret-handling discipline applied to user
   * content as well).
   */
  const handleTranslate = useCallback(async () => {
    reportInteraction('grafana_nlq_translate_clicked', {
      dsType: dsSettings.type,
      inputLength: input.length,
    });
    await translate(input);
  }, [translate, input, dsSettings.type]);

  /**
   * Fired when the user clicks Run inside the `NLQQueryPreview`. Emits an
   * `edited` flag derived from comparing the displayed `editedQuery` with
   * the original `translatedQuery` so analytics can measure how often the
   * LLM's output is accepted unchanged.
   *
   * The narrowing on `language` ensures the optional callback is only
   * invoked with the well-known supported set; an unexpected language
   * value (which would only occur if the server misbehaved) is silently
   * ignored rather than propagated to the parent.
   */
  const handleRun = useCallback(() => {
    reportInteraction('grafana_nlq_run_clicked', {
      dsType: dsSettings.type,
      edited: editedQuery !== translatedQuery,
    });
    if (language === 'promql' || language === 'logql') {
      onRun?.(editedQuery, language);
    }
  }, [onRun, editedQuery, translatedQuery, language, dsSettings.type]);

  /**
   * Fired when the user clicks Add as Panel inside the `NLQQueryPreview`.
   * Same narrowing pattern as `handleRun`.
   */
  const handleAddPanel = useCallback(() => {
    reportInteraction('grafana_nlq_add_panel_clicked', { dsType: dsSettings.type });
    if (language === 'promql' || language === 'logql') {
      onAddPanel?.(editedQuery, language);
    }
  }, [onAddPanel, editedQuery, language, dsSettings.type]);

  /**
   * Fired when the user clicks Clear. Resets both local state slices
   * (`input`, `editedQuery`) AND the hook's translation state so the bar
   * returns to its Idle appearance.
   */
  const handleClear = useCallback(() => {
    setInput('');
    setEditedQuery('');
    reset();
  }, [reset]);

  // ─── Derived render values ─────────────────────────────────────────────

  // Pre-compute strings that are used in attribute positions (where JSX
  // children like `<Trans>` are not valid). Each call to `t()` flows through
  // the Crowdin pipeline because Grafana's i18n extractor parses these
  // call sites statically.
  const collapsableLabel = t('nlq.bar.title', 'Ask a question');
  const unsupportedTitle = t('nlq.bar.unsupported-title', 'Unsupported data source');
  const inputLabel = t('nlq.input.label', 'Question');
  const inputDescription = t('nlq.input.description', 'Describe what you want to see in plain English.');
  const inputPlaceholder = t(
    'nlq.input.placeholder',
    'e.g. Show me failed login attempts in the last hour grouped by IP'
  );
  const inputAriaLabel = t('nlq.input.aria-label', 'Natural language question');
  const errorTitle = t('nlq.error.title', 'Translation failed');
  const errorGeneric = t('nlq.error.generic', 'An error occurred while translating your question. Please try again.');
  const warningTitle = t('nlq.warning.title', 'Translation warnings');

  // ─── Render ────────────────────────────────────────────────────────────

  return (
    <div className={styles.container} data-testid="nlq-bar">
      <CollapsableSection
        label={collapsableLabel}
        isOpen={isOpen}
        onToggle={handleToggle}
        headerDataTestId="nlq-bar-header"
        contentDataTestId="nlq-bar-content"
      >
        <Stack direction="column" gap={2}>
          {isUnsupportedDatasource && (
            <Alert severity="warning" title={unsupportedTitle} data-testid="nlq-bar-unsupported-alert">
              <Trans i18nKey="nlq.bar.unsupported-description">
                Natural language queries are not supported for this data source. NLQ currently supports Prometheus and
                Loki only.
              </Trans>
            </Alert>
          )}

          {!isUnsupportedDatasource && (
            <>
              <Field label={inputLabel} description={inputDescription} noMargin>
                <TextArea
                  value={input}
                  onChange={(e) => setInput(e.currentTarget.value)}
                  placeholder={inputPlaceholder}
                  rows={3}
                  aria-label={inputAriaLabel}
                  disabled={isLoading}
                  data-testid="nlq-bar-input"
                />
              </Field>

              <Stack direction="row" gap={1} alignItems="center">
                <Button
                  variant="primary"
                  onClick={handleTranslate}
                  disabled={!input.trim() || isLoading}
                  // Don't render an icon while loading — the inline Spinner
                  // beside the button is the canonical loading affordance.
                  icon={isLoading ? undefined : 'sync'}
                  data-testid="nlq-bar-translate-button"
                >
                  {isLoading ? (
                    <Trans i18nKey="nlq.button.translating">Translating…</Trans>
                  ) : (
                    <Trans i18nKey="nlq.button.translate">Translate</Trans>
                  )}
                </Button>

                {isLoading && <Spinner inline size="md" />}

                {(input !== '' || translatedQuery !== '') && !isLoading && (
                  <Button variant="secondary" fill="outline" onClick={handleClear} data-testid="nlq-bar-clear-button">
                    <Trans i18nKey="nlq.button.clear">Clear</Trans>
                  </Button>
                )}
              </Stack>

              {error && (
                <Alert severity="error" title={errorTitle} data-testid="nlq-bar-error-alert">
                  {error.message || errorGeneric}
                </Alert>
              )}

              {warnings.length > 0 && (
                <Alert severity="info" title={warningTitle} data-testid="nlq-bar-warning-alert">
                  <ul className={styles.warningList}>
                    {warnings.map((w, i) => (
                      <li key={`${i}-${w}`}>{w}</li>
                    ))}
                  </ul>
                </Alert>
              )}

              {translatedQuery !== '' && (language === 'promql' || language === 'logql') && (
                <NLQQueryPreview
                  translatedQuery={editedQuery}
                  language={language}
                  explanation={explanation === '' ? undefined : explanation}
                  onChange={setEditedQuery}
                  onRun={handleRun}
                  onAddPanel={handleAddPanel}
                />
              )}
            </>
          )}
        </Stack>
      </CollapsableSection>
    </div>
  );
}

/**
 * Themed style factory consumed via `useStyles2`.
 *
 * Every value resolves to a `GrafanaTheme2` design token — no hardcoded
 * colors, spacings, or radii are introduced (per AAP §0.5.3 token rules).
 *
 * The wrapper exists primarily to give the bar a visible boundary
 * (background + radius + padding + bottom margin) that separates it from
 * the existing `QueryGroupTopSection` rendered below.
 */
const getStyles = (theme: GrafanaTheme2) => ({
  container: css({
    padding: theme.spacing(2),
    backgroundColor: theme.colors.background.secondary,
    borderRadius: theme.shape.radius.default,
    marginBottom: theme.spacing(2),
  }),
  /**
   * Compact list styling for the warnings Alert body. We keep the default
   * `<ul>` semantics for screen readers but tighten the spacing so the
   * notice fits cleanly inside the Alert without dominating the bar.
   */
  warningList: css({
    margin: 0,
    paddingLeft: theme.spacing(2.5),
  }),
});
