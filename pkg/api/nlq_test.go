// NLQ feature: HTTP API surface integration tests.
//
// These tests validate POST /api/nlq/translate end-to-end through the
// HTTPServer test harness. The route itself is registered by
// pkg/services/nlq/service.go (the NLQ service self-registers via
// RouteRegister.Group("/api/nlq", ...)). These tests construct the NLQ
// service inside the SetupAPITestServer customization callback so the
// route is mounted on the same RouteRegister used by the webtest server.
//
// Validation criteria covered (AAP §0.6.4):
//   - row 1: 200 OK with valid Prometheus request
//   - row 2: 200 OK with valid Loki request (covered indirectly via
//     pkg/services/nlq/service_test.go)
//   - row 7: 502 surfaced on LLM upstream 500
//   - row 9: 400 surfaced on unsupported datasource type
//   - row 10: API key never echoed into response body
//
// SECURITY INVARIANT (AAP §0.8.5):
// The test TestNLQAPI_Translate_DoesNotEchoAPIKey is the HTTP-layer
// enforcement of the no-leak invariant. The LLM API key MUST NEVER
// appear in any HTTP response body returned by the NLQ endpoint —
// neither on success nor on any error path. Every test that needs
// the API key uses t.Setenv (Go 1.17+) so the value is automatically
// restored when the test exits regardless of pass/fail/panic.
//
// IMPLEMENTATION NOTES:
//   - The package import for the datasources fakes is aliased as
//     `fakeDatasources` because the package declaration in
//     pkg/services/datasources/fakes/fake_datasource_service.go is
//     `package datasources` (not `fakes`), which would otherwise
//     collide with the actual `datasources` import in this file. The
//     same alias is used in pkg/api/ds_query_test.go.
//   - The setupNLQTestServer helper seeds the FakeDataSourceService
//     with a Prometheus and a Loki datasource (each with an
//     unreachable URL) so the schema-context lookup succeeds (the UID
//     resolves) while the live HTTP metadata fetch SOFT-fails (the
//     URL is unreachable). This matches the test fixtures in
//     pkg/services/nlq/service_test.go and lets tests exercise the
//     full LLM call path. Tests that need a different datasource
//     topology pass an explicit dsService argument.
package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pluginfakes "github.com/grafana/grafana/pkg/plugins/manager/pluginfakes"
	"github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/accesscontrol/acimpl"
	"github.com/grafana/grafana/pkg/services/datasources"
	fakeDatasources "github.com/grafana/grafana/pkg/services/datasources/fakes"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/nlq"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/web/webtest"
)

// nlqTestOrgID is the canonical organization ID used by every NLQ test
// in this file. The seeded datasources are registered under this org so
// the UID lookup in fetchSchemaContext (which is scoped by orgID derived
// from c.GetOrgID() in PostTranslate) resolves correctly.
const nlqTestOrgID int64 = 1

// nlqTestUserID is the canonical user ID used by every NLQ test in this
// file. Permissions are attached to this user via authedUserWithPermissions
// (declared in common_test.go).
const nlqTestUserID int64 = 1

// nlqTestFailingCallResource is the default plugin-client behavior used
// by the NLQ API integration tests: every CallResource invocation
// returns a transport error so the live schema-metadata fetch issued by
// pkg/services/nlq/schema_context.go SOFT-fails. That produces the
// Warnings entry on the TranslateResponse body which these tests
// assert against.
//
// Tests that need a different behavior (e.g., a 2xx response with
// canned label data) construct their own pluginfakes.FakePluginClient
// with a customised CallResourceHandlerFunc.
var nlqTestFailingCallResource = backend.CallResourceHandlerFunc(func(_ context.Context, _ *backend.CallResourceRequest, _ backend.CallResourceResponseSender) error {
	return errNLQTestFailingCallResource
})

// errNLQTestFailingCallResource is the sentinel error returned by
// nlqTestFailingCallResource. Centralised so its message can be
// audited in one place — the message MUST NEVER contain anything
// that resembles an API key or response body so it never confuses
// the no-leak assertions in TestNLQAPI_Translate_DoesNotEchoAPIKey.
var errNLQTestFailingCallResource = errNLQTestSentinelError("synthetic plugin transport failure for NLQ API tests")

// errNLQTestSentinelError is a typed string error used by the NLQ
// API tests' plugin-client fake. Declared as its own type so future
// type assertions or errors.As classification stay explicit.
type errNLQTestSentinelError string

// Error satisfies the error interface for errNLQTestSentinelError.
func (e errNLQTestSentinelError) Error() string { return string(e) }

// seedNLQDataSources returns a FakeDataSourceService pre-populated with
// a Prometheus and a Loki datasource. The URLs point to a deliberately
// unreachable host so the live HTTP metadata fetch issued by
// pkg/services/nlq/schema_context.go SOFT-fails (producing a Warnings
// entry on TranslateResponse) while the UID lookup itself succeeds.
//
// This topology matches the test fixtures in
// pkg/services/nlq/service_test.go's seedDataSources helper. Tests that
// need a different topology (e.g., to exercise the HARD failure on UID
// lookup) should construct their own FakeDataSourceService and pass it
// to setupNLQTestServer.
func seedNLQDataSources() *fakeDatasources.FakeDataSourceService {
	return &fakeDatasources.FakeDataSourceService{
		DataSources: []*datasources.DataSource{
			{
				ID:    1,
				OrgID: nlqTestOrgID,
				UID:   "prom-uid",
				Type:  "prometheus",
				URL:   "http://127.0.0.1:1", // unreachable; SOFT-fail the live schema fetch
			},
			{
				ID:    2,
				OrgID: nlqTestOrgID,
				UID:   "loki-uid",
				Type:  "loki",
				URL:   "http://127.0.0.1:1",
			},
		},
	}
}

// setupNLQTestServer builds a webtest.Server with the POST /api/nlq/translate
// route registered.
//
// Parameters:
//   - llmURL:    the base URL of an httptest.Server that mocks the external
//     LLM provider. Set on cfg.NLQEndpoint so the NLQ service issues outbound
//     calls there instead of hitting api.openai.com.
//   - dsService: the (possibly nil) DataSourceService implementation. When
//     nil, the seedNLQDataSources helper is used (Prometheus + Loki seeded
//     with unreachable URLs). Pass a custom service to exercise alternative
//     datasource topologies.
//
// Lifecycle:
//  1. SetupAPITestServer constructs an &HTTPServer{RouteRegister:..., ...}
//     with default fields (see pkg/api/common_test.go:L298-L305).
//  2. The opt callback (this function's body) populates hs.Cfg,
//     hs.Features, and hs.AccessControl with NLQ-aware values and invokes
//     nlq.ProvideService(...). ProvideService's constructor body calls
//     registerAPIEndpoints() because the nlqEnabled feature toggle is on
//     (per the harmonized-gate contract introduced by the MAJOR review
//     finding fix in pkg/services/nlq/service.go: route registration is
//     keyed solely on the feature toggle; cfg.NLQEnabled becomes a
//     runtime kill switch enforced inside the handler via
//     ErrServiceDisabled -> HTTP 503). This mounts POST /api/nlq/translate
//     onto hs.RouteRegister.
//  3. After the opt callback returns, SetupAPITestServer calls
//     hs.registerRoutes() to mount the legacy HTTPServer routes on the
//     same RouteRegister. The legacy registration does not conflict with
//     /api/nlq/translate because no legacy route uses that path.
//  4. webtest.NewServer(t, hs.RouteRegister) starts the test HTTP server.
//
// Why hs.AccessControl is a REAL acimpl.ProvideAccessControl(features) and
// NOT a fake: the NLQ service performs UID-scoped authorization inside the
// PostTranslate handler via s.ac.Evaluate(...) using
// ac.EvalPermission(datasources.ActionQuery, ScopeProvider.GetResourceScopeUID(uid)).
// A fake that always returns true would short-circuit the check and make
// the 403 test meaningless. The real implementation evaluates the
// permission against the user's attached permission set, matching the
// production code path.
func setupNLQTestServer(t *testing.T, llmURL string, dsService datasources.DataSourceService) *webtest.Server {
	t.Helper()
	return SetupAPITestServer(t, func(hs *HTTPServer) {
		cfg := setting.NewCfg()
		// NLQ feature: enable both gates so the route is registered
		// AND the handler does not short-circuit with ErrServiceDisabled.
		// cfg.NLQEnabled is now an in-handler kill switch (see the
		// harmonized-gate contract above); leaving it true means the
		// happy-path tests reach the translation pipeline.
		cfg.NLQEnabled = true
		cfg.NLQProvider = "openai"
		cfg.NLQEndpoint = llmURL
		cfg.NLQModel = "gpt-4o-test"
		hs.Cfg = cfg

		// NLQ feature: enable the feature flag so ProvideService mounts
		// the route. Under the harmonized-gate contract (MAJOR review
		// finding fix), route registration is keyed solely on the
		// feature toggle; the cfg.NLQEnabled gate is a runtime
		// operator kill switch enforced inside the handler.
		features := featuremgmt.WithFeatures(featuremgmt.FlagNlqEnabled)
		hs.Features = features

		// NLQ feature: use a REAL AccessControl so the UID-scoped
		// authorization in PostTranslate evaluates against the user's
		// attached permission set (not a fake that short-circuits).
		hs.AccessControl = acimpl.ProvideAccessControl(features)

		if dsService == nil {
			dsService = seedNLQDataSources()
		}

		// NLQ feature MAJOR fix (review feedback — CallResource
		// integration): the NLQ service requires a plugins.Client and
		// a *plugincontext.Provider for live schema metadata retrieval.
		// In these API integration tests we want the live fetch to
		// SOFT-fail (so the response body contains a Warnings entry
		// without blocking the LLM call) — supplying a FakePluginClient
		// whose CallResourceHandlerFunc returns an error, plus a nil
		// pluginCtx, makes the plugin runtime defensive check inside
		// schema_context.go return early with "plugin runtime not
		// configured". That error is treated as a soft failure by the
		// orchestrator, exactly matching the pre-fix behavior these
		// tests were written against.
		pluginClient := &pluginfakes.FakePluginClient{
			CallResourceHandlerFunc: nlqTestFailingCallResource,
		}

		// NLQ feature: register the translation service. ProvideService's
		// constructor calls registerAPIEndpoints() synchronously when the
		// feature toggle is enabled, mounting POST /api/nlq/translate on
		// hs.RouteRegister (which is the same RouteRegister webtest.NewServer
		// hands to the running HTTP server).
		_, err := nlq.ProvideService(cfg, hs.RouteRegister, dsService, hs.AccessControl, features, pluginClient, nil)
		require.NoError(t, err, "nlq.ProvideService must succeed in the test harness")
	})
}

// mockLLMServer returns an *httptest.Server that responds with the given
// HTTP status code.
//
// When statusCode is 2xx, the response body is a canned OpenAI Chat
// Completions envelope whose choices[0].message.content is a JSON string
// containing the provided query and explanation. The NLQ service's
// parseResponse helper unmarshals this content into its parsedQuery
// struct.
//
// When statusCode is non-2xx, the response is a fixed JSON error body
// at the given status. The NLQ service's callLLM helper detects the
// non-2xx status, discards the body (so no upstream content leaks into
// the client-visible response), and returns ErrLLMUnavailable.
//
// The handler intentionally does NOT assert on the inbound Authorization
// header value because individual tests may use different keys (the
// canonical TestNLQAPI_Translate_DoesNotEchoAPIKey test uses a distinct
// sensitive key string). The key-presence assertion is enforced via
// response-body inspection in TestNLQAPI_Translate_DoesNotEchoAPIKey.
func mockLLMServer(t *testing.T, statusCode int, query, explanation string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if statusCode < 200 || statusCode >= 300 {
			w.WriteHeader(statusCode)
			// NLQ feature: a static error body so the NLQ service's
			// non-2xx path is exercised deterministically.
			_, _ = w.Write([]byte(`{"error":"upstream failure"}`))
			return
		}

		// 2xx: emit a canned OpenAI Chat Completions envelope. The
		// inner JSON string is what NLQ's parseResponse expects to
		// unmarshal into its parsedQuery struct.
		innerJSON, _ := json.Marshal(map[string]string{
			"query":       query,
			"explanation": explanation,
		})
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": string(innerJSON),
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// nlqRequestBody constructs the JSON body of a POST /api/nlq/translate
// request. Empty fields are included so tests can validate the 400
// rejection of empty input without ambiguity about field presence.
//
// The field names ("input", "datasourceUid", "datasourceType") mirror
// the JSON tags on pkg/services/nlq.TranslateRequest. Any drift between
// this helper and the production DTO would break the HTTP contract.
func nlqRequestBody(input, dsUID, dsType string) string {
	body, _ := json.Marshal(map[string]string{
		"input":          input,
		"datasourceUid":  dsUID,
		"datasourceType": dsType,
	})
	return string(body)
}

// -----------------------------------------------------------------
// Test cases — see the file header for the AAP §0.6.4 mapping.
// -----------------------------------------------------------------

// TestNLQAPI_Translate_Returns200_OnValidPrometheusRequest validates
// AAP §0.6.4 row 1 at the HTTP layer: a well-formed Prometheus request
// from a properly-authorized caller yields 200 OK with a PromQL query in
// the response body.
//
// The mock LLM returns a fixed PromQL string; the test asserts that
// string appears verbatim in the response, the language identifier is
// "promql", and the explanation is non-empty.
func TestNLQAPI_Translate_Returns200_OnValidPrometheusRequest(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", "test-api-key")

	mockLLM := mockLLMServer(t, http.StatusOK,
		"sum by (endpoint) (rate(http_requests_total[24h]))",
		"Total API request rate by endpoint over 24h",
	)
	defer mockLLM.Close()

	server := setupNLQTestServer(t, mockLLM.URL, nil)

	body := nlqRequestBody(
		"Graph total API request rate by endpoint over the last 24 hours",
		"prom-uid",
		"prometheus",
	)
	req := webtest.RequestWithSignedInUser(
		server.NewPostRequest("/api/nlq/translate", strings.NewReader(body)),
		authedUserWithPermissions(nlqTestUserID, nlqTestOrgID, []accesscontrol.Permission{
			{Action: datasources.ActionQuery, Scope: datasources.ScopeAll},
		}),
	)

	res, err := server.SendJSON(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	assert.Equal(t, http.StatusOK, res.StatusCode, "expected 200 OK on valid request")

	var resp struct {
		Query       string   `json:"query"`
		Language    string   `json:"language"`
		Explanation string   `json:"explanation"`
		Warnings    []string `json:"warnings"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&resp))
	assert.Equal(t, "sum by (endpoint) (rate(http_requests_total[24h]))", resp.Query,
		"the translated query must match the LLM-returned PromQL verbatim")
	assert.Equal(t, "promql", resp.Language,
		"the response language must be 'promql' for a Prometheus datasource")
	assert.NotEmpty(t, resp.Explanation,
		"the response explanation must be populated when the LLM returns one")
}

// TestNLQAPI_Translate_Returns400_OnMissingInput validates that an
// empty "input" field is rejected by the handler with 400 BEFORE any
// LLM call. The mock LLM records whether it was invoked; the test
// asserts it was not.
//
// Flow:
//  1. web.Bind succeeds (the body is valid JSON, just with empty input)
//  2. PostTranslate's UID-non-empty check passes
//  3. authorizeDatasourceQuery passes (user has datasources.ActionQuery)
//  4. Translate is called -> empty NaturalLanguage -> ErrEmptyInput
//  5. errorResponse maps ErrEmptyInput -> 400 Bad Request
func TestNLQAPI_Translate_Returns400_OnMissingInput(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", "test-api-key")

	called := false
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer mockLLM.Close()

	server := setupNLQTestServer(t, mockLLM.URL, nil)

	body := nlqRequestBody("", "prom-uid", "prometheus") // empty input
	req := webtest.RequestWithSignedInUser(
		server.NewPostRequest("/api/nlq/translate", strings.NewReader(body)),
		authedUserWithPermissions(nlqTestUserID, nlqTestOrgID, []accesscontrol.Permission{
			{Action: datasources.ActionQuery, Scope: datasources.ScopeAll},
		}),
	)

	res, err := server.SendJSON(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	assert.Equal(t, http.StatusBadRequest, res.StatusCode,
		"expected 400 on empty natural-language input")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when input is empty (AAP §0.8.5 fail-closed input validation)")
}

// TestNLQAPI_Translate_Returns401_WhenUnauthenticated validates that
// the route-level middleware.ReqSignedIn rejects unauthenticated
// requests with 401.
//
// The webtest server does NOT attach a signed-in user unless
// webtest.RequestWithSignedInUser is called on the request. Issuing
// a request without that wrap leaves c.IsSignedIn = false, which
// causes middleware.ReqSignedIn to invoke notAuthorized — which
// returns 401 for /api requests (verified by inspection of
// pkg/middleware/auth.go).
func TestNLQAPI_Translate_Returns401_WhenUnauthenticated(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", "test-api-key")

	mockLLM := mockLLMServer(t, http.StatusOK, "up", "Liveness check")
	defer mockLLM.Close()

	server := setupNLQTestServer(t, mockLLM.URL, nil)

	body := nlqRequestBody("Show up", "prom-uid", "prometheus")
	// NO webtest.RequestWithSignedInUser wrapping -> anonymous request.
	req := server.NewPostRequest("/api/nlq/translate", strings.NewReader(body))

	res, err := server.SendJSON(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, res.StatusCode,
		"expected 401 when middleware.ReqSignedIn rejects an unauthenticated request")
}

// TestNLQAPI_Translate_Returns403_WhenLacksDatasourceQueryPermission
// validates that the UID-scoped authorization in PostTranslate
// (ac.EvalPermission(datasources.ActionQuery, ScopeProvider.GetResourceScopeUID(uid)))
// rejects users without the required permission with 403 Forbidden.
//
// The user is signed in (so middleware.ReqSignedIn passes) but has
// only an unrelated permission attached. The real
// acimpl.ProvideAccessControl evaluates the permission set, finds no
// matching grant for datasources:query, and returns hasAccess=false.
// PostTranslate's authorizeDatasourceQuery converts this to
// ErrForbiddenDatasource -> 403.
//
// This is the assertion that proves the UID-scoped authorization is
// wired correctly and that a fake AccessControl would have hidden the
// failure mode.
func TestNLQAPI_Translate_Returns403_WhenLacksDatasourceQueryPermission(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", "test-api-key")

	mockLLM := mockLLMServer(t, http.StatusOK, "up", "Liveness check")
	defer mockLLM.Close()

	server := setupNLQTestServer(t, mockLLM.URL, nil)

	body := nlqRequestBody("Show up", "prom-uid", "prometheus")
	// User is signed in but has NO datasources:query permission — only
	// an unrelated permission. The real AccessControl will evaluate the
	// user's permission set and find no matching grant.
	req := webtest.RequestWithSignedInUser(
		server.NewPostRequest("/api/nlq/translate", strings.NewReader(body)),
		authedUserWithPermissions(nlqTestUserID, nlqTestOrgID, []accesscontrol.Permission{
			{Action: "some-other-permission", Scope: "*"},
		}),
	)

	res, err := server.SendJSON(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	assert.Equal(t, http.StatusForbidden, res.StatusCode,
		"expected 403 when caller lacks datasources.ActionQuery permission for the requested UID")
}

// TestNLQAPI_Translate_Returns502_OnLLMUpstreamFailure validates
// AAP §0.6.4 row 7: when the LLM upstream returns 500, the NLQ
// service surfaces this as ErrLLMUnavailable, which the handler
// maps to 502 Bad Gateway via errorResponse.
//
// The seeded Prometheus datasource (prom-uid) ensures the schema
// context lookup succeeds (UID resolves), so the LLM is actually
// called. The unreachable URL on the datasource causes a SOFT
// schema-fetch failure (warning), but Translate continues and
// calls the LLM. The LLM mock returns 500, triggering
// ErrLLMUnavailable -> 502.
func TestNLQAPI_Translate_Returns502_OnLLMUpstreamFailure(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", "test-api-key")

	// NLQ feature: mock LLM returns 500 -> NLQ service wraps as
	// ErrLLMUnavailable -> errorResponse maps to 502 Bad Gateway.
	mockLLM := mockLLMServer(t, http.StatusInternalServerError, "", "")
	defer mockLLM.Close()

	server := setupNLQTestServer(t, mockLLM.URL, nil)

	body := nlqRequestBody("Show up", "prom-uid", "prometheus")
	req := webtest.RequestWithSignedInUser(
		server.NewPostRequest("/api/nlq/translate", strings.NewReader(body)),
		authedUserWithPermissions(nlqTestUserID, nlqTestOrgID, []accesscontrol.Permission{
			{Action: datasources.ActionQuery, Scope: datasources.ScopeAll},
		}),
	)

	res, err := server.SendJSON(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	assert.Equal(t, http.StatusBadGateway, res.StatusCode,
		"expected 502 Bad Gateway when LLM upstream returns 500 (ErrLLMUnavailable mapping)")
}

// TestNLQAPI_Translate_Returns400_OnUnsupportedDatasourceType validates
// AAP §0.6.4 row 9 at the HTTP layer: when the request's datasourceType
// is not in the supported set ("prometheus", "mimir", "loki"), the
// translation short-circuits with 400 BEFORE the LLM is called.
//
// The mock LLM records whether it was invoked; the test asserts it was
// not, codifying the fail-closed invariant: an unsupported datasource
// type must never trigger external traffic.
func TestNLQAPI_Translate_Returns400_OnUnsupportedDatasourceType(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", "test-api-key")

	called := false
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer mockLLM.Close()

	server := setupNLQTestServer(t, mockLLM.URL, nil)

	body := nlqRequestBody("Show me logs", "mysql-uid", "mysql")
	req := webtest.RequestWithSignedInUser(
		server.NewPostRequest("/api/nlq/translate", strings.NewReader(body)),
		authedUserWithPermissions(nlqTestUserID, nlqTestOrgID, []accesscontrol.Permission{
			{Action: datasources.ActionQuery, Scope: datasources.ScopeAll},
		}),
	)

	res, err := server.SendJSON(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	assert.Equal(t, http.StatusBadRequest, res.StatusCode,
		"expected 400 Bad Request for unsupported datasource type (ErrUnsupportedDatasource mapping)")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called for unsupported datasource types (AAP §0.6.4 row 9, §0.8.5)")
}

// TestNLQAPI_Translate_DoesNotEchoAPIKey is the CRITICAL SECURITY TEST
// for AAP §0.8.5. It is the HTTP-layer enforcement of the no-leak
// invariant: the LLM API key MUST NEVER appear in any HTTP response
// body — neither on success nor on failure.
//
// The test deliberately exercises an error path (the mock LLM returns
// 500) because error paths are the most common source of accidental
// secret leakage. A sensitive key value is set via t.Setenv (so it is
// automatically restored when the test exits), a request is issued,
// and the entire response body is scanned for the key string AND a
// substring of it (the latter catches partial leakage where the key
// might be wrapped in JSON quotes or concatenated with other content).
//
// If this test ever fails, the NLQ service has introduced a regression
// that exposes the operator's LLM API key to the client. That is a
// CRITICAL security defect and MUST be fixed before the regression
// merges.
func TestNLQAPI_Translate_DoesNotEchoAPIKey(t *testing.T) {
	const sensitiveKey = "super-secret-api-key-do-not-leak-12345"
	t.Setenv("GF_NLQ_LLM_API_KEY", sensitiveKey)

	// NLQ feature: mock LLM intentionally returns 500 so the error
	// path is exercised. The test verifies the API key does not leak
	// even when the upstream call fails — a common source of
	// accidental error-message leakage.
	mockLLM := mockLLMServer(t, http.StatusInternalServerError, "", "")
	defer mockLLM.Close()

	server := setupNLQTestServer(t, mockLLM.URL, nil)

	body := nlqRequestBody("Show me up", "prom-uid", "prometheus")
	req := webtest.RequestWithSignedInUser(
		server.NewPostRequest("/api/nlq/translate", strings.NewReader(body)),
		authedUserWithPermissions(nlqTestUserID, nlqTestOrgID, []accesscontrol.Permission{
			{Action: datasources.ActionQuery, Scope: datasources.ScopeAll},
		}),
	)

	res, err := server.SendJSON(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	// The status code is NOT the focus of this test — the response
	// body is. We assert the response is NOT 200 (the LLM returned
	// 500) to confirm the error path was exercised, then drain the
	// body and inspect for any trace of the sensitive key.
	assert.NotEqual(t, http.StatusOK, res.StatusCode,
		"expected a non-2xx status because the mock LLM returned 500")

	respBytes, err := io.ReadAll(res.Body)
	require.NoError(t, err)

	// CRITICAL SECURITY ASSERTION (AAP §0.8.5):
	// The full key MUST NOT appear anywhere in the response body.
	assert.NotContains(t, string(respBytes), sensitiveKey,
		"AAP §0.8.5 violated: the full API key leaked into the HTTP response body")
	// Defense-in-depth: a unique prefix of the key MUST NOT appear
	// either. This catches partial leakage scenarios where the key
	// might be embedded in a wrapped error message but truncated.
	assert.NotContains(t, string(respBytes), "super-secret",
		"AAP §0.8.5 violated: an API key fragment leaked into the HTTP response body")
}

// TestNLQAPI_Translate_Returns200_WithWarningsOnSchemaFetchFailure
// validates AAP §0.6.4 row 8 at the HTTP layer: when the live HTTP
// schema-context fetch fails (the datasource exists but the metadata
// endpoint is unreachable), the translation still succeeds with 200 OK
// and the response includes a non-empty Warnings array.
//
// The seeded Prometheus datasource has a URL of http://127.0.0.1:1
// (intentionally unreachable). The schema fetch initiated by
// fetchSchemaContext attempts an HTTP call to that URL and fails. The
// HARD/SOFT failure semantics in schema_context.go treat this as a
// SOFT failure (the UID resolved cleanly; only the metadata fetch
// failed), so Translate continues with the base hints and attaches a
// Warnings entry to the response.
//
// This codifies the "graceful degradation" contract from AAP §0.1.1.1:
// "failures in the LLM HTTP call, the schema-context fetch, or query
// parsing MUST be surfaced through an Alert component within the NLQ
// bar — never as a thrown exception."
func TestNLQAPI_Translate_Returns200_WithWarningsOnSchemaFetchFailure(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", "test-api-key")

	mockLLM := mockLLMServer(t, http.StatusOK, "up", "Liveness check")
	defer mockLLM.Close()

	// Default seeded datasources have unreachable URLs (127.0.0.1:1)
	// so the live schema fetch will SOFT-fail and produce a Warnings
	// entry. The UID lookup itself succeeds because the seeded
	// datasource matches "prom-uid".
	server := setupNLQTestServer(t, mockLLM.URL, nil)

	body := nlqRequestBody("Show up", "prom-uid", "prometheus")
	req := webtest.RequestWithSignedInUser(
		server.NewPostRequest("/api/nlq/translate", strings.NewReader(body)),
		authedUserWithPermissions(nlqTestUserID, nlqTestOrgID, []accesscontrol.Permission{
			{Action: datasources.ActionQuery, Scope: datasources.ScopeAll},
		}),
	)

	res, err := server.SendJSON(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	assert.Equal(t, http.StatusOK, res.StatusCode,
		"schema-fetch SOFT failure must NOT fail the translation; expected 200 OK")

	var resp struct {
		Query    string   `json:"query"`
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&resp))
	assert.Equal(t, "up", resp.Query,
		"the translated query must be returned even when schema fetch fails")
	assert.NotEmpty(t, resp.Warnings,
		"Warnings must contain the schema-fetch failure entry (graceful degradation contract)")
}
