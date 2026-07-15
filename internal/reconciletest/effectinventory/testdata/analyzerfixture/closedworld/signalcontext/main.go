// Package main exercises os/signal.NotifyContext effect discovery.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func waitForTermination(parent context.Context) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
}

func main() {
	waitForTermination(context.Background())
}
