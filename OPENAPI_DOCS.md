# HAL API Documentation Auto-Generation

## Current Setup (Manual)

The OpenAPI spec is embedded in the binary using `go:embed`. The file must be at:
```
internal/handlers/openapi.yaml
```

## Serving Documentation

- **Swagger UI**: http://cubeos.cube:6005/hal/docs
- **OpenAPI YAML**: http://cubeos.cube:6005/hal/openapi.yaml

## Option 1: Manual Updates (Current)

Edit `internal/handlers/openapi.yaml` manually when adding endpoints.

## Option 2: Auto-Generation with swaggo/swag (Recommended)

### Setup

1. Install swag CLI:
```bash
go install github.com/swaggo/swag/cmd/swag@latest
```

2. Add main.go annotations:
```go
// @title CubeOS HAL API
// @version 1.1.0
// @description Hardware Abstraction Layer API for CubeOS
// @host cubeos.cube:6005
// @BasePath /hal
func main() {
```

3. Add handler annotations (example):
```go
// GetUptime returns system uptime information
// @Summary Get system uptime
// @Description Returns uptime in seconds, formatted string, and boot time
// @Tags System
// @Accept json
// @Produce json
// @Success 200 {object} UptimeInfo
// @Router /system/uptime [get]
func (h *HALHandler) GetUptime(w http.ResponseWriter, r *http.Request) {
```

4. Generate spec:
```bash
cd /path/to/cubeos-hal
swag init -g cmd/cubeos-hal/main.go -o internal/handlers --parseDependency --parseInternal
```

5. Rename output:
```bash
mv internal/handlers/swagger.yaml internal/handlers/openapi.yaml
```

### CI/CD Integration

Add to `.gitlab-ci.yml`:
```yaml
generate-docs:
  stage: build
  script:
    - go install github.com/swaggo/swag/cmd/swag@latest
    - swag init -g cmd/cubeos-hal/main.go -o internal/handlers
    - mv internal/handlers/swagger.yaml internal/handlers/openapi.yaml
  artifacts:
    paths:
      - internal/handlers/openapi.yaml
```

### Makefile Target

```makefile
.PHONY: docs
docs:
	@echo "Generating OpenAPI spec..."
	swag init -g cmd/cubeos-hal/main.go -o internal/handlers --parseDependency
	mv internal/handlers/swagger.yaml internal/handlers/openapi.yaml
	mv internal/handlers/swagger.json internal/handlers/openapi.json 2>/dev/null || true
	rm -f internal/handlers/docs.go  # swag creates this, we have our own
	@echo "Done! Spec at internal/handlers/openapi.yaml"
```

## Annotation Reference

### Endpoint Annotations
```go
// @Summary     Brief description
// @Description Detailed description
// @Tags        Category
// @Accept      json
// @Produce     json
// @Param       name path string true "Parameter description"
// @Param       body body RequestType true "Request body"
// @Success     200 {object} ResponseType
// @Failure     400 {object} ErrorResponse
// @Failure     404 {object} ErrorResponse
// @Router      /endpoint [get]
```

### Struct Annotations
```go
type UptimeInfo struct {
    Seconds   float64   `json:"seconds" example:"593949.26"`
    Formatted string    `json:"formatted" example:"6d 21h 5m"`
    BootTime  string    `json:"boot_time" example:"2026-01-27T19:00:00Z"`
}
```

## Quick Reference

| Annotation | Purpose |
|------------|---------|
| @Summary | One-line description |
| @Description | Detailed description |
| @Tags | API category grouping |
| @Accept | Request content types |
| @Produce | Response content types |
| @Param | Path/query/body parameters |
| @Success | Success response |
| @Failure | Error response |
| @Router | Endpoint path and method |
