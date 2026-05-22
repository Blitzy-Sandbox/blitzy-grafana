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

import { GrafanaTheme2 } from '@grafana/data';
import { Trans, t } from '@grafana/i18n';
import { Button, CodeEditor, Stack, Text, useStyles2 } from '@grafana/ui';

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
   * Both languages are pre-registered globally by Grafana's Monaco setup
   * (see `PromQueryCodeEditor.tsx` and `LokiQueryCodeEditor.tsx`); no
   * additional language registration is required here.
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

  // Pre-compute the editor's accessible label so the string flows through the
  // i18n pipeline (Crowdin) exactly once per render, and so the value is
  // stable across re-renders for the same locale.
  const editorAriaLabel = t('nlq.preview.editor-aria-label', 'Generated query editor');

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
            height={120}
            showMiniMap={false}
            showLineNumbers={true}
            // Both onBlur AND onSave wire to the same onChange callback so
            // edits propagate whether the user clicks away from the editor
            // or hits Cmd/Ctrl+S inside it (per AAP §0.6.1.4 validation
            // checklist requiring both signals to be wired).
            onBlur={onChange}
            onSave={onChange}
            monacoOptions={{
              wordWrap: 'on',
              scrollBeyondLastLine: false,
              fontSize: 13,
              padding: { top: 8, bottom: 8 },
              // Explicitly disable the minimap to keep the preview compact
              // even though `showMiniMap={false}` above already suppresses
              // it — defense in depth against future Monaco default changes.
              minimap: { enabled: false },
            }}
          />
        </div>

        {explanation && (
          <Text variant="bodySmall" color="secondary">
            {explanation}
          </Text>
        )}

        <Stack direction="row" gap={1} alignItems="center">
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
