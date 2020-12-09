package filters

import (
	"compress/gzip"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/http/protolet"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	metrics "github.com/buildbuddy-io/buildbuddy/server/metrics"
)

var (
	uuidV4Regexp = regexp.MustCompile("[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}")
)

func RedirectHTTPS(env environment.Env, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")

		protocol := r.Header.Get("X-Forwarded-Proto") // Set by load balancer
		if sslConfig := env.GetConfigurator().GetSSLConfig(); sslConfig != nil && sslConfig.EnableSSL && protocol == "http" {
			http.Redirect(w, r, "https://"+r.Host+r.URL.String(), http.StatusMovedPermanently)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// gzip, courtesy of https://gist.github.com/CJEnright/bc2d8b8dc0c1389a9feeddb110f822d7
var gzPool = sync.Pool{
	New: func() interface{} {
		w := gzip.NewWriter(ioutil.Discard)
		return w
	},
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		// Firefox will not decompress the response if the below header
		// is not also set.
		w.Header().Set("Transfer-Encoding", "chunked")

		gz := gzPool.Get().(*gzip.Writer)
		defer gzPool.Put(gz)

		gz.Reset(w)
		defer gz.Close()

		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

func Authenticate(env environment.Env, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := env.GetAuthenticator().AuthenticateHTTPRequest(w, r)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := uuid.SetInContext(r.Context())
		if err != nil {
			// Should never happen, but just in case, fail safe.
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// instrumentedResponseWriter wraps http.ResponseWriter, recording info needed for
// Prometheus metrics.
type instrumentedResponseWriter struct {
	http.ResponseWriter
	metrics responseMetrics
}

type responseMetrics struct {
	statusCode        int
	responseSizeBytes int
}

func (w *instrumentedResponseWriter) Write(bytes []byte) (int, error) {
	w.metrics.responseSizeBytes += len(bytes)
	return w.ResponseWriter.Write(bytes)
}
func (w *instrumentedResponseWriter) WriteHeader(statusCode int) {
	w.metrics.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func LogRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m := method(r)
		rt := route(r)
		recordRequestMetrics(rt, m)
		irw := &instrumentedResponseWriter{
			ResponseWriter: w,
			metrics: responseMetrics{
				// net/http defaults to 200 if not written.
				statusCode: 200,
			},
		}
		next.ServeHTTP(irw, r)
		duration := time.Since(start)
		log.LogHTTPRequest(r.Context(), r.URL.Path, duration, irw.metrics.statusCode, nil)
		recordResponseMetrics(rt, m, &irw.metrics, duration)
	})
}

func route(r *http.Request) string {
	path := r.URL.Path

	// TODO(bduffany): migrate to a routing solution that doesn't require
	// updating this function when we add new HTTP routes.

	// Strip prefixes on large static file directories to avoid
	// creating new metrics series for every static file that we serve.
	if strings.HasPrefix(path, "/image/") {
		return "/image/:path"
	}
	if strings.HasPrefix(path, "/favicon/") {
		return "/favicon/:path"
	}

	// Replace path parameters to avoid creating a series for
	// every possible parameter value.

	// Currently we only use UUID v4 path params so this
	// find-and-replace is good enough for now.
	return uuidV4Regexp.ReplaceAllLiteralString(path, ":id")
}

func method(r *http.Request) string {
	if r.Method == "" {
		return "GET"
	}
	return r.Method
}

func recordRequestMetrics(route, method string) {
	metrics.HTTPRequestCount.With(prometheus.Labels{
		metrics.HTTPRouteLabel:  route,
		metrics.HTTPMethodLabel: method,
	}).Inc()
}

func recordResponseMetrics(route, method string, rm *responseMetrics, duration time.Duration) {
	labels := prometheus.Labels{
		metrics.HTTPRouteLabel:        route,
		metrics.HTTPMethodLabel:       method,
		metrics.HTTPResponseCodeLabel: strconv.Itoa(rm.statusCode),
	}
	metrics.HTTPRequestHandlerDurationUsec.With(labels).Observe(float64(duration.Microseconds()))
	metrics.HTTPResponseSizeBytes.With(labels).Observe(float64(rm.responseSizeBytes))
}

type wrapFn func(http.Handler) http.Handler

func wrapHandler(env environment.Env, next http.Handler, wrapFns *[]wrapFn) http.Handler {
	handler := next
	for _, fn := range *wrapFns {
		handler = fn(handler)
	}
	return handler
}

func WrapAuthenticatedExternalProtoletHandler(env environment.Env, httpPrefix string, handlers *protolet.HTTPHandlers) http.Handler {
	return wrapHandler(env, handlers.RequestHandler, &[]wrapFn{
		Gzip,
		func(h http.Handler) http.Handler { return Authenticate(env, h) },
		// The request message is parsed before authentication since the request_context
		// field needs to be authenticated if it's present.
		func(h http.Handler) http.Handler {
			return http.StripPrefix(httpPrefix, handlers.BodyParserMiddleware(h))
		},
		func(h http.Handler) http.Handler { return RedirectHTTPS(env, h) },
		LogRequest,
		RequestID,
	})
}

func WrapExternalHandler(env environment.Env, next http.Handler) http.Handler {
	// NB: These are called in reverse order, so the 0th element will be
	// called last before the handler itself is called.
	return wrapHandler(env, next, &[]wrapFn{
		Gzip,
		func(h http.Handler) http.Handler { return RedirectHTTPS(env, h) },
		LogRequest,
		RequestID,
	})
}

func WrapAuthenticatedExternalHandler(env environment.Env, next http.Handler) http.Handler {
	// NB: These are called in reverse order, so the 0th element will be
	// called last before the handler itself is called.
	return wrapHandler(env, next, &[]wrapFn{
		Gzip,
		func(h http.Handler) http.Handler { return Authenticate(env, h) },
		func(h http.Handler) http.Handler { return RedirectHTTPS(env, h) },
		LogRequest,
		RequestID,
	})
}

func ServeGRPCOverHTTPPort(grpcServer *grpc.Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(
			r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			next.ServeHTTP(w, r)
		}
	})
}
