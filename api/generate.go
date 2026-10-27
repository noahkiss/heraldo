// Package api holds the Go types generated from schema/openapi.yaml and the
// envelope v1 JSON Schema. Regenerate with `go generate ./...`.
//
// oapi-codegen cannot follow the JSON Schema's $defs references, so
// internal/bundle inlines the schema into the OpenAPI document first.
package api

//go:generate go run ./internal/bundle -config oapi-codegen.yaml ../schema/openapi.yaml
