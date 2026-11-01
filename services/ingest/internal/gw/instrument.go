package gw

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tasktrooper/telemetry/services/ingest/internal/obs"
	"github.com/tasktrooper/telemetry/services/ingest/internal/selfobs"
)

// statusWriter records the response status and the ingest error (if any) for metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
	err    *ingestError
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// countingBody counts wire bytes read from the request body.
type countingBody struct {
	io.ReadCloser
	n int
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += n
	return n, err
}

// instrument wraps an ingest handler with request / byte / auth / rate-limit metrics. The endpoint label is the path.
// With self-telemetry on, it also registers a pending server span that starts once the tenant is known (s.begin).
func (s *Server) instrument(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		cb := &countingBody{ReadCloser: r.Body}
		r.Body = cb
		var req *selfobs.Request
		if s.tel != nil {
			var ctx context.Context
			ctx, req = s.tel.WithRequest(r.Context(), r.Method, r.URL.Path, false, start)
			r = r.WithContext(ctx)
		}
		h.ServeHTTP(sw, r)
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		if req != nil {
			var err error
			if sw.err != nil {
				err = sw.err
			}
			req.End(sw.status, 0, err)
		}
		observe(r.URL.Path, sw.status, sw.err, cb.n, start)
	})
}

func observe(endpoint string, status int, ie *ingestError, bytes int, start time.Time) {
	obs.GWRequests.WithLabelValues(endpoint, strconv.Itoa(status)).Inc()
	obs.GWRequestBytes.WithLabelValues(endpoint).Add(float64(bytes))
	obs.GWRequestDuration.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())
	switch {
	case ie == nil:
	case ie.authReason != "":
		obs.GWAuthFailures.WithLabelValues(endpoint, ie.authReason).Inc()
	case ie.status == http.StatusTooManyRequests:
		obs.GWRateLimited.WithLabelValues(endpoint).Inc()
	}
}
