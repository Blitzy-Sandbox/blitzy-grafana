// translate.go implements the translation pipeline at the heart of
// the NLQ service:
//
//   - PostTranslate is the HTTP handler for POST /api/nlq/translate.
//     It performs request-body binding, derives orgID from the
//     authenticated session, delegates to Translate, and converts
//     typed errors into HTTP status codes.
//   - Translate is the orchestrator: validate input, fetch schema
//     context (best effort), build the LLM prompt, call the LLM, parse
//     the response, attach warnings.
//   - buildPrompt, callLLM, parseResponse are unexported helpers
//     scoped to this file.
//   - safeHost is a package-private free helper that strips a URL
//     down to its host component for safe inclusion in log lines.
//     Every log call in this file that references the configured
//     LLM endpoint MUST route the value through safeHost so that
//     accidentally embedded credentials in the URL (a misconfiguration
//     pattern) never reach the log stream.
//
// The LLM call uses ONLY the Go standard library net/http per AAP
// §0.2.1 ("Use standard net/http: backend LLM calls use Go standard
// library net/http — no new module dependency").
//
// SECURITY — API key handling (AAP §0.8.5):
// The LLM API key is read EXCLUSIVELY from os.Getenv("GF_NLQ_LLM_API_KEY")
// at the call site inside callLLM. It is:
//   - NEVER stored on the Service struct (the struct has no key field).
//   - NEVER read from the ini configuration tree.
//   - NEVER logged (Debug/Info/Warn/Error in this file include only
//     identifiers, status codes, and error messages from underlying
//     stdlib types; never the key value).
//   - NEVER echoed back in TranslateResponse or any error message.
//   - NEVER persisted across the lifetime of a single request.
//
// SECURITY — user input handling (AAP §0.8.6):
// The user-supplied NaturalLanguage field is forwarded to the LLM
// as the user-role message content. It is intentionally NOT logged
// at any level (log scraping in production must not leak user
// queries) and is intentionally NOT echoed back in error messages.

package nlq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	// NLQ feature: PromQL parser used to validate the LLM's translated
	// output is syntactically valid before returning it to the client.
	// Imported from prometheus/prometheus which is already a direct
	// dependency of Grafana (see go.mod) — no new dependency is added.
	promparser "github.com/prometheus/prometheus/promql/parser"

	// NLQ feature: LogQL parser used for the same purpose against the
	// Loki dialect. Imported from grafana/loki which is already a
	// direct dependency of Grafana (see go.mod) — no new dependency
	// is added. ParseExpr is the strict variant that rejects empty
	// {} selectors and other malformed expressions; this matches the
	// validation contract a real Loki datasource would enforce.
	logqlsyntax "github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/grafana/grafana/pkg/api/response"
	"github.com/grafana/grafana/pkg/apimachinery/identity"
	ac "github.com/grafana/grafana/pkg/services/accesscontrol"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/datasources"
	"github.com/grafana/grafana/pkg/web"
)

// nlqAPIKeyEnvVar is the canonical environment-variable NAME that
// supplies the LLM provider API key at runtime. It follows the
// EnvKey convention documented at pkg/setting/setting.go:L886-L892
// ("GF_<SECTION>_<KEY>"), with the suffix "_LLM_API_KEY" matching
// the explicit user-facing contract called out in AAP §0.1.1 and
// the conf/defaults.ini comments.
//
// This constant holds ONLY the name of the env var, never its
// value. The actual API key is read at translate-time via
// os.Getenv(nlqAPIKeyEnvVar) inside callLLM and is immediately
// scoped out (per AAP §0.8.5).
//
// Centralizing the name here makes the security invariant easy to
// audit: "grep for 'GF_NLQ_LLM_API_KEY' must produce exactly one
// match in the .go source tree".
//
//nolint:gosec // G101: this is an environment-variable name, not a credential value.
const nlqAPIKeyEnvVar = "GF_NLQ_LLM_API_KEY"

// supportedLanguages maps a normalized datasource type to the query
// language identifier returned in TranslateResponse.Language. The
// frontend uses the returned language to configure the CodeEditor
// syntax-highlighting mode.
//
// "mimir" maps to "promql" because Grafana Mimir is a Prometheus-API
// compatible long-term storage backend (per AAP §0.6.3.1); queries
// against Mimir are PromQL.
//
// Updating this map is the entire surface for adding a new
// supported datasource. The schema_context.go and buildPrompt code
// must also be updated in lockstep, but the validation path in
// Translate consults this map as the source of truth.
var supportedLanguages = map[string]string{
	"prometheus": "promql",
	"mimir":      "promql",
	"loki":       "logql",
}

// llmRequest is the JSON shape of the outbound request body sent to
// the LLM provider. It follows the OpenAI Chat Completions schema
// (https://platform.openai.com/docs/api-reference/chat) which is the
// de-facto standard for hosted LLM providers — most non-OpenAI
// providers (Anthropic, Mistral, vLLM, llama.cpp, Ollama) implement
// the same wire shape behind a compatibility shim.
//
// The struct intentionally omits optional fields (temperature,
// top_p, response_format, etc.) so the LLM provider's defaults
// apply. A future configuration knob can expose these via
// conf/defaults.ini without breaking the wire shape.
type llmRequest struct {
	Model    string       `json:"model"`
	Messages []llmMessage `json:"messages"`
}

// llmMessage is one element of llmRequest.Messages. Role is either
// "system" or "user"; Content is the raw text of the prompt segment.
type llmMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// llmResponse is the JSON shape of the inbound response from the
// LLM provider. Only the fields actually consumed by parseResponse
// are declared; any extra fields the provider returns (usage,
// system_fingerprint, etc.) are tolerated because encoding/json
// silently ignores them.
type llmResponse struct {
	Choices []struct {
		Message llmMessage `json:"message"`
	} `json:"choices"`
}

// parsedQuery is the JSON shape the LLM is instructed to return
// inside Choices[0].Message.Content. The Query field is the
// PromQL/LogQL string; Explanation is an optional one-sentence
// human-readable description.
type parsedQuery struct {
	Query       string `json:"query"`
	Explanation string `json:"explanation"`
}

// PostTranslate is the HTTP handler for POST /api/nlq/translate.
//
// Request lifecycle:
//  1. web.Bind decodes the JSON body into a TranslateRequest. The
//     request is rejected with 400 Bad Request on a malformed body.
//  2. orgID is derived from the authenticated session via
//     c.GetOrgID() — NEVER from the request body — so a client
//     cannot spoof another organization's datasource by submitting
//     an arbitrary UID.
//  3. UID-scoped authorization runs BEFORE any expensive work.
//     ac.EvalPermission(datasources.ActionQuery, scope) where scope
//     is derived from req.DatasourceUID enforces per-datasource
//     RBAC; a caller with broad datasources:query permission cannot
//     reach the LLM for a UID they should not query. This addresses
//     the CRITICAL review finding that route-level evaluation was
//     scope-less.
//  4. Translate is invoked with the validated, authorized request
//     and the authenticated orgID. It returns a TranslateResponse
//     on success and a typed sentinel error on failure.
//  5. errors.Is is used to classify the returned error and map it
//     to the appropriate HTTP status code (see errorResponse below).
//  6. On success, response.JSON serialises TranslateResponse with
//     200 OK and the application/json content type. The frontend
//     consumes the resulting body directly.
//
// CORS / CSRF / auth: the upstream middleware chain registered in
// service.go's registerAPIEndpoints (middleware.ReqSignedIn) has
// enforced authentication before this handler runs. UID-scoped
// authorization happens here because the target UID is only
// available after the request body has been parsed.
func (s *Service) PostTranslate(c *contextmodel.ReqContext) response.Response {
	req := TranslateRequest{}
	if err := web.Bind(c.Req, &req); err != nil {
		// SECURITY: do not echo the underlying request body in the
		// error message. web.Bind returns generic parsing errors
		// that do not include user content, so passing err through
		// directly is safe.
		return response.Error(http.StatusBadRequest, "bad request data", err)
	}

	// NLQ feature CRITICAL fix: validate that DatasourceUID is
	// present and non-whitespace BEFORE invoking the access-control
	// system. ScopeProvider.GetResourceScopeUID(req.DatasourceUID)
	// would otherwise compose a malformed scope like
	// "datasources:uid:" and accidentally match a permission
	// configured with the same prefix. Failing fast with 400 also
	// matches the AAP §0.8.5 contract: "validate the input payload
	// before invoking the LLM".
	if strings.TrimSpace(req.DatasourceUID) == "" {
		return response.Error(http.StatusBadRequest, "datasourceUid is required", ErrInvalidDatasource)
	}

	// orgID is read from the authenticated session — see the
	// security note in the function-level comment. c.GetOrgID() is
	// inherited from the embedded *user.SignedInUser on
	// *contextmodel.ReqContext.
	orgID := c.GetOrgID()

	// NLQ feature CRITICAL fix: UID-scoped datasources:query
	// authorization. We evaluate the permission against the
	// body-supplied UID by composing the scope via
	// datasources.ScopeProvider.GetResourceScopeUID(uid). This is
	// the same scope shape used by the existing /api/ds/query route
	// and by pkg/services/ngalert/accesscontrol/rules.go's
	// getRulesQueryEvaluator. A caller without permission for THIS
	// specific UID is rejected with 403 Forbidden BEFORE any
	// schema fetch or LLM call occurs.
	//
	// SECURITY (AAP §0.8.5): the access-control error (if any) is
	// logged at debug level only; the response surface uses the
	// typed ErrForbiddenDatasource so no underlying error message
	// can leak into the client-visible response body.
	if err := s.authorizeDatasourceQuery(c, req.DatasourceUID); err != nil {
		return s.errorResponse(err)
	}

	// NLQ feature MAJOR fix (review feedback — CallResource
	// integration): forward c.SignedInUser to Translate so the
	// downstream schema-fetch path can attach the caller's user
	// identity to the plugin context envelope. This is how every
	// other datasource-resource path in Grafana propagates caller
	// identity into the plugin runtime (see pkg/expr/ml.go and
	// pkg/registry/apis/datasource/querier.go for the established
	// pattern).
	resp, err := s.Translate(c.Req.Context(), req, orgID, c.SignedInUser)
	if err != nil {
		return s.errorResponse(err)
	}

	return response.JSON(http.StatusOK, resp)
}

// authorizeDatasourceQuery enforces UID-scoped datasources:query
// permission on the authenticated caller. Called from PostTranslate
// AFTER request body binding (so req.DatasourceUID is known) and
// BEFORE any schema fetch / LLM call (so an unauthorized caller
// cannot trigger upstream work).
//
// Returns:
//   - nil when the caller is authorized.
//   - ErrForbiddenDatasource when the caller lacks the permission
//     (mapped to 403 by errorResponse).
//   - ErrForbiddenDatasource (also) when the access-control system
//     itself returns an error; we DO NOT distinguish between
//     "infrastructure failure" and "no permission" in the response
//     surface because doing so could allow a caller to probe the
//     state of the auth backend (oracle attack). The underlying
//     error is logged at debug level so operators can diagnose.
//
// SECURITY (AAP §0.8.5):
//   - dsUID is included in the debug log line because it is a
//     non-secret identifier supplied by the caller.
//   - The underlying err is logged but NOT returned to the client.
//   - No portion of req.NaturalLanguage reaches the log or the
//     response — see the body of PostTranslate which never logs
//     the request.
func (s *Service) authorizeDatasourceQuery(c *contextmodel.ReqContext, dsUID string) error {
	if s.ac == nil {
		// Defense-in-depth: if access control is not wired (e.g. a
		// misbehaving test fixture), treat as denied rather than
		// falling open. This contradicts the test convention of
		// "ExpectedEvaluate: true on FakeAccessControl" but it
		// only triggers when the AccessControl field is nil — which
		// no production constructor path produces.
		s.log.Debug("NLQ feature: access control not configured; denying request", "datasourceUID", dsUID)
		return ErrForbiddenDatasource
	}

	evaluator := ac.EvalPermission(datasources.ActionQuery, datasources.ScopeProvider.GetResourceScopeUID(dsUID))
	hasAccess, err := s.ac.Evaluate(c.Req.Context(), c.SignedInUser, evaluator)
	if err != nil {
		// SECURITY: log only the UID and a generic error message;
		// surface only ErrForbiddenDatasource to the client.
		s.log.Debug(
			"NLQ feature: access control evaluation failed",
			"datasourceUID", dsUID,
			"err", err,
		)
		return ErrForbiddenDatasource
	}
	if !hasAccess {
		return ErrForbiddenDatasource
	}
	return nil
}

// errorResponse maps a typed error from Translate to an HTTP
// response. The mapping is intentionally narrow: only the sentinels
// declared in models.go are recognised; any unrecognised error is
// reported as 500 Internal Server Error with a generic message.
//
// SECURITY (AAP §0.8.5): the error message passed to response.Error
// is the canonical sentinel string — never the underlying err.Error()
// content — to prevent accidental leakage of API key fragments,
// prompt content, or request body content into response bodies.
// Grafana's response.Error includes the underlying error in the
// server log (via resp.err) but not the client-facing JSON body.
//
// Special-case for ErrMissingAPIKey: a nil underlying error is
// passed so that NO portion of any future wrapped error message
// could ever reach the response object — and an explicit
// s.log.Error call is issued with a fully hardcoded safe message
// so the operator still sees an actionable log line. The current
// ErrMissingAPIKey sentinel only mentions the env-var NAME (not
// its value, which is empty by definition when this error fires),
// but the nil-passing pattern survives any future refactor that
// might wrap additional context into the sentinel.
func (s *Service) errorResponse(err error) response.Response {
	switch {
	case errors.Is(err, ErrEmptyInput):
		return response.Error(http.StatusBadRequest, "natural language input is required", err)
	case errors.Is(err, ErrUnsupportedDatasource):
		return response.Error(http.StatusBadRequest, "unsupported datasource type", err)
	case errors.Is(err, ErrInvalidDatasource):
		// NLQ feature MAJOR fix: invalid UID, unresolvable UID, or
		// claimed/registered type mismatch all surface as 400 Bad
		// Request. The earlier behavior of downgrading these to
		// warnings allowed callers to trigger LLM calls with
		// inconsistent datasource identity.
		return response.Error(http.StatusBadRequest, "invalid datasource for this request", err)
	case errors.Is(err, ErrForbiddenDatasource):
		// NLQ feature CRITICAL fix: UID-scoped authorization
		// failure. The response.Error err argument is nil to
		// prevent any wrapped underlying detail from reaching the
		// client (the wrapped detail, if present, would have been
		// logged by authorizeDatasourceQuery already).
		return response.Error(http.StatusForbidden, "not authorized to query this datasource", nil)
	case errors.Is(err, ErrServiceDisabled):
		// NLQ feature MAJOR fix (review feedback — feature gate
		// harmonization): operator has disabled the feature via
		// [nlq] enabled=false. We return 503 Service Unavailable
		// so the frontend Alert can render a clear localized
		// message. The route is still mounted (gated only on the
		// feature toggle) so the response is structured and
		// recognizable — never a 404.
		return response.Error(http.StatusServiceUnavailable, "NLQ translation is disabled by the operator", err)
	case errors.Is(err, ErrMissingAPIKey):
		// 500 (not 401/403) because this is an operator
		// misconfiguration, not a caller problem. The caller
		// cannot fix it; the operator must set GF_NLQ_LLM_API_KEY.
		//
		// NLQ feature security (AAP §0.8.5): log explicitly with
		// a hardcoded string and pass nil to response.Error so
		// no key-related context can ever reach the response
		// object's stored err. The "error" field below is a
		// hardcoded string, NOT the value of the env var.
		s.log.Error(
			"NLQ feature: LLM API key is not configured",
			"error", "GF_NLQ_LLM_API_KEY not set",
		)
		return response.Error(http.StatusInternalServerError, "NLQ LLM provider is not configured", nil)
	case errors.Is(err, ErrInvalidQuerySyntax):
		// NLQ feature MAJOR fix: 502 Bad Gateway because the
		// failure originated in the upstream LLM (it produced a
		// syntactically invalid PromQL/LogQL string). The wrapped
		// parser error message IS safe to surface — it contains
		// only language-spec diagnostics, never Grafana secrets.
		return response.Error(http.StatusBadGateway, "NLQ LLM produced an invalid query", err)
	case errors.Is(err, ErrLLMUnavailable):
		// 502 Bad Gateway because the immediate failure is in an
		// upstream service (the LLM provider), not in Grafana.
		return response.Error(http.StatusBadGateway, "NLQ LLM provider is unavailable", err)
	default:
		return response.Error(http.StatusInternalServerError, "failed to translate query", err)
	}
}

// Translate orchestrates the natural-language-to-query pipeline.
//
// Phases:
//
//  1. Input validation (HARD failures — return without LLM call):
//     - NaturalLanguage must be non-empty (after TrimSpace).
//     - DatasourceUID must be non-empty (after TrimSpace).
//     - DatasourceType must resolve to a supported language via
//     supportedLanguages.
//
//  2. Schema context fetch — HYBRID failure semantics:
//     - HARD failures (datasource UID unresolved, type mismatch
//     between request and registered datasource): fail with
//     ErrInvalidDatasource. The translation never reaches the
//     LLM with an inconsistent datasource identity. This
//     addresses the MAJOR review finding that previously
//     downgraded these to warnings.
//     - SOFT failures (live metadata fetch failed but the
//     datasource exists and types match): proceed with the
//     base hints, attach a Warnings entry.
//
//  3. Prompt construction: buildPrompt yields a system prompt
//     (instructions + schema hints) and a user prompt (the raw
//     NaturalLanguage).
//
//  4. LLM invocation: callLLM issues the HTTP request to the
//     configured provider endpoint with the API key from
//     GF_NLQ_LLM_API_KEY.
//
//  5. Response parsing + syntactic validation: parseResponse
//     extracts the JSON object from Choices[0].Message.Content;
//     validateQuerySyntax parses the resulting query string with
//     the language-specific parser (PromQL or LogQL). A parser
//     error becomes ErrInvalidQuerySyntax. This addresses the
//     MAJOR review finding that previous behavior only checked
//     for non-emptiness.
//
//  6. Final assembly: the TranslateResponse is composed from the
//     parsed query, the language identifier, the explanation, and
//     any warnings accumulated along the way.
//
// orgID is the authenticated user's active org, supplied by the
// caller (PostTranslate) from c.GetOrgID(). It is forwarded to
// fetchSchemaContext for the multi-tenant datasource lookup.
//
// user is the authenticated caller's identity envelope. It is
// forwarded into fetchSchemaContext so plugin-context construction
// can embed the caller's user identity in the CallResource request
// (matching how every other datasource resource call attaches user
// identity). Tests may pass nil for non-RBAC-sensitive paths; the
// schema-fetch code-path treats nil as "anonymous service caller".
//
// Context propagation: ctx is propagated into fetchSchemaContext
// (so a downstream cancellation aborts the datasource lookup) and
// into callLLM (so a downstream cancellation aborts the LLM HTTP
// request). All I/O in this function is context-aware.
//
// Error semantics: returns ONLY the typed sentinels declared in
// models.go (potentially wrapped with %w to add context). Callers
// MUST use errors.Is for classification.
func (s *Service) Translate(ctx context.Context, req TranslateRequest, orgID int64, user identity.Requester) (TranslateResponse, error) {
	// NLQ feature MAJOR fix (review feedback — feature gate
	// harmonization): if the operator has disabled the feature via
	// [nlq] enabled=false, short-circuit BEFORE any input
	// validation, schema fetch, or LLM call. The route is mounted
	// whenever the feature toggle is on, but this gate gives the
	// operator a runtime kill switch — useful when the LLM endpoint
	// must be taken down for maintenance without disabling the
	// frontend feature flag (which would require redeploying the
	// bootdata).
	if s.cfg == nil || !s.cfg.NLQEnabled {
		return TranslateResponse{}, ErrServiceDisabled
	}

	// 1. Validate input.
	naturalLanguage := strings.TrimSpace(req.NaturalLanguage)
	if naturalLanguage == "" {
		return TranslateResponse{}, ErrEmptyInput
	}
	// Replace the raw input with the trimmed value so downstream
	// consumers (prompt builder) see a normalized string.
	req.NaturalLanguage = naturalLanguage

	// NLQ feature MAJOR fix: validate DatasourceUID is non-empty/
	// non-whitespace before any schema fetch or LLM call. The
	// PostTranslate handler also performs this check before the
	// authorization step, but Translate may be called directly
	// from tests or future callers — defense-in-depth.
	dsUID := strings.TrimSpace(req.DatasourceUID)
	if dsUID == "" {
		return TranslateResponse{}, fmt.Errorf("%w: datasourceUid is empty", ErrInvalidDatasource)
	}
	req.DatasourceUID = dsUID

	normalizedType := strings.ToLower(strings.TrimSpace(req.DatasourceType))
	language, ok := supportedLanguages[normalizedType]
	if !ok {
		// SECURITY: echo only the claimed type (which the client
		// already knows) in the wrapped error. No request-body
		// content beyond the type identifier leaves the service.
		return TranslateResponse{}, fmt.Errorf("%w: %q", ErrUnsupportedDatasource, req.DatasourceType)
	}
	req.DatasourceType = normalizedType

	// 2. Fetch schema context. The fetcher distinguishes two error
	// classes (per the MAJOR review finding):
	//   - HARD: datasource lookup failed OR registered type does
	//     not match the request's claim. These come back wrapped
	//     in ErrInvalidDatasource and short-circuit the entire
	//     translation with a 400 response. No LLM call is made.
	//   - SOFT: any other error (e.g. the live /api/v1/labels
	//     metadata fetch via CallResource failed). These come back
	//     wrapped in a generic transport error and are downgraded
	//     to a Warnings entry. The translation proceeds with the
	//     base hints.
	var warnings []string
	schemaCtx, schemaErr := s.fetchSchemaContext(ctx, req, orgID, user)
	if schemaErr != nil {
		if errors.Is(schemaErr, ErrInvalidDatasource) {
			// HARD fail: the datasource UID is not consistent with
			// the request's claim. errorResponse maps this to 400.
			return TranslateResponse{}, schemaErr
		}
		// SOFT fail: the datasource exists and the type matches,
		// but the live metadata fetch was unsuccessful. Continue
		// with the (possibly partially populated) base schema
		// context returned by fetchSchemaContext.
		//
		// NLQ CP10 O-FINDING-1 fix: promoted from Debug to Warn so
		// the graceful-degradation event surfaces at the default
		// log level (info). The Warnings entry is also returned to
		// the caller; together they ensure operators AND callers
		// see the schema-fetch fallback. Level promotion only —
		// structured fields are unchanged and remain safe per
		// AAP §0.8.5 (no input content, no LLM response, no key).
		s.log.Warn(
			"NLQ feature: live schema metadata fetch failed; proceeding with base hints",
			"datasourceUID", req.DatasourceUID,
			"err", schemaErr,
		)
		warnings = append(warnings, "Live schema metadata was unavailable; translation proceeded with general datasource hints.")
	}

	// 3. Build the LLM prompt.
	systemPrompt, userPrompt := s.buildPrompt(req, schemaCtx, language)

	// 4. Invoke the LLM.
	rawBody, err := s.callLLM(ctx, systemPrompt, userPrompt)
	if err != nil {
		// err is already a wrapped ErrMissingAPIKey or
		// ErrLLMUnavailable from callLLM; return it as-is so the
		// HTTP handler can classify with errors.Is.
		return TranslateResponse{}, err
	}

	// 5. Parse the LLM response.
	parsed, err := s.parseResponse(rawBody)
	if err != nil {
		return TranslateResponse{}, err
	}

	// 5a. NLQ feature MAJOR fix: syntactic validation against the
	// language-specific parser. A failure here is treated as an
	// upstream issue (502 Bad Gateway) — the request was valid;
	// the LLM produced an unusable result.
	if err := validateQuerySyntax(parsed.Query, language); err != nil {
		s.log.Warn(
			"NLQ feature: LLM produced syntactically invalid query",
			"language", language,
			"err", err,
		)
		return TranslateResponse{}, fmt.Errorf("%w: %v", ErrInvalidQuerySyntax, err)
	}

	// 6. Assemble the response.
	return TranslateResponse{
		Query:       parsed.Query,
		Language:    language,
		Explanation: parsed.Explanation,
		Warnings:    warnings,
	}, nil
}

// validateQuerySyntax parses the LLM-produced query string with the
// language-specific parser (PromQL or LogQL) and returns a
// non-nil error if the parser rejects the string. A nil return
// indicates the query is syntactically well-formed and safe to
// surface to the client.
//
// The parser libraries used here are already in Grafana's module
// graph (see go.mod — github.com/prometheus/prometheus and
// github.com/grafana/loki/v3) so no new dependency is introduced.
//
// SECURITY (AAP §0.8.5): the returned error wraps the parser's own
// error message verbatim. Parser errors describe language-spec
// violations (e.g., "expected colon but got identifier") and never
// contain Grafana-internal secrets. Tests confirm the API key
// cannot appear in this error path (TestTranslate_APIKeyNotLeaked).
//
// language is the language identifier already resolved by Translate
// — one of "promql" or "logql". An unexpected value returns nil
// (we cannot validate something we do not understand; the LLM-
// output is the safer default than rejecting all queries for an
// unrecognised dialect).
func validateQuerySyntax(query, language string) error {
	q := strings.TrimSpace(query)
	if q == "" {
		// parseResponse already rejects empty queries before
		// calling this helper; defensive check preserves the
		// invariant regardless of refactor order.
		return errors.New("query is empty after whitespace trim")
	}
	switch language {
	case "promql":
		if _, err := promparser.ParseExpr(q); err != nil {
			return err
		}
	case "logql":
		if _, err := logqlsyntax.ParseExpr(q); err != nil {
			return err
		}
	default:
		// Unrecognised language — skip validation. supportedLanguages
		// is the source of truth and only emits "promql" or "logql",
		// so this branch is defensive.
	}
	return nil
}

// buildPrompt constructs the (system, user) prompt pair for the
// LLM. The system prompt sets the role (PromQL/LogQL expert),
// constrains the output format to a JSON object, and seeds the
// model with the available labels, metrics, and stream selectors
// from the SchemaContext. The user prompt is the raw natural-
// language question, verbatim.
//
// The function is pure and deterministic: same inputs produce the
// same outputs. This makes prompt construction trivially unit-
// testable without any LLM mock.
//
// language is the target query language ("promql" or "logql"),
// already resolved by Translate from the datasource type. It is
// used in the system prompt to anchor the LLM's vocabulary.
func (s *Service) buildPrompt(req TranslateRequest, schemaCtx SchemaContext, language string) (string, string) {
	upperLang := strings.ToUpper(language)

	var sys strings.Builder
	sys.WriteString("You are an expert in ")
	sys.WriteString(upperLang)
	sys.WriteString(". Translate the user's plain-English question into a single, syntactically valid ")
	sys.WriteString(upperLang)
	sys.WriteString(" query. ")
	sys.WriteString("Respond with ONLY a JSON object on a single line with these exact keys:\n")
	sys.WriteString(`  "query"       : the `)
	sys.WriteString(upperLang)
	sys.WriteString(" string\n")
	sys.WriteString(`  "explanation" : a one-sentence human-readable description of what the query computes`)
	sys.WriteString("\n")
	sys.WriteString("Do not include any text outside the JSON object. Do not wrap the JSON in markdown code fences.")

	if len(schemaCtx.Labels) > 0 {
		sys.WriteString("\n\nAvailable labels: ")
		sys.WriteString(strings.Join(schemaCtx.Labels, ", "))
		sys.WriteString(".")
	}
	if len(schemaCtx.Metrics) > 0 {
		sys.WriteString("\n\nCommon metrics: ")
		sys.WriteString(strings.Join(schemaCtx.Metrics, ", "))
		sys.WriteString(".")
	}
	if len(schemaCtx.Streams) > 0 {
		sys.WriteString("\n\nExample stream selectors: ")
		sys.WriteString(strings.Join(schemaCtx.Streams, ", "))
		sys.WriteString(".")
	}

	return sys.String(), req.NaturalLanguage
}

// callLLM issues a single HTTP POST request to the configured LLM
// provider endpoint. Returns the raw response body on success or a
// wrapped ErrMissingAPIKey / ErrLLMUnavailable on failure.
//
// SECURITY (AAP §0.8.5):
//   - The API key is read from os.Getenv at the start of the
//     function and discarded when the function returns. It is
//     never stored on the Service struct, never written to logs,
//     and never returned in any error.
//   - http.Header.Set("Authorization", "Bearer ...") composes the
//     header value as a local string and never logs it. The
//     httpClient internals also do not log header values.
//   - The system and user prompts ARE part of the request body
//     sent over TLS to the LLM provider — that is the entire
//     purpose of the call. They are NEVER logged on the Grafana
//     side, however; the only fields logged are the endpoint URL
//     (which is configuration, not secret) and the HTTP status
//     code.
//
// Context propagation: ctx is attached to the outbound request via
// http.NewRequestWithContext. A client disconnect on the Grafana
// side cancels the LLM call before the 30s httpClient.Timeout
// would.
func (s *Service) callLLM(ctx context.Context, systemPrompt, userPrompt string) ([]byte, error) {
	// 1. Read the API key from the environment. NEVER from cfg.
	apiKey := os.Getenv(nlqAPIKeyEnvVar)
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}

	// 2. Compose the request body.
	body := llmRequest{
		Model: s.cfg.NLQModel,
		Messages: []llmMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		// Marshalling a fixed-shape struct should never fail; if
		// it does, classify as transport-level for the handler
		// mapping (502 Bad Gateway) so the client sees a
		// retryable error rather than a 500.
		return nil, fmt.Errorf("%w: marshal request: %v", ErrLLMUnavailable, err)
	}

	// 3. Build the HTTP request. http.NewRequestWithContext attaches
	// the context for cancellation propagation.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.NLQEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		// SECURITY (AAP §0.8.5 — MINOR review finding):
		// http.NewRequestWithContext wraps URL parse errors in
		// *url.Error which embeds the full request URL — including
		// any path, query string, or (in pathological
		// misconfigurations) userinfo from the configured endpoint.
		// Route the underlying error through sanitizeTransportError
		// to strip the URL surface before composing the wrapped
		// message, matching the treatment used downstream in
		// httpClient.Do failures. The configured endpoint must
		// never leak through an error envelope; only the host
		// (via safeHost in the structured log fields below) is
		// safe to surface.
		sanitized := sanitizeTransportError(err)
		s.log.Error("NLQ feature: LLM request construction failed",
			"host", safeHost(s.cfg.NLQEndpoint),
			"model", s.cfg.NLQModel,
			"error", sanitized,
		)
		return nil, fmt.Errorf("%w: build request: %s", ErrLLMUnavailable, sanitized)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	// SECURITY (AAP §0.8.5): the apiKey value is composed into the
	// Authorization header here and never logged. The Service
	// struct never holds it; the local apiKey variable is the
	// only in-memory copy and it leaves scope when this function
	// returns.
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	// 4. Issue the request. Transport-level failures (network
	// error, timeout, context cancellation) are reported via the
	// returned error.
	httpResp, err := s.httpClient.Do(httpReq)
	if err != nil {
		// SECURITY (AAP §0.8.5): http.Client.Do wraps transport
		// errors in *url.Error, which includes the full request
		// URL — including any path, query, or (in pathological
		// misconfigurations) userinfo embedded in the endpoint
		// configuration. The sanitizeTransportError helper unwraps
		// *url.Error and returns ONLY the inner error message, so
		// the configured endpoint cannot leak through the log or
		// the response envelope. Only the URL host (which is
		// non-secret operator configuration) is included in the
		// structured log fields via safeHost. The prompts and API
		// key are NEVER part of the log line. The configured
		// model name is non-secret operator configuration.
		sanitized := sanitizeTransportError(err)
		s.log.Error("NLQ feature: LLM call transport failure",
			"host", safeHost(s.cfg.NLQEndpoint),
			"model", s.cfg.NLQModel,
			"err", sanitized,
		)
		return nil, fmt.Errorf("%w: transport failure: %s", ErrLLMUnavailable, sanitized)
	}
	defer func() {
		// Drain and close the body so the underlying connection
		// can be returned to the pool. Errors on Close are
		// ignored — at this point we've already read what we need.
		_ = httpResp.Body.Close()
	}()

	// 5. Read the response body. Cap reads at a generous ceiling
	// (1 MiB) to defend against a maliciously oversized response
	// from a misbehaving provider; legitimate completions are
	// well under that.
	const maxResponseBytes = 1 << 20 // 1 MiB
	respBytes, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes))
	if err != nil {
		// SECURITY (AAP §0.8.5): apply the same sanitization the
		// transport-failure branch uses. io.ReadAll over an HTTP
		// response body can surface *url.Error or transport-layer
		// errors that include the full request URL.
		sanitized := sanitizeTransportError(err)
		s.log.Error("NLQ feature: LLM response read failure",
			"host", safeHost(s.cfg.NLQEndpoint),
			"model", s.cfg.NLQModel,
			"status", httpResp.StatusCode,
			"err", sanitized,
		)
		return nil, fmt.Errorf("%w: read response: %s", ErrLLMUnavailable, sanitized)
	}

	// 6. Check the HTTP status. Anything outside 2xx is a
	// transport-level failure; the body is discarded for the
	// purposes of error reporting (it may contain provider-
	// internal diagnostic content not safe for the client).
	//
	// CRITICAL (AAP §0.8.5): DO NOT include respBytes in the
	// returned error or in the log line. The body may in
	// unusual edge cases echo back portions of the request,
	// which could inadvertently include the prompt or be
	// misinterpreted as containing sensitive data. We log only
	// the status code, host, and configured model.
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		s.log.Error("NLQ feature: LLM non-2xx response",
			"host", safeHost(s.cfg.NLQEndpoint),
			"model", s.cfg.NLQModel,
			"status", httpResp.StatusCode,
		)
		return nil, fmt.Errorf("%w: HTTP %d", ErrLLMUnavailable, httpResp.StatusCode)
	}

	return respBytes, nil
}

// parseResponse extracts the translated query from the LLM
// response envelope.
//
// Expected wire shape (OpenAI Chat Completions):
//
//	{
//	  "choices": [
//	    { "message": { "role": "assistant", "content": "{\"query\":\"...\",\"explanation\":\"...\"}" } }
//	  ]
//	}
//
// The Content field is the LLM's text completion, which is itself
// a JSON object (per the buildPrompt system instructions). Some
// LLMs occasionally wrap their JSON in ```json ... ``` markdown
// code fences despite explicit instructions not to; stripCodeFences
// handles this gracefully.
//
// Validation:
//   - At least one Choices entry must be present.
//   - The decoded parsedQuery.Query must be non-empty after
//     TrimSpace; an empty query is treated as a transport-level
//     failure rather than success.
func (s *Service) parseResponse(body []byte) (parsedQuery, error) {
	var env llmResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return parsedQuery{}, fmt.Errorf("%w: parse envelope: %v", ErrLLMUnavailable, err)
	}
	if len(env.Choices) == 0 {
		return parsedQuery{}, fmt.Errorf("%w: LLM returned no choices", ErrLLMUnavailable)
	}

	content := strings.TrimSpace(env.Choices[0].Message.Content)
	content = stripCodeFences(content)

	var pq parsedQuery
	if err := json.Unmarshal([]byte(content), &pq); err != nil {
		return parsedQuery{}, fmt.Errorf("%w: parse content as JSON: %v", ErrLLMUnavailable, err)
	}

	pq.Query = strings.TrimSpace(pq.Query)
	pq.Explanation = strings.TrimSpace(pq.Explanation)

	if pq.Query == "" {
		return parsedQuery{}, fmt.Errorf("%w: empty query in LLM response", ErrLLMUnavailable)
	}

	return pq, nil
}

// stripCodeFences removes a single layer of ``` or ```json fencing
// from the given string, if present. The function is tolerant of
// either ``` followed by a language tag (```json\n...\n```) or a
// bare ``` (```\n...\n```), and of trailing whitespace.
//
// Why this defense?  LLMs frequently ignore the explicit "do not
// wrap in markdown code fences" instruction. Rather than fail the
// translation outright, we strip the fences and try to parse the
// inner content. If the inner content is still invalid JSON,
// parseResponse surfaces the canonical error.
//
// The function returns the original string unchanged if no fences
// are detected. strings.HasSuffix is used as an explicit guard
// around the closing fence trim so the function reads symmetrically
// (HasPrefix on entry, HasSuffix before the final TrimSuffix).
func stripCodeFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop everything from the opening ``` up to (and including)
	// the first newline. This handles both ```json\n... and
	// ```\n... openings.
	idx := strings.IndexByte(s, '\n')
	if idx == -1 {
		// Pathological: a single-line ``` something``` with no
		// newline. Strip the prefix only, then attempt a closing
		// fence trim on whatever remains.
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSpace(s)
		// TrimSuffix is a no-op when the suffix is absent, so the
		// HasSuffix guard was redundant (staticcheck S1017).
		s = strings.TrimSuffix(s, "```")
		return strings.TrimSpace(s)
	}
	s = s[idx+1:]
	// Strip the closing fence, tolerating trailing whitespace.
	s = strings.TrimSpace(s)
	// TrimSuffix is a no-op when the suffix is absent (staticcheck S1017).
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// safeHost returns just the host portion of a URL, suitable for
// safe inclusion in log lines. It strips the scheme, path, query,
// fragment, and — critically — any userinfo (username:password)
// component that an operator might have embedded in the configured
// LLM endpoint. This is a defense-in-depth measure: the canonical
// NLQ deployment passes secrets via GF_NLQ_LLM_API_KEY rather than
// in the URL, but a future operator who configures a self-hosted
// LLM with HTTP basic auth could inadvertently include credentials
// in NLQEndpoint. Logging only u.Host guarantees those credentials
// never reach the log stream.
//
// Returns "<invalid-endpoint>" if the URL fails to parse or has an
// empty host. This sentinel is deliberately distinct from the empty
// string so a log scraper grep for empty endpoints does not match
// it accidentally.
//
// Security invariant (AAP §0.8.5): every log call in this file that
// references s.cfg.NLQEndpoint MUST route the value through
// safeHost. The constant set of log call-sites is small (callLLM
// only); reviewers can verify this invariant by grepping for
// 's.cfg.NLQEndpoint' in this file and confirming each match is
// wrapped in safeHost(...).
func safeHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "<invalid-endpoint>"
	}
	return u.Host
}

// sanitizeTransportError converts a transport-layer error from
// net/http into a string suitable for logging and returning to the
// client without leaking the full request URL.
//
// MOTIVATION (NLQ feature MAJOR review finding, translate.go L469-474):
// http.Client.Do wraps transport-level errors (DNS failure, TCP
// reset, TLS handshake error, context deadline) in *url.Error. The
// *url.Error.Error() method renders as:
//
//	"<Op> <URL>: <inner error>"
//
// where <URL> is the FULL request URL — scheme, host, path, query,
// and (in pathological misconfigurations) userinfo. That partially
// defeats the safeHost helper used in adjacent structured log
// fields. If an operator misconfigures NLQEndpoint with embedded
// credentials, or includes a sensitive path segment, the full URL
// would otherwise reach the log stream and the HTTP response
// envelope.
//
// SANITIZATION:
//   - For *url.Error values, return ONLY the inner error message
//     (err.Err.Error()) prefixed by the operation name. The URL
//     component is dropped entirely; the host is available via
//     the separately-logged safeHost field.
//   - For all other error values, return err.Error() unchanged.
//     These typically come from io.ReadAll on a successful
//     response and do not contain the request URL.
//
// SECURITY GUARANTEE (AAP §0.8.5): the API key, the configured
// endpoint path/query/userinfo, and the user-supplied natural
// language prompt MUST NEVER appear in the returned string. Tests
// (TestTranslate_APIKeyNotLeaked, TestSanitizeTransportError_*)
// codify this invariant.
//
// Returns "<transport error>" for a nil input (defensive — callers
// should not pass nil, but the helper preserves the invariant
// without panicking).
func sanitizeTransportError(err error) string {
	if err == nil {
		return "<transport error>"
	}
	// Unwrap *url.Error and use only the operation name + inner
	// error message. errors.As walks the chain in case the
	// transport error has been wrapped by an intermediate layer.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		op := urlErr.Op
		if op == "" {
			op = "request"
		}
		if urlErr.Err == nil {
			return op + ": transport failure"
		}
		// Recurse to sanitize any further nested *url.Error
		// (transport stacks can wrap multiple times in edge cases).
		return op + ": " + sanitizeTransportError(urlErr.Err)
	}
	return err.Error()
}
