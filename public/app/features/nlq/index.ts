// NLQ feature: public API surface. Only NaturalLanguageQueryBar is exported;
// all other files (hook, preview, types, api client) are internal implementation details.
//
// Consumers — specifically
// public/app/features/dashboard-scene/panel-edit/PanelDataPane/PanelDataQueriesTab.tsx
// (per AAP §0.3.1.1 "Panel Editor Touchpoint" / §0.4.3.5) — must import via the
// `app/features/nlq` module path:
//
//   import { NaturalLanguageQueryBar } from 'app/features/nlq';
//
// and MUST NOT reach into internal implementation files (NLQQueryPreview,
// useNLQTranslation, nlqApi, types). This encapsulation rule is mandated by
// AAP §0.7.1.1 (in-scope file list) and §0.3.2.2 (barrel description), and
// matches the precedent of other feature folders in `public/app/features/`.
//
// The `no-barrel-files/no-barrel-files` ESLint rule (configured at
// eslint.config.js for `public/app/**/*.{ts,tsx}` files) flags any
// `export { X } from './Y'` form as a re-export. Per the established
// codebase pattern (see public/app/api/clients/iam/v0alpha1/index.ts and
// public/app/features/migrate-to-cloud/api/index.ts), the line below is
// suppressed with a targeted `eslint-disable-next-line` directive because
// a single-name re-export is the explicit, prescribed shape of this file
// (AAP §0.3.2.2). The directive applies only to the next statement and does
// NOT enable broader barrel-file usage anywhere else in the NLQ feature.
// eslint-disable-next-line no-barrel-files/no-barrel-files
export { NaturalLanguageQueryBar } from './NaturalLanguageQueryBar';
