// Package main provides a closed-executable analyzer fixture.
package main

var initCallback = initRoute

func Effect() {}

func initRoute() {
	Effect()
}

func init() {
	initCallback()
}

func main() {}
