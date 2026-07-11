// Package api contains types and server interfaces for the miroxy admin API.
//
// Source of truth: admin-openapi.yaml (OpenAPI 3.1 spec, same directory).
//
// Generated file: admin_api.gen.go — do not edit by hand.
// Regenerate after editing the spec:
//
//	make gen
//
// Requires oapi-codegen v2 installed:
//
//	go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
//
// Admin API summary (served on port 9001):
//
//	GET  /health                    liveness probe (no auth)
//	GET  /stat                      runtime stats + token usage
//	GET  /v1/config                 full effective config (keys masked)
//	GET  /v1/config/providers       resolved provider definitions
//	GET  /v1/config/keypools        keypools with masked keys
//	GET  /v1/config/routes          model routes + providers
//	POST /admin/reload              hot-reload config file
//	POST /admin/proxy/stop          stop proxy listener
//	POST /admin/proxy/start         start proxy listener
//
// Authentication: Authorization: Bearer <token> where token is any value from
// auth.allowed_keys, or the admin session token from POST /admin/login.
package api

// Code generation is managed via Makefile, not go:generate, to avoid requiring
// a pre-installed binary. Use:
//
//	make gen        regenerate admin_api.gen.go from admin-openapi.yaml
//	make gen-check  verify generated file is in sync (CI use)

// AdminAPIVersion is the version of the admin API spec embedded in this package.
// Updated whenever admin-openapi.yaml changes its info.version field.
const AdminAPIVersion = "1.0"
