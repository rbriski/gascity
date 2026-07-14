// Package reflectchannel exercises every reflective channel operation that
// can cross an exact inventoried channel boundary.
package reflectchannel

import "reflect"

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target)

// ReflectSend sends to Approved through reflect.Value.Send.
func ReflectSend(target Target) {
	reflect.ValueOf(Approved).Send(reflect.ValueOf(target))
}

// ReflectTrySend sends to Approved through reflect.Value.TrySend.
func ReflectTrySend(target Target) bool {
	return reflect.ValueOf(Approved).TrySend(reflect.ValueOf(target))
}

// ReflectRecv receives from Approved through reflect.Value.Recv.
func ReflectRecv() (reflect.Value, bool) {
	return reflect.ValueOf(Approved).Recv()
}

// ReflectTryRecv receives from Approved through reflect.Value.TryRecv.
func ReflectTryRecv() (reflect.Value, bool) {
	return reflect.ValueOf(Approved).TryRecv()
}

// ReflectClose closes Approved through reflect.Value.Close.
func ReflectClose() {
	reflect.ValueOf(Approved).Close()
}

// ReflectSelectSend sends to Approved through reflect.Select.
func ReflectSelectSend(target Target) {
	reflect.Select([]reflect.SelectCase{
		{
			Dir:  reflect.SelectSend,
			Chan: reflect.ValueOf(Approved),
			Send: reflect.ValueOf(target),
		},
	})
}

// ReflectSelectRecv receives from Approved through reflect.Select.
func ReflectSelectRecv() {
	reflect.Select([]reflect.SelectCase{
		{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(Approved),
		},
	})
}
