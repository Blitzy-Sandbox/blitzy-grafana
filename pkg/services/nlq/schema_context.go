// schema_context.go provides the datasource-metadata bridge between
// the NLQ translation orchestrator (translate.go's Translate) and
// Grafana's existing datasources.DataSourceService + plugin runtime.
//
// The single exported behavior in this file is the unexported method
// (*Service).fetchSchemaContext, which the orchestrator calls to enrich
// the LLM prompt with datasource-specific hints (label names, metric
// names, stream selectors).
//
// FAILURE SEMANTICS — CRITICAL (NLQ feature MAJOR review finding):
//
// Two distinct error classes are returned from fetchSchemaContext and
// the orchestrator MUST distinguish them:
//
//  1. HARD failures — wrapped in ErrInvalidDatasource:
//     - The datasource UID does not resolve to a known datasource
//       within the caller's org.
//     - The registered datasource type does not match the claimed
//       type in the request body (e.g. caller submits a MySQL UID
//       with claimed type "prometheus").
//     Translate converts these into 400 Bad Request and never
//     reaches the LLM. This prevents an attacker from triggering
//     LLM work against an inconsistent datasource identity.
//
//  2. SOFT failures — any other error (live metadata fetch over
//     CallResource failed for any reason — DNS, plugin error, 5xx,
//     malformed JSON, etc.).
//     Translate downgrades these to a Warnings entry on the
//     response and continues with the base hints. The translation
//     still produces a usable result; the LLM simply lacks the
//     full real-time label/metric vocabulary.
//
// SCOPE — LIVE METADATA FETCH (NLQ feature MAJOR review finding,
// AAP §0.1.1 schema grounding + §0.4.3.6 integration requirements):
// This file performs a best-effort live metadata fetch against the
// datasource backend via the canonical Grafana CallResource RPC.
// CallResource routes the call through the full plugin runtime
// transport chain:
//
//   - secureSocksProxy egress
//   - TLS configuration (CA bundle, client certs, skip verify)
//   - HTTP client middleware (auth headers, OAuth identity
//     forwarding, custom headers, basic-auth credentials —
//     INCLUDING credentials stored in SecureJsonData)
//   - the plugin's CallResource implementation
//
// Endpoint mapping (matches the upstream contracts that the
// Prometheus and Loki backends implement):
//
//   - Prometheus / Mimir:
//     Path "api/v1/labels"                        — label names
//     Path "api/v1/label/__name__/values"         — metric names
//     (see pkg/promlib/resource/resource.go for the canonical
//     proxy registration of these paths.)
//   - Loki:
//     Path "loki/api/v1/labels"                   — log-stream
//     label names
//     (see pkg/tsdb/loki/api.go for the resource handler.)
//
// SECURITY (AAP §0.8.5):
//   - The plugin context construction reads decrypted
//     SecureJsonData via plugincontext.GetWithDataSource; the
//     resulting backend.PluginContext is opaque to this file —
//     no credential value is touched, read, logged, or returned.
//   - The CallResource sender accumulates ONLY the response body
//     up to schemaContextMaxBytes; status codes are inspected but
//     not used to populate error messages.
//   - The full request URL is not constructed in this file. The
//     plugin runtime constructs it from ds.URL and the path
//     suffix passed here; only the host (via safeHost on ds.URL)
//     is logged in structured fields, never the full URL.

package nlq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/grafana/grafana/pkg/apimachinery/identity"
	"github.com/grafana/grafana/pkg/services/datasources"
)

// schemaContextMaxBytes caps the response body accumulated for any
// live metadata fetch. The Prometheus /api/v1/labels endpoint
// typically returns a few KiB; this ceiling defends against a
// misbehaving upstream returning an unbounded body.
const schemaContextMaxBytes = 1 << 20 // 1 MiB

// schemaContextMaxLabels caps the number of labels/metrics/streams
// included in the SchemaContext. The LLM prompt has a finite token
// budget; including the entire label catalogue of a large
// Prometheus deployment would consume the budget and waste tokens
// on rarely-used labels. The cap is high enough to include the
// canonical set (~50-100) while bounding worst-case prompt size.
const schemaContextMaxLabels = 64

// fetchSchemaContext retrieves datasource metadata and constructs a
// SchemaContext to enrich the LLM prompt.
//
// Algorithm:
//
//  1. Look up the datasource by UID + OrgID via the injected
//     datasources.DataSourceService. A lookup failure is HARD and
//     short-circuits to ErrInvalidDatasource (400 Bad Request).
//  2. Verify the registered datasource type matches the request's
//     claim. A mismatch is HARD and short-circuits to
//     ErrInvalidDatasource (400 Bad Request).
//  3. Build the base (static) hints for the resolved type — these
//     are the deterministic fallback used by the LLM prompt when
//     the live fetch fails or returns nothing useful.
//  4. Attempt the live metadata fetch via CallResource. On success,
//     merge the live results into the base hints (deduplicated,
//     capped). On failure, return the base hints alongside a SOFT
//     error so Translate can attach a Warnings entry.
//
// Parameters:
//   - ctx:   request-scoped context propagated into the datasource
//     lookup AND the CallResource call so a client cancellation
//     aborts cleanly.
//   - req:   the validated TranslateRequest. DatasourceUID and
//     DatasourceType are read from this struct; NaturalLanguage is
//     intentionally NOT consumed here.
//   - orgID: the organization scope for the datasource lookup. The
//     caller (PostTranslate in translate.go) MUST derive this from
//     c.GetOrgID() — never from the request body — so a client
//     cannot spoof another organization's datasource.
//   - user:  the authenticated caller's identity envelope. Forwarded
//     into plugin-context construction so the plugin runtime can
//     attach the caller's identity to any downstream auth checks
//     and (for OAuth-forwarding datasources) the upstream HTTP
//     request. May be nil for service-mode callers.
//
// Returns:
//   - On HARD failure: (SchemaContext{}, error wrapping
//     ErrInvalidDatasource). Translate maps this to 400.
//   - On SOFT failure: (baseSchemaContext, error NOT wrapping
//     ErrInvalidDatasource). Translate downgrades to a Warning.
//   - On success: (mergedSchemaContext, nil). The merged context
//     contains the base hints supplemented by any successful live
//     fetch results.
func (s *Service) fetchSchemaContext(ctx context.Context, req TranslateRequest, orgID int64, user identity.Requester) (SchemaContext, error) {
	// Step 1: Verify the datasource exists and is accessible by this org.
	//
	// SECURITY (AAP §0.8.5): only the datasource UID and orgID are
	// logged. The DatasourceUID is a non-secret identifier; the
	// orgID is a public identifier within the tenant; the err is
	// produced by the datasources package and contains no
	// NLQ-specific secrets (no API key, no prompt, no LLM
	// response). The NaturalLanguage field of req is intentionally
	// NOT logged so user-supplied input does not leak into logs.
	ds, err := s.dsService.GetDataSource(ctx, &datasources.GetDataSourceQuery{
		UID:   req.DatasourceUID,
		OrgID: orgID,
	})
	if err != nil {
		s.log.Debug("NLQ feature: datasource lookup failed (hard error)",
			"datasourceUID", req.DatasourceUID,
			"orgID", orgID,
			"err", err,
		)
		// NLQ feature MAJOR fix: wrap with ErrInvalidDatasource so
		// the orchestrator can map this to 400 Bad Request rather
		// than downgrading to a warning. An unresolvable UID is a
		// client error, not an upstream service hiccup.
		return SchemaContext{}, fmt.Errorf("%w: datasource lookup failed for UID %q", ErrInvalidDatasource, req.DatasourceUID)
	}

	// Step 2: Verify the registered datasource type matches the request's claim.
	//
	// This is a defense-in-depth check: Translate already validates
	// the claimed type is in the supported set; here we ensure the
	// UID actually resolves to a datasource of that type. Without
	// this check, a caller could submit a MySQL UID alongside
	// claimed type "prometheus" and coax a PromQL translation that
	// would later silently fail when executed.
	claimedType := strings.ToLower(strings.TrimSpace(req.DatasourceType))
	registeredType := strings.ToLower(strings.TrimSpace(ds.Type))
	if !typesMatch(claimedType, registeredType) {
		s.log.Debug("NLQ feature: datasource type mismatch (hard error)",
			"datasourceUID", req.DatasourceUID,
			"claimedType", claimedType,
			"registeredType", registeredType,
		)
		// NLQ feature MAJOR fix: wrap with ErrInvalidDatasource. The
		// error message echoes only client-supplied or non-secret
		// values (UID, claimed type, registered type).
		return SchemaContext{}, fmt.Errorf(
			"%w: datasource type mismatch — request claims %q but UID %q is registered as %q",
			ErrInvalidDatasource, claimedType, req.DatasourceUID, registeredType,
		)
	}

	// Step 3: Build the base (static) schema context. This is the
	// deterministic fallback used when the live fetch fails AND
	// the seed corpus that the live fetch augments on success.
	base := buildBaseSchemaContext(claimedType)

	// Step 4: Attempt the live metadata fetch via CallResource. On
	// any failure here, return the base hints alongside a SOFT
	// error so Translate downgrades it to a warning.
	live, fetchErr := s.fetchLiveSchema(ctx, ds, claimedType, user)
	if fetchErr != nil {
		s.log.Debug("NLQ feature: live schema metadata fetch failed (soft error)",
			"datasourceUID", req.DatasourceUID,
			"datasourceType", claimedType,
			"err", fetchErr,
		)
		// SOFT failure: return the BASE hints (NOT empty) so the
		// LLM still has a usable vocabulary, and propagate the
		// error WITHOUT wrapping ErrInvalidDatasource. Translate
		// will see this as a non-ErrInvalidDatasource error and
		// add a Warnings entry instead of failing the request.
		return base, fmt.Errorf("live metadata fetch failed: %w", fetchErr)
	}

	// Merge the live results into the base hints. The merge is
	// dedup-aware: a label that appears in both lists is included
	// only once. The base hints come FIRST so they appear early in
	// the LLM prompt (priming the LLM toward the canonical
	// vocabulary), then the live additions follow. The total is
	// capped at schemaContextMaxLabels to bound prompt size.
	return mergeSchemaContexts(base, live), nil
}

// fetchLiveSchema issues CallResource RPCs against the datasource's
// plugin backend to retrieve real label names (and metric names for
// Prometheus).
//
// CallResource routes the call through the full plugin runtime
// transport chain (secureSocksProxy, TLS client-cert auth, OAuth
// identity forwarding, custom headers, basic-auth and SecureJsonData
// credential handling), so deployments that depend on those features
// see their datasource requests respected — unlike the prior direct
// HTTP implementation which bypassed every plugin middleware.
//
// dsType is expected to be already normalized (TrimSpace + ToLower)
// and to be one of "prometheus", "mimir", or "loki". The plugin ID
// passed to GetWithDataSource is ds.Type (the registered plugin
// identifier on the datasource record) — this is correct because
// Mimir datasources in Grafana are typically registered against the
// "prometheus" plugin while exposing the Mimir-compatible endpoint.
//
// SECURITY (AAP §0.8.5):
//   - The plugin context envelope is opaque; no credential value
//     touched by this function.
//   - The CallResource sender accumulates response bytes up to
//     schemaContextMaxBytes and ignores everything beyond.
//   - The full upstream URL is never constructed in this file; the
//     plugin runtime composes it from ds.URL + path suffix.
//   - Errors returned to the caller contain only the path and
//     status code; never the body or any credential material.
//
// Returns:
//   - (live SchemaContext, nil) on success. live contains whatever
//     was successfully retrieved; partial successes (labels OK,
//     metrics fail) are accepted — only label retrieval is required
//     for the result to be considered useful.
//   - (SchemaContext{}, error) on hard fetch failure. The error is
//     suitable for inclusion in a SOFT-failure Warnings entry.
func (s *Service) fetchLiveSchema(ctx context.Context, ds *datasources.DataSource, dsType string, user identity.Requester) (SchemaContext, error) {
	if ds == nil {
		return SchemaContext{}, errors.New("nil datasource")
	}
	if s.pluginClient == nil || s.pluginContext == nil {
		// Defense-in-depth: a unit-test fixture that did not wire
		// the plugin client / plugin-context provider falls through
		// to base hints rather than panicking.
		return SchemaContext{}, errors.New("plugin runtime not configured")
	}

	var live SchemaContext

	switch dsType {
	case "prometheus", "mimir":
		// Fetch label names from "api/v1/labels". This is the
		// canonical Prometheus label-catalogue endpoint (see
		// pkg/promlib/resource/resource.go which proxies the same
		// upstream contract).
		labels, err := s.callDatasourceResource(ctx, ds, user, "api/v1/labels")
		if err != nil {
			return SchemaContext{}, fmt.Errorf("prometheus labels fetch: %w", err)
		}
		live.Labels = capStringSlice(labels, schemaContextMaxLabels)

		// Fetch metric names from "api/v1/label/__name__/values".
		// This is a best-effort enrichment: if it fails, we still
		// return the labels (so the caller can decide whether the
		// partial result is useful).
		if metrics, err := s.callDatasourceResource(ctx, ds, user, "api/v1/label/__name__/values"); err == nil {
			live.Metrics = capStringSlice(metrics, schemaContextMaxLabels)
		} else {
			// Log at debug only; the labels result is still useful.
			s.log.Debug("NLQ feature: prometheus metric names fetch failed (partial success)",
				"host", safeHost(ds.URL),
				"err", err,
			)
		}
		return live, nil

	case "loki":
		// Fetch label names from "loki/api/v1/labels". See
		// pkg/tsdb/loki/api.go which serves the same contract.
		labels, err := s.callDatasourceResource(ctx, ds, user, "loki/api/v1/labels")
		if err != nil {
			return SchemaContext{}, fmt.Errorf("loki labels fetch: %w", err)
		}
		live.Labels = capStringSlice(labels, schemaContextMaxLabels)
		// Loki does not expose a metric-name catalogue (LogQL
		// queries operate on log streams, not metrics), so no
		// equivalent metrics fetch is performed.
		return live, nil

	default:
		// Defense-in-depth: should never trigger because the
		// caller has already validated the type. Return an empty
		// SchemaContext rather than an error so the caller can
		// proceed gracefully.
		return SchemaContext{}, nil
	}
}

// callDatasourceResource issues a GET CallResource RPC at the
// supplied path against the datasource's plugin backend and parses
// the response body as the canonical Prometheus / Loki labels
// envelope: `{"status":"success","data":["label1","label2",...]}`.
//
// This shape is shared between Prometheus's /api/v1/labels endpoint
// and Loki's /loki/api/v1/labels endpoint (Loki copies the
// Prometheus response envelope), so a single helper covers both.
//
// SECURITY (AAP §0.8.5):
//   - The CallResource RPC carries no NLQ-specific secrets — the
//     LLM API key is read only inside translate.go's callLLM.
//   - Plugin-context construction uses GetWithDataSource which
//     attaches decrypted SecureJsonData (when permitted) to the
//     context envelope. The decrypted values flow through the
//     plugin runtime; this function never reads them directly.
//   - Errors returned are bounded to the path identifier and the
//     status code; never the response body content.
//   - Response bodies are accumulated up to schemaContextMaxBytes
//     to defend against unbounded upstream responses.
//
// Returns:
//   - On success: the data slice from the response envelope
//     (potentially empty).
//   - On failure: an error suitable for inclusion in a
//     SOFT-failure Warnings entry. The error message contains the
//     resource path and an upstream status code class — no
//     response body and no credential material.
func (s *Service) callDatasourceResource(ctx context.Context, ds *datasources.DataSource, user identity.Requester, path string) ([]string, error) {
	if s.pluginClient == nil || s.pluginContext == nil {
		return nil, errors.New("plugin runtime not configured")
	}

	// Build the plugin context envelope. GetWithDataSource resolves
	// the plugin record, attaches the datasource instance settings
	// (including decrypted SecureJsonData), and includes the caller's
	// identity. ds.Type is the registered plugin identifier; for
	// Mimir-on-Prometheus deployments this is "prometheus" and the
	// upstream endpoint is configured on ds.URL.
	pCtx, err := s.pluginContext.GetWithDataSource(ctx, ds.Type, user, ds)
	if err != nil {
		// SECURITY: the error from GetWithDataSource contains no
		// credentials (it indexes by plugin ID); pass through.
		return nil, fmt.Errorf("plugin context: %w", err)
	}

	req := &backend.CallResourceRequest{
		PluginContext: pCtx,
		Path:          path,
		Method:        http.MethodGet,
		URL:           path,
		Headers: map[string][]string{
			"Accept": {"application/json"},
		},
	}

	// Accumulate the response body up to schemaContextMaxBytes.
	// The plugin runtime may stream the response across multiple
	// CallResourceResponse callbacks; we concatenate the Body
	// slices and capture the first observed StatusCode. The cap
	// is enforced inline so a runaway upstream cannot drive
	// unbounded memory growth.
	var (
		collected  []byte
		statusCode int
	)
	sender := backend.CallResourceResponseSenderFunc(func(r *backend.CallResourceResponse) error {
		if r == nil {
			return nil
		}
		if statusCode == 0 {
			statusCode = r.Status
		}
		if len(r.Body) == 0 {
			return nil
		}
		remaining := schemaContextMaxBytes - len(collected)
		if remaining <= 0 {
			// Already at the cap; drop further chunks.
			return nil
		}
		body := r.Body
		if len(body) > remaining {
			body = body[:remaining]
		}
		collected = append(collected, body...)
		return nil
	})

	if err := s.pluginClient.CallResource(ctx, req, sender); err != nil {
		// SECURITY: the plugin runtime's error message describes
		// the plugin / path / RPC class — never the response body
		// or credential material. We safely pass it through.
		return nil, fmt.Errorf("call resource %q: %w", path, err)
	}

	if statusCode != 0 && (statusCode < 200 || statusCode >= 300) {
		// SECURITY: only the status code is included in the error.
		// The response body of an error response from a third-party
		// upstream may contain sensitive operator information.
		return nil, fmt.Errorf("upstream resource %q returned status %d", path, statusCode)
	}

	if len(collected) == 0 {
		// Empty body on a 2xx is treated as "no data". Return an
		// empty slice rather than an error so the caller can
		// proceed with base hints.
		return nil, nil
	}

	// Canonical Prometheus / Loki response envelope:
	//   {"status":"success","data":["label1","label2",...]}
	var envelope struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(collected, &envelope); err != nil {
		return nil, fmt.Errorf("decode response for %q: %w", path, err)
	}
	if envelope.Status != "" && envelope.Status != "success" {
		return nil, fmt.Errorf("upstream resource %q reported status %q", path, envelope.Status)
	}
	return envelope.Data, nil
}

// typesMatch reports whether the request's claimed datasource type
// matches the registered datasource type. Both arguments are expected
// to have already been normalized by the caller (TrimSpace + ToLower).
//
// Mimir compatibility (AAP §0.6.3.1):
// Grafana Mimir is a Prometheus-API-compatible long-term-storage
// backend; queries against a Mimir datasource use PromQL. The NLQ
// feature therefore treats {prometheus, mimir} as interchangeable
// when matching the registered type to the claimed type. This means:
//
//   - claimed "prometheus" + registered "mimir"  -> match
//   - claimed "mimir"      + registered "prometheus" -> match
//   - claimed "prometheus" + registered "prometheus" -> match (exact)
//   - claimed "mimir"      + registered "mimir"      -> match (exact)
//   - claimed "loki"       + registered "loki"       -> match (exact)
//   - any other combination                          -> NO match
//
// The function is a free helper rather than a method on Service
// because it has no dependency on the service struct's fields and
// can therefore be exercised directly in unit tests without
// constructing a Service.
func typesMatch(claimed, registered string) bool {
	// Fast path: exact match (the overwhelmingly common case in
	// production).
	if claimed == registered {
		return true
	}
	// Cross-match: Mimir <-> Prometheus interoperability.
	if (claimed == "prometheus" && registered == "mimir") ||
		(claimed == "mimir" && registered == "prometheus") {
		return true
	}
	return false
}

// buildBaseSchemaContext returns a SchemaContext seeded with common
// label, metric, and stream identifiers for the given datasource type.
//
// The lists are general-purpose hints whose role is to anchor the
// LLM's output to recognisable PromQL or LogQL constructs. They are
// intentionally short to keep the LLM prompt token count low. They
// remain in use both as the base seed (merged with the live fetch)
// AND as the standalone fallback when the live fetch fails.
//
// Determinism and purity:
// The function is pure and deterministic — its output depends only on
// its input, it reads no environment, it issues no I/O, and it
// allocates fresh slices each call so the caller may mutate them
// without affecting later invocations.
//
// dsType is expected to be already normalized by the caller
// (TrimSpace + ToLower).
func buildBaseSchemaContext(dsType string) SchemaContext {
	switch dsType {
	case "prometheus", "mimir":
		// Prometheus-flavored hints. Labels include the canonical
		// __name__ pseudo-label (Prometheus's metric-name selector)
		// plus the Kubernetes / cloud-native labels that appear in
		// nearly every Prometheus deployment. Metrics list the most
		// common HTTP, process, and runtime metrics that show up in
		// nearly all instrumentation.
		return SchemaContext{
			Labels: []string{
				"__name__", "instance", "job", "endpoint", "method",
				"code", "status", "handler", "namespace", "pod",
				"container", "service",
			},
			Metrics: []string{
				"http_requests_total",
				"http_request_duration_seconds",
				"process_cpu_seconds_total",
				"process_resident_memory_bytes",
				"up",
				"go_goroutines",
			},
		}
	case "loki":
		// Loki-flavored hints. Labels are the common log-stream
		// label set; Streams are illustrative selector strings that
		// the LLM can use as a template when constructing its
		// {label="value"} stream selector.
		return SchemaContext{
			Labels: []string{
				"job", "namespace", "pod", "container", "filename",
				"host", "level", "service_name",
			},
			Streams: []string{
				`{job="varlogs"}`,
				`{job="nginx"}`,
				`{namespace="default"}`,
				`{container="app"}`,
			},
		}
	default:
		// Defense-in-depth fallback: if this function is somehow
		// invoked with an unsupported type (which should not happen
		// because Translate validates the type before fetchSchemaContext
		// is called), return an empty SchemaContext so the prompt
		// builder degrades gracefully to prompt-only translation.
		//
		// This branch deliberately does NOT panic, log, or return an
		// error — the function's contract is "return a SchemaContext",
		// and an empty one is a valid SchemaContext per the model
		// definition in models.go.
		return SchemaContext{}
	}
}

// mergeSchemaContexts combines a base SchemaContext (static, always
// non-empty for supported types) with a live SchemaContext (CallResource
// fetch result, may be empty for partial successes). The merge is:
//
//   - dedup-aware: a label/metric/stream present in both lists
//     appears only once in the output.
//   - order-preserving: base entries come first (anchoring the LLM
//     to canonical vocabulary), followed by live entries.
//   - cap-bounded: the merged list is truncated at
//     schemaContextMaxLabels to bound prompt size.
//
// The function is pure and deterministic. It allocates fresh slices
// so the caller may mutate the result without affecting subsequent
// invocations of buildBaseSchemaContext.
func mergeSchemaContexts(base, live SchemaContext) SchemaContext {
	return SchemaContext{
		Labels:  mergeStringSlicesUnique(base.Labels, live.Labels, schemaContextMaxLabels),
		Metrics: mergeStringSlicesUnique(base.Metrics, live.Metrics, schemaContextMaxLabels),
		Streams: mergeStringSlicesUnique(base.Streams, live.Streams, schemaContextMaxLabels),
	}
}

// mergeStringSlicesUnique returns a new slice combining a + b with
// duplicates removed, preserving the order of first occurrence
// (a's elements come first, then b's that are not already in a).
// The result is capped at the supplied limit.
//
// nil or empty inputs are handled gracefully — passing two empty
// slices returns an empty (non-nil) slice.
func mergeStringSlicesUnique(a, b []string, limit int) []string {
	if limit <= 0 {
		return []string{}
	}
	out := make([]string, 0, len(a)+len(b))
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) >= limit {
			return out
		}
	}
	for _, s := range b {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
		if len(out) >= limit {
			return out
		}
	}
	return out
}

// capStringSlice returns at most the first `limit` non-empty entries
// of s. nil/empty input returns an empty (non-nil) slice.
func capStringSlice(s []string, limit int) []string {
	if limit <= 0 {
		return []string{}
	}
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v == "" {
			continue
		}
		out = append(out, v)
		if len(out) >= limit {
			break
		}
	}
	return out
}
