// schema_context.go provides the datasource-metadata bridge between
// the NLQ translation orchestrator (translate.go's Translate) and
// Grafana's existing datasources.DataSourceService interface.
//
// The single exported behavior in this file is the unexported method
// (*Service).fetchSchemaContext, which the orchestrator calls to enrich
// the LLM prompt with datasource-specific hints (label names, metric
// names, stream selectors).
//
// FAILURE SEMANTICS — CRITICAL (AAP §0.6.1.1):
// Schema fetching is best-effort. Any failure here (datasource not found,
// permission denied for the org, claimed datasource type does not match
// the registered datasource type) is reported back to the caller as a
// non-fatal error which the orchestrator transforms into an entry in
// TranslateResponse.Warnings. The translation pipeline MUST continue with
// an empty SchemaContext rather than aborting; the LLM falls back to a
// prompt-only translation. This file is therefore architected to NEVER
// panic and to ALWAYS return a sensible value (even if just an empty
// SchemaContext) alongside the error.
//
// SCOPE — CRITICAL (AAP §0.7.2):
// This file deliberately does NOT call the datasource backend's
// /api/v1/labels resource endpoint. The datasources.DataSourceService
// interface does not expose CallResource; expanding the interface or
// adding a plugin-client dependency is out of scope for this change set.
// The schema hints in buildBaseSchemaContext are static seeds that anchor
// the LLM's output to recognisable PromQL/LogQL constructs. A future
// enhancement can replace these with a live CallResource fetch.

package nlq

import (
	"context"
	"fmt"
	"strings"

	"github.com/grafana/grafana/pkg/services/datasources"
)

// fetchSchemaContext retrieves datasource metadata and constructs a
// SchemaContext to enrich the LLM prompt. It uses the injected
// datasources.DataSourceService to look up the datasource by UID,
// verifies the caller's claimed datasource type matches the registered
// type (defense against a UID/type mismatch in the request body), and
// seeds the SchemaContext with common labels / metrics / streams for
// that datasource type.
//
// Parameters:
//   - ctx:   request-scoped context propagated into the datasource
//     lookup so a client cancellation aborts the I/O cleanly.
//   - req:   the validated TranslateRequest. DatasourceUID and
//     DatasourceType are read from this struct; NaturalLanguage is
//     intentionally NOT consumed here (no LLM call occurs in this
//     file).
//   - orgID: the organization scope for the datasource lookup. The
//     caller (PostTranslate in translate.go) MUST derive this from
//     c.SignedInUser.GetOrgID() — never from the request body — so a
//     client cannot spoof another organization's datasource by
//     submitting an arbitrary UID. orgID is part of Grafana's
//     multi-tenancy model and is required by GetDataSourceQuery
//     (see pkg/services/datasources/models.go).
//
// Returns:
//   - SchemaContext{} (zero value) on any error, suitable for direct
//     use by translate.go's buildPrompt (which treats a zero
//     SchemaContext as "no hints available").
//   - A populated SchemaContext on success, containing the static
//     base hints for the resolved datasource type.
//
// Error contract (AAP §0.6.1.1 — critical no-fail rule):
//
//	The error returned by this method is ALWAYS a warning-grade
//	error, NEVER a fatal one. The orchestrator in translate.go
//	must capture it, append a string to TranslateResponse.Warnings,
//	and continue with the empty SchemaContext. Returning an error
//	from this method MUST NOT result in an HTTP error response to
//	the client.
func (s *Service) fetchSchemaContext(ctx context.Context, req TranslateRequest, orgID int64) (SchemaContext, error) {
	// Step 1: Verify the datasource exists and is accessible by this org.
	//
	// The lookup uses UID + OrgID, which is the canonical GetDataSource
	// shape across the codebase (see, for example, the same call in
	// pkg/services/correlations/database.go's createCorrelation). The
	// underlying implementation enforces RBAC by org boundary; a UID
	// from a different org returns datasources.ErrDataSourceNotFound.
	ds, err := s.dsService.GetDataSource(ctx, &datasources.GetDataSourceQuery{
		UID:   req.DatasourceUID,
		OrgID: orgID,
	})
	if err != nil {
		// Log at debug level — a schema-context fetch failure is a
		// soft fallback path, not an operator-actionable alert. At
		// debug it remains diagnosable in development while staying
		// out of production logs by default.
		//
		// SECURITY (AAP §0.8.5): only the datasource UID and orgID are
		// logged. The DatasourceUID is a non-secret identifier; the
		// orgID is a public identifier within the tenant; the err is
		// produced by the datasources package and contains no
		// NLQ-specific secrets (no API key, no prompt, no LLM
		// response). The NaturalLanguage field of req is intentionally
		// NOT logged so user-supplied input does not leak into logs.
		s.log.Debug("NLQ feature: schema context fetch failed",
			"datasourceUID", req.DatasourceUID,
			"orgID", orgID,
			"err", err,
		)
		// Wrap the underlying error with %w so callers using
		// errors.Is can still detect, e.g., datasources.ErrDataSourceNotFound.
		// The wrapped message is generic and does not echo any
		// request body content beyond the UID (which the client
		// already knows).
		return SchemaContext{}, fmt.Errorf("datasource lookup failed: %w", err)
	}

	// Step 2: Verify the registered datasource type matches the request's claim.
	//
	// This is a defense-in-depth check, NOT a substitute for the
	// type validation that Translate performs on the request body
	// before calling this method. The earlier validation rejects
	// claimed types that are not in {prometheus, mimir, loki}; this
	// check ensures the UID actually resolves to a datasource of that
	// type — preventing a malicious or accidentally misconfigured
	// client from passing, say, a MySQL datasource's UID alongside
	// claimed type "prometheus" in an attempt to coax a PromQL
	// translation that would later silently fail when executed.
	//
	// Strings are normalized (TrimSpace + ToLower) before comparison
	// so trivial differences such as " Prometheus " versus
	// "prometheus" do not produce false mismatches.
	claimedType := strings.ToLower(strings.TrimSpace(req.DatasourceType))
	registeredType := strings.ToLower(strings.TrimSpace(ds.Type))
	if !typesMatch(claimedType, registeredType) {
		// SECURITY (AAP §0.8.5): the error message echoes only the
		// claimed type, the UID, and the registered type. None of
		// these are secrets — UIDs are client-supplied and types are
		// part of the public plugin catalogue. The error message
		// intentionally does NOT echo any other field of the
		// DataSource record (URL, BasicAuthUser, JsonData, etc.) which
		// could include configuration metadata that is not strictly
		// public.
		return SchemaContext{}, fmt.Errorf(
			"datasource type mismatch: request claims %q but UID %q is registered as %q",
			claimedType, req.DatasourceUID, registeredType,
		)
	}

	// Step 3: Build the schema context with common labels / metrics /
	// streams. The hints are deterministic and side-effect-free —
	// see buildBaseSchemaContext for the rationale on static seeds.
	//
	// Branching is on claimedType (already lowercased and trimmed)
	// rather than registeredType because the two are equivalent at
	// this point (typesMatch returned true) and using claimedType
	// keeps the dataflow symmetric with how Translate later selects
	// the response Language ("promql" vs "logql").
	return buildBaseSchemaContext(claimedType), nil
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
// intentionally short to keep the LLM prompt token count low. They are
// NOT an exhaustive enumeration of the labels and metrics in a user's
// deployment — they merely supply enough vocabulary for the LLM to
// produce a plausible first draft that the user can then refine in the
// editable CodeEditor preview.
//
// Why static and not live-fetched?
// A richer schema context would require invoking the datasource's
// /api/v1/labels (Prometheus) or /loki/api/v1/labels (Loki) resource
// endpoint via CallResource. The datasources.DataSourceService
// interface does not expose CallResource; obtaining it would require
// taking a dependency on the plugin client layer
// (pkg/services/pluginsintegration/...), which in turn would expand
// ProvideService's Wire signature and increase the blast radius of
// this change set beyond what the Minimal Change Clause (AAP §0.8.1)
// permits. A future enhancement, out of scope for this change, can
// replace the static seed with a live label fetch behind a new
// SchemaClient interface.
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
