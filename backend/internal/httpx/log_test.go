package httpx

import (
	"io"
	"log/slog"
)

func nil2() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
