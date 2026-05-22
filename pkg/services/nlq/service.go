// service.go is the entry point of the Natural Language Query (NLQ)
// translation service. It declares:
//
//  1. The Service struct, which holds the injected dependencies that
//     the translation pipeline (translate.go) and the schema-context
//     fetcher (schema_context.go) operate on.
//  2. The ProvideService Wire DI constructor referenced by
//     pkg/server/wire.go's wireBasicSet. The constructor instantiates
//     the named logger ("nlq"), constructs the shared *http.Client used
//     for outbound LLM calls, and self-registers the HTTP endpoint
//     POST /api/nlq/translate when the nlqEnabled feature flag is on.
//  3. The unexported registerAPIEndpoints method that mounts the
//     POST /api/nlq/translate route under the existing RouteRegister
//     with the standard authentication (middleware.ReqSignedIn) and
//     authorization (ac.EvalPermission(datasources.ActionQuery))
//     middleware chain.
//
// The file is the "glue" that ties together models.go, translate.go,
// and schema_context.go into a single registered domain service. It
// intentionally contains NO translation logic, NO HTTP body parsing,
// and NO LLM-provider integration — those concerns live in translate.go.
//
// IMPLEMENTATION NOTES (AAP §0.8.5 — security):
//   - The Service struct deliberately has NO field that could hold the
//     LLM API key. The key is read at translate-time from the environment
//     variable GF_NLQ_LLM_API_KEY inside translate.go's callLLM, and is
//     never persisted on the struct, in the cfg, or in any log statement.
//   - The named logger "nlq" produced by log.New("nlq") MUST NEVER
//     receive the API key as a value. This invariant is enforced
//     contextually in translate.go, but it begins here, with the
//     deliberate absence of the field on the struct.
//
// CHANGE TRACEABILITY (AAP §0.8.6):
//   - This entire file is part of the NLQ feature. Every declaration
//     in it is new code introduced by the NLQ change set; no edits to
//     pre-existing code are required.

package nlq

import (
	"context"
	"net/http"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/grafana/grafana/pkg/api/routing"
	"github.com/grafana/grafana/pkg/apimachinery/identity"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/middleware"
	"github.com/grafana/grafana/pkg/plugins"
	ac "github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/datasources"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/pluginsintegration/plugincontext"
	"github.com/grafana/grafana/pkg/setting"
)

// pluginContextProvider is the narrow interface the NLQ service
// requires from the plugin-context subsystem. Declared here (not
// imported from pluginsintegration/plugincontext) so unit tests
// can supply a lightweight fake without dragging in the full
// concrete provider's dependency chain.
//
// The single method matches the signature of
// plugincontext.Provider.GetWithDataSource verbatim so the concrete
// type is assignable to this interface without any adapter.
//
// SECURITY (AAP §0.8.5): the implementation MUST NEVER receive or
// produce values containing the LLM API key. The signature deliberately
// has no API-key parameter or return.
type pluginContextProvider interface {
	GetWithDataSource(ctx context.Context, pluginID string, user identity.Requester, ds *datasources.DataSource) (backend.PluginContext, error)
}

// llmHTTPTimeout caps the duration of any single outbound LLM provider
// HTTP request. 30 seconds matches the agent-prompt specification and
// is conservative enough to allow large-prompt completions on the
// slowest supported providers while preventing a stuck goroutine from
// outliving a typical user request lifecycle.
//
// The translate.go callLLM also calls http.NewRequestWithContext with
// the request-scoped context, so a client disconnect cancels the call
// even sooner than the timeout would.
const llmHTTPTimeout = 30 * time.Second

// Service is the NLQ domain service. It owns the translation pipeline
// (natural language -> PromQL/LogQL) and self-registers its HTTP
// endpoint when the nlqEnabled feature flag is on.
//
// Lifetime: a single instance constructed by Wire DI at server startup;
// shared across all HTTP requests. There is no per-request state on
// this struct.
//
// Thread safety: all fields are read-only after construction.
//   - cfg is treated as read-only; Grafana convention is that
//     *setting.Cfg may be reloaded by the settings package itself but
//     downstream consumers see a coherent snapshot per-request.
//   - routeRegister is used exactly once, at construction time, inside
//     registerAPIEndpoints. After that it is dormant.
//   - dsService, ac, features, and log are concurrency-safe by Grafana
//     convention (they back interface methods whose implementations
//     hold no per-call state on the Service).
//   - httpClient is safe for concurrent use per net/http docs:
//     "Clients are safe for concurrent use by multiple goroutines."
//
// NO API key field (AAP §0.8.5): the LLM API key is intentionally
// absent from this struct. translate.go's callLLM reads it from the
// environment at the point of use and discards it after the request
// is constructed. This minimises the surface area on which the secret
// can leak (no field reachable by reflection, no field written to logs,
// no field exposed to handlers).
type Service struct {
	// cfg supplies the NLQ-specific configuration block parsed by
	// pkg/setting/setting.go's readNLQSettings: NLQEnabled,
	// NLQProvider, NLQEndpoint, NLQModel. It is also read at
	// translate-time so that a settings reload (when supported) is
	// picked up without a service restart.
	cfg *setting.Cfg

	// routeRegister is the Grafana HTTP routing tree used to mount
	// POST /api/nlq/translate from registerAPIEndpoints. Provided
	// by the existing Wire graph; never modified after the call to
	// Group.
	routeRegister routing.RouteRegister

	// dsService is the datasources.DataSourceService interface used
	// by schema_context.go's fetchSchemaContext to verify the
	// datasource UID resolves to a registered datasource of the
	// claimed type in the caller's org. The interface is wire-bound
	// to the existing datasources package implementation; this
	// service never persists datasources, only reads them.
	dsService datasources.DataSourceService

	// ac is the AccessControl interface bound to ac.Middleware in
	// registerAPIEndpoints to enforce datasources:query permission
	// on POST /api/nlq/translate. The field name "ac" matches the
	// import alias used throughout the package, avoiding a name
	// clash with the legacy AccessControl type identifier in the
	// pre-Wire era.
	ac ac.AccessControl

	// features is consulted EXACTLY once, in ProvideService, to
	// decide whether to register the HTTP endpoint. After construction
	// the field is not read again — the per-request flag check happens
	// implicitly via "the route is not registered when the flag is
	// off", which gives the same observable behavior with one fewer
	// branch on the hot path.
	features featuremgmt.FeatureToggles

	// log is the named structured logger produced by log.New("nlq"),
	// following the pattern set by pkg/services/correlations/correlations.go.
	// SECURITY: log.With/Debug/Info/Warn/Error calls in this package
	// MUST NEVER include the LLM API key value. The "logger=nlq" key
	// added by log.New is harmless and aids correlation in production.
	log log.Logger

	// httpClient is the shared *http.Client used by translate.go's
	// callLLM for outbound LLM provider requests. A single shared
	// client enables connection reuse (per net/http best practice)
	// and lets the operator set a single ceiling timeout
	// (llmHTTPTimeout) for every LLM call. translate.go always uses
	// http.NewRequestWithContext so request-scoped context
	// cancellation propagates as a secondary cancellation signal.
	httpClient *http.Client

	// pluginClient is the in-process plugin runtime gateway used by
	// schema_context.go's fetchLiveSchema to retrieve real datasource
	// metadata (label names, metric names, log-stream label names)
	// via the canonical CallResource RPC.
	//
	// Why CallResource instead of a direct HTTP call:
	// The plugin client routes the request through the full datasource
	// transport chain — secureSocksProxy, TLS client-cert auth,
	// OAuth identity forwarding, custom headers, basic-auth (including
	// SecureJsonData-stored credentials), TLS configuration, plugin
	// middleware, and request-context propagation. A direct
	// http.Client.Do bypasses every one of these features and breaks
	// in deployments that rely on them (NLQ feature MAJOR review
	// finding — schema_context.go integration).
	//
	// Wire-bound to *backend.MiddlewareHandler at
	// pkg/services/pluginsintegration/pluginsintegration.go:L153
	// via `wire.Bind(new(plugins.Client), new(*backend.MiddlewareHandler))`.
	pluginClient plugins.Client

	// pluginContext provides datasource-aware backend.PluginContext
	// values consumed by every CallResource call. Built by
	// GetWithDataSource which resolves the plugin record, attaches
	// the datasource instance settings (including decrypted
	// SecureJsonData when permitted), and constructs the user
	// identity envelope.
	//
	// Provided concretely as *plugincontext.Provider by Wire DI;
	// stored here through the narrow pluginContextProvider interface
	// so the NLQ service unit tests can substitute a lightweight
	// fake without dragging in the full plugincontext concrete
	// dependency chain (pluginstore, datasource cache, secrets,
	// plugin settings cache).
	pluginContext pluginContextProvider
}

// ProvideService is the Wire DI constructor for the NLQ service.
//
// Wire integration: this provider is appended to wireBasicSet in
// pkg/server/wire.go by a separate change in the NLQ feature scope.
// The signature MUST remain stable so the generated wire_gen.go
// continues to compile across regenerations. If a new dependency is
// added in a future change, prefer extending this signature over
// reaching into a global or modifying an unrelated provider.
//
// Construction order:
//  1. Build the Service struct with the injected dependencies in
//     their canonical field order (cfg, routeRegister, dsService,
//     ac, features, log, httpClient, pluginClient, pluginContext).
//  2. Instantiate the named logger via log.New("nlq") — matches the
//     pattern in pkg/services/correlations/correlations.go.
//  3. Instantiate the shared HTTP client with the package-level
//     llmHTTPTimeout. The client is reused across all requests for
//     connection pooling.
//  4. Register the HTTP route when the nlqEnabled feature toggle is
//     enabled globally (AAP §0.6.1.1). When the toggle is off, no
//     /api/nlq/* route is mounted on the server; this makes the
//     deployment exactly equivalent to baseline Grafana, satisfying
//     the Minimal Change Clause (AAP §0.8.1).
//  5. Return (s, nil). Per AAP §0.6.1.1 test case (a), the
//     constructed Service is always non-nil — even when the flag is
//     disabled — so dependents that hold a *Service reference can
//     rely on it being usable for non-HTTP callers (currently none,
//     but reserved for future extension).
//
// Feature gate harmonization (review feedback MAJOR finding):
// The previous design required BOTH cfg.NLQEnabled AND the
// nlqEnabled feature toggle for route registration. The frontend,
// however, gates only on the feature toggle. With a toggle-on /
// cfg-off configuration the panel editor rendered the NLQ bar but
// /api/nlq/translate returned 404 — a confusing user experience.
//
// The corrected design registers the route whenever the feature
// toggle is enabled, regardless of cfg.NLQEnabled. The cfg.NLQEnabled
// gate is now consulted inside the handler: when it is false,
// PostTranslate / Translate short-circuit with ErrServiceDisabled
// (HTTP 503 Service Unavailable) — a clear, localizable signal that
// the frontend can render through its existing Alert surface. This
// matches the resolution guidance in the review findings:
// "register the route whenever the frontend flag can render and have
// the handler return a localized disabled response when
// `[nlq] enabled=false`. Ensure config comments, backend behavior,
// and frontend behavior match."
//
// Errors: this constructor returns no error today. The error return
// is retained for forward compatibility with Wire's expectation of
// a "(value, error)" provider signature. Returning nil here keeps
// the Wire graph trivially error-free; any future failure mode (for
// example, a misconfigured endpoint URL) should be reported through
// this return rather than through panics.
//
// This function is called exactly once per server lifetime.
func ProvideService(
	cfg *setting.Cfg,
	routeRegister routing.RouteRegister,
	dsService datasources.DataSourceService,
	accessControl ac.AccessControl,
	features featuremgmt.FeatureToggles,
	pluginClient plugins.Client,
	pluginCtx *plugincontext.Provider,
) (*Service, error) {
	// Construct the Service in one literal so the field bindings stay
	// adjacent to the struct definition and are easy to audit during
	// review. No conditional field is set after this point.
	s := &Service{
		cfg:           cfg,
		routeRegister: routeRegister,
		dsService:     dsService,
		ac:            accessControl,
		features:      features,
		log:           log.New("nlq"), // NLQ feature: named logger; never receives the LLM API key.
		httpClient: &http.Client{
			// NLQ feature: 30s cap; translate.go also passes
			// request-scoped contexts via http.NewRequestWithContext
			// so client disconnects propagate.
			Timeout: llmHTTPTimeout,
		},
		// NLQ feature: plugin client + plugin-context provider drive
		// the live schema-metadata fetch through the canonical
		// Grafana datasource transport chain (CallResource). See the
		// schema_context.go file comment and the field-level comments
		// above for the security and integration rationale.
		pluginClient: pluginClient,
	}

	// NLQ feature: avoid the Go "nil interface trap" when a caller
	// (e.g. an integration test) passes a nil *plugincontext.Provider.
	// Assigning a typed-nil pointer to the pluginContextProvider
	// interface field would yield a non-nil interface value whose
	// dynamic type is *plugincontext.Provider and whose dynamic value
	// is nil. Subsequent `s.pluginContext == nil` checks in
	// schema_context.go would then evaluate to FALSE and a downstream
	// method call would panic. By collapsing a nil concrete pointer to
	// a nil interface here, we make `s.pluginContext == nil` behave
	// intuitively for both production (concrete provider supplied by
	// Wire) and test (caller passes nil to opt out of live schema
	// fetch) call sites. The defensive nil-check in
	// schema_context.go's fetchLiveSchema then steers the schema fetch
	// into the SOFT-failure path that returns base hints + a Warnings
	// entry — exactly the behavior the integration tests assert
	// against.
	if pluginCtx != nil {
		s.pluginContext = pluginCtx
	}

	// NLQ feature (review feedback MAJOR — feature gate harmonization):
	// Register the HTTP route whenever the nlqEnabled feature toggle
	// is enabled globally. The cfg.NLQEnabled (operator kill switch)
	// gate is enforced inside the handler via ErrServiceDisabled
	// (HTTP 503), giving the frontend a clear localized error to
	// render without ever exposing a 404 to a user who sees the bar.
	//
	// When the feature toggle is OFF, no /api/nlq/* route is mounted —
	// this ensures the server is exactly equivalent to baseline
	// Grafana when the feature is disabled, satisfying the Minimal
	// Change Clause (AAP §0.8.1) and the off-by-default requirement
	// (AAP §0.1.1).
	//
	// We use IsEnabledGlobally because nlqEnabled is an operator-
	// controlled, server-wide flag (per AAP §0.6.3.1); per-tenant
	// gating is not in scope for this release.
	//
	// The deprecation warning on IsEnabledGlobally recommends migrating
	// to OpenFeature; the NLQ feature follows the established Grafana
	// pattern for now (matching pkg/services/ldap/service/ldap.go and
	// dozens of other call sites). A future migration to OpenFeature
	// is out of scope for this change set per AAP §0.7.2.4.
	if s.routeRegistrationEnabled() {
		s.registerAPIEndpoints()
	}

	return s, nil
}

// routeRegistrationEnabled reports whether the nlqEnabled feature
// toggle is on. Extracted from ProvideService as a tiny helper so the
// gate contract is easy to audit and easy to unit-test in isolation.
// The result is snapshot-read at construction time; route registration
// never changes after server startup so re-reading the gates per
// request would not change behavior.
//
// NOTE (review feedback MAJOR — feature gate harmonization):
// cfg.NLQEnabled is deliberately NOT consulted here. That gate is now
// enforced inside the handler (Translate → ErrServiceDisabled → 503)
// so the frontend, which is gated only on the feature toggle, never
// encounters a 404 when the operator has cfg-disabled the feature
// while leaving the toggle on. See the ProvideService docstring for
// the full rationale.
//
//nolint:staticcheck // not yet migrated to OpenFeature
func (s *Service) routeRegistrationEnabled() bool {
	if s.features == nil {
		return false
	}
	return s.features.IsEnabledGlobally(featuremgmt.FlagNlqEnabled)
}

// registerAPIEndpoints mounts POST /api/nlq/translate under the
// existing route register. Authentication is enforced via
// middleware.ReqSignedIn (the same authentication chain used by every
// other Grafana endpoint that requires a signed-in user). UID-scoped
// authorization for datasources.ActionQuery is performed INSIDE the
// handler (PostTranslate in translate.go), AFTER the request body has
// been parsed, because the target datasource UID lives in the JSON
// body and is not available to route-level middleware.
//
// SECURITY (AAP §0.8.5 + review feedback CRITICAL finding):
// Earlier revisions of this method attached an authorize() middleware
// at the route level evaluating ac.EvalPermission(datasources.ActionQuery)
// WITHOUT a scope. That allowed a caller with any datasources:query
// permission to reach the handler for a UID they should not query.
// The corrected design moves the evaluation inline into the handler
// so we can pass the body-supplied UID to
// ac.EvalPermission(ActionQuery, ScopeProvider.GetResourceScopeUID(req.DatasourceUID)),
// enforcing per-datasource authorization. See translate.go's
// authorizeDatasourceQuery for the implementation.
//
// Pattern source: pkg/services/correlations/api.go's
// registerAPIEndpoints (lines 16-32) for the Group shape;
// pkg/services/ngalert/accesscontrol/rules.go for the UID-scoped
// EvalPermission pattern.
//
// Why grouped under /api/nlq even with a single route?
//   - Future extension (e.g. /api/nlq/feedback, /api/nlq/history)
//     can attach inside the same closure without further edits.
//   - The trailing middleware.ReqSignedIn applies uniformly across
//     the group, so a future addition cannot accidentally skip auth.
//   - Visual symmetry with the established correlations pattern
//     simplifies code review.
//
// Why datasources.ActionQuery and not a new NLQ-specific permission?
//   - AAP §0.6.1.1 and §0.7.2 forbid introducing new permission
//     constants in this change set.
//   - The NLQ feature is "natural-language-to-datasource-query"; the
//     logical permission is identical to issuing a datasource query
//     directly.
//
// Side effects:
//   - This method calls s.routeRegister.Group exactly once. The
//     RouteRegister implementation is append-only, so calling it
//     after the registration phase has closed would panic. In
//     practice it is always called from ProvideService at server
//     startup, before Run.
func (s *Service) registerAPIEndpoints() {
	// NLQ feature: register the translation endpoint. Routes inside
	// the Group inherit the trailing middleware.ReqSignedIn so an
	// unauthenticated caller is rejected before any handler logic
	// runs. UID-scoped datasources:query authorization is performed
	// inside the handler (PostTranslate) after the request body has
	// been bound — this is required to evaluate the permission
	// against the body-supplied DatasourceUID. See the function-level
	// comment for the security rationale.
	s.routeRegister.Group("/api/nlq", func(nlqRoute routing.RouteRegister) {
		// NLQ feature: POST /api/nlq/translate handler binding.
		// routing.Wrap adapts s.PostTranslate (declared in
		// translate.go) from the response.Response-returning
		// Grafana convention into the underlying web.Handler shape.
		nlqRoute.Post("/translate", routing.Wrap(s.PostTranslate))
	}, middleware.ReqSignedIn)
}
