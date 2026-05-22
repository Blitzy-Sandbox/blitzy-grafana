// NLQ feature: editable query preview component.
//
// Rendered by `NaturalLanguageQueryBar` in the "Translated" state once the
// backend `POST /api/nlq/translate` endpoint returns a non-empty query string.
//
// Responsibilities (per AAP §0.3.2.2 and §0.6.1.4):
//   - Display the LLM-generated PromQL or LogQL query inside an editable
//     Monaco-based `CodeEditor` from `@grafana/ui`, with syntax highlighting
//     driven by the `language` prop (`'promql' | 'logql'`).
//   - Forward user edits back to the parent via `onChange`, fired on both
//     editor blur AND Cmd/Ctrl+S so refinements propagate regardless of how
//     the user signals completion.
//   - Display the LLM's optional natural-language `explanation` text.
//   - Expose two primary actions: "Run" (re-runs the generated query against
//     the active datasource via the parent bar) and "Add as Panel" (delegates
//     to the existing panel-creation flow with visualization suggestion).
//
// Design-system compliance (per AAP §0.5):
//   - All UI primitives are sourced exclusively from `@grafana/ui`
//     (`Button`, `CodeEditor`, `Stack`, `Text`, `useStyles2`); no external
//     React component libraries are introduced.
//   - All visible strings flow through `<Trans>` / `t()` from `@grafana/i18n`
//     so the feature participates in the Crowdin localization pipeline.
//   - All visual values resolve to `GrafanaTheme2` design tokens — there are
//     no hardcoded colors, spacings, radii, or shadows in this file.

import { css } from '@emotion/css';
import { useCallback, useMemo } from 'react';

import { GrafanaTheme2 } from '@grafana/data';
import { Trans, t } from '@grafana/i18n';
import { Button, CodeEditor, type Monaco, Stack, Text, useStyles2, useTheme2 } from '@grafana/ui';

// NLQ feature Checkpoint 6 QA fix (MAJOR Issue 1 — PromQL/LogQL Monaco
// language lazy-registers, preview shows plaintext on first translation).
// `ensureNLQLanguageRegistered` synchronously registers the language ID
// with Monaco BEFORE the editor's model is created (via the
// `onBeforeEditorMount` callback wired below) and asynchronously installs
// the Monarch tokenizer + language configuration so the first NLQ
// translation renders with syntax highlighting — regardless of whether
// the user has previously opened the Prometheus/Loki query editor's
// "Code" mode. See `./monacoLanguages.ts` for the full design rationale.
import { ensureNLQLanguageRegistered } from './monacoLanguages';

/**
 * NLQ feature: props consumed by {@link NLQQueryPreview}.
 *
 * Mirrors the `TranslateResponse` shape declared in `types.ts` projected onto
 * the editable-preview surface, plus three callback handlers wired by the
 * parent `NaturalLanguageQueryBar`.
 */
export interface NLQQueryPreviewProps {
  /**
   * The translated query string returned by the LLM, displayed in the
   * `CodeEditor`. Flows through unchanged so that the user sees exactly what
   * the backend produced — no client-side reformatting, normalization, or
   * defaulting is applied.
   */
  translatedQuery: string;

  /**
   * Monaco language identifier used for syntax highlighting and tokenization.
   *
   * Constrained at the type level to the two languages this release supports
   * (per AAP §0.6.3.1):
   * - `'promql'` when the active datasource is `prometheus` (or Mimir).
   * - `'logql'` when the active datasource is `loki`.
   *
   * Both languages are registered with Monaco by the `onBeforeEditorMount`
   * callback wired below — see `./monacoLanguages.ts` for the registration
   * helpers and the Checkpoint 6 QA fix rationale (Issue 1, MAJOR). Prior
   * to that fix, the preview rendered as plaintext on first translation
   * because Grafana lazy-registers PromQL/LogQL inside the Prometheus and
   * Loki query-editor MonacoQueryField components (which only mount when
   * the user opens "Code" mode in those editors).
   */
  language: 'promql' | 'logql';

  /**
   * Optional natural-language explanation returned by the LLM describing how
   * it constructed the query. Rendered as a secondary-styled `<Text>` block
   * below the editor when present. Hidden entirely when omitted or empty.
   */
  explanation?: string;

  /**
   * Fired with the editor's current value whenever the user finishes editing
   * the generated query — specifically on editor blur AND on Cmd/Ctrl+S save.
   * The parent bar should treat this as the canonical "user has refined the
   * generated query" signal.
   */
  onChange: (newQuery: string) => void;

  /**
   * Fired when the user clicks the primary "Run" button. The parent bar is
   * responsible for executing the (possibly edited) query against the active
   * datasource using the existing query pipeline.
   */
  onRun: () => void;

  /**
   * Fired when the user clicks the secondary "Add as Panel" button. The
   * parent bar is responsible for delegating to the existing panel-creation
   * flow (which itself invokes `getAllSuggestions` for the visualization
   * recommendation) using the current query string.
   */
  onAddPanel: () => void;
}

/**
 * NLQ feature: editable query preview rendered by `NaturalLanguageQueryBar`
 * in the "Translated" state.
 *
 * Composition (top-to-bottom inside a vertical `Stack`):
 *   1. A small "Generated query" heading rendered with `<Text element="h6">`.
 *   2. A bordered wrapper containing a `<CodeEditor>` with PromQL/LogQL
 *      syntax highlighting. The wrapper supplies a `role="group"` plus
 *      `aria-label` so screen readers can identify the editor region.
 *   3. The LLM's `explanation` text (conditional).
 *   4. A horizontal `Stack` of two action buttons: "Run" (primary, icon
 *      `play`) and "Add as Panel" (secondary, icon `plus`).
 *
 * Accessibility:
 *   - `Button` components carry implicit `role="button"` and keyboard support.
 *   - The editor's surrounding region carries an explicit `aria-label`
 *     because `CodeEditor` itself does not accept `aria-label` as a prop
 *     (verified by reading `packages/grafana-ui/src/components/Monaco/types.ts`).
 *   - Tab order follows DOM order — input -> editor -> Run -> Add as Panel.
 */
export function NLQQueryPreview({
  translatedQuery,
  language,
  explanation,
  onChange,
  onRun,
  onAddPanel,
}: NLQQueryPreviewProps) {
  const styles = useStyles2(getStyles);

  // NLQ feature MINOR review fix (NLQQueryPreview.tsx L160-164):
  // Resolve Monaco's numeric configuration values (font size, vertical
  // padding) from the active GrafanaTheme2 so the preview honors the
  // design system rather than hardcoding pixel literals.
  //
  // Conversion notes:
  //  - `theme.typography.bodySmall.fontSize` is a CSS string (e.g.
  //    "12px"). Monaco's `fontSize` option requires a numeric pixel
  //    count, so `parseFontSizeToNumber` strips the unit and falls
  //    back to the Grafana default body-small size when the theme
  //    value is non-numeric (defense against future theme changes).
  //  - `theme.spacing(1)` returns a CSS string ("8px" in the default
  //    theme). Monaco's `padding.top` / `padding.bottom` likewise
  //    require numeric pixels, so the same parser is reused.
  //
  // The `useMemo` keeps the options object stable across re-renders
  // (which Monaco prefers — repeated identity changes can cause
  // unnecessary editor reconfiguration).
  const theme = useTheme2();
  const monacoOptions = useMemo(
    () => ({
      wordWrap: 'on' as const,
      scrollBeyondLastLine: false,
      fontSize: parseCssLengthToPx(theme.typography.bodySmall.fontSize, 12),
      padding: {
        top: parseCssLengthToPx(theme.spacing(1), 8),
        bottom: parseCssLengthToPx(theme.spacing(1), 8),
      },
      // Explicitly disable the minimap to keep the preview compact
      // even though `showMiniMap={false}` below already suppresses
      // it — defense in depth against future Monaco default changes.
      minimap: { enabled: false },
    }),
    [theme]
  );

  // NLQ feature CRITICAL review fix (NLQQueryPreview.tsx L188 — design
  // token compliance): resolve the editor height through the active
  // GrafanaTheme2 spacing scale rather than hardcoding a pixel literal.
  //
  // Grafana's default theme spacing base is 8px, so `theme.spacing(15)`
  // resolves to "120px" — visually identical to the previous hardcoded
  // value while ensuring the preview height tracks the design system if
  // the spacing scale ever changes. `parseCssLengthToPx` strips the
  // unit because Grafana's `CodeEditor.height` prop accepts only a
  // numeric pixel count (verified in
  // packages/grafana-ui/src/components/Monaco/types.ts).
  //
  // The `useMemo` keeps the numeric height stable across re-renders
  // (matching the `monacoOptions` memoization rationale above) so the
  // editor does not see height-prop identity churn that could trigger
  // unnecessary internal reflow.
  const editorHeightPx = useMemo(() => parseCssLengthToPx(theme.spacing(15), 120), [theme]);

  // Pre-compute the editor's accessible label so the string flows through the
  // i18n pipeline (Crowdin) exactly once per render, and so the value is
  // stable across re-renders for the same locale.
  const editorAriaLabel = t('nlq.preview.editor-aria-label', 'Generated query editor');

  // NLQ feature Checkpoint 6 QA fix (MAJOR Issue 1):
  //
  // `onBeforeEditorMount` is fired by the `CodeEditor` after Monaco has been
  // loaded but BEFORE the underlying editor and model are created. This is
  // the canonical hook for registering custom languages so that the model
  // created with `language: 'promql' | 'logql'` is immediately recognised by
  // Monaco rather than falling back to the plaintext tokenizer.
  //
  // The registration is idempotent: `ensureNLQLanguageRegistered` (see
  // `./monacoLanguages.ts`) guards each language behind a module-scoped flag
  // so re-mounting the preview many times within one page session pays the
  // setup cost at most once per language.
  //
  // We memoize on `language` so the callback identity is stable across
  // re-renders that do not change the language prop — preventing
  // `CodeEditor` from observing a fresh function on every render (which
  // some Monaco-React wrappers treat as a configuration change).
  const handleBeforeEditorMount = useCallback(
    (monaco: Monaco) => {
      ensureNLQLanguageRegistered(monaco, language);
    },
    [language]
  );

  return (
    <div className={styles.container} data-testid="nlq-query-preview">
      <Stack direction="column" gap={1}>
        <Text element="h6" variant="bodySmall" color="secondary">
          <Trans i18nKey="nlq.preview.label">Generated query</Trans>
        </Text>

        {/*
          The wrapper supplies both visual boundary (border + radius +
          overflow:hidden so Monaco's internal scrollbars stay inside) AND
          the accessible region label, since CodeEditor itself does not
          accept `aria-label` per its CodeEditorProps interface.
        */}
        <div
          className={styles.editorWrapper}
          role="group"
          aria-label={editorAriaLabel}
          data-testid="nlq-query-preview-editor"
        >
          <CodeEditor
            value={translatedQuery}
            language={language}
            // NLQ feature CRITICAL review fix: height is derived from
            // the design-system spacing scale (see the editorHeightPx
            // useMemo above) — no hardcoded pixel literal remains.
            height={editorHeightPx}
            showMiniMap={false}
            showLineNumbers={true}
            // NLQ feature Checkpoint 6 QA fix (MAJOR Issue 1): register the
            // PromQL / LogQL Monaco language BEFORE the editor's model is
            // created so the first translation shows proper syntax
            // highlighting rather than plaintext. See
            // `handleBeforeEditorMount` above and `./monacoLanguages.ts`
            // for the full rationale.
            onBeforeEditorMount={handleBeforeEditorMount}
            // Both onBlur AND onSave wire to the same onChange callback so
            // edits propagate whether the user clicks away from the editor
            // or hits Cmd/Ctrl+S inside it (per AAP §0.6.1.4 validation
            // checklist requiring both signals to be wired).
            onBlur={onChange}
            onSave={onChange}
            monacoOptions={monacoOptions}
          />
        </div>

        {explanation && (
          <Text variant="bodySmall" color="secondary">
            {explanation}
          </Text>
        )}

        {/*
          NLQ feature Checkpoint 6 QA fix (MINOR Issue 3 — Run + Add as
          Panel button row overflows at sub-800px viewports).
          `wrap` allows the Add-as-Panel button to flow onto a second row
          when the bar is narrower than the buttons' natural width
          (observed at viewports < ~800px, e.g. mobile portrait at 480px).
          At realistic ≥960px panel-editor sidebar widths the two buttons
          fit on one row and the layout is visually identical to the
          pre-fix appearance — `wrap` is a no-op when the container has
          room. Per `Stack.tsx:L18`, the `wrap` prop maps to CSS
          `flex-wrap: wrap` so the design-system contract is preserved
          (no raw flex CSS introduced).
        */}
        <Stack direction="row" gap={1} alignItems="center" wrap="wrap">
          <Button variant="primary" onClick={onRun} icon="play">
            <Trans i18nKey="nlq.preview.run-button">Run</Trans>
          </Button>
          <Button variant="secondary" onClick={onAddPanel} icon="plus">
            <Trans i18nKey="nlq.preview.add-panel-button">Add as Panel</Trans>
          </Button>
        </Stack>
      </Stack>
    </div>
  );
}

/**
 * Themed style factory consumed via `useStyles2`.
 *
 * Every value resolves to a `GrafanaTheme2` design token — no hardcoded
 * colors, spacings, or radii are introduced (per AAP §0.5.3 token rules).
 */
const getStyles = (theme: GrafanaTheme2) => ({
  container: css({
    padding: theme.spacing(1.5),
    backgroundColor: theme.colors.background.primary,
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
  }),
  editorWrapper: css({
    // Bound Monaco's overflow so the editor lives cleanly inside the bar
    // and never visually escapes the NLQ container's rounded corners.
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
    overflow: 'hidden',
  }),
});

/**
 * NLQ feature: parse a CSS length string (e.g. "12px", "0.75rem") to a
 * numeric pixel value for Monaco editor options that require integers.
 *
 * Why this exists:
 *   `GrafanaTheme2` exposes typography sizes and spacing values as CSS
 *   length strings (the same strings the design system uses in stylesheets),
 *   but Monaco's editor options (`fontSize`, `padding.top`, `padding.bottom`)
 *   require numeric pixel counts. This helper bridges the two
 *   representations without introducing hardcoded fallback literals at
 *   the call site — every value in the preview still traces back to the
 *   `useTheme2()` hook.
 *
 * Conversion rules:
 *   - Strings matching the pixel format ("12px") are parsed directly.
 *   - Strings matching the rem format ("0.75rem") are converted to
 *     pixels using a 16px-per-rem assumption (the CSS default; Grafana's
 *     theme uses this convention).
 *   - Any non-numeric or unrecognised input falls back to `defaultPx`.
 *     This keeps the function total — it never throws — so a future
 *     theme change cannot crash the preview.
 *
 * Both arguments are required so the call site documents what the
 * fallback represents semantically (e.g., "default body-small size").
 */
export function parseCssLengthToPx(value: string | number | undefined, defaultPx: number): number {
  if (value == null) {
    return defaultPx;
  }
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value;
  }
  if (typeof value !== 'string') {
    return defaultPx;
  }
  const trimmed = value.trim();
  const pxMatch = /^(-?\d+(?:\.\d+)?)px$/i.exec(trimmed);
  if (pxMatch) {
    const n = parseFloat(pxMatch[1]);
    return Number.isFinite(n) ? n : defaultPx;
  }
  const remMatch = /^(-?\d+(?:\.\d+)?)rem$/i.exec(trimmed);
  if (remMatch) {
    const n = parseFloat(remMatch[1]) * 16;
    return Number.isFinite(n) ? n : defaultPx;
  }
  // Plain numeric strings ("12") are accepted as pixel counts.
  const plain = parseFloat(trimmed);
  if (!Number.isNaN(plain) && trimmed === String(plain)) {
    return plain;
  }
  return defaultPx;
}
