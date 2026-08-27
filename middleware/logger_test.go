package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rambollwong/rainbowlog/level"
	"github.com/rambollwong/rainbowlog/log"
)

// ============================== responseWriter tests ==============================

func TestResponseWriter_WriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}

	rw.WriteHeader(http.StatusNotFound)

	if rw.status != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rw.status)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("underlying recorder status: expected %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestResponseWriter_Write(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}
	rw.WriteHeader(http.StatusOK)

	body := []byte("hello world")
	n, err := rw.Write(body)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(body) {
		t.Fatalf("expected %d bytes written, got %d", len(body), n)
	}
	if rw.size != len(body) {
		t.Fatalf("expected size %d, got %d", len(body), rw.size)
	}
}

func TestResponseWriter_WriteDefaultStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}

	// Write without calling WriteHeader first — should default to 200.
	n, err := rw.Write([]byte("ok"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 bytes written, got %d", n)
	}
	if rw.status != http.StatusOK {
		t.Fatalf("expected default status 200, got %d", rw.status)
	}
}

func TestResponseWriter_WriteAccumulatesSize(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}

	rw.Write([]byte("abc"))
	rw.Write([]byte("def"))

	if rw.size != 6 {
		t.Fatalf("expected accumulated size 6, got %d", rw.size)
	}
}

// ============================== RainbowLogger tests ==============================

// newTestLogger creates a logger that writes JSON to buf so we can inspect it.
// Meta keys include level so it appears in the JSON output.
func newTestLogger(buf *bytes.Buffer, lv level.Level) *log.Logger {
	return log.New(
		log.WithDefault(),
		log.WithLevel(lv),
		log.AppendsEncoderWriters(log.JsonEnc, buf),
	)
}

func TestRainbowLogger_CallsNextHandler(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := newTestLogger(buf, level.Info)
	middleware := RainbowLogger(logger, level.Info)

	called := false
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler was not called")
	}
}

func TestRainbowLogger_LogsMethodAndURI(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := newTestLogger(buf, level.Info)
	middleware := RainbowLogger(logger, level.Info)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	output := buf.String()
	if !strings.Contains(output, "POST") {
		t.Fatalf("expected log to contain method POST, got: %s", output)
	}
	if !strings.Contains(output, "/api/v1/users") {
		t.Fatalf("expected log to contain URI /api/v1/users, got: %s", output)
	}
}

func TestRainbowLogger_LogsStatus(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := newTestLogger(buf, level.Info)
	middleware := RainbowLogger(logger, level.Info)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	output := buf.String()
	if !strings.Contains(output, "201") {
		t.Fatalf("expected log to contain status 201, got: %s", output)
	}
}

func TestRainbowLogger_LogsDefault200(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := newTestLogger(buf, level.Info)
	middleware := RainbowLogger(logger, level.Info)

	// Handler that does NOT call WriteHeader — should default to 200.
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	output := buf.String()
	if !strings.Contains(output, "200") {
		t.Fatalf("expected log to contain default status 200, got: %s", output)
	}
}

func TestRainbowLogger_LogsResponseSize(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := newTestLogger(buf, level.Info)
	middleware := RainbowLogger(logger, level.Info)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	output := buf.String()
	if !strings.Contains(output, "5B") {
		t.Fatalf("expected log to contain size 5B, got: %s", output)
	}
}

func TestRainbowLogger_UsesGivenLevel(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := newTestLogger(buf, level.Debug)
	middleware := RainbowLogger(logger, level.Warn)

	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	output := buf.String()

	// Check that the level key in JSON output is "WARN" (the level passed to RainbowLogger).
	if !strings.Contains(output, `"WARN"`) && !strings.Contains(output, `WAR`) {
		t.Fatalf("expected log level to be warn, got: %s", output)
	}
}
