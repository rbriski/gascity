//go:build windows

package main

func terminateManagedDoltTestProcessGroup(_ int) (bool, error) {
	return false, nil
}
