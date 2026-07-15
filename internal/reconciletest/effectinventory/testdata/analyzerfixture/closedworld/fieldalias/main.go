// Package main proves that private field addresses can still alias writes.
package main

func Effect() {}

func safeCallback() {}

type callbackHolder struct {
	callback func()
}

func replace(callback *func()) {
	*callback = Effect
}

func main() {
	holder := callbackHolder{callback: safeCallback}
	replace(&holder.callback)
	holder.callback()
}
