package handlers

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed openapi.yaml
var openapiSpec []byte

// getCORSHostname returns the hostname allowed for CORS from env or default.
func getCORSHostname() string {
	if h := os.Getenv("HAL_CORS_HOSTNAME"); h != "" {
		return h
	}
	return "cubeos.cube"
}

// ServeOpenAPISpec serves the OpenAPI specification.
// @Summary Get OpenAPI spec
// @Description Returns the OpenAPI 3.0 specification in YAML format
// @Tags Documentation
// @Produce text/yaml
// @Success 200 {string} string "OpenAPI YAML specification"
// @Router /docs/openapi.yaml [get]
func (h *HALHandler) ServeOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	// Restrict CORS to localhost and configured hostname (HAL is not internet-facing)
	origin := r.Header.Get("Origin")
	if origin != "" && (strings.HasPrefix(origin, "http://127.0.0.1") ||
		strings.HasPrefix(origin, "http://localhost") ||
		strings.HasPrefix(origin, "http://"+getCORSHostname())) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}
	w.Write(openapiSpec)
}

// ServeSwaggerUI serves the Swagger UI.
// @Summary Swagger UI
// @Description Interactive API documentation
// @Tags Documentation
// @Produce text/html
// @Success 200 {string} string "Swagger UI HTML"
// @Router /docs [get]
func (h *HALHandler) ServeSwaggerUI(w http.ResponseWriter, r *http.Request) {
	// Determine base path from request
	basePath := "/hal"
	if strings.HasPrefix(r.URL.Path, "/hal") {
		basePath = "/hal"
	}

	// Check if local swagger-ui assets exist (offline-first)
	swaggerDir := os.Getenv("HAL_SWAGGER_DIR")
	if swaggerDir == "" {
		swaggerDir = "/app/swagger-ui"
	}

	cssURL := "https://unpkg.com/swagger-ui-dist@5/swagger-ui.css"
	bundleURL := "https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"
	presetURL := "https://unpkg.com/swagger-ui-dist@5/swagger-ui-standalone-preset.js"

	// Use local assets if available
	if _, err := os.Stat(filepath.Join(swaggerDir, "swagger-ui.css")); err == nil {
		cssURL = basePath + "/docs/swagger-ui/swagger-ui.css"
		bundleURL = basePath + "/docs/swagger-ui/swagger-ui-bundle.js"
		presetURL = basePath + "/docs/swagger-ui/swagger-ui-standalone-preset.js"
	}

	html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>CubeOS HAL API Documentation</title>
    <link rel="stylesheet" type="text/css" href="` + cssURL + `">
    <style>
        html { box-sizing: border-box; overflow-y: scroll; }
        *, *:before, *:after { box-sizing: inherit; }
        body { margin: 0; background: #fafafa; }
        .swagger-ui .topbar { display: none; }
        .swagger-ui .info { margin: 20px 0; }
        .swagger-ui .info .title { font-size: 2em; }
    </style>
</head>
<body>
    <div id="swagger-ui"></div>
    <script src="` + bundleURL + `"></script>
    <script src="` + presetURL + `"></script>
    <script>
        window.onload = function() {
            window.ui = SwaggerUIBundle({
                url: "` + basePath + `/docs/openapi.yaml",
                dom_id: '#swagger-ui',
                deepLinking: true,
                presets: [
                    SwaggerUIBundle.presets.apis,
                    SwaggerUIStandalonePreset
                ],
                plugins: [
                    SwaggerUIBundle.plugins.DownloadUrl
                ],
                layout: "StandaloneLayout",
                defaultModelsExpandDepth: 1,
                defaultModelExpandDepth: 1,
                displayRequestDuration: true,
                filter: true,
                showExtensions: true,
                showCommonExtensions: true,
                tryItOutEnabled: true
            });
        };
    </script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

// ServeSwaggerAsset serves bundled Swagger UI static files for offline-first operation.
func (h *HALHandler) ServeSwaggerAsset(w http.ResponseWriter, r *http.Request) {
	swaggerDir := os.Getenv("HAL_SWAGGER_DIR")
	if swaggerDir == "" {
		swaggerDir = "/app/swagger-ui"
	}

	// Extract filename from URL path (e.g., /hal/docs/swagger-ui/swagger-ui.css -> swagger-ui.css)
	requestedFile := filepath.Base(r.URL.Path)

	// Only serve known swagger-ui files
	allowedFiles := map[string]string{
		"swagger-ui.css":                  "text/css; charset=utf-8",
		"swagger-ui-bundle.js":            "application/javascript; charset=utf-8",
		"swagger-ui-standalone-preset.js": "application/javascript; charset=utf-8",
	}

	contentType, ok := allowedFiles[requestedFile]
	if !ok {
		http.NotFound(w, r)
		return
	}

	filePath := filepath.Join(swaggerDir, requestedFile)

	// Prevent path traversal
	absPath, err := filepath.Abs(filePath)
	if err != nil || !strings.HasPrefix(absPath, swaggerDir) {
		http.NotFound(w, r)
		return
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

// HealthCheck returns health status.
// @Summary Health check
// @Description Returns HAL service health status
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /health [get]
func (h *HALHandler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	version := os.Getenv("CUBEOS_VERSION")
	if version == "" {
		version = "dev"
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "healthy",
		"service": "cubeos-hal",
		"version": version,
	})
}
