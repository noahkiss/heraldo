// Package schema embeds the envelope v1 JSON Schema and the OpenAPI document,
// so the server validates input against the same files the docs describe.
package schema

import _ "embed"

// Envelope is envelope.v1.schema.json.
//
//go:embed envelope.v1.schema.json
var Envelope []byte

// EnvelopeID is the schema's $id.
const EnvelopeID = "https://github.com/noahkiss/heraldo/schema/envelope.v1.schema.json"
