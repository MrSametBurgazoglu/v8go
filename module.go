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

// DynamicImport is one import() call as the host hook receives it: the
// specifier as written, the URL of the script or module that made the call,
// and the import attributes of its options argument (`with: {type: "json"}`
// is Attributes["type"] == "json").
type DynamicImport struct {
	Specifier  string
	Referrer   string
	Attributes map[string]string
}

// DynamicImportErrorKind is the class of error an import() rejects with.
type DynamicImportErrorKind int

const (
	// DynamicImportError rejects with a plain Error.
	DynamicImportError DynamicImportErrorKind = iota
	// DynamicImportTypeError rejects with a TypeError: what HTML has for a
	// module that could not be fetched, a MIME type that does not match its
	// module type, and an unknown module type.
	DynamicImportTypeError
	// DynamicImportSyntaxError rejects with a SyntaxError: what HTML has for
	// an import attribute it does not support, and for a JSON module whose
	// body does not parse.
	DynamicImportSyntaxError
)

// DynamicImportFailure is a resolver's refusal of an import().
type DynamicImportFailure struct {
	Kind    DynamicImportErrorKind
	Message string
}

// dynamicImportResolvers holds the import() resolver for each context.
var dynamicImportResolvers sync.Map // C.ContextPtr -> func(DynamicImport) (string, *DynamicImportFailure)

// SetDynamicImportResolver installs the embedder's answer to every import()
// in the context, with its import attributes.
//
// The resolver returns the registry key the module was (or already is)
// registered under — a module's identity in HTML is its URL AND its module
// type, so `import(u)` and `import(u, {with: {type: "json"}})` are two
// modules and need two keys — or a failure, which the promise is rejected
// with as an error of that kind. An empty key with no failure means the
// specifier itself, looked up as before.
//
// Without it, import() consults the registry by specifier and falls back to
// SetModuleResolver for one it has not seen; the attributes are dropped and a
// miss rejects with a plain Error. With it, SetModuleResolver is not asked
// about import() at all.
func (c *Context) SetDynamicImportResolver(resolve func(DynamicImport) (string, *DynamicImportFailure)) {
	if c == nil || c.ptr == nil {
		return
	}
	if resolve == nil {
		dynamicImportResolvers.Delete(c.ptr)
		return
	}
	dynamicImportResolvers.Store(c.ptr, resolve)
}

//export goResolveDynamicImport
func goResolveDynamicImport(ctxPtr C.ContextPtr, specifier *C.char, referrer *C.char,
	attrs **C.char, nattrs C.int, outKey **C.char, outErrKind *C.int, outErrMsg **C.char) C.int {
	entry, ok := dynamicImportResolvers.Load(ctxPtr)
	if !ok {
		return 0
	}
	resolve, ok := entry.(func(DynamicImport) (string, *DynamicImportFailure))
	if !ok {
		return 0
	}
	request := DynamicImport{Specifier: C.GoString(specifier), Referrer: C.GoString(referrer)}
	if nattrs > 0 {
		pairs := unsafe.Slice(attrs, int(nattrs)*2)
		request.Attributes = make(map[string]string, int(nattrs))
		for i := 0; i < int(nattrs); i++ {
			request.Attributes[C.GoString(pairs[2*i])] = C.GoString(pairs[2*i+1])
		}
	}
	key, failure := resolve(request)
	if failure != nil {
		*outErrKind = C.int(failure.Kind)
		*outErrMsg = C.CString(failure.Message)
		return 2
	}
	if key != "" {
		*outKey = C.CString(key)
	}
	return 1
}
