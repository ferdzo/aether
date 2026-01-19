package logger

import (
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
)

var Log *slog.Logger

func init() {
	Log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

func Init(level slog.Level, json bool) {
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}

	if json {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = tint.NewHandler(os.Stdout, &tint.Options{
			Level:      level,
			TimeFormat: "15:04:05",
		})
	}

	Log = slog.New(handler)
	slog.SetDefault(Log)
}

func Debug(msg string, args ...any) { Log.Debug(msg, args...) }
func Info(msg string, args ...any)  { Log.Info(msg, args...) }
func Warn(msg string, args ...any)  { Log.Warn(msg, args...) }
func Error(msg string, args ...any) { Log.Error(msg, args...) }

func With(args ...any) *slog.Logger {
	return Log.With(args...)
}
