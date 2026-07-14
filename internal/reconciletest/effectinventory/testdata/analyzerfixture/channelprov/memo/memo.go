// Package memo supplies a repeated channel-return call context. Each level
// calls the previous level twice with the same actual argument, forming an
// exponentially large path tree over a linear-size SSA graph.
package memo

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/callescape/externaldep"

// Approved is the exact channel boundary.
var Approved = make(chan string, 1)

func level0(channel chan string) chan string { return channel }

func level1(channel chan string) chan string {
	left, right := level0(channel), level0(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level2(channel chan string) chan string {
	left, right := level1(channel), level1(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level3(channel chan string) chan string {
	left, right := level2(channel), level2(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level4(channel chan string) chan string {
	left, right := level3(channel), level3(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level5(channel chan string) chan string {
	left, right := level4(channel), level4(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level6(channel chan string) chan string {
	left, right := level5(channel), level5(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level7(channel chan string) chan string {
	left, right := level6(channel), level6(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level8(channel chan string) chan string {
	left, right := level7(channel), level7(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level9(channel chan string) chan string {
	left, right := level8(channel), level8(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level10(channel chan string) chan string {
	left, right := level9(channel), level9(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level11(channel chan string) chan string {
	left, right := level10(channel), level10(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

func level12(channel chan string) chan string {
	left, right := level11(channel), level11(channel)
	if len(channel) == 0 {
		return left
	}
	return right
}

// Escape hands the repeated call result to an unauthored dependency.
func Escape() {
	externaldep.AcceptStringChannel(level12(Approved))
}
