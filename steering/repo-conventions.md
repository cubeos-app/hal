# Steering — Repo conventions

## Build

```bash
cd hal
go build ./cmd/cubeos-hal     # → cubeos-hal binary
go test ./...                  # all packages
golangci-lint run              # lint
```

## Run locally (privileged Docker)

```bash
docker run --rm --privileged --network host \
  -e HAL_KEY=<key> \
  -e CUBEOS_TIER=full \
  -v /etc:/etc \
  -v /sys:/sys \
  -v /dev:/dev \
  cubeos-hal:dev
```

In production this is managed by Docker Swarm via `coreapps/cubeos-hal/docker-compose.yml`.

## Env vars

| Env | Default | Purpose |
|---|---|---|
| `HAL_KEY` | (none — exits if unset) | Master API key for unauthenticated initial bootstrap; replaced by per-key ACL via `HAL_ACL_KEYS` or `HAL_ACL_KEYS_FILE` |
| `HAL_ACL_KEYS` | (none) | Inline JSON map `{"key":"role"}` |
| `HAL_ACL_KEYS_FILE` | `/cubeos/config/hal-acl.json` | Path to JSON ACL file |
| `CUBEOS_TIER` | `full` | `full` or `container` — gates destructive endpoints |
| `HAL_DISABLE_IRIDIUM` | `false` | Set to `true` to skip IridiumDriver init |
| `HAL_DISABLE_MESHTASTIC` | `false` | Set to `true` to skip MeshtasticDriver init |
| `HAL_PORT` | `6005` | Listen port |

## Branches

- `main` is always-deployable.
- Feature branches: `feat/<short-slug>` (human) or `merge/<feature_id>` (parallel-dev).

## Commit messages

```
type(scope): description [CUBEOS-XX]
```

Operator identity:

```
git -c user.name="Kyriakos Papadopoulos" -c user.email="ncpjfuzl@mxmx.email" commit ...
```

## File layout

```
/
  cmd/cubeos-hal/main.go        ← entry point
  internal/
    config/acl.go               ← role-based ACL config loader
    devices/i2c.go              ← low-level helpers
    middleware/auth.go          ← X-HAL-Key + role-prefix gate
    handlers/                   ← ~47 .go files (one per /<domain>/ prefix)
  go.mod / go.sum
  Makefile                      ← lint, test, build aliases
  OPENAPI_DOCS.md               ← canonical API docs
  README.md
  CLAUDE.md                     ← LOCAL-ONLY, gitignored
  PROJECT.json + PROJECT.md     ← spec-kit charter
  constitution.md
  steering/                     ← this dir
  adr/
  spec/
  .agentic/slot-config.entry.json
  .gitignore
```

## Release

Per CubeOS Article XV: push to `main` → CI auto-deploys to every registered Pi.

For parallel-dev waves: per ADR-0007.
