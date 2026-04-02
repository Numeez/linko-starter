package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	"gopkg.in/natefinch/lumberjack.v2"

	"boot.dev/linko/internal"
	"boot.dev/linko/internal/store"
	pkgerr "github.com/pkg/errors"
)

type closeFunc func() error
type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}
type multiError interface {
	error
	Unwrap() []error
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logFile := os.Getenv("LINKO_LOG_FILE")
	logger, close, err := initializeLogger(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to flush logger writer: %v\n", err)
		}
	}()
	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Info(fmt.Sprintf("failed to create store: %v\n", err))
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		s.logger.Info(fmt.Sprintf("failed to shutdown server: %v\n", err))
		return 1
	}
	if serverErr != nil {
		s.logger.Info(fmt.Sprintf("server error: %v\n", serverErr))
		return 1
	}
	return 0
}

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	var (
		handlers []slog.Handler
		closers  []closeFunc
	)

	handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
		ReplaceAttr: replaceAttr,
		Level:       slog.LevelDebug,
		NoColor:     !(isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())),
	}))
	handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
		ReplaceAttr: replaceAttr,
		Level:       slog.LevelError,
		NoColor:     !(isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())),
	}))

	if logFile != "" {
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
			Level:       slog.LevelInfo,
		}))
		closers = append(closers, func() error {
			if err := logger.Close(); err != nil {
				return err
			}
			return nil
		})
	}

	close := func() error {
		var errs []error
		for _, closer := range closers {
			errs = append(errs, closer())
		}
		return errors.Join(errs...)
	}
	return slog.New(slog.NewMultiHandler(handlers...)), close, nil
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	var sensitiveKeys = []string{"password", "key", "apikey", "secret", "pin", "creditcardno", "username"}
	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Key == "error" {
		err, ok := a.Value.Any().(multiError)
		if ok {
			var errAttrs []slog.Attr
			for i, err := range err.Unwrap() {
				errAttrs = append(errAttrs, slog.String(fmt.Sprintf("error_%d", i+1), err.Error()))
			}
			return slog.GroupAttrs("errors", errAttrs...)
		}
		if _, ok := errors.AsType[stackTracer](err); ok {
			errorAttributes := internal.Attrs(err)
			return slog.GroupAttrs("error", errorAttributes...)
		}
		return slog.String("error", fmt.Sprintf("%+v", err))
	}
	return a

}

func getTintOptions(level slog.Leveler, replaceAttr func(groups []string, attr slog.Attr) slog.Attr, isFile bool) *tint.Options {
	if isFile {
		return &tint.Options{
			Level:       level,
			ReplaceAttr: replaceAttr,
			NoColor:     true,
		}

	}
	options := &tint.Options{
		Level:       level,
		ReplaceAttr: replaceAttr,
	}
	if isatty.IsCygwinTerminal(os.Stderr.Fd()) {
		options.NoColor = true
	}
	return options

}
