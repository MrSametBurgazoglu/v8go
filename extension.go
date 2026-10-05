package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"

import (
	"fmt"
	"unicode/utf8"
	"unsafe"
)

// RegisterExtension registers a V8 extension: a script V8 compiles as
// EMBEDDER code and runs while building every context of an isolate that
// installs it (Isolate.SetContextExtensions). Functions its source defines are
// not user JavaScript to V8, so Function.prototype.toString answers
// "function f() { [native code] }" for them, as for any built-in -- the
// embedder's way to implement built-ins in JavaScript.
//
// Registration is process-wide and permanent, and must happen before a
// context installing the extension is created. V8 reads an extension's source
// as one-byte (Latin-1) text: source is UTF-8 and every character in it must
// be at most U+00FF, which is transcoded; anything wider is an error rather
// than a silently mangled script. deps names extensions this one needs
// installed first.
func RegisterExtension(name, source string, deps ...string) error {
	latin1 := make([]byte, 0, len(source))
	for i, r := range source {
		if r == utf8.RuneError {
			return fmt.Errorf("v8go: extension %q: invalid UTF-8 at byte %d", name, i)
		}
		if r > 0xFF {
			return fmt.Errorf("v8go: extension %q: %U at byte %d is not Latin-1", name, r, i)
		}
		latin1 = append(latin1, byte(r))
	}
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	var csrc *C.char
	if len(latin1) > 0 {
		csrc = (*C.char)(C.CBytes(latin1))
		defer C.free(unsafe.Pointer(csrc))
	} else {
		csrc = C.CString("")
		defer C.free(unsafe.Pointer(csrc))
	}
	cdeps, free := cStrings(deps)
	defer free()
	C.RegisterExtensionSource(cname, csrc, C.int(len(latin1)), cdeps, C.int(len(deps)))
	return nil
}

// SetContextExtensions names the extensions every context created in this
// isolate from now on installs. Nil restores the default. An extension whose
// top level throws makes context creation fail, so register only sources that
// cannot.
func (i *Isolate) SetContextExtensions(names []string) {
	cnames, free := cStrings(names)
	defer free()
	C.IsolateSetContextExtensions(i.ptr, cnames, C.int(len(names)))
}

// cStrings is a C array of C strings and the function that frees both.
func cStrings(list []string) (**C.char, func()) {
	if len(list) == 0 {
		return nil, func() {}
	}
	arr := C.malloc(C.size_t(len(list)) * C.size_t(unsafe.Sizeof(uintptr(0))))
	ptrs := unsafe.Slice((**C.char)(arr), len(list))
	for i, s := range list {
		ptrs[i] = C.CString(s)
	}
	return (**C.char)(arr), func() {
		for _, p := range ptrs {
			C.free(unsafe.Pointer(p))
		}
		C.free(arr)
	}
}

// SetName sets the function's own name: the one Function.prototype.toString
// prints for a built-in, and the one stack traces use. It belongs to the
// function's code, so every closure made from the same literal shares it.
func (fn *Function) SetName(name string) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	C.FunctionSetName(fn.ptr, cname, C.int(len(name)))
}
