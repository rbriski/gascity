// Package main provides closed callable-provenance shapes matching production.
package main

import (
	"context"
	"net/http"
)

func ErrorEffect() error { return nil }

func ContextEffect(context.Context) error { return nil }

func StringEffect(string) error { return nil }

func safeString(string) error { return nil }

type spawnDependencies struct {
	start func() (func() error, error)
}

func closedStart() (func() error, error) {
	return ErrorEffect, nil
}

func startReserved(dependencies spawnDependencies) {
	wait, err := dependencies.start()
	if err != nil {
		return
	}
	go func() { _ = wait() }()
}

type directoryHooks struct {
	before func(string) error
}

func (hooks *directoryHooks) install(before func(string) error) func() {
	original := hooks.before
	hooks.before = func(path string) error {
		if original != nil {
			if err := original(path); err != nil {
				return err
			}
		}
		if before != nil {
			return before(path)
		}
		return nil
	}
	return func() {
		hooks.before = original
	}
}

func (hooks directoryHooks) opening(path string) error {
	if hooks.before == nil {
		return nil
	}
	return hooks.before(path)
}

type provider struct {
	shutdowns []func(context.Context) error
}

func (p *provider) addShutdown(shutdown func(context.Context) error) {
	p.shutdowns = append(p.shutdowns, shutdown)
}

func (p *provider) shutdown(ctx context.Context) {
	for _, shutdown := range p.shutdowns {
		_ = shutdown(ctx)
	}
}

func main() {
	startReserved(spawnDependencies{start: closedStart})

	hooks := directoryHooks{before: StringEffect}
	restore := hooks.install(safeString)
	_ = hooks.opening("fixture")
	restore()

	server := &http.Server{}
	p := &provider{}
	p.addShutdown(ContextEffect)
	p.addShutdown(server.Shutdown)
	p.shutdown(context.Background())
}
