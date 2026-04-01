package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

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

func initializeLogger(logFile string) (*slog.Logger, func() error, error) {
	if logFile == "" {
		return nil, func() error { return nil }, errors.New("empty log file name")
	}
	file, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	bufferedWriter := bufio.NewWriterSize(file, 8192)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	writer := io.MultiWriter(bufferedWriter, os.Stderr)
	close := func() error {
		if err := bufferedWriter.Flush(); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return nil
	}
	infoHandler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: replaceAttr,
	})
	debugHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
	})
	errorHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelError,
		ReplaceAttr: replaceAttr,
	})
	return slog.New(slog.NewMultiHandler(infoHandler, debugHandler, errorHandler)), close, nil
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
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
