package api

// NLQ feature: thin DTO mirror file for Swagger generation; handler is in
// pkg/services/nlq/translate.go. Route registration is in
// pkg/services/nlq/service.go via RouteRegister.Group("/api/nlq", ...).
//
// The types declared here exist solely to attach Swagger documentation to
// the POST /api/nlq/translate endpoint. They are NOT used by any runtime
// code — only by the go-swagger documentation generator, which scans
// pkg/api/*.go for "// swagger:..." directives and instantiates the
// wrapper structs via reflection in order to derive the OpenAPI schema.
//
// IMPORTANT: per AAP §0.8 Minimal Change Clause, this file MUST NOT:
//   - declare a method on *HTTPServer
//   - register any HTTP route
//   - add a field to the HTTPServer struct
//   - import anything from pkg/server/ or modify pkg/api/api.go
//
// The thin-DTO-mirror approach was chosen because every alternative
// (adding a field to HTTPServer, looking up the service through a
// global registry, or extending ProvideHTTPServer's signature) would
// violate either AAP §0.8.1 (Minimal Change Clause) or §0.8.4
// (Stability Contracts). See AAP §0.6.1.2 and the agent prompt for
// the full rationale.

import (
	"github.com/grafana/grafana/pkg/services/nlq"
)

// swagger:route POST /nlq/translate nlq translateNLQ
//
// Translate a natural-language question into a PromQL or LogQL query.
//
// Translates a plain-English natural-language input into a syntactically
// valid query for the active panel datasource. Supports Prometheus (PromQL)
// and Loki (LogQL) datasources. The endpoint requires the caller to have
// the `datasources:query` permission against the target datasource.
//
// The handler is registered by the NLQ service at startup (see
// pkg/services/nlq/service.go) — this directive only documents the
// existing endpoint for OpenAPI consumers.
//
// Responses:
// 200: nlqTranslateResponse
// 400: badRequestError
// 401: unauthorisedError
// 403: forbiddenError
// 500: internalServerError
// 502: internalServerError

// swagger:parameters translateNLQ
type NLQTranslateParams struct {
	// The natural-language translation request.
	// in:body
	// required:true
	Body nlq.TranslateRequest `json:"body"`
}

// swagger:response nlqTranslateResponse
type NLQTranslateResponse struct {
	// The translated query and metadata.
	// in: body
	Body nlq.TranslateResponse `json:"body"`
}
