package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"

import (
	"sync"
	"unsafe"
)

// Weak handles.
//
// Every *Value is a strong Global by default: V8 can never collect what it
// names, and a Go-side cache of wrappers (a DOM node's JS object, say)
// therefore pins every object it ever handed out until the context closes.
// SetWeak turns one handle weak — V8 may collect the object once nothing in
// JS reaches it — and reports the collection to Go so the cache can forget
// it. ClearWeak makes it strong again.
//
// Contract: the callback runs on the isolate's thread, from inside the
// garbage collection that found the object unreachable, with the Global
// already reset. It must Release the Value (which frees the wrapper) and
// must not create V8 handles or run script. The isolate is single-threaded,
// so Go maps touched only from that thread need no lock of their own.

var weakHandlers = struct {
	sync.Mutex
	byPtr map[unsafe.Pointer]func()
}{byPtr: map[unsafe.Pointer]func(){}}

// SetWeak makes the handle weak: onCollected runs when V8 collects the
// value. A second SetWeak replaces the callback.
func (v *Value) SetWeak(onCollected func()) {
	if v == nil || v.ptr == nil || onCollected == nil {
		return
	}
	weakHandlers.Lock()
	weakHandlers.byPtr[unsafe.Pointer(v.ptr)] = onCollected
	weakHandlers.Unlock()
	C.ValueSetWeak(v.ptr)
}

// ClearWeak makes a weak handle strong again and drops its callback. A
// no-op on a handle that is not weak.
func (v *Value) ClearWeak() {
	if v == nil || v.ptr == nil {
		return
	}
	weakHandlers.Lock()
	_, weak := weakHandlers.byPtr[unsafe.Pointer(v.ptr)]
	delete(weakHandlers.byPtr, unsafe.Pointer(v.ptr))
	weakHandlers.Unlock()
	if weak {
		C.ValueClearWeak(v.ptr)
	}
}

// IsWeak reports whether SetWeak is in force on the handle.
func (v *Value) IsWeak() bool {
	if v == nil || v.ptr == nil {
		return false
	}
	weakHandlers.Lock()
	defer weakHandlers.Unlock()
	_, weak := weakHandlers.byPtr[unsafe.Pointer(v.ptr)]
	return weak
}

//export goWeakCallback
func goWeakCallback(ptr unsafe.Pointer) {
	weakHandlers.Lock()
	fn := weakHandlers.byPtr[ptr]
	delete(weakHandlers.byPtr, ptr)
	weakHandlers.Unlock()
	if fn != nil {
		fn()
	}
}
