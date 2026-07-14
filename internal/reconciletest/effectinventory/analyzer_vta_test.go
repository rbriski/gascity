package effectinventory

import (
	"go/token"
	"go/types"
	"testing"
)

func TestTypeUsesUnsafePointerTerminatesOnRecursiveTypes(t *testing.T) {
	name := types.NewTypeName(token.NoPos, nil, "node", nil)
	node := types.NewNamed(name, nil, nil)
	next := types.NewField(token.NoPos, nil, "next", types.NewPointer(node), false)
	node.SetUnderlying(types.NewStruct([]*types.Var{next}, nil))

	if typeUsesUnsafePointer(node) {
		t.Fatal("typeUsesUnsafePointer(recursive safe type) = true, want false")
	}

	unsafeName := types.NewTypeName(token.NoPos, nil, "unsafeAlias", nil)
	unsafeAlias := types.NewNamed(unsafeName, types.Typ[types.UnsafePointer], nil)
	if !typeUsesUnsafePointer(unsafeAlias) {
		t.Fatal("typeUsesUnsafePointer(named unsafe pointer) = false, want true")
	}
}
