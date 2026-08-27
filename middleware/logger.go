package middleware

import (
	"net/http"
	"time"

	"github.com/rambollwong/rainbowlog/level"
	"github.com/rambollwong/rainbowlog/log"
)

// responseWriter wraps http.ResponseWriter to capture the status code and the
// number of bytes written to the response body.
//
// responseWriter 包装 http.ResponseWriter，用于捕获状态码和写入响应体的字节数。
type responseWriter struct {
	http.ResponseWriter
	status int
	size   int
}

// WriteHeader captures the status code and delegates to the underlying writer.
// WriteHeader 捕获状态码并委托给底层 writer。
func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Write captures the number of bytes written. If WriteHeader has not been
// called explicitly it defaults to 200 OK.
//
// Write 捕获写入的字节数。若 WriteHeader 未被显式调用则默认为 200 OK。
func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

func RainbowLogger(logger *log.Logger, level level.Level) func(next http.Handler) http.Handler {
	l := logger
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w}
			next.ServeHTTP(rw, r)
			// l.Record().WithLevel(level).IgnoreCaller().
			// 	Str("method", r.Method).
			// 	Str("uri", r.URL.RequestURI()).
			// 	Int("status", rw.status).
			// 	Dur("use", time.Microsecond, time.Since(start).Truncate(time.Microsecond)).
			// 	Int("size(B)", rw.size).
			// 	Done()
			l.Record().WithLevel(level).IgnoreCaller().
				Msgf("%s %s %d %v %dB",
					r.Method,
					r.URL.RequestURI(),
					rw.status,
					time.Since(start).Truncate(time.Microsecond),
					rw.size,
				).Done()
		})
	}
}
