// Package adminspec embeds the admin API contract, so that a transport
// built on it (the MCP endpoint) reads its operations, parameters and
// descriptions from the same document the generated code comes from.
package adminspec

import _ "embed"

// OpenAPI is api/admin/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
