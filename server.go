package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	_ "net/http/pprof"
	"os"
	"time"

	"boot.dev/linko/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

type trackReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (r *trackReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

type trackResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (rw *trackResponseWriter) Write(p []byte) (int, error) {
	if rw.statusCode == 0 {
		rw.statusCode = http.StatusOK
	}
	n, err := rw.ResponseWriter.Write(p)
	rw.bytesWritten += n
	return n, err
}

func (rw *trackResponseWriter) WriteHeader(statusCode int) {
	rw.statusCode = statusCode
	rw.ResponseWriter.WriteHeader(statusCode)
}

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			trackerReadeCloser := &trackReadCloser{
				ReadCloser: r.Body,
			}
			trackerResponseWriter := &trackResponseWriter{
				ResponseWriter: w,
			}
			r.Body = trackerReadeCloser
			logContext := context.WithValue(r.Context(), logContextKey, &LogContext{})
			r = r.WithContext(logContext)
			next.ServeHTTP(trackerResponseWriter, r)
			clientIp, err := redactIp(r.RemoteAddr)
			logger.Debug("fetching clientIp from redactIp failed", []slog.Attr{
				slog.Any("error", err),
			},
			)
			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", clientIp),
				slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", trackerReadeCloser.bytesRead),
				slog.Int("response_body_bytes", trackerResponseWriter.bytesWritten),
				slog.Int("response_status", trackerResponseWriter.statusCode),
			}
			logContextValue, ok := r.Context().Value(logContextKey).(*LogContext)
			if ok {
				username := logContextValue.Username
				if username != "" {
					attrs = append(attrs, slog.String("username", username))
				}
				logContextError := logContextValue.Error
				if logContextError != nil {
					attrs = append(attrs, slog.Any("ERROR", logContextError.Error()))
				}
			}
			requestId := trackerResponseWriter.ResponseWriter.Header().Get("X-Request-ID")
			attrs = append(attrs, slog.String("request_id", requestId))
			logger.Info("Served request", attrs...)

		})
	}
}

func newServer(store store.Store, port int, cancel context.CancelFunc, logger *slog.Logger) *server {
	mux := http.NewServeMux()
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: metricsMiddleware(requestIdMiddleware(requestLogger(logger)(mux))),
	}

	s := &server{
		httpServer: srv,
		store:      store,
		cancel:     cancel,
		logger:     logger,
	}
	mux.Handle("GET debug/pprof", s.authMiddleware(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET debug/pprof/profile", s.authMiddleware(http.HandlerFunc(pprof.Profile)))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /", http.HandlerFunc(s.handlerIndex))
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)

	return s
}

func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	s.logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%d", ln.Addr().(*net.TCPAddr).Port))
	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) shutdown(ctx context.Context) error {
	s.logger.Debug("Linko is shutting down")
	return s.httpServer.Shutdown(ctx)
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
}

func redactIp(address string) (string, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("invalid IP")
	}

	if ipv4 := ip.To4(); ipv4 != nil {
		return fmt.Sprintf("%d.%d.x.x", ipv4[0], ipv4[1]), nil
	}

	return "ipv6_redacted", nil
}
