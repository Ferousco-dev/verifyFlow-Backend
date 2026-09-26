package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSwaggerDocs(t *testing.T) {
	handler := New(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	request := httptest.NewRequest(http.MethodGet, "/docs", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPermanentRedirect || response.Header().Get("Location") != "/docs/" {
		t.Fatalf("GET /docs: status %d, location %q", response.Code, response.Header().Get("Location"))
	}

	for _, path := range []string{"/docs/", "/docs/openapi.yaml"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", path, response.Code, response.Body.String())
		}
		if path == "/docs/" && !strings.Contains(response.Body.String(), `url: "/docs/openapi.yaml"`) {
			t.Fatalf("Swagger UI page does not point at the embedded OpenAPI file")
		}
		if path == "/docs/openapi.yaml" && !strings.Contains(response.Body.String(), "openapi: 3.0.3") {
			t.Fatalf("OpenAPI document was not served")
		}
		if path == "/docs/openapi.yaml" && response.Header().Get("Content-Type") != "application/yaml; charset=utf-8" {
			t.Fatalf("OpenAPI document has content type %q", response.Header().Get("Content-Type"))
		}
	}
}