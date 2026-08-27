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
