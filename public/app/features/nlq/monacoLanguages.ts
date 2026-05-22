// NLQ feature: idempotent registration of the PromQL and LogQL Monaco
// languages used by `NLQQueryPreview`'s `<CodeEditor>` (Checkpoint 6 QA fix
// for MAJOR Issue 1 — PromQL/LogQL preview rendered as plaintext on first
// translation).
//
// Why this file exists
// --------------------
// Grafana does NOT pre-register PromQL or LogQL into the Monaco singleton
// at app boot. Instead, both languages are registered LAZILY by the
// Prometheus and Loki query-editor `MonacoQueryField` components — but only
// when the user activates "Code" mode in those specific editors. (See
// `packages/grafana-prometheus/src/components/monaco-query-field/MonacoQueryField.tsx:L66-L78`
// for the canonical PromQL `ensurePromQL` setup, and
// `public/app/plugins/datasource/loki/components/monaco-query-field/MonacoQueryField.tsx:L64-L79`
// for the equivalent LogQL `ensureLogQL` setup.)
//
// The NLQ Preview renders an editable Monaco `CodeEditor` BEFORE the user
// has interacted with the datasource's own query editor — and typical NLQ
// workflows never require the user to touch Code mode at all. Without the
// language being registered, the editor falls back to the plaintext
// tokenizer and the AAP-promised "syntax highlighting" experience
// (AAP §0.6.1.4) is silently degraded to uniform-color text. This is
// precisely the defect documented in the Checkpoint 6 QA report
// (Issue 1, MAJOR).
//
// Fix approach
// ------------
// Per the QA finding's suggested fix #1 ("Import the language registration
// side-effect modules directly … inside a `useEffect` triggered by the
// chosen `language` prop") and #2 ("Expose a `registerLanguage(language)`
// utility from the language module"), this module:
//
//   1. Exposes `ensurePromQLRegistered(monaco)` and
//      `ensureLogQLRegistered(monaco)` — idempotent setup functions that
//      register each language at most once per page load. Mirrors the
//      `PROMQL_SETUP_STARTED` / `LANGUAGE_SETUP_STARTED` boolean-flag
//      pattern used by the existing plugin components, so the registration
//      cost (~150 KB of Monarch tokenizer JSON for PromQL) is paid at most
//      once per page load regardless of how many NLQ Preview instances mount.
//   2. Exposes a `ensureNLQLanguageRegistered(monaco, language)`
//      dispatcher consumed by `NLQQueryPreview`'s `onBeforeEditorMount`
//      callback so a single hook fires per editor mount and the choice of
//      language is encapsulated here rather than leaking into the consuming
//      component.
//
// Why the imports are safe
// ------------------------
//  - `@grafana/monaco-logql` is a first-class root dependency declared in
//    the top-level `package.json` (`^0.0.8`), so this import is
//    type-safe and explicitly resolved by the bundler.
//  - `monaco-promql` is a transitive dependency declared by the
//    `@grafana/prometheus` workspace package (root depends on
//    `@grafana/prometheus`, which depends on `monaco-promql@1.8.0`). The
//    Yarn workspace hoists `monaco-promql` to root `node_modules/`, the
//    bundler resolves it via standard Node module resolution, and TypeScript
//    finds its `index.d.ts` via the same lookup. The Jest config
//    (`jest.config.js:L21`) already includes `monaco-promql` in the
//    `esModules` list so the package is correctly transformed under test.
//
// Why both static imports are top-level (not dynamic)
// ---------------------------------------------------
// Both packages export only data structures (plain objects describing the
// language definitions). Neither package executes Monaco API calls at
// module evaluation time — verified by reading
// `node_modules/monaco-promql/index.js`,
// `node_modules/monaco-promql/promql/promql.contribution.js`, and
// `node_modules/@grafana/monaco-logql/index.js`. The `monaco-promql`
// package defers the heavy tokenizer payload via a `loader()` function
// (dynamic `import('./promql')`) so the tokenizer is only fetched when the
// preview actually mounts. This matches the lazy-load discipline used by
// the existing Prometheus plugin.
//
// Testing notes
// -------------
// `NaturalLanguageQueryBar.test.tsx` mocks `@grafana/ui`'s `CodeEditor`
// with a plain `<textarea>` — the mock never fires `onBeforeEditorMount`,
// so the registration functions here are not invoked in unit tests. The
// module still loads via the import chain (`NaturalLanguageQueryBar.tsx`
// -> `NLQQueryPreview.tsx` -> `monacoLanguages.ts`), but the top-level
// imports of `monaco-promql` and `@grafana/monaco-logql` execute cleanly in
// jsdom because neither package touches the Monaco singleton at load time.
//
// Production timing model
// -----------------------
// 1. `onBeforeEditorMount` fires when the lazy `ReactMonacoEditor`
//    component has loaded Monaco but BEFORE the underlying editor + model
//    are created.
// 2. `ensureNLQLanguageRegistered(monaco, language)` runs synchronously,
//    registering the language ID (`monaco.languages.register({id})`).
// 3. The editor / model are then created with `language: 'promql'` or
//    `'logql'`. Monaco recognises the language ID immediately, but the
//    tokenizer for PromQL is still loading asynchronously.
// 4. The PromQL `loader()` Promise resolves a few milliseconds later;
//    `setMonarchTokensProvider` is called and Monaco retroactively
//    re-tokenises the existing model. (Documented Monaco behaviour: when
//    a Monarch tokens provider is set after model creation, all models
//    with that language ID are re-tokenised. This is how the existing
//    Prometheus plugin works and how Monaco's `onLanguage` hook is
//    designed.)
// 5. LogQL has no loader pattern — `@grafana/monaco-logql` exports the
//    tokenizer eagerly — so steps 3 + 4 are collapsed into step 2 for the
//    LogQL path: registration is fully synchronous.
//
// Security / privacy
// ------------------
// Per AAP §0.8.5 ("the API key MUST NOT appear in any log line", extended
// in NLQ feature discipline to all user-supplied content), the error
// handler in `ensurePromQLRegistered` logs ONLY library / loader
// information — never any user-supplied content (which this module never
// sees in the first place, as it operates purely on the Monaco-side
// language registration surface).

import { promLanguageDefinition } from 'monaco-promql';

import {
  languageConfiguration as logqlLanguageConfiguration,
  monarchlanguage as logqlMonarchLanguage,
} from '@grafana/monaco-logql';
import type { Monaco } from '@grafana/ui';

// Module-scoped registration flags. Mirrors the
// `PROMQL_SETUP_STARTED` / `LANGUAGE_SETUP_STARTED` flags used by the
// existing Prometheus and Loki MonacoQueryField components so the cost of
// registering each language is paid at most once per page load.
//
// `let` (mutable) is deliberate — see `ensurePromQLRegistered` below for
// the failure-path branch that resets `PROMQL_REGISTERED` so a subsequent
// editor mount may retry the async tokenizer load if the first attempt
// failed (e.g. transient network blip when the lazy chunk fetch races
// against a service worker cache rebuild).
let PROMQL_REGISTERED = false;
let LOGQL_REGISTERED = false;

/**
 * NLQ feature: idempotently registers the PromQL Monaco language so that
 * the NLQ Preview's `<CodeEditor language="promql">` renders with proper
 * syntax highlighting on first mount.
 *
 * Synchronous portion (always runs on first call):
 *   - Registers the language ID (`'promql'`) and its associated aliases,
 *     extensions, and mimetypes via `monaco.languages.register`. This is
 *     what teaches Monaco to recognise `language: 'promql'` on a model.
 *
 * Asynchronous portion:
 *   - Lazily loads the Monarch tokenizer + language configuration via
 *     `promLanguageDefinition.loader()` (a dynamic `import('./promql')`
 *     internally). Once the module resolves, the tokenizer and language
 *     configuration are installed. Monaco re-tokenises existing models
 *     with this language ID on `setMonarchTokensProvider`, so any model
 *     created before the async load completes is upgraded retroactively
 *     to syntax-highlighted display.
 *
 * Failure handling:
 *   - On loader rejection (e.g. transient chunk-load failure), the module
 *     flag is reset so a future mount can retry the load. A non-sensitive
 *     warning is emitted via `console.warn`; no user-supplied content is
 *     included in the log line. This satisfies the AAP §0.8.5
 *     secret-handling discipline applied to user content as well.
 *
 * @param monaco - The Monaco namespace passed by the `CodeEditor`'s
 *   `onBeforeEditorMount` callback (i.e. the `monaco` global, scoped to
 *   the editor instance).
 */
export function ensurePromQLRegistered(monaco: Monaco): void {
  if (PROMQL_REGISTERED) {
    return;
  }
  PROMQL_REGISTERED = true;

  const { id, aliases, extensions, mimetypes, loader } = promLanguageDefinition;
  monaco.languages.register({ id, aliases, extensions, mimetypes });

  loader().then(
    (mod) => {
      // Monaco's `setMonarchTokensProvider` accepts an
      // `IMonarchLanguage` definition. The upstream typings on
      // `monaco-promql`'s `promql.d.ts` declare the export as
      // `languages.IMonarchLanguage`, which structurally matches.
      monaco.languages.setMonarchTokensProvider(id, mod.language);
      monaco.languages.setLanguageConfiguration(id, mod.languageConfiguration);
    },
    (err: unknown) => {
      // Reset the flag so a subsequent mount may retry the async
      // tokenizer load (e.g., if the first attempt failed due to a
      // transient chunk-load network error). We deliberately do NOT log
      // the language id or any caller-supplied data — only the bare
      // error value forwarded from the Promise rejection — so the
      // diagnostic is useful for operators without leaking user content
      // (per AAP §0.8.5 discipline applied to user content).
      PROMQL_REGISTERED = false;
      // eslint-disable-next-line no-console
      console.warn('NLQ: failed to load PromQL Monaco language module', err);
    }
  );
}

/**
 * NLQ feature: idempotently registers the LogQL Monaco language so that
 * the NLQ Preview's `<CodeEditor language="logql">` renders with proper
 * syntax highlighting on first mount.
 *
 * `@grafana/monaco-logql` ships the language configuration and Monarch
 * tokenizer as eagerly-exported constants (no loader pattern), so all
 * three Monaco API calls execute synchronously here — registration is
 * complete by the time this function returns.
 *
 * @param monaco - The Monaco namespace passed by the `CodeEditor`'s
 *   `onBeforeEditorMount` callback.
 */
export function ensureLogQLRegistered(monaco: Monaco): void {
  if (LOGQL_REGISTERED) {
    return;
  }
  LOGQL_REGISTERED = true;

  // LogQL has no aliases/extensions/mimetypes published by
  // `@grafana/monaco-logql`, so the minimal registration mirrors the
  // pattern in the existing Loki MonacoQueryField setup (which also
  // registers with `{ id: LANG_ID }` only).
  monaco.languages.register({ id: 'logql' });
  monaco.languages.setMonarchTokensProvider('logql', logqlMonarchLanguage);
  monaco.languages.setLanguageConfiguration('logql', logqlLanguageConfiguration);
}

/**
 * NLQ feature: dispatcher that selects the correct registration function
 * for the NLQ-supported language set.
 *
 * Consumed by `NLQQueryPreview`'s `onBeforeEditorMount` so a single
 * callback handles both languages without leaking the choice into the
 * consuming component. The language-narrowing type signature
 * (`'promql' | 'logql'`) matches `NLQQueryPreviewProps.language`, so
 * TypeScript prevents accidentally calling this with a value outside the
 * NLQ-supported set.
 *
 * Unknown values cannot be reached under the typed contract, but a
 * defensive no-op fallback keeps the helper total in case the consumer is
 * ever extended with a wider language type.
 *
 * @param monaco - The Monaco namespace passed by the `CodeEditor`'s
 *   `onBeforeEditorMount` callback.
 * @param language - One of `'promql'` or `'logql'`, sourced from
 *   `NLQQueryPreviewProps.language`.
 */
export function ensureNLQLanguageRegistered(monaco: Monaco, language: 'promql' | 'logql'): void {
  if (language === 'promql') {
    ensurePromQLRegistered(monaco);
    return;
  }
  if (language === 'logql') {
    ensureLogQLRegistered(monaco);
    return;
  }
}
