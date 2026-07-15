// Package main provides an external callable result that cannot be proven closed.
package main

import "context"

func Effect() {}

type callbackHooks struct {
	callback func()
}

func (hooks *callbackHooks) install() func() {
	original := hooks.callback
	hooks.callback = func() {
		if original != nil {
			original()
		}
	}
	return func() {
		hooks.callback = original
	}
}

func main() {
	_, cancel := context.WithCancel(context.Background())
	hooks := callbackHooks{callback: cancel}
	restore := hooks.install()
	hooks.callback()
	restore()
}
