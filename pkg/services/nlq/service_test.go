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
//
// NLQ feature MAJOR review enhancements (CP1 follow-up):
//   - newTestService now seeds the FakeDataSourceService with both
//     a Prometheus and a Loki datasource so the new HARD-vs-SOFT
//     error semantics in fetchSchemaContext succeed for the
//     happy-path tests instead of incorrectly classifying them
//     as ErrInvalidDatasource.
//   - TestTranslate_EmptyDatasourceUID, TestTranslate_TypeMismatch,
//     TestTranslate_DatasourceNotFound exercise the new HARD
//     validation paths.
//   - TestTranslate_LiveSchemaFetchFailure_ContinuesWithWarning
//     verifies the SOFT-failure warning-only fallback.
//   - TestTranslate_InvalidQuerySyntax_PromQL / _LogQL verify the
//     new syntactic validation step.
//   - TestSanitizeTransportError_DoesNotLeakURL pins the URL
//     sanitization guarantee.
//   - TestPostTranslate_* exercise the handler-level pipeline
//     including response.Response serialization and UID-scoped
//     authorization.
//   - TestTranslate_APIKeyNotLeaked is extended with a capturing
//     logger so the no-leak invariant covers log lines, not just
//     error strings and response bodies.
package nlq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gokitlog "github.com/go-kit/log"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/grafana/pkg/api/routing"
	"github.com/grafana/grafana/pkg/apimachinery/identity"
	"github.com/grafana/grafana/pkg/infra/log"
	pluginfakes "github.com/grafana/grafana/pkg/plugins/manager/pluginfakes"
	"github.com/grafana/grafana/pkg/services/accesscontrol/actest"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/datasources"

	// dsfakes is aliased because the package declared inside
	// pkg/services/datasources/fakes is named "datasources" (the
	// directory name and the package name diverge in this case),
	// which would otherwise collide with the actual datasources
	// package if it were ever needed in the same scope. The alias
	// matches the convention established elsewhere in the codebase
	// (e.g., pkg/services/ngalert/schedule/recording_rule_test.go).
	dsfakes "github.com/grafana/grafana/pkg/services/datasources/fakes"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/web"
)

// fakePluginContextProvider is a lightweight stand-in for
// *plugincontext.Provider used by the NLQ schema-fetch tests. The
// production type carries a substantial dependency chain
// (pluginstore, cache service, datasource cache, plugin settings,
// secrets) that is irrelevant to the NLQ service contract; the
// fake constructs a minimal backend.PluginContext envelope that
// the FakePluginClient's CallResourceHandlerFunc can consume.
//
// Tests that want the live schema fetch to SOFT-fail set
// CallResourceErr on the fake plugin client (the typical pattern,
// since most tests assert the Warnings entry that arises from a
// soft failure).
type fakePluginContextProvider struct {
	err error
}

func (f *fakePluginContextProvider) GetWithDataSource(_ context.Context, pluginID string, _ identity.Requester, ds *datasources.DataSource) (backend.PluginContext, error) {
	if f.err != nil {
		return backend.PluginContext{}, f.err
	}
	ctxOrgID := int64(0)
	if ds != nil {
		ctxOrgID = ds.OrgID
	}
	return backend.PluginContext{
		OrgID:    ctxOrgID,
		PluginID: pluginID,
	}, nil
}

// failingCallResourceFunc is the default plugin-client behavior in
// tests: every CallResource invocation returns a transport error so
// fetchLiveSchema downgrades to a SOFT failure and Translate attaches
// the Warnings entry that the happy-path tests rely on. Tests that
// want a different outcome (e.g. a 2xx response with canned label
// names) construct their own FakePluginClient instead.
var failingCallResourceFunc = backend.CallResourceHandlerFunc(func(_ context.Context, _ *backend.CallResourceRequest, _ backend.CallResourceResponseSender) error {
	return errors.New("synthetic plugin transport failure")
})

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

// seedDataSources returns a FakeDataSourceService pre-populated with
// the canonical Prometheus and Loki datasources used by the test
// suite. Tests that need a different topology (e.g., empty fake,
// type mismatch) construct the FakeDataSourceService directly
// instead of calling this helper.
//
// The seeded datasources use predictable UIDs ("prom-uid", "loki-uid",
// "mimir-uid") so test bodies read naturally. Each datasource has a
// non-empty URL pointing to a placeholder host that the SOFT-failure
// path can reach via the live-schema HTTP fetcher; tests that want
// the live fetch to succeed override the URL with an httptest.Server
// after constructing the service.
func seedDataSources() *dsfakes.FakeDataSourceService {
	return &dsfakes.FakeDataSourceService{
		DataSources: []*datasources.DataSource{
			{
				ID:    1,
				OrgID: testOrgID,
				UID:   "prom-uid",
				Type:  "prometheus",
				URL:   "http://127.0.0.1:1", // unreachable on purpose; live fetch will SOFT-fail by default
			},
			{
				ID:    2,
				OrgID: testOrgID,
				UID:   "loki-uid",
				Type:  "loki",
				URL:   "http://127.0.0.1:1",
			},
			{
				ID:    3,
				OrgID: testOrgID,
				UID:   "mimir-uid",
				Type:  "mimir",
				URL:   "http://127.0.0.1:1",
			},
			{
				ID:    4,
				OrgID: testOrgID,
				UID:   "mysql-uid",
				Type:  "mysql",
				URL:   "http://127.0.0.1:1",
			},
		},
	}
}

// newTestService constructs a *Service pre-wired with fakes for use
// in Translate tests. The returned service is fully functional
// minus the LLM endpoint, which the caller supplies via the
// llmEndpoint argument (typically the URL of an httptest.NewServer
// configured for the test).
//
// Default fakes:
//   - cfg.NLQEnabled       = true (so the new ErrServiceDisabled
//     short-circuit in Translate does NOT
//     fire; tests that want to exercise
//     that path override cfg.NLQEnabled
//     to false).
//   - cfg.NLQProvider      = "openai"
//   - cfg.NLQEndpoint      = llmEndpoint (from the caller).
//   - cfg.NLQModel         = "gpt-4o-test"
//   - features             = WithFeatures(FlagNlqEnabled) (flag ON).
//   - ac                   = FakeAccessControl{ExpectedEvaluate: true}
//     (grants every permission).
//   - dsService            = seedDataSources() (Prometheus, Loki,
//     Mimir, and MySQL seeded so the
//     happy-path tests resolve cleanly).
//   - httpClient           = &http.Client{Timeout: 5 * time.Second}
//     (a short timeout sufficient for
//     httptest local connections; production
//     uses 30s via llmHTTPTimeout).
//   - pluginClient          = FakePluginClient configured to fail
//     every CallResource invocation (so
//     the live schema fetch SOFT-fails and
//     the happy-path tests observe the
//     expected Warnings entry).
//   - pluginContext         = fakePluginContextProvider returning a
//     minimal envelope; the FakePluginClient
//     above never inspects it.
//
// Tests that need different behavior (e.g., a fake dsService with
// no datasources, a denying access control, or a CallResource
// that succeeds with canned data) override the relevant field on
// the returned *Service.
func newTestService(t *testing.T, llmEndpoint string) *Service {
	t.Helper()

	cfg := setting.NewCfg()
	cfg.NLQEnabled = true
	cfg.NLQProvider = "openai"
	cfg.NLQEndpoint = llmEndpoint
	cfg.NLQModel = "gpt-4o-test"

	return &Service{
		cfg:           cfg,
		routeRegister: routing.NewRouteRegister(),
		dsService:     seedDataSources(),
		ac:            actest.FakeAccessControl{ExpectedEvaluate: true},
		features:      featuremgmt.WithFeatures(featuremgmt.FlagNlqEnabled),
		log:           log.New("test.nlq"),
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		// NLQ feature MAJOR fix (review feedback — CallResource
		// integration): default plugin client fails CallResource
		// so the happy-path tests observe the SOFT-fail Warnings
		// behavior that previously came from the unreachable URL
		// in seedDataSources. Tests that want a 2xx fetch construct
		// their own FakePluginClient locally.
		pluginClient: &pluginfakes.FakePluginClient{
			CallResourceHandlerFunc: failingCallResourceFunc,
		},
		pluginContext: &fakePluginContextProvider{},
	}
}

// TestProvideService_ConstructsWithoutError validates AAP §0.6.4
// row 5 (the "Service can be constructed when the feature is off"
// path) by invoking the Wire DI constructor (ProvideService)
// directly with the feature flag DISABLED. The constructor must
// return a non-nil *Service and nil error so the Wire graph
// continues to compile and run even when nlqEnabled is off.
func TestProvideService_ConstructsWithoutError(t *testing.T) {
	cfg := setting.NewCfg()
	cfg.NLQEnabled = false
	cfg.NLQProvider = "openai"
	cfg.NLQEndpoint = "https://api.openai.com/v1/chat/completions"
	cfg.NLQModel = "gpt-4o"

	rr := routing.NewRouteRegister()
	dsService := &dsfakes.FakeDataSourceService{}
	accessControl := actest.FakeAccessControl{ExpectedEvaluate: true}
	features := featuremgmt.WithFeatures()
	pluginClient := &pluginfakes.FakePluginClient{CallResourceHandlerFunc: failingCallResourceFunc}

	svc, err := ProvideService(cfg, rr, dsService, accessControl, features, pluginClient, nil)
	require.NoError(t, err, "ProvideService must not return an error when the feature flag is off")
	require.NotNil(t, svc, "ProvideService must return a non-nil *Service even when the feature flag is off")
}

// TestProvideService_FlagOff_DoesNotRegister covers one leg of the
// post-harmonization gate contract (NLQ feature MAJOR review
// finding — feature gate harmonization). With the nlqEnabled
// feature toggle OFF, the route MUST NOT be mounted regardless of
// the value of cfg.NLQEnabled. This makes the server byte-equivalent
// to baseline Grafana when the toggle is off.
func TestProvideService_FlagOff_DoesNotRegister(t *testing.T) {
	cfg := setting.NewCfg()
	cfg.NLQEnabled = true // cfg ON, but flag OFF — route MUST NOT register
	cfg.NLQEndpoint = "https://example.invalid"

	rr := routing.NewRouteRegister()
	features := featuremgmt.WithFeatures() // FlagNlqEnabled is OFF

	svc, err := ProvideService(
		cfg,
		rr,
		&dsfakes.FakeDataSourceService{},
		actest.FakeAccessControl{ExpectedEvaluate: true},
		features,
		&pluginfakes.FakePluginClient{CallResourceHandlerFunc: failingCallResourceFunc},
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, svc)

	// Inspect routes for the absence of /api/nlq/translate.
	require.False(t, routeRegistered(rr, "POST", "/api/nlq/translate"),
		"route MUST NOT be registered when the nlqEnabled feature toggle is off, even if cfg.NLQEnabled is true")
}

// TestProvideService_FlagOnCfgOff_RegistersRoute_AndHandlerReturnsServiceDisabled
// covers the harmonized-gate behavior introduced to address the
// MAJOR review finding (feature gate harmonization). When the
// feature toggle is ON but cfg.NLQEnabled is OFF, the route MUST
// be registered (so the frontend never sees a 404 when its
// nlqEnabled-gated bar is visible), and the handler MUST short-
// circuit with ErrServiceDisabled (HTTP 503) instead of doing any
// schema fetch or LLM work.
func TestProvideService_FlagOnCfgOff_RegistersRoute_AndHandlerReturnsServiceDisabled(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	cfg := setting.NewCfg()
	cfg.NLQEnabled = false // operator kill switch ON
	cfg.NLQEndpoint = "https://example.invalid"

	rr := routing.NewRouteRegister()
	features := featuremgmt.WithFeatures(featuremgmt.FlagNlqEnabled)

	svc, err := ProvideService(
		cfg,
		rr,
		seedDataSources(),
		actest.FakeAccessControl{ExpectedEvaluate: true},
		features,
		&pluginfakes.FakePluginClient{CallResourceHandlerFunc: failingCallResourceFunc},
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, svc)

	// Route IS registered — the frontend can render its bar and
	// will reach a structured 503 response, not a 404.
	require.True(t, routeRegistered(rr, "POST", "/api/nlq/translate"),
		"route MUST be registered when nlqEnabled feature toggle is on (regardless of cfg.NLQEnabled)")

	// Translate short-circuits with ErrServiceDisabled. The schema
	// fetch and the LLM call never run.
	_, err = svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "Show liveness",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrServiceDisabled,
		"the returned error MUST be classifiable as ErrServiceDisabled via errors.Is when cfg.NLQEnabled is false")
}

// TestProvideService_BothOn_Registers verifies that with BOTH
// gates enabled the route is in fact registered. This is the
// positive control for the two previous negative tests.
func TestProvideService_BothOn_Registers(t *testing.T) {
	cfg := setting.NewCfg()
	cfg.NLQEnabled = true
	cfg.NLQEndpoint = "https://example.invalid"

	rr := routing.NewRouteRegister()
	features := featuremgmt.WithFeatures(featuremgmt.FlagNlqEnabled)

	svc, err := ProvideService(
		cfg,
		rr,
		&dsfakes.FakeDataSourceService{},
		actest.FakeAccessControl{ExpectedEvaluate: true},
		features,
		&pluginfakes.FakePluginClient{CallResourceHandlerFunc: failingCallResourceFunc},
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, svc)

	require.True(t, routeRegistered(rr, "POST", "/api/nlq/translate"),
		"route MUST be registered when both cfg.NLQEnabled and the feature toggle are on")
}

// TestPostTranslate_ServiceDisabled_Returns503 exercises the
// handler-level path for the feature-gate-harmonization fix. When
// cfg.NLQEnabled is false, the handler MUST return HTTP 503 with
// a structured JSON body — never a 404, even though the operator
// has disabled the feature.
func TestPostTranslate_ServiceDisabled_Returns503(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	svc := newTestService(t, "")
	svc.cfg.NLQEnabled = false

	rr := newReqRecorder(t, svc, `{"input":"test","datasourceUid":"prom-uid","datasourceType":"prometheus"}`)

	require.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"handler MUST return 503 when [nlq] enabled=false (review feedback — feature gate harmonization)")
}

// TestTranslate_PrometheusSuccess validates AAP §0.6.4 row 1: the
// happy-path Prometheus translation. The test mocks the LLM
// provider via httptest and asserts both the outbound request
// shape (Authorization header, Content-Type header, OpenAI-format
// JSON body) and the inbound TranslateResponse (PromQL query,
// "promql" language identifier, non-empty explanation).
func TestTranslate_PrometheusSuccess(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer "+testAPIKey, r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload),
			"outbound LLM request body must be valid JSON")
		assert.Equal(t, "gpt-4o-test", payload["model"],
			"the cfg.NLQModel must be propagated into the LLM request body")
		messages, ok := payload["messages"].([]any)
		require.True(t, ok, "outbound LLM request body must contain a 'messages' array")
		require.GreaterOrEqual(t, len(messages), 2,
			"outbound LLM request must contain at least a system and a user message")

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
		NaturalLanguage: "Graph total API request rate by endpoint over the last 24 hours",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID, nil)

	require.NoError(t, err, "Translate must succeed on a well-formed Prometheus request")
	assert.Equal(t, "sum by (endpoint) (rate(http_requests_total[24h]))", resp.Query,
		"the translated query must match the LLM-returned PromQL")
	assert.Equal(t, "promql", resp.Language,
		"the response language must be 'promql' for a Prometheus datasource")
	assert.NotEmpty(t, resp.Explanation,
		"the response explanation must be populated when the LLM returns one")
	// Live schema fetch against the placeholder URL is expected to
	// SOFT-fail (the URL is unreachable), so a Warnings entry is
	// expected. This verifies the new hybrid HARD/SOFT contract.
	assert.NotEmpty(t, resp.Warnings,
		"Warnings MUST be present because the placeholder datasource URL is unreachable (SOFT failure)")
}

// TestTranslate_LokiSuccess validates AAP §0.6.4 row 2: the
// happy-path Loki translation.
func TestTranslate_LokiSuccess(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": `{"query": "{job=\"nginx\"} |= \"failed login\"", "explanation": "Failed login attempts"}`,
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	resp, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "Show me failed login attempts in the last hour grouped by IP",
		DatasourceUID:   "loki-uid",
		DatasourceType:  "loki",
	}, testOrgID, nil)

	require.NoError(t, err, "Translate must succeed on a well-formed Loki request")
	assert.Contains(t, resp.Query, "failed login",
		"the translated LogQL query must preserve the LLM-returned filter expression")
	assert.Equal(t, "logql", resp.Language,
		"the response language must be 'logql' for a Loki datasource")
}

// TestTranslate_UnsupportedDatasource validates AAP §0.6.4 row 9
// (backend side): an unsupported datasource type must short-
// circuit BEFORE any HTTP request reaches the LLM, returning
// ErrUnsupportedDatasource.
func TestTranslate_UnsupportedDatasource(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

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
	}, testOrgID, nil)

	require.Error(t, err, "Translate must return an error for an unsupported datasource type")
	assert.ErrorIs(t, err, ErrUnsupportedDatasource,
		"the returned error must be classifiable as ErrUnsupportedDatasource via errors.Is")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called for unsupported datasource types — see AAP §0.6.4 row 9 and §0.8.5")
}

// TestTranslate_MissingAPIKey validates AAP §0.6.4 row 10: when
// GF_NLQ_LLM_API_KEY is unset (or empty), Translate must return
// ErrMissingAPIKey WITHOUT issuing an HTTP request.
func TestTranslate_MissingAPIKey(t *testing.T) {
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
	}, testOrgID, nil)

	require.Error(t, err, "Translate must return an error when GF_NLQ_LLM_API_KEY is unset")
	assert.ErrorIs(t, err, ErrMissingAPIKey,
		"the returned error must be classifiable as ErrMissingAPIKey via errors.Is")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when the API key is missing — see AAP §0.8.5")
}

// TestTranslate_LiveSchemaFetchFailure_ContinuesWithWarning validates
// the NEW SOFT-failure semantics introduced to address the MAJOR
// review finding (schema_context.go L78-89 / L148-157 / L206-217 plus
// translate.go L306-318). When the datasource exists and its type
// matches the request, but the live metadata HTTP fetch fails, the
// translation MUST continue with the base hints and surface a
// Warnings entry — it MUST NOT fail.
func TestTranslate_LiveSchemaFetchFailure_ContinuesWithWarning(t *testing.T) {
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

	// seedDataSources gives prom-uid a URL of http://127.0.0.1:1 which
	// is unreachable — this triggers the SOFT-failure path inside
	// fetchLiveSchema. Type matches request claim so no HARD failure.
	svc := newTestService(t, mockLLM.URL)

	resp, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "Show up metric",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID, nil)

	require.NoError(t, err,
		"Translate MUST NOT fail when LIVE schema fetch fails — AAP §0.6.4 row 8 requires graceful degradation")
	assert.NotEmpty(t, resp.Query,
		"the LLM-returned query MUST still be produced when live schema fetch fails")
	assert.Equal(t, "up", resp.Query,
		"the query string MUST be exactly what the mocked LLM returned")
	assert.NotEmpty(t, resp.Warnings,
		"Warnings MUST contain at least one entry describing the live schema-fetch failure (AAP §0.6.4 row 8)")
}

// TestTranslate_DatasourceNotFound_HardFailure validates the new
// HARD-failure semantics introduced to address the MAJOR review
// finding (schema_context.go L133-145 + translate.go L306-318).
// When the datasource UID does not resolve, Translate MUST return
// ErrInvalidDatasource so PostTranslate maps it to 400. Crucially,
// NO LLM call may be issued.
func TestTranslate_DatasourceNotFound_HardFailure(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	var called bool
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer mockLLM.Close()

	// Build a service with an EMPTY FakeDataSourceService so the
	// UID lookup fails.
	svc := newTestService(t, mockLLM.URL)
	svc.dsService = &dsfakes.FakeDataSourceService{}

	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "valid input",
		DatasourceUID:   "unknown-uid",
		DatasourceType:  "prometheus",
	}, testOrgID, nil)

	require.Error(t, err, "Translate MUST return an error when the datasource UID does not resolve")
	assert.ErrorIs(t, err, ErrInvalidDatasource,
		"the returned error MUST be classifiable as ErrInvalidDatasource via errors.Is")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when the datasource UID does not resolve — see review finding L306-318")
}

// TestTranslate_TypeMismatch_HardFailure validates the new
// HARD-failure semantics for type mismatch (schema_context.go L133-145
// + translate.go L306-318). When the registered datasource type
// does NOT match the request's claimed type, Translate MUST return
// ErrInvalidDatasource. NO LLM call may be issued.
func TestTranslate_TypeMismatch_HardFailure(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	var called bool
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer mockLLM.Close()

	// mysql-uid is registered as a "mysql" datasource in the seed.
	// The request below claims it is a "prometheus" datasource —
	// this must HARD-fail.
	svc := newTestService(t, mockLLM.URL)

	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "valid input",
		DatasourceUID:   "mysql-uid",
		DatasourceType:  "prometheus",
	}, testOrgID, nil)

	require.Error(t, err, "Translate MUST return an error on UID/type mismatch")
	assert.ErrorIs(t, err, ErrInvalidDatasource,
		"the returned error MUST be classifiable as ErrInvalidDatasource via errors.Is")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when the registered type does not match the claimed type")
}

// TestTranslate_EmptyDatasourceUID_HardFailure validates the MAJOR
// review finding (translate.go L280-299): an empty or whitespace-
// only DatasourceUID MUST be rejected with ErrInvalidDatasource
// before any schema fetch or LLM call occurs.
func TestTranslate_EmptyDatasourceUID_HardFailure(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	var called bool
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)

	for _, tc := range []struct {
		name string
		uid  string
	}{
		{"empty string", ""},
		{"whitespace-only", "   "},
		{"tab-only", "\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Translate(context.Background(), TranslateRequest{
				NaturalLanguage: "valid input",
				DatasourceUID:   tc.uid,
				DatasourceType:  "prometheus",
			}, testOrgID, nil)

			require.Error(t, err, "Translate MUST reject empty/whitespace DatasourceUID")
			assert.ErrorIs(t, err, ErrInvalidDatasource,
				"the error MUST be classifiable as ErrInvalidDatasource for an empty/whitespace UID")
		})
	}

	assert.False(t, called,
		"LLM endpoint MUST NOT be called when DatasourceUID is empty or whitespace-only")
}

// TestTranslate_LLMReturnsError_ReturnsErrLLMUnavailable validates
// AAP §0.6.4 row 7.
func TestTranslate_LLMReturnsError_ReturnsErrLLMUnavailable(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "test query",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID, nil)

	require.Error(t, err, "Translate must return an error when the LLM returns a non-2xx status")
	assert.ErrorIs(t, err, ErrLLMUnavailable,
		"the returned error must be classifiable as ErrLLMUnavailable via errors.Is")
}

// TestTranslate_InvalidQuerySyntax_PromQL validates the NEW
// syntactic-validation step introduced to address the MAJOR review
// finding (translate.go L544-565). When the LLM returns a non-empty
// query that fails PromQL parsing, Translate MUST return
// ErrInvalidQuerySyntax so the client sees 502.
func TestTranslate_InvalidQuerySyntax_PromQL(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Malformed PromQL: missing closing brace.
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"query": "sum(http_requests_total{job=\"\"\")", "explanation": "broken"}`}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "broken query test",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID, nil)

	require.Error(t, err, "Translate MUST reject a syntactically invalid PromQL query")
	assert.ErrorIs(t, err, ErrInvalidQuerySyntax,
		"the returned error MUST be classifiable as ErrInvalidQuerySyntax via errors.Is")
}

// TestTranslate_InvalidQuerySyntax_LogQL validates the syntactic
// validation step for the LogQL dialect.
func TestTranslate_InvalidQuerySyntax_LogQL(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Malformed LogQL: empty {} selector is not permitted.
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"query": "{}", "explanation": "broken"}`}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	_, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "broken loki query",
		DatasourceUID:   "loki-uid",
		DatasourceType:  "loki",
	}, testOrgID, nil)

	require.Error(t, err, "Translate MUST reject a syntactically invalid LogQL query")
	assert.ErrorIs(t, err, ErrInvalidQuerySyntax,
		"the returned error MUST be classifiable as ErrInvalidQuerySyntax via errors.Is")
}

// TestValidateQuerySyntax_PromQL is a focused unit test on the
// validateQuerySyntax helper that bypasses the full Translate
// pipeline. This locks in the parser contract for future refactors.
func TestValidateQuerySyntax_PromQL(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		require.NoError(t, validateQuerySyntax(`sum by (endpoint) (rate(http_requests_total[5m]))`, "promql"))
	})
	t.Run("invalid", func(t *testing.T) {
		require.Error(t, validateQuerySyntax(`sum(`, "promql"))
	})
	t.Run("valid logql", func(t *testing.T) {
		require.NoError(t, validateQuerySyntax(`{job="nginx"} |= "error"`, "logql"))
	})
	t.Run("invalid logql", func(t *testing.T) {
		require.Error(t, validateQuerySyntax(`{}`, "logql"))
	})
	t.Run("empty input", func(t *testing.T) {
		require.Error(t, validateQuerySyntax("   ", "promql"),
			"whitespace-only input must fail validation")
	})
	t.Run("unknown language defaults to OK", func(t *testing.T) {
		// Defensive: if Translate were ever extended with a new
		// language without updating validateQuerySyntax, we choose
		// to pass rather than fail-closed; the existing
		// supportedLanguages map keeps this branch unreachable in
		// production.
		require.NoError(t, validateQuerySyntax(`anything`, "esoteric"))
	})
}

// TestSanitizeTransportError_DoesNotLeakURL is the MAJOR review
// finding fix (translate.go L469-474). It pins the contract that
// the URL component of a *url.Error is always stripped, leaving
// ONLY the operation name plus the inner error message.
func TestSanitizeTransportError_DoesNotLeakURL(t *testing.T) {
	// Build a *url.Error whose URL field contains a value a careless
	// log scraper might mistake for a credential — username:password
	// in userinfo plus a sensitive path.
	sensitive := "https://leakedUser:leakedPass@internal.example.com/secret-path?token=do-not-log"
	urlErr := mustParseTransportError(t, sensitive)

	got := sanitizeTransportError(urlErr)

	assert.NotContains(t, got, "leakedUser", "sanitized output must not contain userinfo")
	assert.NotContains(t, got, "leakedPass", "sanitized output must not contain userinfo password")
	assert.NotContains(t, got, "secret-path", "sanitized output must not contain URL path")
	assert.NotContains(t, got, "token=do-not-log", "sanitized output must not contain URL query")
	assert.NotContains(t, got, "internal.example.com", "sanitized output must not contain host")
	// The inner cause "synthetic transport error" SHOULD be present
	// — the sanitization strips the URL but keeps the diagnosable
	// inner cause.
	assert.Contains(t, got, "synthetic transport error",
		"sanitized output must include the inner error cause for diagnosability")
}

// TestSanitizeTransportError_NilSafe pins that the helper does not
// panic on a nil input.
func TestSanitizeTransportError_NilSafe(t *testing.T) {
	got := sanitizeTransportError(nil)
	assert.NotEmpty(t, got, "sanitize must return a non-empty sentinel for nil input")
}

// TestSanitizeTransportError_NonURLError pins that non-*url.Error
// values are returned as-is (they typically come from io.ReadAll
// on a successful response and do not contain URLs).
func TestSanitizeTransportError_NonURLError(t *testing.T) {
	got := sanitizeTransportError(fmt.Errorf("plain error"))
	assert.Equal(t, "plain error", got)
}

// TestTranslate_APIKeyNotLeaked is the canonical security test.
// AAP §0.8.5 mandates the API key MUST NEVER appear in any
// response body, error message, OR log line. The previous revision
// of this test inspected only the error string and the response
// JSON. This revision (NLQ feature MAJOR review finding,
// service_test.go L514-522 / L575-584) ALSO captures log output
// via a custom log sink AND exercises the full PostTranslate
// handler so the actual HTTP response body is observed.
func TestTranslate_APIKeyNotLeaked(t *testing.T) {
	const sensitiveKey = "super-secret-test-key-12345"
	t.Setenv("GF_NLQ_LLM_API_KEY", sensitiveKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "internal server error"}`))
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)

	// Capturing logger so every Debug/Info/Warn/Error call is
	// observable. The capturing logger swaps in the gokit-level
	// underlying writer — see newCapturingLogger for the wiring
	// details.
	cap := newCapturingLogger(t, "nlq-capture")
	svc.log = cap.ConcreteLogger

	// ----- Direct Translate path: error path 1 -----
	resp, err := svc.Translate(context.Background(), TranslateRequest{
		NaturalLanguage: "test query",
		DatasourceUID:   "prom-uid",
		DatasourceType:  "prometheus",
	}, testOrgID, nil)
	require.Error(t, err, "the test precondition requires the LLM call to fail")

	// Invariant 1: error message MUST NOT contain the key.
	assert.NotContains(t, err.Error(), sensitiveKey,
		"AAP §0.8.5 violated: API key leaked into error message: %v", err)

	// Invariant 2: response body MUST NOT contain the key.
	respBytes, marshalErr := json.Marshal(resp)
	require.NoError(t, marshalErr, "TranslateResponse must always be JSON-serialisable")
	assert.NotContains(t, string(respBytes), sensitiveKey,
		"AAP §0.8.5 violated: API key leaked into response body: %s", string(respBytes))

	// Invariant 3: log output MUST NOT contain the key (NEW
	// invariant added to address the MAJOR review finding).
	assert.NotContains(t, cap.String(), sensitiveKey,
		"AAP §0.8.5 violated: API key leaked into log output: %s", cap.String())

	// ----- Handler path: assert the wire-visible response body
	// produced by the actual response.Response serialization does
	// not contain the key either. We exercise PostTranslate
	// against an httptest harness that mirrors the real /api/nlq/
	// translate endpoint.
	cap.Reset()
	rr := newReqRecorder(t, svc, `{"input":"test","datasourceUid":"prom-uid","datasourceType":"prometheus"}`)
	assert.NotContains(t, rr.Body.String(), sensitiveKey,
		"AAP §0.8.5 violated: API key leaked into HTTP response body: %s", rr.Body.String())
	assert.NotContains(t, cap.String(), sensitiveKey,
		"AAP §0.8.5 violated: API key leaked into log output during handler-level call: %s", cap.String())
}

// TestTranslate_EmptyInput validates the input-validation contract.
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
	}, testOrgID, nil)

	require.Error(t, err, "Translate must return an error when NaturalLanguage is empty")
	assert.ErrorIs(t, err, ErrEmptyInput,
		"the returned error must be classifiable as ErrEmptyInput via errors.Is")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when the natural-language input is empty")
}

// TestTranslate_AcceptsMimirAsPrometheusCompatible validates AAP
// §0.6.3.1: Grafana Mimir is a Prometheus-API-compatible
// long-term-storage backend, so a Mimir datasource type must be
// accepted and produce a "promql" language identifier.
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
	}, testOrgID, nil)

	require.NoError(t, err, "Translate must accept 'mimir' as a Prometheus-compatible datasource type")
	assert.Equal(t, "promql", resp.Language,
		"the response language must be 'promql' for a Mimir datasource (AAP §0.6.3.1)")
}

// TestPostTranslate_UIDScopedAuthorization_Forbidden validates the
// CRITICAL review finding (service.go L291-294 → translate.go
// authorizeDatasourceQuery). When the AccessControl denies the
// caller's UID-scoped permission, the handler MUST return 403
// without invoking the LLM.
func TestPostTranslate_UIDScopedAuthorization_Forbidden(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	var called bool
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	// AccessControl denies every evaluation. The handler MUST
	// surface 403 without calling the LLM.
	svc.ac = actest.FakeAccessControl{ExpectedEvaluate: false}

	rr := newReqRecorder(t, svc, `{"input":"test","datasourceUid":"prom-uid","datasourceType":"prometheus"}`)

	require.Equal(t, http.StatusForbidden, rr.Code,
		"handler MUST return 403 when AccessControl denies the UID-scoped permission")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when authorization fails — see CRITICAL review finding")
}

// TestPostTranslate_EmptyUID_BadRequest validates the MAJOR review
// finding fix in PostTranslate. An empty UID MUST be rejected with
// 400 before any access-control evaluation or LLM call.
func TestPostTranslate_EmptyUID_BadRequest(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	var called bool
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	rr := newReqRecorder(t, svc, `{"input":"test","datasourceUid":"   ","datasourceType":"prometheus"}`)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"handler MUST return 400 when DatasourceUID is empty/whitespace-only")
	assert.False(t, called,
		"LLM endpoint MUST NOT be called when DatasourceUID is empty/whitespace-only")
}

// TestPostTranslate_HappyPath_ResponseSerialization exercises the
// full handler pipeline including the response.Response
// serialization that produces the wire-visible JSON body. This
// addresses the NLQ feature MAJOR review finding which required
// the no-leak invariant to be exercised against the handler-level
// response body, not just the in-process TranslateResponse struct.
func TestPostTranslate_HappyPath_ResponseSerialization(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"query": "up", "explanation": "Liveness check"}`}},
			},
		})
	}))
	defer mockLLM.Close()

	svc := newTestService(t, mockLLM.URL)
	rr := newReqRecorder(t, svc, `{"input":"Show liveness","datasourceUid":"prom-uid","datasourceType":"prometheus"}`)

	require.Equal(t, http.StatusOK, rr.Code,
		"handler MUST return 200 on a well-formed request")

	var body TranslateResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body),
		"the response body MUST be parseable as TranslateResponse JSON")
	assert.Equal(t, "up", body.Query)
	assert.Equal(t, "promql", body.Language)
}

// TestPostTranslate_MalformedJSON validates that a malformed request
// body is rejected with 400 — verifying web.Bind's error path is
// surfaced safely.
func TestPostTranslate_MalformedJSON(t *testing.T) {
	t.Setenv("GF_NLQ_LLM_API_KEY", testAPIKey)

	svc := newTestService(t, "")
	rr := newReqRecorder(t, svc, `not-a-json-body`)

	require.Equal(t, http.StatusBadRequest, rr.Code,
		"handler MUST return 400 on a malformed request body")
}

// -----------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------

// routeRegistered reports whether the given verb/path is registered
// in the supplied route register. Used by the dual-gate tests.
//
// IMPLEMENTATION: routing.RouteRegister does not expose direct
// inspection, so we materialize the registered routes by calling
// .Register against a recording routing.Router. The recorder
// captures each (method, pattern) pair so the test can scan for
// the specific route under test.
func routeRegistered(rr routing.RouteRegister, verb, path string) bool {
	rec := &recordingRouter{}
	rr.Register(rec)
	for _, r := range rec.routes {
		if r.method == verb && r.pattern == path {
			return true
		}
	}
	return false
}

// recordingRouter implements routing.Router and records every
// (method, pattern) pair passed to Handle/Get. Used by
// routeRegistered to inspect a RouteRegister's contents without
// running a real HTTP server.
type recordingRouter struct {
	routes []recordedRoute
}

type recordedRoute struct {
	method  string
	pattern string
}

func (r *recordingRouter) Handle(method, pattern string, handlers []web.Handler) {
	r.routes = append(r.routes, recordedRoute{method: method, pattern: pattern})
}

func (r *recordingRouter) Get(pattern string, handlers ...web.Handler) {
	r.routes = append(r.routes, recordedRoute{method: http.MethodGet, pattern: pattern})
}

// mustParseTransportError builds a real *net/url.Error that wraps
// the supplied sensitive URL string. We use the actual stdlib type
// so errors.As inside sanitizeTransportError unwraps correctly and
// the test exercises the production code path exactly.
func mustParseTransportError(t *testing.T, sensitiveURL string) error {
	t.Helper()
	return &url.Error{
		Op:  "Get",
		URL: sensitiveURL,
		Err: fmt.Errorf("synthetic transport error"),
	}
}

// newReqRecorder constructs a fake ReqContext, binds the supplied
// JSON body into the request, invokes PostTranslate, and writes
// the resulting response.Response into the returned recorder.
//
// The harness mirrors the real middleware chain at the call
// boundary that matters most for security review: web.Bind reads
// the body, the handler enforces UID-scoped auth, and the
// response.Response is serialized into a real HTTP wire body. The
// only piece omitted is the surrounding middleware.ReqSignedIn —
// the test always supplies a non-anonymous *user.SignedInUser so
// the authentication step is effectively pre-satisfied. Tests
// that need to assert the auth middleware's behavior should target
// the route registration in service.go directly.
func newReqRecorder(t *testing.T, svc *Service, body string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/nlq/translate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	signedIn := &user.SignedInUser{UserID: 1, OrgID: testOrgID, Login: "tester"}
	c := &contextmodel.ReqContext{
		Context: &web.Context{
			Req:  req,
			Resp: web.NewResponseWriter(req.Method, rec),
		},
		SignedInUser: signedIn,
		Logger:       log.New("test.nlq.ctx"),
	}

	resp := svc.PostTranslate(c)
	resp.WriteTo(c)
	return rec
}

// -----------------------------------------------------------------
// Capturing logger for AAP §0.8.5 log-line invariant assertions.
// -----------------------------------------------------------------

// capturingLogger wraps a *log.ConcreteLogger that has been re-
// wired to write into an in-memory buffer. Tests use this to assert
// that no Debug/Info/Warn/Error call references a secret value.
//
// The implementation works by:
//  1. Constructing a real *log.ConcreteLogger via log.New (named
//     so it does not collide with the rest of the test suite).
//  2. Building a gokit-level logfmt logger backed by a sync-
//     wrapped bytes.Buffer.
//  3. Swapping the ConcreteLogger's underlying gokit writer to
//     our buffer via the *ConcreteLogger.SwapLogger field's Swap
//     method (re-exported by the embedded gokitlog.SwapLogger).
type capturingLogger struct {
	*log.ConcreteLogger
	mu  sync.Mutex
	buf *bytes.Buffer
}

// newCapturingLogger constructs a capturingLogger named with the
// supplied identifier (visible in the captured output as the
// "logger" key) and returns it ready for use.
func newCapturingLogger(t *testing.T, name string) *capturingLogger {
	t.Helper()
	concrete := log.New(name)
	buf := &bytes.Buffer{}
	c := &capturingLogger{
		ConcreteLogger: concrete,
		buf:            buf,
	}
	// gokitlog.NewLogfmtLogger writes one line per Log call;
	// NewSyncWriter wraps the buffer with a mutex so concurrent
	// log calls do not interleave bytes.
	logger := gokitlog.NewLogfmtLogger(gokitlog.NewSyncWriter(&safeBuffer{mu: &c.mu, buf: buf}))
	concrete.Swap(logger)
	return c
}

// String returns the captured log contents up to this call. Tests
// use this to assert on the absence (or presence) of substrings.
func (c *capturingLogger) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Reset clears the captured log buffer. Used between phases of a
// multi-step test (e.g., before exercising the handler path after
// already exercising the Translate path).
func (c *capturingLogger) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

// safeBuffer is a tiny adapter so gokit's sync writer wraps a
// shared mutex with the capturingLogger's other state (String /
// Reset). Without this we would need two mutexes (one for the
// gokit writer and one for the buffer); a single mutex is
// simpler and avoids deadlock risks.
type safeBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
