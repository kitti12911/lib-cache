# lib-cache

Shared Redis-compatible cache helpers for homelab services.

The default codec is JSON, so services can store and retrieve normal structs
without writing repetitive marshaling code. Dragonfly works because it speaks
the Redis protocol.

## project structure

```bash
lib-cache/
├── cache.go      # typed cache wrapper, key namespacing, TTL, and invalidation
├── codec.go      # codec interface and JSON codec implementation
├── config.go     # Redis-compatible cache configuration
├── Makefile
├── go.mod
└── README.md
```

## install

```bash
go get github.com/kitti12911/lib-cache
```

## ci commands

reusable CI entrypoints live in `scripts/ci/` so GitHub Actions and GitLab CI
can call the same commands with provider-specific orchestration around them.

| command                                    | purpose                           |
| ------------------------------------------ | --------------------------------- |
| `./scripts/ci/go-lint.sh`                  | run `go vet` and `golangci-lint`  |
| `./scripts/ci/go-test.sh`                  | run tests with coverage           |
| `./scripts/ci/markdownlint.sh`             | run Markdown linting              |
| `./scripts/ci/security-scan.sh`            | run `govulncheck` and Semgrep     |
| `./scripts/ci/supply-chain-scan.sh`        | run Trivy and Gitleaks            |
| `./scripts/ci/semantic-release-plan.sh`    | preview the next semantic release |
| `./scripts/ci/semantic-release-publish.sh` | publish the semantic release      |

GitHub Actions uses `TOOLCHAIN_REGISTRY` and `TOOLCHAIN_IMAGE_NAMESPACE` to
resolve the shared toolchain images. GitLab should map its CI variables and
image credentials to the same script inputs instead of duplicating the command
logic.
The current semantic-release config publishes GitHub releases; GitLab release
publishing also needs `@semantic-release/gitlab` in the release toolchain and a
GitLab-specific release config.

`GO_TEST_RACE=true` or `GO_TEST_CGO=true` requires a C compiler in the selected
toolchain image. `lib-cache` sets `GO_TEST_RACE=false` in GitHub Actions while
using `image-toolchain` v1.0.1 because that image does not include one.

## usage

```go
type UserView struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

cacheClient, err := cache.New(
    ctx,
    cache.Config{
        Addr:       "dragonfly.database.svc.cluster.local:6379",
        Password:   "example",
        KeyPrefix:  "oas-sandbox",
        DefaultTTL: 5 * time.Minute,
    },
    cache.WithSingleflight(true),
)
if err != nil {
    return err
}
defer cacheClient.Close()

users := cache.Use[UserView](cacheClient)

view, err := users.GetOrLoad(ctx, "users:"+id, func(ctx context.Context) (UserView, error) {
    return loadUserView(ctx, id)
})
```

Use `Set` when a caller already has the value:

```go
err = users.Set(ctx, "users:"+id, view, cache.WithTTL(time.Minute))
```

Use `Delete` for one or more keys:

```go
err = users.Delete(ctx, "users:"+id)
```

Use `Clear` for operational invalidation of every key in the configured
namespace:

```go
err = cacheClient.Clear(ctx)
```

When `KeyPrefix` is set, `Clear` only deletes keys under that prefix. Without a
prefix, it falls back to `FLUSHDB`, so production services should set a prefix.

## behavior

- `DefaultTTL` is the global default invalidation time.
- `cache.WithTTL(...)` overrides TTL for a single write or load.
- `cache.WithSingleflight(true)` collapses concurrent cache misses for the same
  key into one loader call.
- `Delete` invalidates specific keys.
- `Clear` invalidates the full configured cache namespace.

## requirements

- go 1.26 or higher

Optional:

- [prettier](https://prettier.io/) for Markdown, YAML, JSON, and JSONC formatting
- [golangci-lint](https://golangci-lint.run/) for `make lint`
- [markdownlint-cli2](https://github.com/DavidAnson/markdownlint-cli2) for `make lint`

## available commands

| Command       | Description                                     |
| ------------- | ----------------------------------------------- |
| `make tidy`   | Run `go mod tidy`                               |
| `make lint`   | Run Go and Markdown linting                     |
| `make vet`    | Run `go vet ./...`                              |
| `make fmt`    | Format Go code with `go fmt`                    |
| `make pretty` | Format Markdown, YAML, JSON, and JSONC          |
| `make format` | Run Go and document/config formatting           |
| `make test`   | Run tests with the race detector                |
| `make cov`    | Generate and open an HTML coverage report       |
| `make fix`    | Apply standard Go source rewrites with `go fix` |
