package docs

import "embed"

// Files contains the Swagger UI page and OpenAPI definition served by the API.
//go:embed index.html openapi.yaml
var Files embed.FS