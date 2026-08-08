// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import "unsafe"

// RegisterModule registers ES module source under the given specifier on this
// context, so `await import(spec)` run via this context's RunScript resolves
// to a compiled and evaluated module whose namespace carries the module's
// exports.
//
// spec is matched verbatim against the string JS passes to import():
// import('m.js') looks up "m.js". source must be a valid ES module body
// (e.g. "export const x = 42"); a top-level await in the module is not
// supported by the minimal in-memory loader.
func (c *Context) RegisterModule(spec string, source string) {
	cSpec := C.CString(spec)
	cSource := C.CString(source)
	defer C.free(unsafe.Pointer(cSpec))
	defer C.free(unsafe.Pointer(cSource))
	C.ContextRegisterModule(c.ptr, cSpec, cSource)
}
