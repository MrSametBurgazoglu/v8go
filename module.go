// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"sync"
	"unsafe"
)

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

// moduleResolvers holds the on-demand module loader for each context, keyed
// by the C pointer the host hook hands back.
var moduleResolvers sync.Map // C.ContextPtr -> func(specifier, referrer string) bool

// SetModuleResolver installs the loader consulted when import() names a
// specifier the registry has never seen.
//
// A bundler's runtime builds its chunk URLs while it runs — webpack asks for
// `./chunk-93080-….js?&r=3`, a name no static scan of the source could have
// predicted — so a registry filled in advance is always one step behind it.
// The resolver is handed the specifier and the URL of the module that asked
// for it, and is expected to fetch the source and RegisterModule it (along
// with anything it statically imports) before returning true. Returning false
// leaves import() rejecting with "Cannot find module", which is what a real
// 404 should do.
//
// It is called synchronously from inside the host hook, on the thread already
// running JS, which is the same thread the embedder called RunScript on.
func (c *Context) SetModuleResolver(resolve func(specifier, referrer string) bool) {
	if c == nil || c.ptr == nil {
		return
	}
	if resolve == nil {
		moduleResolvers.Delete(c.ptr)
		return
	}
	moduleResolvers.Store(c.ptr, resolve)
}

//export goResolveModule
func goResolveModule(ctxPtr C.ContextPtr, specifier *C.char, referrer *C.char) C.int {
	entry, ok := moduleResolvers.Load(ctxPtr)
	if !ok {
		return 0
	}
	resolve, ok := entry.(func(string, string) bool)
	if !ok || !resolve(C.GoString(specifier), C.GoString(referrer)) {
		return 0
	}
	return 1
}
