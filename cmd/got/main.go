package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/yougg/got/internal/cli"
)

func main() {
	initLogger()

	app := cli.NewApp()
	code, err := app.Run(context.Background(), os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "got: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func initLogger() {
	level := slog.LevelWarn
	if isDebugEnabled(os.Getenv("GOT_DEBUG")) {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(handler))
}

func isDebugEnabled(val string) bool {
	switch strings.ToLower(strings.TrimSpace(val)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
