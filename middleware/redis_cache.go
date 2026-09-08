package middleware

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Default caching parameters. They are package-level variables so callers can
// tune them before creating the middleware; mutating them after requests are
// in flight is racy.
//
// 缓存默认参数。以包级变量暴露，调用方可在创建中间件前调整；
// 请求进行中修改存在并发风险。
var (
	// CacheKeyPrefix is the Redis key namespace for cached entries.
	// CacheKeyPrefix 是缓存项的 Redis key 命名空间。
	CacheKeyPrefix = "ferret:cache:"

	// EmptyCacheTTL is the TTL used for 404 placeholder entries that prevent
	// cache penetration.
	// EmptyCacheTTL 是 404 占位项的 TTL，用于防止缓存穿透。
	EmptyCacheTTL = 30 * time.Second

	// MaxCacheBodyBytes is the largest response body that will be cached.
	// MaxCacheBodyBytes 是可被缓存的最大响应体字节数。
	MaxCacheBodyBytes = 1 << 20 // 1 MiB

	// RedisOpTimeout bounds every Redis operation made by the middleware.
	// RedisOpTimeout 是中间件每次 Redis 操作的超时。
	RedisOpTimeout = 200 * time.Millisecond

	// BreakerFailureThreshold is the number of consecutive Redis failures
	// that opens the circuit breaker.
	// BreakerFailureThreshold 是打开熔断所需的连续 Redis 失败次数。
	BreakerFailureThreshold = 3

	// BreakerCooldown is how long the circuit breaker stays open.
	// BreakerCooldown 是熔断打开后的冷却时长。
	BreakerCooldown = 30 * time.Second
)

// CacheMetricEvent identifies the kind of cache metric event.
// CacheMetricEvent 标识缓存指标事件类型。
type CacheMetricEvent string

const (
	// CacheMetricHit means the response was served from cache.
	// CacheMetricHit 表示响应来自缓存。
	CacheMetricHit CacheMetricEvent = "hit"

	// CacheMetricMiss means the cache had no usable entry and the handler ran.
	// CacheMetricMiss 表示缓存无可用条目，执行了 handler。
	CacheMetricMiss CacheMetricEvent = "miss"
)

// CacheMetric is reported through OnCacheMetric for GET/HEAD cache lookups.
// CacheMetric 通过 OnCacheMetric 上报 GET/HEAD 缓存查询结果。
type CacheMetric struct {
	Event  CacheMetricEvent
	Key    string
	Method string
}

// OnCacheMetric, when non-nil, receives one event per GET/HEAD cache lookup.
// Configure it before creating the middleware.
// OnCacheMetric 非 nil 时，每次 GET/HEAD 缓存查询上报一个事件。
// 请在创建中间件前配置。
var OnCacheMetric func(CacheMetric)

// reportMetric dispatches a cache metric event.
// reportMetric 分发缓存指标事件。
func reportMetric(event CacheMetricEvent, key, method string) {
	if OnCacheMetric != nil {
		OnCacheMetric(CacheMetric{Event: event, Key: key, Method: method})
	}
}

// cacheEntry is the payload stored in Redis: the response status code,
// Content-Type (or full headers when applicable) and body.
// cacheEntry 是存入 Redis 的载荷：状态码、Content-Type（或适用时的完整响应头）与响应体。
type cacheEntry struct {
	Status      int         `json:"status"`
	ContentType string      `json:"content_type,omitempty"`
	Headers     http.Header `json:"headers,omitempty"`
	Body        []byte      `json:"body,omitempty"`
	Vary        []string    `json:"vary,omitempty"`
}

// cacheWriter captures the response status and body for caching.
// cacheWriter 捕获响应状态码与响应体，用于写入缓存。
type cacheWriter struct {
	http.ResponseWriter
	status   int
	body     bytes.Buffer
	tooBig   bool
	streamed bool
}

// WriteHeader captures the status code and delegates to the underlying writer.
// WriteHeader 捕获状态码并委托给底层 writer。
func (w *cacheWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Write records the body and writes it through. It defaults the status to 200
// when WriteHeader has not been called. Bodies larger than MaxCacheBodyBytes
// are not captured.
// Write 记录响应体并透传写入。未显式调用 WriteHeader 时状态码默认为 200。
// 超过 MaxCacheBodyBytes 的响应体不会被捕获。
func (w *cacheWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.tooBig && !w.streamed {
		if w.body.Len()+len(b) > MaxCacheBodyBytes {
			w.tooBig = true
			w.body.Reset()
		} else {
			w.body.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

// Flush marks the response as streamed and forwards Flush to the underlying
// writer when supported. Streamed responses are never cached.
// Flush 将响应标记为流式，并在底层 writer 支持时透传 Flush。流式响应不缓存。
func (w *cacheWriter) Flush() {
	w.streamed = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack marks the response as hijacked and forwards the call to the
// underlying writer when supported. Hijacked responses are never cached.
// Hijack 将响应标记为已劫持，并在底层 writer 支持时透传。已劫持响应不缓存。
func (w *cacheWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.streamed = true
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return h.Hijack()
}

// cacheBreaker is a tiny circuit breaker for Redis operations. After
// BreakerFailureThreshold consecutive failures it opens for BreakerCooldown,
// during which all Redis operations are skipped.
// cacheBreaker 是 Redis 操作的小型熔断器。连续失败达到 BreakerFailureThreshold 次后
// 打开熔断并保持 BreakerCooldown，期间跳过所有 Redis 操作。
type cacheBreaker struct {
	mu          sync.Mutex
	failures    int
	openedUntil time.Time
}

func (b *cacheBreaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.openedUntil)
}

func (b *cacheBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

func (b *cacheBreaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= BreakerFailureThreshold {
		b.openedUntil = time.Now().Add(BreakerCooldown)
		b.failures = 0
	}
}

// RedisCache returns a caching middleware. The cache key is derived from the
// request path and query string (the HTTP method is not part of the key):
//
//   - GET: lazy-load. On a hit the cached response is replayed; on a miss the
//     handler runs under singleflight so only one request per key reaches the
//     backend, and the response is cached afterwards. 404 responses are cached
//     with a short TTL (EmptyCacheTTL) to prevent cache penetration.
//   - HEAD: reads the same cache entry as GET but only replays status and
//     Content-Type. On a miss the handler runs and nothing is cached, so a
//     HEAD response can never pollute the GET cache with an empty body.
//   - POST: caches the response when the handler returns a 2xx status.
//   - PUT / PATCH: updates the cache when the handler returns a 2xx status.
//   - DELETE: removes the cache entry when the handler returns a 2xx status.
//   - Other methods pass through untouched.
//
// Cache-layer errors never fail the request. When onError is non-nil it is
// called with the error; otherwise the error is silently ignored. Redis
// operations run with RedisOpTimeout; after consecutive failures the breaker
// opens and Redis is skipped for BreakerCooldown.
//
// extraStatusCodes optionally adds HTTP status codes (besides 2xx and 404)
// that participate in caching, such as 302 for redirects. They are cached with
// ttl.
//
// RedisCache 返回缓存中间件。缓存键由请求路径与查询串组成（HTTP method 不参与）：
//
//   - GET：懒加载。命中时直接回放缓存响应；未命中时在 singleflight 下执行
//     handler（同一 key 只有一个请求打到后端），并在返回后写缓存。
//     404 响应以短 TTL（EmptyCacheTTL）缓存，用于防止缓存穿透。
//   - HEAD：读取与 GET 相同的缓存项，但只回放状态码与 Content-Type。
//     未命中时执行 handler 且不写缓存，避免空 body 污染 GET 缓存。
//   - POST：handler 返回 2xx 时建立缓存。
//   - PUT / PATCH：handler 返回 2xx 时更新缓存。
//   - DELETE：handler 返回 2xx 时移除缓存。
//   - 其他 method 直接透传。
//
// 缓存层错误不会导致请求失败。onError 非 nil 时会被调用；为 nil 时错误被静默忽略。
// Redis 操作带 RedisOpTimeout 超时；连续失败后熔断打开，BreakerCooldown 内跳过 Redis。
//
// extraStatusCodes 可选，用于额外指定参与缓存的状态码（2xx 与 404 之外），
// 例如 302 重定向；这些状态码按 ttl 缓存。
func RedisCache(client *redis.Client, ttl time.Duration, onError func(error), extraStatusCodes ...int) func(next http.Handler) http.Handler {
	var (
		breaker cacheBreaker
		sf      singleflight.Group
	)

	extra := make(map[int]bool, len(extraStatusCodes))
	for _, code := range extraStatusCodes {
		if code >= 100 && code <= 999 {
			extra[code] = true
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := cacheKey(r)
			ctx := r.Context()

			switch r.Method {
			case http.MethodGet:
				if breaker.allow() {
					hit, err := lookupAndReplay(w, client, ctx, key, r, false)
					switch {
					case err == nil && hit:
						reportMetric(CacheMetricHit, key, r.Method)
						breaker.recordSuccess()
						return
					case errors.Is(err, redis.Nil):
						breaker.recordSuccess()
					default:
						report(onError, fmt.Errorf("redis cache: get key %q: %w", key, err))
						breaker.recordFailure()
					}
				}

				led := false
				sf.Do(key, func() (any, error) {
					led = true
					reportMetric(CacheMetricMiss, key, r.Method)
					cw := &cacheWriter{ResponseWriter: w}
					next.ServeHTTP(cw, r)
					cacheAndRecord(&breaker, client, ctx, key, r, cw, ttl, onError, extra)
					return nil, nil
				})
				if led {
					return
				}

				if breaker.allow() {
					hit, err := lookupAndReplay(w, client, ctx, key, r, false)
					switch {
					case err == nil && hit:
						reportMetric(CacheMetricHit, key, r.Method)
						breaker.recordSuccess()
						return
					case errors.Is(err, redis.Nil):
						breaker.recordSuccess()
					default:
						report(onError, fmt.Errorf("redis cache: get key %q: %w", key, err))
						breaker.recordFailure()
					}
				}
				reportMetric(CacheMetricMiss, key, r.Method)
				next.ServeHTTP(w, r)

			case http.MethodHead:
				if breaker.allow() {
					hit, err := lookupAndReplay(w, client, ctx, key, r, true)
					switch {
					case err == nil && hit:
						reportMetric(CacheMetricHit, key, r.Method)
						breaker.recordSuccess()
						return
					case errors.Is(err, redis.Nil):
						breaker.recordSuccess()
					default:
						report(onError, fmt.Errorf("redis cache: get key %q: %w", key, err))
						breaker.recordFailure()
					}
				}
				reportMetric(CacheMetricMiss, key, r.Method)
				next.ServeHTTP(w, r)

			case http.MethodPost, http.MethodPut, http.MethodPatch:
				cw := &cacheWriter{ResponseWriter: w}
				next.ServeHTTP(cw, r)
				cacheAndRecord(&breaker, client, ctx, key, r, cw, ttl, onError, extra)

			case http.MethodDelete:
				cw := &cacheWriter{ResponseWriter: w}
				next.ServeHTTP(cw, r)
				if cw.status < 200 || cw.status >= 300 || !breaker.allow() {
					return
				}
				if err := delWithTimeout(client, ctx, key); err != nil {
					report(onError, fmt.Errorf("redis cache: del key %q: %w", key, err))
					breaker.recordFailure()
				} else {
					breaker.recordSuccess()
				}

			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// cacheAndRecord stores the captured response when applicable and updates the
// breaker. Skipped responses (not cacheable, too big, streamed) leave the
// breaker untouched.
// cacheAndRecord 在适用时写入捕获的响应并更新熔断器。被跳过的响应
// （不可缓存、过大、流式）不改变熔断器状态。
func cacheAndRecord(breaker *cacheBreaker, client *redis.Client, ctx context.Context, key string, r *http.Request, w *cacheWriter, ttl time.Duration, onError func(error), extra map[int]bool) {
	if !breaker.allow() {
		return
	}
	cacheableTTL := ttlForStatus(ttl, w.status, extra)
	if cacheableTTL < 0 {
		return
	}
	cached, err := store(client, ctx, key, r, w, cacheableTTL)
	if err != nil {
		report(onError, fmt.Errorf("redis cache: store key %q: %w", key, err))
		breaker.recordFailure()
		return
	}
	if cached {
		breaker.recordSuccess()
	}
}

// report invokes onError when non-nil; otherwise the error is ignored.
// report 在 onError 非 nil 时调用它；否则忽略错误。
func report(onError func(error), err error) {
	if onError != nil {
		onError(err)
	}
}

// parseVary parses the Vary header values into a sorted, deduplicated header
// name list. It reports noCache=true when Vary: * makes the response
// uncacheable per variant.
// parseVary 将 Vary 头解析为去重排序的头名列表。出现 Vary: * 时
// 返回 noCache=true，表示无法按变体缓存。
func parseVary(values []string) (vary []string, noCache bool) {
	seen := make(map[string]bool)
	for _, v := range values {
		for _, name := range strings.Split(v, ",") {
			name = strings.TrimSpace(name)
			if name == "*" {
				return nil, true
			}
			if name != "" && !seen[name] {
				seen[name] = true
				vary = append(vary, name)
			}
		}
	}
	sort.Strings(vary)
	return vary, false
}

// varyKey builds the variant cache key for a base key and the request's values
// of the Vary header names. vary is expected to be sorted.
// varyKey 根据基础 key 与请求中 Vary 头名对应的值生成变体缓存 key。
// vary 应已排序。
func varyKey(base string, vary []string, r *http.Request) string {
	key := base
	for _, name := range vary {
		key += "|" + url.QueryEscape(strings.ToLower(name)) + "=" + url.QueryEscape(r.Header.Get(name))
	}
	return key
}

// cacheKey builds the Redis key from path and query string, without the method.
// cacheKey 根据路径与查询串生成 Redis key，不包含 method。
func cacheKey(r *http.Request) string {
	key := CacheKeyPrefix + r.URL.Path
	if q := r.URL.Query().Encode(); q != "" {
		key += "?" + q
	}
	return key
}

// ttlForStatus returns the TTL to cache with, or -1 when the response should
// not be cached. 2xx and extra status codes use ttl; 404 uses a short TTL to
// prevent penetration; other statuses are not cached.
// ttlForStatus 返回写缓存应使用的 TTL；返回 -1 表示不应缓存。
// 2xx 与额外状态码使用 ttl；404 使用短 TTL 防穿透；其他状态码不缓存。
func ttlForStatus(ttl time.Duration, status int, extra map[int]bool) time.Duration {
	switch {
	case status >= 200 && status < 300:
		return ttl
	case extra[status]:
		return ttl
	case status == http.StatusNotFound:
		if ttl <= 0 || ttl > EmptyCacheTTL {
			return EmptyCacheTTL
		}
		return ttl
	default:
		return -1
	}
}

// filteredHeaders returns a copy of the response headers with hop-by-hop
// headers and Set-Cookie removed. It returns nil when nothing remains.
// filteredHeaders 返回剔除 hop-by-hop 头与 Set-Cookie 后的响应头副本；
// 无剩余内容时返回 nil。
func filteredHeaders(h http.Header) http.Header {
	excluded := map[string]bool{
		"Connection":          true,
		"Keep-Alive":          true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"TE":                  true,
		"Trailer":             true,
		"Set-Cookie":          true,
	}
	// Headers listed by Connection are also hop-by-hop.
	// Connection 中列出的头同样视为 hop-by-hop。
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				excluded[http.CanonicalHeaderKey(name)] = true
			}
		}
	}

	filtered := make(http.Header, len(h))
	for k, vv := range h {
		if len(vv) == 0 || excluded[http.CanonicalHeaderKey(k)] {
			continue
		}
		filtered[k] = append([]string(nil), vv...)
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// store serializes the captured response and writes it to Redis. It reports
// whether the response was actually cached; oversized, streamed or Vary: *
// responses are skipped without error. Hop-by-hop headers and Set-Cookie are
// filtered from the cached headers. Vary responses are stored under a variant
// key with a base index entry.
// store 序列化捕获的响应并写入 Redis，返回是否真正写入了缓存。
// 过大、流式或 Vary: * 的响应会被跳过且不返回错误。缓存头会剔除
// hop-by-hop 头与 Set-Cookie。带 Vary 的响应按变体 key 存储，并在基础 key
// 写入索引条目。
func store(client *redis.Client, ctx context.Context, key string, r *http.Request, w *cacheWriter, ttl time.Duration) (bool, error) {
	if w.tooBig || w.streamed {
		return false, nil
	}
	entry := cacheEntry{
		Status: w.status,
		Body:   w.body.Bytes(),
	}
	if headers := filteredHeaders(w.Header()); headers != nil {
		entry.Headers = headers
	} else {
		entry.ContentType = w.Header().Get("Content-Type")
	}

	vary, noCache := parseVary(w.Header().Values("Vary"))
	if noCache {
		return false, nil
	}
	if len(vary) == 0 {
		return storeEntry(client, ctx, key, entry, ttl)
	}
	if _, err := storeEntry(client, ctx, varyKey(key, vary, r), entry, ttl); err != nil {
		return false, err
	}
	index := cacheEntry{Vary: vary}
	if _, err := storeEntry(client, ctx, key, index, ttl); err != nil {
		return false, err
	}
	return true, nil
}

// storeEntry marshals one cache entry and writes it to Redis.
// storeEntry 序列化单个缓存条目并写入 Redis。
func storeEntry(client *redis.Client, ctx context.Context, key string, entry cacheEntry, ttl time.Duration) (bool, error) {
	data, err := json.Marshal(entry)
	if err != nil {
		return false, err
	}
	if err := setWithTimeout(client, ctx, key, data, ttl); err != nil {
		return false, err
	}
	return true, nil
}

// decodeCacheEntry unmarshals a cached payload.
// decodeCacheEntry 反序列化缓存载荷。
func decodeCacheEntry(val string) (cacheEntry, error) {
	var entry cacheEntry
	err := json.Unmarshal([]byte(val), &entry)
	return entry, err
}

// replayCacheEntry writes a decoded cache entry to the response writer.
// When headOnly is true only status and headers are replayed.
// replayCacheEntry 将解码后的缓存条目回放到响应 writer。
// headOnly 为 true 时只回放状态码与响应头。
func replayCacheEntry(w http.ResponseWriter, entry cacheEntry, headOnly bool) error {
	if entry.Status < 100 || entry.Status > 999 {
		return fmt.Errorf("invalid cached status %d", entry.Status)
	}
	if entry.Headers != nil {
		for k, vv := range entry.Headers {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
	} else if entry.ContentType != "" {
		w.Header().Set("Content-Type", entry.ContentType)
	}
	w.WriteHeader(entry.Status)
	if !headOnly {
		w.Write(entry.Body)
	}
	return nil
}

// lookupAndReplay reads the cache for key and replays the matching variant.
// A base entry carrying Vary is an index: the actual content is stored under
// varyKey(base, vary, r). It returns (hit, err); a redis.Nil err means miss.
// lookupAndReplay 读取 key 对应缓存并回放匹配的变体。带 Vary 的基础条目是索引，
// 实际内容存在 varyKey(base, vary, r) 下。返回 (hit, err)；
// err 为 redis.Nil 表示未命中。
func lookupAndReplay(w http.ResponseWriter, client *redis.Client, ctx context.Context, key string, r *http.Request, headOnly bool) (bool, error) {
	val, err := getWithTimeout(client, ctx, key)
	if err != nil {
		return false, err
	}
	entry, err := decodeCacheEntry(val)
	if err != nil {
		return false, err
	}
	if len(entry.Vary) > 0 {
		val, err = getWithTimeout(client, ctx, varyKey(key, entry.Vary, r))
		if err != nil {
			return false, err
		}
		entry, err = decodeCacheEntry(val)
		if err != nil {
			return false, err
		}
	}
	if err := replayCacheEntry(w, entry, headOnly); err != nil {
		return false, err
	}
	return true, nil
}

// getWithTimeout runs client.Get with a short timeout.
// getWithTimeout 以短超时执行 client.Get。
func getWithTimeout(client *redis.Client, ctx context.Context, key string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, RedisOpTimeout)
	defer cancel()
	return client.Get(ctx, key).Result()
}

// setWithTimeout runs client.Set with a short timeout.
// setWithTimeout 以短超时执行 client.Set。
func setWithTimeout(client *redis.Client, ctx context.Context, key string, data []byte, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, RedisOpTimeout)
	defer cancel()
	return client.Set(ctx, key, data, ttl).Err()
}

// delWithTimeout runs client.Del with a short timeout.
// delWithTimeout 以短超时执行 client.Del。
func delWithTimeout(client *redis.Client, ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, RedisOpTimeout)
	defer cancel()
	return client.Del(ctx, key).Err()
}
