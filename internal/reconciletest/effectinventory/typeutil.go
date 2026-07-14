package effectinventory

import "go/types"

// objectRefForFunction builds the stable ObjectRef identity for a resolved
// function or method: its package path, receiver type name (empty for plain
// functions), and its own name.
func objectRefForFunction(function *types.Func) ObjectRef {
	reference := ObjectRef{Name: function.Name()}
	if function.Pkg() != nil {
		reference.Package = function.Pkg().Path()
	}
	if signature, ok := function.Type().(*types.Signature); ok && signature.Recv() != nil {
		reference.Receiver = receiverTypeName(signature.Recv().Type())
	}
	return reference
}

// receiverTypeName returns the bare named-type name of a method receiver,
// unwrapping pointer and alias types. It returns the empty string for receivers
// without a named type.
func receiverTypeName(receiver types.Type) string {
	receiver = types.Unalias(receiver)
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = types.Unalias(pointer.Elem())
	}
	if named, ok := receiver.(*types.Named); ok && named.Obj() != nil {
		return named.Obj().Name()
	}
	return ""
}
