package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })
	return client
}

func newClosedTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		MaxRetries:  -1,
		DialTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { client.Close() })
	mr.Close()
	return client
}

func TestRedisCacheGetMissThenHit(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a?x=1", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("first response: code=%d body=%q", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/a?x=1", nil))
	if rec2.Code != http.StatusOK || rec2.Body.String() != "hello" {
		t.Fatalf("cached response: code=%d body=%q", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("cached Content-Type: %q", rec2.Header().Get("Content-Type"))
	}
	if calls != 1 {
		t.Fatalf("handler should be called once, got %d", calls)
	}
}

func TestRedisCacheGet404UsesShortTTL(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Hour, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("first status: %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("cached status: %d", rec2.Code)
	}
	if calls != 1 {
		t.Fatalf("handler should be called once (penetration prevented), got %d", calls)
	}
}

func TestRedisCachePostCreatesCache(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	postCalls := 0
	getCalls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			postCalls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":1}`))
		default:
			getCalls++
			w.Write([]byte("from-handler"))
		}
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/items", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("post status: %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/items", nil))
	if rec2.Code != http.StatusCreated || rec2.Body.String() != `{"id":1}` {
		t.Fatalf("get should hit post cache: code=%d body=%q", rec2.Code, rec2.Body.String())
	}
	if postCalls != 1 || getCalls != 0 {
		t.Fatalf("unexpected calls: post=%d get=%d", postCalls, getCalls)
	}
}

func TestRedisCachePutUpdatesCache(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			w.Write([]byte("new"))
		default:
			w.Write([]byte("old"))
		}
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/item/1", nil))
	if rec.Body.String() != "old" {
		t.Fatalf("initial get: %q", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodPut, "/item/1", nil))
	if rec2.Body.String() != "new" {
		t.Fatalf("put: %q", rec2.Body.String())
	}

	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/item/1", nil))
	if rec3.Body.String() != "new" {
		t.Fatalf("get after put should be updated, got %q", rec3.Body.String())
	}
}

func TestRedisCacheDeleteRemovesCache(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	getCalls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			getCalls++
			w.Write([]byte("gone"))
		}
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/item/1", nil))
	if rec.Body.String() != "gone" {
		t.Fatalf("initial get: %q", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodDelete, "/item/1", nil))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("delete status: %d", rec2.Code)
	}

	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/item/1", nil))
	if rec3.Body.String() != "gone" {
		t.Fatalf("get after delete should miss cache, got %q", rec3.Body.String())
	}
	if getCalls != 2 {
		t.Fatalf("get handler should run twice after delete, got %d", getCalls)
	}
}

func TestRedisCacheReportsErrorToOnError(t *testing.T) {
	client := newClosedTestRedis(t)

	var got error
	mw := RedisCache(client, time.Minute, func(err error) {
		got = err
	})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/err", nil))

	if got == nil {
		t.Fatal("expected onError to be called")
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("request should still succeed: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestRedisCacheHeadHitsGetCache(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("hello"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/head", nil))
	if rec.Body.String() != "hello" {
		t.Fatalf("get body: %q", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodHead, "/head", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("head status: %d", rec2.Code)
	}
	if rec2.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("head Content-Type: %q", rec2.Header().Get("Content-Type"))
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("head body should be empty, got %q", rec2.Body.String())
	}
	if calls != 1 {
		t.Fatalf("handler should be called once, got %d", calls)
	}
}

func TestRedisCacheHeadMissDoesNotCache(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("from-handler"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/head-miss", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("head: code=%d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/head-miss", nil))
	if rec2.Body.String() != "from-handler" {
		t.Fatalf("get after head miss should run handler, got %q", rec2.Body.String())
	}
	if calls != 2 {
		t.Fatalf("handler should run for head and get, got %d", calls)
	}
}

func TestRedisCachePatchUpdatesCache(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			w.Write([]byte("new"))
		default:
			w.Write([]byte("old"))
		}
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/patch/1", nil))
	if rec.Body.String() != "old" {
		t.Fatalf("initial get: %q", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodPatch, "/patch/1", nil))
	if rec2.Body.String() != "new" {
		t.Fatalf("patch: %q", rec2.Body.String())
	}

	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/patch/1", nil))
	if rec3.Body.String() != "new" {
		t.Fatalf("get after patch should be updated, got %q", rec3.Body.String())
	}
}

func TestRedisCacheGetSingleflightPreventsStampede(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	var calls int32
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond)
		w.Write([]byte("ok"))
	}))

	const n = 20
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sf", nil))
			codes[i] = rec.Code
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("handler should run once under singleflight, got %d", calls)
	}
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusOK || bodies[i] != "ok" {
			t.Fatalf("request %d: code=%d body=%q", i, codes[i], bodies[i])
		}
	}
}

func TestRedisCacheSkipsOversizedBody(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	big := bytes.Repeat([]byte("a"), MaxCacheBodyBytes+1)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write(big)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/big", nil))
	if rec.Body.Len() != len(big) {
		t.Fatalf("first body length: %d", rec.Body.Len())
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/big", nil))
	if rec2.Body.Len() != len(big) {
		t.Fatalf("second body length: %d", rec2.Body.Len())
	}
	if calls != 2 {
		t.Fatalf("oversized response should not be cached, calls=%d", calls)
	}
}

func TestRedisCacheSkipsFlushedResponse(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		w.Write([]byte("stream"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/flush", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first status: %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/flush", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status: %d", rec2.Code)
	}
	if calls != 2 {
		t.Fatalf("flushed response should not be cached, calls=%d", calls)
	}
}

func TestRedisCacheBreakerSkipsRedisAfterFailures(t *testing.T) {
	client := newClosedTestRedis(t)

	var errCount int32
	mw := RedisCache(client, time.Minute, func(err error) {
		atomic.AddInt32(&errCount, 1)
	})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	for i := 0; i < BreakerFailureThreshold; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/breaker", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("request %d should still succeed", i)
		}
	}

	afterFailures := atomic.LoadInt32(&errCount)
	if afterFailures == 0 {
		t.Fatal("expected cache errors before breaker opens")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/breaker", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("request after breaker should still succeed: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&errCount) != afterFailures {
		t.Fatalf("breaker should skip Redis after failures, errors %d -> %d", afterFailures, atomic.LoadInt32(&errCount))
	}
}

func TestRedisCacheKeyPrefix(t *testing.T) {
	old := CacheKeyPrefix
	CacheKeyPrefix = "custom:"
	defer func() { CacheKeyPrefix = old }()

	r := httptest.NewRequest(http.MethodGet, "/p?x=1", nil)
	if got := cacheKey(r); got != "custom:/p?x=1" {
		t.Fatalf("cacheKey = %q", got)
	}
}

func TestRedisCacheMetrics(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	var events []CacheMetric
	old := OnCacheMetric
	OnCacheMetric = func(m CacheMetric) {
		events = append(events, m)
	}
	defer func() { OnCacheMetric = old }()

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if len(events) != 2 {
		t.Fatalf("expected 2 metric events, got %d: %+v", len(events), events)
	}
	if events[0].Event != CacheMetricMiss || events[1].Event != CacheMetricHit {
		t.Fatalf("unexpected events: %+v", events)
	}
	if events[0].Key != CacheKeyPrefix+"/metrics" || events[1].Key != CacheKeyPrefix+"/metrics" {
		t.Fatalf("unexpected keys: %+v", events)
	}
}

func TestRedisCacheSkips302ByDefault(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "/target")
		w.WriteHeader(http.StatusFound)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/redirect-default", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("first status: %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/redirect-default", nil))
	if rec2.Code != http.StatusFound {
		t.Fatalf("second status: %d", rec2.Code)
	}
	if calls != 2 {
		t.Fatalf("302 should not be cached by default, calls=%d", calls)
	}
}

func TestRedisCacheCachesExtraStatusWithLocation(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil, http.StatusFound)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "/target")
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(http.StatusFound)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/redirect", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/target" {
		t.Fatalf("first response: code=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/redirect", nil))
	if rec2.Code != http.StatusFound {
		t.Fatalf("cached status: %d", rec2.Code)
	}
	if rec2.Header().Get("Location") != "/target" {
		t.Fatalf("cached Location: %q", rec2.Header().Get("Location"))
	}
	if rec2.Header().Get("X-Custom") != "yes" {
		t.Fatalf("cached X-Custom: %q", rec2.Header().Get("X-Custom"))
	}
	if calls != 1 {
		t.Fatalf("302 with extra status should be cached, calls=%d", calls)
	}
}

func TestRedisCacheCachesFullHeadersOnCacheControlPublic(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Custom", "hello")
		w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("first response: code=%d body=%q", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rec2.Body.String() != "ok" {
		t.Fatalf("cached body: %q", rec2.Body.String())
	}
	if rec2.Header().Get("X-Custom") != "hello" {
		t.Fatalf("cached X-Custom: %q", rec2.Header().Get("X-Custom"))
	}
	if rec2.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("cached Cache-Control: %q", rec2.Header().Get("Cache-Control"))
	}
	if calls != 1 {
		t.Fatalf("public response should be cached, calls=%d", calls)
	}
}

func TestRedisCacheFiltersDangerousHeaders(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Keep", "yes")
		w.Header().Set("Set-Cookie", "session=abc")
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "hop")
		w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/headers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first status: %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/headers", nil))
	if rec2.Code != http.StatusOK || rec2.Body.String() != "ok" {
		t.Fatalf("cached response: code=%d body=%q", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("X-Keep") != "yes" {
		t.Fatalf("X-Keep should be cached: %q", rec2.Header().Get("X-Keep"))
	}
	if rec2.Header().Get("Set-Cookie") != "" {
		t.Fatalf("Set-Cookie should be filtered, got %q", rec2.Header().Get("Set-Cookie"))
	}
	if rec2.Header().Get("X-Hop") != "" {
		t.Fatalf("Connection-listed header should be filtered, got %q", rec2.Header().Get("X-Hop"))
	}
	if rec2.Header().Get("Connection") != "" {
		t.Fatalf("Connection should be filtered, got %q", rec2.Header().Get("Connection"))
	}
	if calls != 1 {
		t.Fatalf("response should be cached, calls=%d", calls)
	}
}

func TestRedisCacheVaryVariants(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "Accept-Encoding")
		w.Write([]byte("encoding=" + r.Header.Get("Accept-Encoding")))
	}))

	reqGzip := httptest.NewRequest(http.MethodGet, "/vary", nil)
	reqGzip.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, reqGzip)
	if rec.Body.String() != "encoding=gzip" {
		t.Fatalf("gzip response: %q", rec.Body.String())
	}

	reqIdentity := httptest.NewRequest(http.MethodGet, "/vary", nil)
	reqIdentity.Header.Set("Accept-Encoding", "identity")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, reqIdentity)
	if rec2.Body.String() != "encoding=identity" {
		t.Fatalf("identity response: %q", rec2.Body.String())
	}
	if calls != 2 {
		t.Fatalf("different variants should each miss, calls=%d", calls)
	}

	reqGzip2 := httptest.NewRequest(http.MethodGet, "/vary", nil)
	reqGzip2.Header.Set("Accept-Encoding", "gzip")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, reqGzip2)
	if rec3.Body.String() != "encoding=gzip" {
		t.Fatalf("gzip cached response: %q", rec3.Body.String())
	}
	if rec3.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("Vary header should be replayed: %q", rec3.Header().Get("Vary"))
	}
	if calls != 2 {
		t.Fatalf("same variant should hit cache, calls=%d", calls)
	}
}

func TestRedisCacheVaryStarSkipsCache(t *testing.T) {
	client := newTestRedis(t)
	mw := RedisCache(client, time.Minute, nil)

	calls := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "*")
		w.Write([]byte("unstable"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vary-star", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first status: %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/vary-star", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status: %d", rec2.Code)
	}
	if calls != 2 {
		t.Fatalf("Vary: * should not be cached, calls=%d", calls)
	}
}
