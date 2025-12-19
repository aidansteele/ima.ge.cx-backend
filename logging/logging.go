package logging

import (
	"context"
	"log/slog"
	"os"

	slogctx "github.com/veqryn/slog-context"
)

// Init initializes the global slog logger with slog-context support.
// Call this at the start of main() in each Lambda.
func Init() {
	handler := slogctx.NewHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}),
		&slogctx.HandlerOptions{
			Prependers: []slogctx.AttrExtractor{
				slogctx.ExtractPrepended,
			},
			Appenders: []slogctx.AttrExtractor{
				slogctx.ExtractAppended,
			},
		},
	)
	slog.SetDefault(slog.New(handler))
}

// WithRequestPayload adds the Lambda request payload to the context for logging.
func WithRequestPayload(ctx context.Context, payload any) context.Context {
	return slogctx.Prepend(ctx, "request", payload)
}

// WithAttrs adds attributes to the context that will be prepended to all log messages.
func WithAttrs(ctx context.Context, args ...any) context.Context {
	return slogctx.Prepend(ctx, args...)
}
