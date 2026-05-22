// Package nlq contains the Natural Language Query (NLQ) translation
// service, which converts plain-English user input into a syntactically
// valid PromQL or LogQL query for the active panel datasource.
//
// This file declares the foundational domain types and sentinel errors
// used across the package. It intentionally contains NO business logic,
// NO methods, and NO dependencies beyond the standard library `errors`
// package, per the requirements set out in the Agent Action Plan
// (AAP §0.6.1.1 and the file-creation prompt).
package nlq

import "errors"

// TranslateRequest is the JSON body of POST /api/nlq/translate.
//
// The JSON shape mirrors the frontend TypeScript interface declared at
// public/app/features/nlq/types.ts. The two declarations form a single
// HTTP contract; any change in either file is a breaking change.
//
// Field validation (non-empty input, supported datasource type, etc.)
// is performed by the PostTranslate handler in translate.go before the
// request reaches the Translate orchestrator.
type TranslateRequest struct {
	// NaturalLanguage is the user's plain-English question, for
	// example: "Show me failed login attempts in the last hour
	// grouped by IP". The JSON tag is "input" to align with the
	// frontend hook (useNLQTranslation) which posts the body as
	// { input, datasourceUid, datasourceType }.
	NaturalLanguage string `json:"input"`

	// DatasourceUID is the UID of the active panel datasource. It is
	// used both as the scope value for the RBAC permission check
	// (ac.EvalPermission(datasources.ActionQuery, scope)) and as the
	// lookup key for schema-context retrieval in schema_context.go.
	DatasourceUID string `json:"datasourceUid"`

	// DatasourceType identifies the target query language. The
	// supported values are:
	//   - "prometheus" (Prometheus and Mimir share the same backend
	//     type identifier and produce PromQL).
	//   - "mimir"      (kept as a distinct value for forward
	//     compatibility; treated as Prometheus-compatible per AAP
	//     §0.6.3.1).
	//   - "loki"       (produces LogQL).
	// Any other value yields ErrUnsupportedDatasource.
	DatasourceType string `json:"datasourceType"`
}

// TranslateResponse is the JSON body returned by POST /api/nlq/translate
// on success. The JSON shape mirrors the frontend TypeScript interface
// at public/app/features/nlq/types.ts.
//
// IMPORTANT — security invariant (AAP §0.8.5):
// This response is sent verbatim to the client. The implementing
// service MUST NEVER place the LLM API key, the system prompt, or any
// other secret-bearing content into any field of this struct. Frontend
// rendering treats Query as user-editable text and Explanation/Warnings
// as displayable strings; placing a secret in any of them would expose
// it directly in the browser.
type TranslateResponse struct {
	// Query is the translated PromQL or LogQL string. It is always
	// non-empty on success (translate.go's parseResponse validates
	// this before returning).
	Query string `json:"query"`

	// Language identifies the dialect of Query. Permitted values:
	//   - "promql" for Prometheus and Mimir datasources.
	//   - "logql" for Loki datasources.
	// The frontend uses this value to configure the Monaco CodeEditor
	// syntax-highlighting mode for the preview component.
	Language string `json:"language"`

	// Explanation is a short human-readable description of what the
	// query does, returned by the LLM alongside the translated query.
	// It is optional; the omitempty tag hides it from the JSON body
	// when the LLM did not produce one (or it was empty after trim).
	Explanation string `json:"explanation,omitempty"`

	// Warnings is a list of non-fatal advisories surfaced to the
	// user. Schema-context fetch failures (e.g., the datasource was
	// not found, the caller lacks read permission on it, the upstream
	// labels endpoint returned an error) are added here so that the
	// translation can still proceed prompt-only while the user is
	// notified that context was reduced. The omitempty tag suppresses
	// the field from the JSON body when the slice is nil or empty.
	Warnings []string `json:"warnings,omitempty"`
}

// SchemaContext aggregates the datasource metadata that the translate
// pipeline injects into the LLM prompt. It is constructed by
// schema_context.go and consumed by translate.go's buildPrompt; it is
// strictly package-internal and is never serialized to the HTTP wire,
// which is why no JSON tags are declared.
//
// Field population is conditional on the datasource type:
//   - Prometheus / Mimir: Labels and Metrics are populated.
//   - Loki:               Labels and Streams are populated.
//
// An empty SchemaContext (all slices nil) is a valid value. It
// signals that schema metadata was unavailable for some reason and
// instructs translate.go to fall back to a prompt-only translation
// (the LLM relies on its general training to produce a plausible
// query) and to attach an informational entry to TranslateResponse.Warnings.
type SchemaContext struct {
	// Labels is the set of label names known for the datasource.
	// Examples:
	//   - Prometheus: "__name__", "job", "instance", "namespace".
	//   - Loki:       "job", "namespace", "pod", "container".
	Labels []string

	// Metrics is populated ONLY for Prometheus and Mimir datasources.
	// Examples: "http_requests_total", "process_cpu_seconds_total".
	Metrics []string

	// Streams is populated ONLY for Loki datasources. Each entry is a
	// stream selector that frequently appears in the datasource, for
	// example: `{job="varlogs"}` or `{namespace="default"}`.
	Streams []string
}

// Sentinel errors used for typed error classification across the nlq
// package. They are the boundary between the domain orchestrator
// (Translate) and the HTTP error handler (PostTranslate), which uses
// errors.Is(err, ErrXxx) to map each sentinel to the correct HTTP
// status code:
//
//	ErrEmptyInput            -> 400 Bad Request
//	ErrUnsupportedDatasource -> 400 Bad Request
//	ErrMissingAPIKey         -> 500 Internal Server Error
//	ErrLLMUnavailable        -> 502 Bad Gateway
//
// Callers MUST use errors.Is for classification because the Translate
// orchestrator wraps these sentinels with %w to add contextual detail
// (for example, the unsupported datasource type value or the upstream
// HTTP status code). Direct equality comparison would fail in those
// wrapped cases.
//
// All sentinel error strings begin with the "nlq: " prefix to make
// log scraping and correlation trivial, mirroring the convention
// established by pkg/services/correlations/models.go.
//
// Stability: once this package ships, the .Error() string of each
// sentinel becomes public observable behavior and changing it may
// break log scrapers and test assertions. Treat the strings as part
// of the package's stable contract.
var (
	// ErrEmptyInput is returned when the user-supplied natural
	// language input is empty or whitespace-only. PostTranslate
	// surfaces this as a 400 Bad Request.
	ErrEmptyInput = errors.New("nlq: natural language input cannot be empty")

	// ErrUnsupportedDatasource is returned when the request's
	// DatasourceType is not one of the supported values ("prometheus",
	// "mimir", or "loki"). PostTranslate surfaces this as a 400 Bad
	// Request. Callers may wrap this sentinel with %w to include the
	// offending type value in the wrapped message; errors.Is will
	// still classify the result correctly.
	ErrUnsupportedDatasource = errors.New("nlq: unsupported datasource type")

	// ErrMissingAPIKey is returned when the GF_NLQ_LLM_API_KEY
	// environment variable is unset or empty at translate time.
	//
	// CRITICAL — security invariant (AAP §0.8.5): the error value
	// MUST NOT contain or echo the environment variable's value. The
	// message references ONLY the env var name (which is a public
	// contract documented in conf/defaults.ini), never the value.
	// Callers wrapping this error MUST preserve the same invariant.
	ErrMissingAPIKey = errors.New("nlq: GF_NLQ_LLM_API_KEY environment variable is not set")

	// ErrLLMUnavailable is returned when the LLM provider is
	// unreachable, returns a non-2xx HTTP status code, or returns a
	// response body that fails to parse into the expected shape.
	// PostTranslate surfaces this as a 502 Bad Gateway.
	//
	// CRITICAL — security invariant (AAP §0.8.5): callers wrapping
	// this error MUST NOT include the API key, the response body, or
	// any portion of the prompt content in the wrapped message. Only
	// the HTTP status code and a generic transport-level error are
	// safe to include.
	ErrLLMUnavailable = errors.New("nlq: LLM provider is unavailable")
)
