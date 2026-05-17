# Design — Three-layer ACL (spec/001)

Retrospective. Real files: `internal/config/acl.go`, `internal/middleware/auth.go`, `internal/handlers/handlers.go` (tier helper), `internal/handlers/routes.go` (requireFullTier wraps).

## Layer 1 — X-HAL-Key middleware (excerpt of real auth.go)

```go
const HeaderHALKey = "X-HAL-Key"

func HALKeyMiddleware(acl *config.ACLConfig) func(http.Handler) http.Handler {
  return func(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
      if isExempt(r.URL.Path) { next.ServeHTTP(w, r); return }
      key := r.Header.Get(HeaderHALKey)
      if key == "" { http.Error(w, "missing X-HAL-Key", 401); return }
      role, ok := acl.Keys[key]
      if !ok && !acl.Permissive { http.Error(w, "unknown key", 401); return }
      // role attached to ctx, then ACL middleware runs next
      ctx := context.WithValue(r.Context(), roleKey, role)
      next.ServeHTTP(w, r.WithContext(ctx))
    })
  }
}

func isExempt(path string) bool {
  return path == "/health" || strings.HasPrefix(path, "/docs")
}
```

## Layer 2 — Role-based permissions (excerpt)

```go
var rolePermissions = map[config.Role][]permission{
  config.RoleCore: {
    {prefix: "/", methods: allMethods},
  },
  config.RoleMeshSat: {
    {prefix: "/gps/", methods: allMethods},
    {prefix: "/meshtastic/", methods: allMethods},
    {prefix: "/iridium/", methods: allMethods},
    {prefix: "/network/wifi/status/", methods: readOnly},
    {prefix: "/network/status", methods: readOnly},
    {prefix: "/system/info", methods: readOnly},
    {prefix: "/system/temperature", methods: readOnly},
    {prefix: "/system/uptime", methods: readOnly},
    {prefix: "/power/status", methods: readOnly},
    {prefix: "/power/battery", methods: readOnly},
    {prefix: "/sensors/", methods: readOnly},
  },
  config.RoleReadOnly: {
    {prefix: "/system/", methods: readOnly},
    {prefix: "/power/status", methods: readOnly},
    {prefix: "/sensors/", methods: readOnly},
  },
}
```

ACL middleware checks `r.Context().Value(roleKey)` against this table.

## Layer 3 — `requireFullTier` wrapper

In `handlers/handlers.go`:

```go
func (h *HALHandler) requireFullTier(next http.HandlerFunc) http.HandlerFunc {
  return func(w http.ResponseWriter, r *http.Request) {
    if h.tier != "full" {
      errorResponse(w, http.StatusForbidden,
        fmt.Sprintf("endpoint requires CUBEOS_TIER=full, currently '%s'", h.tier))
      return
    }
    next(w, r)
  }
}
```

Wrap sites in `routes.go`: ~7 endpoints currently (see spec/001 REQ-017).

## ACL file format (`/cubeos/config/hal-acl.json`)

```json
{
  "keys": {
    "key-for-cubeos-api-aaaaaaaaaa": "core",
    "key-for-meshsat-userapp-bbbb": "meshsat",
    "key-for-monitoring-prom-cccc": "readonly"
  }
}
```

## Test pattern

`internal/middleware/auth_test.go` exists. New tests added by future PRs follow the same pattern: `httptest.NewRequest` with each documented X-HAL-Key + X-CubeOS-Role combination; assert response code matches the expected role-permission table.
