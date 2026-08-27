# RainbowFerret_Ext

RainbowFerret 生态扩展库，旨在解决原库 `github.com/rambollwong/rainbowferret` 不引入第三方库的约束。

## 安装

```bash
go get github.com/rambollwong/rainbowferret_ext@latest
```

## util — 请求绑定

`util.Bind(r, v)` 类似 Gin 的 `ShouldBind`，根据 `Content-Type` 自动选择解码方式。

| Content-Type | 行为 |
| --- | --- |
| `application/json` | JSON 解码 |
| `application/xml` / `text/xml` | XML 解码 |
| `application/yaml` / `application/x-yaml` / `text/yaml` | YAML 解码 |
| `application/x-www-form-urlencoded` / `multipart/form-data` | 表单解码 |
| 缺失 | 回退为表单/查询参数绑定 |

规则：

- 结构体字段用 `form` tag 匹配表单字段（缺省用字段名）；用 `param` tag 匹配 URL 参数（`r.PathValue` 优先，查询串回退）。
- 绑定目标必须是非 nil 指针（结构体或 `*url.Values` / `*map[string][]string`）。
- 目标实现 `util.Validator`（上游 `github.com/rambollwong/rainbowferret/util`）时，填充后自动调用 `Validate()`。
- 不支持的媒体类型返回 `types.HTTPError`（HTTP 415）。

也可以直接使用导出的解码函数：`util.DecodeJSON`、`util.DecodeXML`、`util.DecodeYAML`、`util.DecodeForm`。

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
    // 处理 req ...
}
```

> 注意：本包导入的 `github.com/rambollwong/rainbowferret/util` 是上游包，与本地 `util` 包同名但不同路径。

## middleware — 中间件

### RainbowLogger

`middleware.RainbowLogger(logger *log.Logger, level level.Level)` 返回访问日志中间件；请求结束后输出一行 `METHOD URI status duration size` 日志。

```go
logger := log.New(log.AppendsEncoderWriters(log.TextEnc, os.Stdout))
root.Use(middleware.RainbowLogger(logger, level.Info))
```

### RedisCache

`middleware.RedisCache(client *redis.Client, ttl time.Duration, onError func(error))` 返回基于 Redis 的响应缓存中间件，推荐挂在需要缓存的特定路由上。缓存键由请求路径与查询串组成（HTTP method 不参与）：

- `GET`：命中时直接回放缓存；未命中时在 `singleflight` 下执行 handler 并写缓存；404 以短 TTL 缓存，防止缓存穿透。
- `HEAD`：读取与 `GET` 相同的缓存项，只回放状态码与 `Content-Type`；未命中不写缓存。
- `POST`：handler 返回 2xx 时写缓存。
- `PUT` / `PATCH`：handler 返回 2xx 时更新缓存。
- `DELETE`：handler 返回 2xx 时删除缓存。
- 其他 method 直接透传。

缓存层错误不会导致请求失败；`onError` 非 nil 时回调，否则静默忽略。可用包级变量调优：`CacheKeyPrefix`、`EmptyCacheTTL`、`MaxCacheBodyBytes`、`RedisOpTimeout`、`BreakerFailureThreshold`、`BreakerCooldown`；设置 `OnCacheMetric` 可接收每次 GET/HEAD 查询的 hit/miss 事件。

```go
rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
cache := middleware.RedisCache(rdb, 5*time.Minute, func(err error) {
    logger.Error().Msg(err.Error()).Done()
})
// 将 cache 挂在需要缓存的特定路由上
root.Get("/users", listUsersHandler, cache)
```

## 完整示例

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
        // 处理 req ...
        w.WriteHeader(http.StatusCreated)
    }, cache)

    http.ListenAndServe(":8080", root.Handler())
}
```

## 验证

```bash
go build ./...
go vet ./...
go test ./...
```
