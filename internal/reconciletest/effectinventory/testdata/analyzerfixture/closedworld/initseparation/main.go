// Package main provides a closed-executable analyzer fixture.
package main

var initCallback = safeCallback

func Effect() {}

func safeCallback() {}

func init() {
	initCallback()
}

func runtimeRoute() {
	Effect()
}

func main() {
	runtimeRoute()
}
