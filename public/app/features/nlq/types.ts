// NLQ feature: TypeScript DTOs for the Natural Language Query module.
//
// This file mirrors the JSON tag names of the Go DTOs declared in
// `pkg/services/nlq/models.go` EXACTLY, so that the over-the-wire payload
// exchanged between the frontend (`getBackendSrv().post('/api/nlq/translate', ...)`)
// and the backend handler is type-safe in both directions.
//
// Rules (per AAP §0.6.1.4 and §0.8):
//   - Pure type declarations only — no imports, no runtime code, no enums, no classes.
//   - Field names are the JSON tag names from the Go side (`input`, `datasourceUid`, …),
//     NOT the Go struct field names (`NaturalLanguage`, `DatasourceUID`, …).
//   - Go `,omitempty` maps to TS optional (`?:`).
//   - Not re-exported by the barrel `index.ts` — these types are internal to the
//     `nlq` feature module (only `NaturalLanguageQueryBar` is the public surface).

/**
 * NLQ feature: request DTO for `POST /api/nlq/translate`.
 *
 * MUST stay structurally identical to the Go DTO defined in
 * `pkg/services/nlq/models.go`:
 *
 * ```go
 * type TranslateRequest struct {
 *   NaturalLanguage string `json:"input"`
 *   DatasourceUID   string `json:"datasourceUid"`
 *   DatasourceType  string `json:"datasourceType"`
 * }
 * ```
 *
 * Field names use the JSON tag names from the Go side so that the over-the-wire
 * payload is the same in both directions.
 */
export interface TranslateRequest {
  /**
   * The user's plain-English question, e.g.
   * `"Show me failed login attempts in the last hour grouped by IP"`.
   * The server validates that this is non-empty and returns 400 otherwise.
   */
  input: string;

  /**
   * UID of the target datasource that the generated query will run against.
   * The server validates that the caller has the `datasources.ActionQuery`
   * permission for this UID before invoking the LLM.
   */
  datasourceUid: string;

  /**
   * Datasource plugin type, e.g. `'prometheus'` or `'loki'`.
   * The server returns 400 if this is outside the supported set
   * (currently Prometheus/Mimir for PromQL and Loki for LogQL).
   */
  datasourceType: string;
}

/**
 * NLQ feature: response DTO returned by `POST /api/nlq/translate`.
 *
 * MUST stay structurally identical to the Go DTO defined in
 * `pkg/services/nlq/models.go`:
 *
 * ```go
 * type TranslateResponse struct {
 *   Query       string   `json:"query"`
 *   Language    string   `json:"language"`
 *   Explanation string   `json:"explanation,omitempty"`
 *   Warnings    []string `json:"warnings,omitempty"`
 * }
 * ```
 *
 * The `language` field is narrowed at the type level to `'promql' | 'logql'`
 * because those are the only two supported datasource backends in this release
 * (per AAP §0.6.3.1). The consuming hook (`useNLQTranslation`) should still
 * narrow defensively at runtime to gracefully handle a misbehaving server.
 */
export interface TranslateResponse {
  /** The generated PromQL or LogQL query string. Never empty on a 200 response. */
  query: string;

  /**
   * Query language identifier used by `NLQQueryPreview` to set the Monaco
   * editor's `language` prop for syntax highlighting and validation.
   */
  language: 'promql' | 'logql';

  /**
   * Optional human-readable explanation of how the LLM constructed the query.
   * Surfaced in the UI as plain text below the query preview when present.
   * Optional because the Go DTO carries `,omitempty` on this field.
   */
  explanation?: string;

  /**
   * Non-fatal warnings emitted by the server. The canonical example is when
   * the schema-context fetch fails and translation proceeds prompt-only; the
   * server attaches a warning so the UI can render an `<Alert severity="info">`
   * informing the user that the result may be less accurate.
   * Optional because the Go DTO carries `,omitempty` on this field.
   */
  warnings?: string[];
}
