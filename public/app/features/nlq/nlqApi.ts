// NLQ feature: thin HTTP client wrapper for the Natural Language Query backend.
//
// This module is the single point in the frontend NLQ feature that imports
// `getBackendSrv` from `@grafana/runtime`. Keeping it isolated from
// `useNLQTranslation.ts` makes the hook trivially testable with MSW (which
// intercepts the underlying `fetch` call) and follows the same separation of
// concerns used throughout the Grafana codebase (e.g. `profile/api.ts`,
// `annotations/api.ts`, `admin/api.ts`).
//
// Per AAP §0.2.1 this module deliberately uses `getBackendSrv()` rather than
// RTK Query because the endpoint is a single request/response with no caching
// benefit. Per AAP §0.8.5 the server-side LLM credential is NEVER transmitted
// from the frontend — it is held server-side only. The browser is
// authenticated by the standard Grafana session cookie that `getBackendSrv()`
// attaches automatically.

import { getBackendSrv } from '@grafana/runtime';

import { TranslateRequest, TranslateResponse } from './types';

/**
 * NLQ feature: the single backend endpoint consumed by the frontend NLQ module.
 *
 * This URL MUST match exactly the route registered by `pkg/services/nlq/service.go`
 * via `s.RouteRegister.Group("/api/nlq", ...).Post("/translate", ...)`. The backend
 * route is gated by `middleware.ReqSignedIn` and `ac.EvalPermission(datasources.ActionQuery)`,
 * so the caller must be authenticated and must hold the datasource-query permission
 * for the target datasource UID (AAP §0.4.3.2, §0.8.5).
 */
const NLQ_TRANSLATE_ENDPOINT = '/api/nlq/translate';

/**
 * Translates a natural-language question into the active datasource's query
 * language (PromQL for Prometheus/Mimir, LogQL for Loki) by calling the Grafana
 * backend NLQ service.
 *
 * Authentication is handled implicitly by `getBackendSrv()` (cookie-based session
 * attached by the browser). The server-side LLM credential is NEVER sent from
 * the frontend — it is held server-side only (per AAP §0.8.5). Callers
 * therefore do not need to provide any credentials.
 *
 * Errors (network failure, 4xx, 5xx) bubble up as a rejected promise. Callers
 * are expected to catch them and surface them through an `<Alert>` component
 * (per AAP §0.2.1 graceful degradation requirements). This function deliberately
 * does NOT swallow errors, log payloads to the console, retry, or apply any
 * resilience pattern (per AAP §0.8.1 minimal-change clause).
 *
 * @param req - the translation request DTO mirroring the Go `TranslateRequest`
 *   struct in `pkg/services/nlq/models.go`. The server validates that
 *   `input` is non-empty and that `datasourceType` is in the supported set
 *   (`prometheus`, `loki`), returning HTTP 400 otherwise.
 * @returns the translated query string plus the language token used by the
 *   `NLQQueryPreview` Monaco editor and any non-fatal warnings collected
 *   during schema-context fetching.
 * @throws rejects with the underlying `getBackendSrv()` error if the backend
 *   responds with a non-2xx status or the network call fails.
 */
export async function postTranslate(req: TranslateRequest): Promise<TranslateResponse> {
  return getBackendSrv().post<TranslateResponse>(NLQ_TRANSLATE_ENDPOINT, req);
}
