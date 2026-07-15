// Package main exercises closed-world generic instantiations whose concrete
// type cannot carry an inventoried channel.
package main

import (
	"encoding/json"
	"sort"
)

// Approved is the exact channel boundary used by the fixture.
var Approved = make(chan string, 1)

type document struct {
	Name string `json:"name"`
}

func decode[T any](data []byte) error {
	var value T
	return json.Unmarshal(data, &value)
}

func register[T any](value *T) {
	sort.Slice([]int(nil), func(_, _ int) bool {
		_, _ = json.Marshal(value)
		return false
	})
}

func main() {
	_ = decode[document](nil)
	register(&document{})
}
