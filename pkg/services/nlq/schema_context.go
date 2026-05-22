// schema_context.go provides the datasource-metadata bridge between
// the NLQ translation orchestrator (translate.go's Translate) and
// Grafana's existing datasources.DataSourceService interface.
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
//     HTTP failed for any reason — DNS, 5xx, timeout, malformed
//     JSON, etc.).
//     Translate downgrades these to a Warnings entry on the
//     response and continues with the base hints. The translation
//     still produces a usable result; the LLM simply lacks the
//     full real-time label/metric vocabulary.
//
// SCOPE — LIVE METADATA FETCH (NLQ feature MAJOR review finding,
// AAP §0.1.1 schema grounding requirement):
// This file performs a best-effort live HTTP call against the
// datasource backend to retrieve real label names (and Prometheus
// metric names) using the same upstream API contracts that the
// existing Prometheus and Loki backends already consume:
//
//   - Prometheus / Mimir: GET <ds.URL>/api/v1/labels and
//     GET <ds.URL>/api/v1/label/__name__/values
//   - Loki:               GET <ds.URL>/loki/api/v1/labels
//
// The HTTP client used is the same httpClient already injected on
// the Service (constructed in service.go with a 30s timeout). The
// call propagates BasicAuth credentials from the datasource record
// when ds.BasicAuth is true. SECURITY: ds.BasicAuthPassword is read
// at call time and never logged or returned in error messages.
//
// This is intentionally lighter-weight than CallResource — it does
// not invoke the plugin client layer (which would require a Wire
// dependency expansion contrary to AAP §0.8.1's Minimal Change
// Clause). The cost is that secureSocksProxy, TLS client-cert
// auth, and other advanced datasource transport features are not
// honored; deployments that rely on those will simply fall back to
// the base hints via the SOFT failure path.

package nlq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/grafana/grafana/pkg/services/datasources"
)

// schemaFetchTimeoutNote captures the Service-level httpClient
// timeout that bounds every live metadata HTTP call. Documented
// here for traceability — the actual timeout is set in
// service.go's ProvideService where the httpClient is constructed.

// schemaContextMaxBytes caps the response body read for any live
// metadata fetch. The Prometheus /api/v1/labels endpoint typically
// returns a few KiB; this ceiling defends against a misbehaving
// upstream returning an unbounded body.
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
//  4. Attempt the live metadata fetch over HTTP. On success, merge
//     the live results into the base hints (deduplicated, capped).
//     On failure, return the base hints alongside a SOFT error so
//     Translate can attach a Warnings entry.
//
// Parameters:
//   - ctx:   request-scoped context propagated into the datasource
//     lookup AND the live HTTP fetch so a client cancellation
//     aborts cleanly.
//   - req:   the validated TranslateRequest. DatasourceUID and
//     DatasourceType are read from this struct; NaturalLanguage is
//     intentionally NOT consumed here.
//   - orgID: the organization scope for the datasource lookup. The
//     caller (PostTranslate in translate.go) MUST derive this from
//     c.GetOrgID() — never from the request body — so a client
//     cannot spoof another organization's datasource.
//
// Returns:
//   - On HARD failure: (SchemaContext{}, error wrapping
//     ErrInvalidDatasource). Translate maps this to 400.
//   - On SOFT failure: (baseSchemaContext, error NOT wrapping
//     ErrInvalidDatasource). Translate downgrades to a Warning.
//   - On success: (mergedSchemaContext, nil). The merged context
//     contains the base hints supplemented by any successful live
//     fetch results.
func (s *Service) fetchSchemaContext(ctx context.Context, req TranslateRequest, orgID int64) (SchemaContext, error) {
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

	// Step 4: Attempt the live metadata fetch. On any failure here,
	// return the base hints alongside a SOFT error so Translate
	// downgrades it to a warning.
	live, fetchErr := s.fetchLiveSchema(ctx, ds, claimedType)
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

// fetchLiveSchema issues HTTP calls against the datasource backend
// to retrieve real label names (and metric names for Prometheus).
//
// SECURITY (AAP §0.8.5):
//   - The HTTP request uses ds.URL composed with a fixed path
//     suffix; no caller-supplied content is embedded in the URL.
//   - When ds.BasicAuth is true, ds.BasicAuthPassword is read
//     directly into the http.Request's BasicAuth field and never
//     logged. The local variable falls out of scope when the
//     function returns.
//   - Transport errors are returned to the caller; only sanitized
//     fields appear in the log lines emitted here.
//   - The function NEVER logs ds.URL in full (only the host via
//     safeHost) to avoid leaking embedded credentials from
//     pathological URL configurations.
//
// dsType is expected to be already normalized (TrimSpace + ToLower)
// and to be one of "prometheus", "mimir", or "loki".
//
// Returns:
//   - (live SchemaContext, nil) on success. live contains whatever
//     was successfully retrieved; partial successes (labels OK,
//     metrics fail) are accepted — only label retrieval is required
//     for the result to be considered useful.
//   - (SchemaContext{}, error) on hard fetch failure (DNS, 5xx,
//     malformed JSON, etc.). The error is suitable for inclusion
//     in a SOFT-failure Warnings entry.
func (s *Service) fetchLiveSchema(ctx context.Context, ds *datasources.DataSource, dsType string) (SchemaContext, error) {
	if ds == nil {
		return SchemaContext{}, errors.New("nil datasource")
	}
	baseURL := strings.TrimRight(ds.URL, "/")
	if baseURL == "" {
		// Without a base URL we cannot issue any live call. Return
		// an error so the caller surfaces a Warnings entry.
		return SchemaContext{}, errors.New("datasource URL is empty")
	}

	var live SchemaContext

	switch dsType {
	case "prometheus", "mimir":
		// Fetch label names from /api/v1/labels. This is the
		// canonical Prometheus label-catalogue endpoint (see
		// pkg/promlib/resource/resource.go which proxies the same
		// upstream contract).
		labels, err := s.fetchPromLabelsResource(ctx, ds, baseURL+"/api/v1/labels")
		if err != nil {
			return SchemaContext{}, fmt.Errorf("prometheus labels fetch: %w", err)
		}
		live.Labels = capStringSlice(labels, schemaContextMaxLabels)

		// Fetch metric names from /api/v1/label/__name__/values.
		// This is a best-effort enrichment: if it fails, we still
		// return the labels (so the caller can decide whether the
		// partial result is useful).
		if metrics, err := s.fetchPromLabelsResource(ctx, ds, baseURL+"/api/v1/label/__name__/values"); err == nil {
			live.Metrics = capStringSlice(metrics, schemaContextMaxLabels)
		} else {
			// Log at debug only; the labels result is still useful.
			s.log.Debug("NLQ feature: prometheus metric names fetch failed (partial success)",
				"host", safeHost(baseURL),
				"err", err,
			)
		}
		return live, nil

	case "loki":
		// Fetch label names from /loki/api/v1/labels. See
		// pkg/tsdb/loki/api.go which proxies the same upstream
		// contract.
		labels, err := s.fetchPromLabelsResource(ctx, ds, baseURL+"/loki/api/v1/labels")
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

// fetchPromLabelsResource issues a GET against the supplied URL
// (which the caller has already constructed as <ds.URL><path>),
// parses the response body as the canonical Prometheus
// "labelValuesResponse" shape — `{"status":"success","data":[...]}` —
// and returns the data slice.
//
// This shape is shared between Prometheus's /api/v1/labels endpoint
// and Loki's /loki/api/v1/labels endpoint (Loki copies the
// Prometheus response envelope), so a single helper covers both.
//
// SECURITY (AAP §0.8.5):
//   - The full request URL is intentionally NOT returned in error
//     messages. Errors echo only the response status code and a
//     generic class identifier; the host is logged via safeHost.
//   - BasicAuth credentials, when configured on the datasource,
//     are attached to the request via the standard library's
//     BasicAuth method and NEVER appear in any returned error
//     message or log line emitted by this function.
//   - The response body read is bounded by schemaContextMaxBytes
//     to defend against unbounded upstream responses.
func (s *Service) fetchPromLabelsResource(ctx context.Context, ds *datasources.DataSource, fullURL string) ([]string, error) {
	if s.httpClient == nil {
		return nil, errors.New("nil http client")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		// SECURITY: NewRequestWithContext errors mention the URL
		// in their message. Replace with a sanitized identifier.
		return nil, fmt.Errorf("build request: %s", sanitizeTransportError(err))
	}
	httpReq.Header.Set("Accept", "application/json")
	// Attach BasicAuth credentials if the datasource is so
	// configured. ds.BasicAuthPassword is read here and never
	// stored on the Service; it leaves scope when this function
	// returns.
	if ds.BasicAuth {
		// In some Grafana deployments the BasicAuth password is
		// stored in SecureJsonData rather than the BasicAuthPassword
		// field. We do not attempt to decrypt SecureJsonData here
		// (that requires the secrets service), so deployments that
		// store credentials there will fall back to the SOFT
		// failure path via 401 from the upstream. This is by design:
		// expanding the dependency surface to include the secrets
		// service exceeds the Minimal Change Clause budget.
		httpReq.SetBasicAuth(ds.BasicAuthUser, ds.BasicAuthPassword)
	}

	httpResp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("transport: %s", sanitizeTransportError(err))
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// SECURITY: only the status code is included in the error.
		// The response body of an error response from a third-party
		// upstream may contain sensitive operator information.
		return nil, fmt.Errorf("upstream returned status %d", httpResp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, schemaContextMaxBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %s", sanitizeTransportError(err))
	}

	// Canonical Prometheus / Loki response envelope:
	//   {"status":"success","data":["label1","label2",...]}
	var envelope struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if envelope.Status != "" && envelope.Status != "success" {
		return nil, fmt.Errorf("upstream reported status %q", envelope.Status)
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
// non-empty for supported types) with a live SchemaContext (HTTP
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
