# RainbowFerret_Ext

RainbowFerret ecosystem extension library, aimed at resolving the constraint that the core library `github.com/rambollwong/rainbowferret` does not introduce third-party libraries.

## Installation

```bash
go get rambollwong/rainbowproject/rainbowferret_ext@latest
```

## util — Request binding

`util.Bind(r, v)` works like Gin's `ShouldBind` and picks a decoder automatically based on the `Content-Type` header.

| Content-Type | Behavior |
| --- | --- |
| `application/json` | JSON decoding |
| `application/xml` / `text/xml` | XML decoding |
| `application/yaml` / `application/x-yaml` / `text/yaml` | YAML decoding |
| `application/x-www-form-urlencoded` / `multipart/form-data` | Form decoding |
| Missing | Falls back to form/query binding |

Rules:

- Struct fields match form values via the `form` tag (field name is the fallback) and URL parameters via the `param` tag (`r.PathValue` first, query string as fallback).
- The binding target must be a non-nil pointer (a struct or `*url.Values` / `*map[string][]string`).
- When the target implements `util.Validator` (from the upstream `github.com/rambollwong/rainbowferret/util` package), `Validate()` is called after filling.
- Unsupported media types return a `types.HTTPError` (HTTP 415).

The exported decoders can also be used directly: `util.DecodeJSON`, `util.DecodeXML`, `util.DecodeYAML`, `util.DecodeForm`.

```go
type CreateUserReq struct {
    Name string `form:"name" json:"name"`
    Age  int    `form:"age" json:"age"`
}

func createUser(w http.ResponseWriter, r *http.Request) {
    var req CreateUserReq
    if err := util.Bind(r, &req); err != nil {
        status := http.StatusBadRequest
        if he, ok := err.(*types.HTTPError); ok {
            status = he.Code
        }
        http.Error(w, err.Error(), status)
        return
    }
    // handle req ...
}
```

> Note: the imported `github.com/rambollwong/rainbowferret/util` is the upstream package; it shares the name `util` with this local package but has a different import path.

## middleware — Middleware

### RainbowLogger

`middleware.RainbowLogger(logger *log.Logger, level level.Level)` returns an access-log middleware; after each request it logs one line in the form `METHOD URI status duration size`.

```go
logger := log.New(log.AppendsEncoderWriters(log.TextEnc, os.Stdout))
root.Use(middleware.RainbowLogger(logger, level.Info))
```

### RedisCache

`middleware.RedisCache(client *redis.Client, ttl time.Duration, onError func(error))` returns a Redis response-cache middleware; it is recommended to attach it to the specific routes that need caching. The cache key is derived from the request path and query string (the HTTP method is not part of the key):

- `GET`: on a hit the cached response is replayed; on a miss the handler runs under `singleflight` and the response is cached afterwards. 404 responses are cached with a short TTL to prevent cache penetration.
- `HEAD`: reads the same cache entry as `GET` but only replays the status code and `Content-Type`; nothing is cached on a miss.
- `POST`: caches the response when the handler returns 2xx.
- `PUT` / `PATCH`: updates the cache when the handler returns 2xx.
- `DELETE`: removes the cache entry when the handler returns 2xx.
- Other methods pass through untouched.

Cache-layer errors never fail the request; `onError` is called when non-nil, otherwise the error is silently ignored. Package-level variables can be tuned: `CacheKeyPrefix`, `EmptyCacheTTL`, `MaxCacheBodyBytes`, `RedisOpTimeout`, `BreakerFailureThreshold`, `BreakerCooldown`. Setting `OnCacheMetric` reports a hit/miss event for every GET/HEAD cache lookup.

```go
rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
cache := middleware.RedisCache(rdb, 5*time.Minute, func(err error) {
    logger.Error().Msg(err.Error()).Done()
})
// Attach cache to the specific route that needs caching.
root.Get("/users", listUsersHandler, cache)
```

## Full example

```go
package main

import (
    "net/http"
    "os"
    "time"

    "github.com/rambollwong/rainbowferret/ferret"
    "github.com/rambollwong/rainbowferret/types"
    "github.com/rambollwong/rainbowlog/level"
    "github.com/rambollwong/rainbowlog/log"
    "github.com/redis/go-redis/v9"

    "rambollwong/rainbowproject/rainbowferret_ext/middleware"
    "rambollwong/rainbowproject/rainbowferret_ext/util"
)

type CreateUserReq struct {
    Name string `form:"name" json:"name"`
    Age  int    `form:"age" json:"age"`
}

func main() {
    sm := http.NewServeMux()

    logger := log.New(log.AppendsEncoderWriters(log.TextEnc, os.Stdout))
    rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})

    cache := middleware.RedisCache(rdb, 5*time.Minute, func(err error) {
        logger.Error().Msg(err.Error()).Done()
    })

    root := ferret.NewRootGroup(sm,
        middleware.RainbowLogger(logger, level.Info),
    )

    root.Post("/users", func(w http.ResponseWriter, r *http.Request) {
        var req CreateUserReq
        if err := util.Bind(r, &req); err != nil {
            status := http.StatusBadRequest
            if he, ok := err.(*types.HTTPError); ok {
                status = he.Code
            }
            http.Error(w, err.Error(), status)
            return
        }
        // handle req ...
        w.WriteHeader(http.StatusCreated)
    }, cache)

    http.ListenAndServe(":8080", root.Handler())
}
```

## Verification

```bash
go build ./...
go vet ./...
go test ./...
```
