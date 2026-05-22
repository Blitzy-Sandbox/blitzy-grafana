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
	"os"
	"strings"

	"github.com/grafana/grafana/pkg/api/response"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
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
//  3. Translate is invoked with the validated request and the
//     authenticated orgID. It returns a TranslateResponse on success
//     and a typed sentinel error on failure.
//  4. errors.Is is used to classify the returned error and map it
//     to the appropriate HTTP status code (see errorResponse below).
//  5. On success, response.JSON serialises TranslateResponse with
//     200 OK and the application/json content type. The frontend
//     consumes the resulting body directly.
//
// CORS / CSRF / auth: the upstream middleware chain registered in
// service.go's registerAPIEndpoints (middleware.ReqSignedIn +
// ac.EvalPermission(datasources.ActionQuery)) has already enforced
// authentication and permission before this handler runs. There is
// nothing for the handler itself to verify on the auth axis.
func (s *Service) PostTranslate(c *contextmodel.ReqContext) response.Response {
	req := TranslateRequest{}
	if err := web.Bind(c.Req, &req); err != nil {
		// SECURITY: do not echo the underlying request body in the
		// error message. web.Bind returns generic parsing errors
		// that do not include user content, so passing err through
		// directly is safe.
		return response.Error(http.StatusBadRequest, "bad request data", err)
	}

	// orgID is read from the authenticated session — see the
	// security note in the function-level comment. c.GetOrgID() is
	// inherited from the embedded *user.SignedInUser on
	// *contextmodel.ReqContext.
	orgID := c.GetOrgID()

	resp, err := s.Translate(c.Req.Context(), req, orgID)
	if err != nil {
		return s.errorResponse(err)
	}

	return response.JSON(http.StatusOK, resp)
}

// errorResponse maps a typed error from Translate to an HTTP
// response. The mapping is intentionally narrow: only the sentinels
// declared in models.go are recognised; any unrecognised error is
// reported as 500 Internal Server Error with a generic message.
//
// SECURITY: the error message passed to response.Error is the
// canonical sentinel string — never the underlying err.Error()
// content — to prevent accidental leakage of API key fragments,
// prompt content, or request body content into response bodies.
// Grafana's response.Error includes the underlying error in the
// server log but not the client-facing JSON body.
func (s *Service) errorResponse(err error) response.Response {
	switch {
	case errors.Is(err, ErrEmptyInput):
		return response.Error(http.StatusBadRequest, "natural language input is required", err)
	case errors.Is(err, ErrUnsupportedDatasource):
		return response.Error(http.StatusBadRequest, "unsupported datasource type", err)
	case errors.Is(err, ErrMissingAPIKey):
		// 500 (not 401/403) because this is an operator
		// misconfiguration, not a caller problem. The caller
		// cannot fix it; the operator must set GF_NLQ_LLM_API_KEY.
		return response.Error(http.StatusInternalServerError, "NLQ LLM provider is not configured", err)
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
//  1. Input validation: NaturalLanguage must be non-empty (after
//     TrimSpace); DatasourceType must resolve to a supported language
//     via supportedLanguages.
//
//  2. Schema context fetch (best-effort): fetchSchemaContext looks
//     up the datasource by UID and seeds a SchemaContext. Any error
//     here is downgraded to a Warnings entry on the response —
//     translation proceeds with an empty SchemaContext.
//
//  3. Prompt construction: buildPrompt yields a system prompt
//     (instructions + schema hints) and a user prompt (the raw
//     NaturalLanguage).
//
//  4. LLM invocation: callLLM issues the HTTP request to the
//     configured provider endpoint with the API key from
//     GF_NLQ_LLM_API_KEY.
//
//  5. Response parsing: parseResponse extracts the JSON object
//     from Choices[0].Message.Content and validates that the
//     Query field is non-empty.
//
//  6. Final assembly: the TranslateResponse is composed from the
//     parsed query, the language identifier, the explanation, and
//     any warnings accumulated along the way.
//
// orgID is the authenticated user's active org, supplied by the
// caller (PostTranslate) from c.GetOrgID(). It is forwarded to
// fetchSchemaContext for the multi-tenant datasource lookup.
//
// Context propagation: ctx is propagated into fetchSchemaContext
// (so a downstream cancellation aborts the datasource lookup) and
// into callLLM (so a downstream cancellation aborts the LLM HTTP
// request). All I/O in this function is context-aware.
//
// Error semantics: returns ONLY the typed sentinels declared in
// models.go (potentially wrapped with %w to add context). Callers
// MUST use errors.Is for classification.
func (s *Service) Translate(ctx context.Context, req TranslateRequest, orgID int64) (TranslateResponse, error) {
	// 1. Validate input.
	naturalLanguage := strings.TrimSpace(req.NaturalLanguage)
	if naturalLanguage == "" {
		return TranslateResponse{}, ErrEmptyInput
	}
	// Replace the raw input with the trimmed value so downstream
	// consumers (prompt builder) see a normalized string.
	req.NaturalLanguage = naturalLanguage

	normalizedType := strings.ToLower(strings.TrimSpace(req.DatasourceType))
	language, ok := supportedLanguages[normalizedType]
	if !ok {
		// SECURITY: echo only the claimed type (which the client
		// already knows) in the wrapped error. No request-body
		// content beyond the type identifier leaves the service.
		return TranslateResponse{}, fmt.Errorf("%w: %q", ErrUnsupportedDatasource, req.DatasourceType)
	}
	req.DatasourceType = normalizedType

	// 2. Fetch schema context. Any error here is non-fatal and is
	// surfaced as a Warnings entry on the response. Schema-fetch
	// failures should never block translation (per AAP §0.6.1.1
	// test case (f) "schema-fetch failure path produces a response
	// with Warnings populated").
	var warnings []string
	schemaCtx, schemaErr := s.fetchSchemaContext(ctx, req, orgID)
	if schemaErr != nil {
		// The schema context fetcher already logs the failure at
		// debug level; here we only translate it into a user-
		// visible warning. The warning text is generic and does
		// not echo the underlying error message (which may include
		// datasource configuration metadata not safe for the
		// client).
		warnings = append(warnings, "Schema context was unavailable; translation proceeded without datasource-specific hints.")
		// Empty SchemaContext is a valid input to buildPrompt
		// (it falls back to language-general hints only).
		schemaCtx = SchemaContext{}
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

	// 6. Assemble the response.
	return TranslateResponse{
		Query:       parsed.Query,
		Language:    language,
		Explanation: parsed.Explanation,
		Warnings:    warnings,
	}, nil
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
		return nil, fmt.Errorf("%w: build request: %v", ErrLLMUnavailable, err)
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
		// SECURITY: only the endpoint URL (configuration, not
		// secret) and the error class are logged; the prompts and
		// API key are NEVER part of the log line.
		s.log.Warn("NLQ feature: LLM call transport failure",
			"endpoint", s.cfg.NLQEndpoint,
			"err", err,
		)
		return nil, fmt.Errorf("%w: %v", ErrLLMUnavailable, err)
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
		s.log.Warn("NLQ feature: LLM response read failure",
			"endpoint", s.cfg.NLQEndpoint,
			"status", httpResp.StatusCode,
			"err", err,
		)
		return nil, fmt.Errorf("%w: read response: %v", ErrLLMUnavailable, err)
	}

	// 6. Check the HTTP status. Anything outside 2xx is a
	// transport-level failure; the body is discarded for the
	// purposes of error reporting (it may contain provider-
	// internal diagnostic content not safe for the client).
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		s.log.Warn("NLQ feature: LLM non-2xx response",
			"endpoint", s.cfg.NLQEndpoint,
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
// are detected.
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
		// newline. Strip the prefix only.
		s = strings.TrimPrefix(s, "```")
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	s = s[idx+1:]
	// Strip the closing fence, tolerating trailing whitespace.
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
