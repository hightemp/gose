package main

import (
	"log/slog"
	"os"
	"strings"
)

var Log *slog.Logger

func init() {
	Log = newLoggerFromEnv()
}

func newLoggerFromEnv() *slog.Logger {
	lvl := parseLevel(strings.TrimSpace(os.Getenv("CRAWLER_LOG_LEVEL")))
	if lvl == nil {
		if isTruthy(os.Getenv("CRAWLER_DEBUG")) {
			l := slog.LevelDebug
			lvl = &l
		} else {
			l := slog.LevelInfo
			lvl = &l
		}
	}
	format := strings.ToLower(strings.TrimSpace(os.Getenv("CRAWLER_LOG_FORMAT")))
	addSource := isTruthy(os.Getenv("CRAWLER_LOG_SOURCE"))

	if format == "json" {
		h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:     lvl,
			AddSource: addSource,
		})
		return slog.New(h)
	}

	// default: console text handler
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:     lvl,
		AddSource: addSource,
	})
	return slog.New(h)
}

func parseLevel(s string) *slog.Level {
	switch strings.ToUpper(s) {
	case "DEBUG":
		l := slog.LevelDebug
		return &l
	case "INFO":
		l := slog.LevelInfo
		return &l
	case "WARN", "WARNING":
		l := slog.LevelWarn
		return &l
	case "ERROR":
		l := slog.LevelError
		return &l
	case "":
		return nil
	default:
		return nil
	}
}

func isTruthy(s string) bool {
	v := strings.ToLower(strings.TrimSpace(s))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// Convenience wrappers

func Debug(msg string, args ...any) { Log.Debug(msg, args...) }
func Info(msg string, args ...any)  { Log.Info(msg, args...) }
func Warn(msg string, args ...any)  { Log.Warn(msg, args...) }
func Error(msg string, args ...any) { Log.Error(msg, args...) }
