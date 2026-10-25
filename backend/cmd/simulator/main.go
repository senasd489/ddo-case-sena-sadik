// Command simulator generates a deterministic stream of smart-meter readings,
// transformer head-meter readings, outage events and suspicious consumption
// patterns and pushes them to the API. The same SIM_SEED always produces the
// same data. (Implemented in step 8.)
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "simulator")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("simulator started", "api", os.Getenv("API_URL"), "seed", os.Getenv("SIM_SEED"))
	<-ctx.Done()
	log.Info("simulator stopped")
}
