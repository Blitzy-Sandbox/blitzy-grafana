// service_test.go is the unit-test suite for the NLQ (Natural
// Language Query) translation service. It validates the runtime
// contract of *Service end-to-end at the package boundary, mocking
// only the external LLM provider via net/http/httptest.
//
// The tests in this file are the primary enforcement mechanism for
// the validation criteria enumerated in AAP §0.6.4 and the security
// invariants in AAP §0.8.5 (the LLM API key MUST NEVER leak to logs,
// error messages, or HTTP response bodies).
//
// SAME-PACKAGE TESTS: the file is declared in package nlq (not
// package nlq_test) so the tests can directly construct *Service
// struct literals, populate unexported fields, and exercise the
// unexported helpers (buildPrompt, callLLM, parseResponse) when the
// integration-level Translate orchestrator is insufficient. This
// mirrors the dashboard service-test pattern at
// pkg/services/dashboards/service/dashboard_service_test.go where
// the test file shares the package with the production code so
// struct literals can directly set unexported fields without going
// through the Wire DI constructor.
//
// TEST ISOLATION:
//   - Every Translate test creates its own httptest.NewServer to
//     mock the LLM endpoint and calls .Close() via defer so the
//     server is reaped on test exit.
//   - Every test that needs the API key uses t.Setenv (Go 1.17+) to
//     scope the GF_NLQ_LLM_API_KEY value to that single test. The
//     env var is automatically restored to its previous value when
//     the test completes, regardless of pass / fail / panic — so
//     no test leaks state into a sibling test.
//   - No test depends on the execution order of any other test.
//
// SECURITY ENFORCEMENT — AAP §0.8.5:
// TestTranslate_APIKeyNotLeaked is the canonical security-invariant
// test. It MUST remain in this file unchanged in spirit: an LLM
// failure path is exercised with a known sensitive key, and the
// returned error.Error() AND the JSON-serialised response body are
// both asserted to NOT contain the key string. Any future refactor
// of translate.go that loses this property would cause this test
// to fail, alerting reviewers immediately.
package nlq

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana/pkg/api/routing"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/accesscontrol/actest"
	// dsfakes is aliased because the package declared inside
	// pkg/services/datasources/fakes is named "datasources" (the
	// directory name and the package name diverge in this case),
	// which would otherwise collide with the actual datasources
	// package if it were ever needed in the same scope. The alias
	// matches the convention established elsewhere in the codebase
	// (e.g., pkg/services/ngalert/schedule/recording_rule_test.go).
	dsfakes "github.com/grafana/grafana/pkg/services/datasources/fakes"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/setting"
)

// testAPIKey is a fixed-value placeholder API key used by every
// test that needs GF_NLQ_LLM_API_KEY to be set to a non-empty value.
// Tests that specifically validate behavior under an UNSET key
// (TestTranslate_MissingAPIKey) set the env var to the empty string
// explicitly via t.Setenv("GF_NLQ_LLM_API_KEY", "").
//
// The value is deliberately short and obviously a test value so it
// cannot be mistaken for a real credential during a grep audit. It
// is also intentionally NOT placed in TestTranslate_APIKeyNotLeaked,
// which uses its own distinct sentinel ("super-secret-test-key-12345")
// so that any cross-contamination between tests would be immediately
// visible.
const testAPIKey = "test-api-key"

// testOrgID is the canonical organization ID used by all tests in
// this file. The fake datasource service does not enforce orgID
// matching unless explicitly populated with seed data; using a
// fixed value documents the assumption clearly.
const testOrgID int64 = 1

// newTestService constructs a *Service pre-wired with fakes for
// every dependency, suitable for direct invocation of Translate
// and its helpers. The caller supplies the LLM endpoint URL — in
// practice, every caller passes the URL of an httptest.NewServer
// instance scoped to that test, so the mocked LLM behavior is
// deterministic and isolated.
//
// Defaults set by this helper:
//   - cfg.NLQEnabled       = true   (off-by-default does not matter
//     here because we are calling
//     Translate directly, bypassing
//     the route-registration gate).
//   - cfg.NLQProvider      = "openai"
//   - cfg.NLQEndpoint      = llmEndpoint (from the caller).
//   - cfg.NLQModel         = "gpt-4o-test" (distinct from the
//     production default "gpt-4o" so
//     payload-shape assertions can
//     detect default leakage).
//   - features             = WithFeatures(FlagNlqEnabled) (flag ON).
//   - ac                   = FakeAccessControl{ExpectedEvaluate: true}
//     (grants every permission).
//   - dsService            = empty FakeDataSourceService (returns
//     ErrDataSourceNotFound for every UID,
//     which the orchestrator handles as a
//     non-fatal Warnings entry).
//   - httpClient           = &http.Client{Timeout: 5 * time.Second}
//     (a short timeout sufficient for
//     httptest local connections; production
//     uses 30s via llmHTTPTimeout).
//
// Tests that need different behavior (e.g., a fake dsService with
// seed data) override the relevant field on the returned *Service.
func newTestService(t *testing.T, llmEndpoint string) *Service {
	t.Helper()

	cfg := setting.NewCfg()
	cfg.NLQEnabled = true
	cfg.NLQProvider = "openai"
	cfg.NLQEndpoint = llmEndpoint
	// "gpt-4o-test" is distinct from the production default
	// ("gpt-4o") so that any test asserting on the outgoing LLM
	// request body can confirm the configured model value is the
	// one being transmitted (not the production default leaking
	// through). See TestTranslate_PrometheusSuccess.
	cfg.NLQModel = "gpt-4o-test"

	return &Service{
		cfg:           cfg,
		routeRegister: routing.NewRouteRegister(),
		dsService:     &dsfakes.FakeDataSourceService{},
		ac:            actest.FakeAccessControl{ExpectedEvaluate: true},
		// FlagNlqEnabled is the constant generated into
		// pkg/services/featuremgmt/toggles_gen.go when the
		// nlqEnabled entry is added to the feature-flag registry.
		// Constructing the service with the flag enabled keeps the
		// test behavior consistent with a production deployment
		// that has the feature turned on.
		features:   featuremgmt.WithFeatures(featuremgmt.FlagNlqEnabled),
		log:        log.New("test.nlq"),
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// TestProvideService_ConstructsWithoutError validates AAP §0.6.4
// row 5 (the "Service can be constructed when the feature is off"
// path) by invoking the Wire DI constructor (ProvideService)
// directly with the feature flag DISABLED. The constructor must
// return a non-nil *Service and nil error so the Wire graph
// continues to compile and run even when nlqEnabled is off.
//
// Why this matters: the AAP requires the entire feature to be
// off-by-default (§0.1.1). ProvideService is invoked unconditionally
// by wire_gen.go's Initialize chain; if it returned an error when
// the flag was off, every Grafana startup would fail even though
// the feature is opt-in. This test pins the graceful-disable
// contract so future refactors of ProvideService cannot accidentally
// regress it.
func TestProvideService_ConstructsWithoutError(t *testing.T) {
	cfg := setting.NewCfg()
	// Match the production "off by default" stance — the test
	// scenario is "feature is disabled but the constructor still
	// gets called by Wire".
	cfg.NLQEnabled = false
	cfg.NLQProvider = "openai"
	cfg.NLQEndpoint = "https://api.openai.com/v1/chat/completions"
	cfg.NLQModel = "gpt-4o"

	rr := routing.NewRouteRegister()
	dsService := &dsfakes.FakeDataSourceService{}
	accessControl := actest.FakeAccessControl{ExpectedEvaluate: true}
	// featuremgmt.WithFeatures() with no arguments produces a
	// FeatureToggles where every flag — including nlqEnabled — is
	// reported as disabled. This is the canonical "feature off"
	// fixture for tests.
	features := featuremgmt.WithFeatures()

	svc, err := ProvideService(cfg, rr, dsService, accessControl, features)
	require.NoError(t, err, "ProvideService must not return an error when the feature flag is off")
	require.NotNil(t, svc, "ProvideService must return a non-nil *Service even when the feature flag is off")
}

// TestTranslate_PrometheusSuccess validates AAP §0.6.4 row 1: the
// happy-path Prometheus translation. The test mocks the LLM
// provider via httptest and asserts both the outbound request
// shape (Authorization header, Content-Type header, OpenAI-format
// JSON body) and the inbound TranslateResponse (PromQL query,
// "promql" language identifier, non-empty explanation).
//
// User Example 3 (AAP §0.1.2.1) is the seed input: "Graph total
// API request rate by endpoint over the last 24 hours". The mocked
// LLM response returns the canonical PromQL translation
// `sum by (endpoint) (rate(http_requests_total[24h]))` so that
// later test runs and log searches can correlate this test with
// the AAP-specified example.
func TestTranslate_PrometheusSuccess(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SECURITY (AAP §0.8.5): assert the Authorization header
		// carries the Bearer-prefixed test key — this is the
		// contract translate.go's callLLM commits to. Asserting it
		// here ensures any future refactor that drops or mangles
		// the header is caught immediately.
		assert.Equal(t, "Bearer "+testAPIKey, r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		// Assert the outbound payload matches the OpenAI Chat
		// Completions API shape: { "model": "...", "messages": [...] }
		// with at least one system and one user message.
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload),
			"outbound LLM request body must be valid JSON")
		// The configured model from cfg.NLQModel must reach the LLM
		// payload (not a hardcoded default).
		assert.Equal(t, "gpt-4o-test", payload["model"],
			"the cfg.NLQModel must be propagated into the LLM request body")
		messages, ok := payload["messages"].([]any)
		require.True(t, ok, "outbound LLM request body must contain a 'messages' array")
		require.GreaterOrEqual(t, len(messages), 2,
			"outbound LLM request must contain at least a system and a user message")

		// Return a well-formed OpenAI Chat Completions response.
		// The .Content field is itself a JSON object string per
		// translate.go's buildPrompt instructions to the LLM.
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": `{"query": "sum by (endpoint) (rate(http_requests_total[24h]))", "explanation": "Total API request rate by endpoint over 24h"}`,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	resp, err := svc.Translate(context.Background(), TranslateRequest{
		// User Example 3 from AAP §0.1.2.1.
		NaturalLanguage: "Graph total API request rate by endpoint over the last 24 hours",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID)

	require.NoError(t, err, "Translate must succeed on a well-formed Prometheus request")
	assert.Equal(t, "sum by (endpoint) (rate(http_requests_total[24h]))", resp.Query,
		"the translated query must match the LLM-returned PromQL")
	assert.Equal(t, "promql", resp.Language,
		"the response language must be 'promql' for a Prometheus datasource")
	assert.NotEmpty(t, resp.Explanation,
		"the response explanation must be populated when the LLM returns one")
}

// TestTranslate_LokiSuccess validates AAP §0.6.4 row 2: the
// happy-path Loki translation. The mocked LLM response returns a
// LogQL stream selector (User Example 1 from AAP §0.1.2.1: "Show
// me failed login attempts in the last hour grouped by IP") and
// the assertions confirm the Language field is "logql" and that
// the returned query contains the marker string "failed login"
// proving the LLM-provided content survived parseResponse intact.
func TestTranslate_LokiSuccess(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Note the nested JSON encoding: the outer envelope is the
		// OpenAI Chat Completions response; the content field is
		// itself a JSON object the LLM is instructed to produce.
		// The escapes here mirror what a real LLM would return for
		// a LogQL query containing double-quoted string literals.
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": `{"query": "{job=\"nginx\"} |= \"failed login\" | json | line_format \"{{.remote_addr}}\"", "explanation": "Failed login attempts grouped by IP"}`,
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	resp, err := svc.Translate(context.Background(), TranslateRequest{
		// User Example 1 from AAP §0.1.2.1.
		NaturalLanguage: "Show me failed login attempts in the last hour grouped by IP",
		DatasourceUID:   "loki-uid",
		DatasourceType:  "loki",
	}, testOrgID)

	require.NoError(t, err, "Translate must succeed on a well-formed Loki request")
	assert.Contains(t, resp.Query, "failed login",
		"the translated LogQL query must preserve the LLM-returned filter expression")
	assert.Equal(t, "logql", resp.Language,
		"the response language must be 'logql' for a Loki datasource")
}

// TestTranslate_UnsupportedDatasource validates AAP §0.6.4 row 9
// (backend side): an unsupported datasource type must short-
// circuit BEFORE any HTTP request reaches the LLM, returning
// ErrUnsupportedDatasource. The test wires a mock LLM that flips
// a "called" boolean on every request and asserts the boolean
// remains false after Translate completes — proving no outbound
// HTTP call was issued.
//
// The unsupported type used here is "mysql" — chosen because
// MySQL is one of the most common non-Prometheus/non-Loki
// datasources in production Grafana deployments and is named
// explicitly in AAP §0.7.2.3 as out-of-scope for NLQ.
func TestTranslate_UnsupportedDatasource(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	// called tracks whether the mock LLM endpoint received any
	// request. For an unsupported datasource type, the orchestrator
	// must reject the request synchronously (no goroutine, no
	// network) — so called MUST remain false.
	var called bool
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "Show me failed logins",
		DatasourceUID:   "mysql-uid",
		DatasourceType:  "mysql",
	}, testOrgID)

	require.Error(t, err, "Translate must return an error for an unsupported datasource type")
	assert.ErrorIs(t, err, ErrUnsupportedDatasource,
		"the returned error must be classifiable as ErrUnsupportedDatasource via errors.Is")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called for unsupported datasource types — see AAP §0.6.4 row 9 and §0.8.5")
}

// TestTranslate_MissingAPIKey validates AAP §0.6.4 row 10 (one of
// the two security-critical paths): when GF_NLQ_LLM_API_KEY is
// unset (or set to the empty string), Translate must return
// ErrMissingAPIKey WITHOUT issuing an HTTP request. The test
// asserts both the typed error AND the absence of any outbound
// call to the mock LLM.
//
// SECURITY RATIONALE: a missing API key is an operator
// configuration error, not a user error. Issuing an HTTP request
// with an empty Authorization header would (a) waste an LLM
// quota call against the provider's anonymous reject path, (b)
// potentially expose the request body (which includes the user's
// natural language input and the schema-derived prompt) to a
// provider endpoint that may log unauthorized request bodies,
// and (c) muddle the diagnosable failure mode. Failing fast on
// missing key is the safer choice.
func TestTranslate_MissingAPIKey(t *testing.T) {
	// Setting the env var to the empty string is equivalent to
	// unsetting it for os.Getenv purposes — both cases produce
	// "" and trigger the ErrMissingAPIKey branch in callLLM.
	t.Setenv("GF_NLQ_LLM_API_KEY", "")

	var called bool
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "test query",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID)

	require.Error(t, err, "Translate must return an error when GF_NLQ_LLM_API_KEY is unset")
	assert.ErrorIs(t, err, ErrMissingAPIKey,
		"the returned error must be classifiable as ErrMissingAPIKey via errors.Is")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when the API key is missing — see AAP §0.8.5")
}

// TestTranslate_SchemaFetchFailure_ContinuesWithWarning validates
// AAP §0.6.4 row 8: graceful degradation when the schema-context
// fetch fails. The contract is "a schema-fetch failure produces a
// Warnings entry but the translation still completes successfully".
//
// MECHANICS: the fake dsService in newTestService is constructed
// EMPTY (no datasources seeded). Its GetDataSource implementation
// (see pkg/services/datasources/fakes/fake_datasource_service.go
// lines 25-35) returns ErrDataSourceNotFound for any non-empty UID
// when the DataSources slice is empty. This triggers the
// schema-fetch failure branch in translate.go's Translate without
// requiring a separate mock harness.
//
// The assertions establish three invariants:
//  1. err == nil (translation must complete despite the schema
//     fetch failure — this is the graceful-degradation property).
//  2. resp.Query is non-empty and matches the LLM-returned query
//     (the LLM call still happened and returned a valid response).
//  3. resp.Warnings is non-empty (the user is informed that
//     schema context was unavailable, so they can adjust their
//     interpretation of the generated query).
func TestTranslate_SchemaFetchFailure_ContinuesWithWarning(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": `{"query": "up", "explanation": "Liveness check"}`,
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockLLM.Close()

	// newTestService wires an EMPTY FakeDataSourceService; any UID
	// will fail to resolve, simulating the schema-fetch failure
	// path. No additional setup is required here.
	svc := newTestService(t, mockLLM.URL)

	resp, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "Show up metric",
		DatasourceUID:   "unknown-uid",
		DatasourceType:  "prometheus",
	}, testOrgID)

	require.NoError(t, err,
		"Translate MUST NOT fail when schema fetch fails — AAP §0.6.4 row 8 requires graceful degradation")
	assert.NotEmpty(t, resp.Query,
		"the LLM-returned query MUST still be produced when schema fetch fails")
	assert.Equal(t, "up", resp.Query,
		"the query string MUST be exactly what the mocked LLM returned")
	assert.NotEmpty(t, resp.Warnings,
		"Warnings MUST contain at least one entry describing the schema-fetch failure (AAP §0.6.4 row 8)")
}

// TestTranslate_LLMReturnsError_ReturnsErrLLMUnavailable validates
// AAP §0.6.4 row 7: any non-2xx HTTP response from the LLM
// provider must be surfaced as ErrLLMUnavailable. The HTTP handler
// for the frontend later maps this to a 502 Bad Gateway response
// so the user sees a clear "the upstream service is unavailable"
// message rather than a generic 500.
//
// The 500 status code is used as the canonical "LLM provider is
// having a bad day" simulation — but any non-2xx (4xx or 5xx)
// would produce the same ErrLLMUnavailable wrap via translate.go's
// callLLM. A separate test could exercise 4xx, but the cost-to-
// coverage ratio of duplicating the assertion is poor: callLLM's
// status check is a single `httpResp.StatusCode < 200 || >= 300`
// condition, so 500 is fully representative.
func TestTranslate_LLMReturnsError_ReturnsErrLLMUnavailable(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// Return a JSON-formatted error so the test exercises the
		// real-world shape of upstream LLM error responses (most
		// providers return application/json error bodies, not
		// plain text).
		_, _ = w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "test query",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID)

	require.Error(t, err, "Translate must return an error when the LLM returns a non-2xx status")
	assert.ErrorIs(t, err, ErrLLMUnavailable,
		"the returned error must be classifiable as ErrLLMUnavailable via errors.Is")
}

// TestTranslate_APIKeyNotLeaked is the most security-critical test
// in this file. It enforces AAP §0.8.5 (and is referenced by AAP
// §0.6.4 row 10): the LLM API key MUST NEVER appear in any
// response body returned to the client OR in any returned error
// message, even on a failure path where the temptation to "just
// include the request that failed" is highest.
//
// METHODOLOGY:
//  1. A sentinel key value ("super-secret-test-key-12345") that is
//     distinct from testAPIKey is used. The distinct value makes
//     any cross-contamination between tests trivially detectable.
//  2. The mock LLM returns a 500 status, deliberately triggering
//     the ErrLLMUnavailable wrap path in callLLM — the path where
//     a careless implementer would be most likely to include the
//     outgoing request (and therefore the Authorization header)
//     in the error context.
//  3. Two assertions:
//     a) err.Error() MUST NOT contain the sentinel key.
//     b) The JSON-marshalled TranslateResponse MUST NOT contain
//     the sentinel key (the response will be zero-valued in
//     this failure path, but the assertion still pins the
//     contract for future refactors that might add fields).
//
// LOG-LINE LEAKAGE: capturing log output and asserting on it would
// require a custom log.Logger sink. The implementation of
// translate.go's callLLM passes ONLY the safeHost(endpoint), the
// model name, and the HTTP status to s.log.Error — it never
// references the apiKey local variable in any log call. Reviewing
// the production source for any `apiKey` reference outside of the
// header.Set("Authorization", ...) line is the canonical audit
// step for this invariant; this test enforces the wire-visible
// portion of the invariant (response body and error string).
//
// Why two distinct test keys (testAPIKey vs. sensitiveKey)?
// Using a different value in this test isolates the security
// concern: a programmer who accidentally bundles the API key into
// an error message in a SUCCESS path test would not fail this
// test (because this test sets a different key). But this test
// fails immediately if the leaking happens in any error path.
// The orthogonality keeps the security signal clean.
func TestTranslate_APIKeyNotLeaked(t *testing.T) {
	const sensitiveKey = "super-secret-test-key-12345"
	t.Setenv("GF_NLQ_LLM_API_KEY", sensitiveKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		// Even if the server echoes back what it received, the
		// API key MUST NOT be reflected in error chains visible
		// to the client. translate.go's callLLM discards the
		// response body on non-2xx status precisely to prevent
		// this kind of accidental echo.
		_, _ = w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	resp, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "test query",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID)

	// The translation must have failed because the mock LLM
	// returned 500 — this is the precondition that activates the
	// "error path" branches we are guarding.
	require.Error(t, err, "the test precondition requires the LLM call to fail")

	// SECURITY INVARIANT 1: the API key must NEVER appear in the
	// error message. If this assertion fires, AAP §0.8.5 has been
	// violated — callers (and log scrapers, if the error is
	// logged anywhere) would see the secret.
	assert.NotContains(t, err.Error(), sensitiveKey,
		"AAP §0.8.5 violated: API key leaked into error message: %v", err)

	// SECURITY INVARIANT 2: the API key must NEVER appear in the
	// serialised response body. The TranslateResponse is zero-
	// valued in this error path, but the assertion still pins
	// the contract for any future refactor that might add fields
	// echoing the original request.
	respBytes, marshalErr := json.Marshal(resp)
	require.NoError(t, marshalErr, "TranslateResponse must always be JSON-serialisable")
	assert.NotContains(t, string(respBytes), sensitiveKey,
		"AAP §0.8.5 violated: API key leaked into response body: %s", string(respBytes))

	// NOTE: log-line capture would require a test log sink
	// (e.g., wiring a buffer-backed gokit logger into the Service
	// struct before calling Translate). The production
	// implementation in translate.go uses safeHost, the configured
	// model name, the HTTP status, and the underlying transport
	// error in its log calls — NEVER the apiKey local variable.
	// Reviewers may verify this invariant by grepping translate.go
	// for the string "apiKey" and confirming it appears only in
	// (a) the os.Getenv read and (b) the header.Set
	// "Authorization" composition.
}

// TestTranslate_EmptyInput validates the input-validation contract
// implied by AAP §0.6.4 (row 5 references the broader empty-input
// rejection, and row 9 enumerates the validation chain). The
// orchestrator must reject an empty NaturalLanguage field with
// ErrEmptyInput BEFORE issuing any HTTP request. As with the
// missing-API-key and unsupported-datasource tests, a "called"
// boolean confirms no outbound network activity occurred.
//
// "Empty" here refers to the literal empty string. Whitespace-
// only strings (e.g., "   ") are normalised via strings.TrimSpace
// in translate.go's Translate before the empty check, so a
// whitespace-only input would also trigger ErrEmptyInput — but
// asserting on the literal empty case is sufficient because the
// TrimSpace normalisation is a pure function whose behavior is
// covered by the stdlib's own tests.
func TestTranslate_EmptyInput(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	var called bool
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID)

	require.Error(t, err, "Translate must return an error when NaturalLanguage is empty")
	assert.ErrorIs(t, err, ErrEmptyInput,
		"the returned error must be classifiable as ErrEmptyInput via errors.Is")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when the natural-language input is empty")
}

// TestTranslate_AcceptsMimirAsPrometheusCompatible validates AAP
// §0.6.3.1: Grafana Mimir is a Prometheus-API-compatible
// long-term-storage backend, so a Mimir datasource type must be
// accepted and produce a "promql" language identifier in the
// response.
//
// The mocked LLM endpoint returns a trivial PromQL query ("up")
// and the assertion only checks the Language field — confirming
// that translate.go's supportedLanguages map correctly resolves
// "mimir" to "promql". Asserting on Language alone (rather than
// the full Query content) keeps this test focused on the Mimir
// compatibility contract; the broader happy-path Prometheus
// assertions are already covered by TestTranslate_PrometheusSuccess.
func TestTranslate_AcceptsMimirAsPrometheusCompatible(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"query": "up", "explanation": "Mimir test"}`}},
			},
		})
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	resp, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "Show liveness",
		DatasourceUID:   "mimir-uid",
		DatasourceType:  "mimir",
	}, testOrgID)

	require.NoError(t, err, "Translate must accept 'mimir' as a Prometheus-compatible datasource type")
	assert.Equal(t, "promql", resp.Language,
		"the response language must be 'promql' for a Mimir datasource (AAP §0.6.3.1)")
}
