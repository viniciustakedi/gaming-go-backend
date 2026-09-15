// Package logging wires log/slog into JSON output and into Fx's own event
// stream, so framework lifecycle events (constructor calls, hook timings,
// start/stop) show up in the same structured log as everything else.
package logging

import (
	"log/slog"
	"os"

	"go.uber.org/fx/fxevent"

	"github.com/viniciustakedi/jungle-gaming-wallet/internal/config"
)

// New builds the process-wide JSON logger. Level comes from validated
// config, so an unparseable level can never reach this constructor.
func New(cfg config.Config) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(cfg.Log.Level),
	})
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// FxLogger adapts the slog logger to fx.WithLogger, so Fx prints its own
// construction and lifecycle events through the same JSON writer instead of
// its default stderr text logger.
func FxLogger(logger *slog.Logger) fxevent.Logger {
	return &fxevent.SlogLogger{Logger: logger}
}
