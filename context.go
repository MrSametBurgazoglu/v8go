// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"runtime"
	"sync"
	"unsafe"
)

// Due to the limitations of passing pointers to C from Go we need to create
// a registry so that we can lookup the Context from any given callback from V8.
// This is similar to what is described here: https://github.com/golang/go/wiki/cgo#function-variables
type ctxRef struct {
	ctx      *Context
	refCount int
}

var ctxMutex sync.RWMutex
var ctxRegistry = make(map[int]*ctxRef)
var ctxSeq = 0

// Context is a global root execution environment that allows separate,
// unrelated, JavaScript applications to run in a single instance of V8.
type Context struct {
	ref int
	ptr C.ContextPtr
	iso *Isolate
}

type contextOptions struct {
	iso   *Isolate
	gTmpl *ObjectTemplate
}

// ContextOption sets options such as Isolate and Global Template to the NewContext
type ContextOption interface {
	apply(*contextOptions)
}

// NewContext creates a new JavaScript context; if no Isolate is passed as a
// ContextOption than a new Isolate will be created.
// Ref is this context's reference number, which identifies it to KeepGlobal
// and NewContextAdoptingGlobal.
func (c *Context) Ref() int {
	if c == nil {
		return 0
	}
	return int(c.ref)
}

// KeepGlobal detaches this context's global proxy and parks it under this
// context's ref for a later NewContextAdoptingGlobal naming that ref, which is
// how one browsing context keeps one `window` across a change of document. The
// context stays valid and the caller closes it as usual, immediately
// afterwards.
//
// A detached context must not run script again.
func (c *Context) KeepGlobal() bool {
	if c == nil || c.ptr == nil {
		return false
	}
	kept := C.ContextKeepGlobal(c.ptr, C.int(c.ref)) != 0
	runtime.KeepAlive(c)
	return kept
}

// NewContextAdoptingGlobal is NewContext built around the global proxy a
// previous KeepGlobal parked under adoptFrom — the ref of the context being
// replaced. With nothing parked, or adoptFrom 0, it is NewContext.
//
// V8 adopts the proxy only when gTmpl is compatible with the template the
// proxy was made for, and silently mints a fresh one otherwise — assert
// identity where it matters rather than assuming it.
func NewContextAdoptingGlobal(iso *Isolate, gTmpl *ObjectTemplate, adoptFrom int) *Context {
	if iso == nil {
		iso = NewIsolate()
	}
	if gTmpl == nil {
		gTmpl = &ObjectTemplate{&template{}}
	}

	ctxMutex.Lock()
	ctxSeq++
	ref := ctxSeq
	ctxMutex.Unlock()

	ctx := &Context{
		ref: ref,
		ptr: C.NewContextAdoptingGlobal(iso.ptr, gTmpl.ptr, C.int(ref), C.int(adoptFrom)),
		iso: iso,
	}
	ctx.register()
	runtime.KeepAlive(gTmpl)
	return ctx
}

func NewContext(opt ...ContextOption) *Context {
	opts := contextOptions{}
	for _, o := range opt {
		if o != nil {
			o.apply(&opts)
		}
	}

	if opts.iso == nil {
		opts.iso = NewIsolate()
	}

	if opts.gTmpl == nil {
		opts.gTmpl = &ObjectTemplate{&template{}}
	}

	ctxMutex.Lock()
	ctxSeq++
	ref := ctxSeq
	ctxMutex.Unlock()

	ctx := &Context{
		ref: ref,
		ptr: C.NewContext(opts.iso.ptr, opts.gTmpl.ptr, C.int(ref)),
		iso: opts.iso,
	}
	ctx.register()
	runtime.KeepAlive(opts.gTmpl)
	return ctx
}

// Isolate gets the current context's parent isolate.
func (c *Context) Isolate() *Isolate {
	return c.iso
}

func (c *Context) RetainedValueCount() int {
	ctxMutex.Lock()
	defer ctxMutex.Unlock()
	return int(C.ContextRetainedValueCount(c.ptr))
}

// InUse reports whether the context is being worked in further up the stack:
// a script or call entered it, or one of its functions is running a Go
// callback. Close must not be called then — the frames below still write
// through the context and release values tracked in it on their way out — so
// an embedder that may tear a context down from inside script asks first. A
// closed context is not in use.
func (c *Context) InUse() bool {
	if c == nil || c.ptr == nil {
		return false
	}
	return C.ContextInUse(c.ptr) != 0
}

// RunScript executes the source JavaScript; origin (a.k.a. filename) provides a
// reference for the script and used in the stack trace if there is an error.
// error will be of type `JSError` if not nil.
func (c *Context) RunScript(source string, origin string) (*Value, error) {
	cSource := C.CString(source)
	cOrigin := C.CString(origin)
	defer C.free(unsafe.Pointer(cSource))
	defer C.free(unsafe.Pointer(cOrigin))

	rtn := C.RunScript(c.ptr, cSource, C.int(len(source)), cOrigin)
	return valueResult(c, rtn)
}

// TakeException returns the value thrown by the most recent JavaScript
// exception that came back to Go as a JSError on this context's isolate, and
// forgets it; nil when it was already taken or has been collected. Call it
// straight after the call that failed: the next JSError replaces it.
func (c *Context) TakeException() *Value {
	ptr := C.ContextTakeException(c.ptr)
	if ptr == nil {
		return nil
	}
	return &Value{ptr, c}
}

// Global returns the global proxy object.
// Global proxy object is a thin wrapper whose prototype points to actual
// context's global object with the properties like Object, etc. This is
// done that way for security reasons.
// Please note that changes to global proxy object prototype most probably
// would break the VM — V8 expects only global object as a prototype of
// global proxy object.
func (c *Context) Global() *Object {
	valPtr := C.ContextGlobal(c.ptr)
	v := &Value{valPtr, c}
	return &Object{v}
}

// PrivateSymbol is the private symbol named name, as a value an embedder's
// own script can key a property by: `obj[key]` reads and writes a property no
// other script can list or reach -- Reflect.ownKeys, getOwnPropertySymbols
// and a Proxy all skip it, and Symbol.for never answers it. It is the same
// private Object.SetPrivate keys by the same name, throughout the isolate.
//
// A private-keyed property is found only on the object that owns it: a read
// does not walk the prototype chain. Anyone holding the value can use it, so
// it must never be handed to script the embedder does not trust. The handle
// is owned by the caller.
func (c *Context) PrivateSymbol(name string) *Value {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return &Value{C.ContextPrivateSymbol(c.ptr, cname), c}
}

// PrivateSymbolFunction is a function of this context that answers
// PrivateSymbol(name) for a string argument, without a crossing into Go: for
// the embedder's own scripts to mint their keys by. Called as (description,
// true) it answers a FRESH private symbol instead, unique per call -- the
// private counterpart of Symbol(description). It must never reach script
// the embedder does not trust. The handle is owned by the caller.
func (c *Context) PrivateSymbolFunction() (*Function, error) {
	val, err := valueResult(c, C.ContextPrivateSymbolFunction(c.ptr))
	if err != nil {
		return nil, err
	}
	return val.AsFunction()
}

// SecurityToken is the value V8 compares when script in one context reaches
// into an object belonging to another. Contexts with tokens that are not the
// same object cannot read each other's globals: the access check fires and the
// read throws. That is what a browser wants between cross-origin documents.
//
// The handle is owned by the caller.
func (c *Context) SecurityToken() *Value {
	valPtr := C.ContextSecurityToken(c.ptr)
	return &Value{valPtr, c}
}

// SetSecurityToken makes this context share token's identity, so that a context
// holding the same token may reach into this one's objects — the same-origin
// relationship two documents of one site have, and what an embedder needs for a
// same-origin iframe. A nil token restores V8's default, which is a token
// unique to this context and so reachable from no other.
func (c *Context) SetSecurityToken(token *Value) {
	var ptr C.ValuePtr
	if token != nil {
		ptr = token.ptr
	}
	C.ContextSetSecurityToken(c.ptr, ptr)
	runtime.KeepAlive(token)
}

// AllowCodeGenerationFromStrings says whether script in this context may
// turn a string into code: eval, new Function, the string forms of
// setTimeout/setInterval that an embedder builds on them. Refused, each throws
// an EvalError whose message is message — the hook an embedder's Content
// Security Policy ("script-src" without 'unsafe-eval') needs.
func (c *Context) AllowCodeGenerationFromStrings(allow bool, message string) {
	flag := C.int(0)
	if allow {
		flag = 1
	}
	var cmsg *C.char
	if !allow && message != "" {
		cmsg = C.CString(message)
		defer C.free(unsafe.Pointer(cmsg))
	}
	C.ContextAllowCodeGeneration(c.ptr, flag, cmsg)
}

// SetCodeGenerationRefusedHandler registers cb to hear each string
// compilation refused in a context of this isolate that refuses them
// (Context.AllowCodeGenerationFromStrings(false, ...)): eval, new Function.
// The refusal stands whatever cb does; cb must not run script.
func (i *Isolate) SetCodeGenerationRefusedHandler(cb func(*Context)) {
	if i.ptr == nil {
		return
	}
	codeGenerationCallbacks.Store(i.ptr, cb)
	C.IsolateSetCodeGenerationRefusedCallback(i.ptr)
}

var codeGenerationCallbacks sync.Map

//export goCodeGenerationRefused
func goCodeGenerationRefused(iso C.IsolatePtr, ctxRef C.int) {
	v, ok := codeGenerationCallbacks.Load(iso)
	if !ok {
		return
	}
	if cb, ok := v.(func(*Context)); ok {
		cb(getContext(int(ctxRef)))
	}
}

// PerformMicrotaskCheckpoint runs the default MicrotaskQueue until empty.
// This is used to make progress on Promises.
func (c *Context) PerformMicrotaskCheckpoint() {
	C.IsolatePerformMicrotaskCheckpoint(c.iso.ptr)
}

// Close will dispose the context and free the memory.
// Access to any values associated with the context after calling Close may panic.
func (c *Context) Close() {
	c.deregister()
	// The resolvers are keyed by the pointer about to be freed, and a later
	// context can be given the same address.
	moduleResolvers.Delete(c.ptr)
	dynamicImportResolvers.Delete(c.ptr)
	importMetaResolvers.Delete(c.ptr)
	C.ContextFree(c.ptr)
	c.ptr = nil
}

func (c *Context) register() {
	ctxMutex.Lock()
	r := ctxRegistry[c.ref]
	if r == nil {
		r = &ctxRef{ctx: c}
		ctxRegistry[c.ref] = r
	}
	r.refCount++
	ctxMutex.Unlock()
}

func (c *Context) deregister() {
	ctxMutex.Lock()
	defer ctxMutex.Unlock()
	r := ctxRegistry[c.ref]
	if r == nil {
		return
	}
	r.refCount--
	if r.refCount <= 0 {
		delete(ctxRegistry, c.ref)
	}
}

func getContext(ref int) *Context {
	ctxMutex.RLock()
	defer ctxMutex.RUnlock()
	r := ctxRegistry[ref]
	if r == nil {
		return nil
	}
	return r.ctx
}

//export goContext
func goContext(ref int) C.ContextPtr {
	ctx := getContext(ref)
	if ctx == nil {
		// The context has been closed while something still held a handle
		// into it — a function belonging to a document that has gone away,
		// called from one that has not. Dereferencing here segfaults the
		// process from inside cgo, where no Go recover can reach it; a null
		// ContextPtr makes the C++ side fail the call instead, and the caller
		// sees an error it can report.
		return nil
	}
	return ctx.ptr
}

func valueResult(ctx *Context, rtn C.RtnValue) (*Value, error) {
	if rtn.value == nil {
		return nil, newJSError(rtn.error)
	}
	return &Value{rtn.value, ctx}, nil
}

func objectResult(ctx *Context, rtn C.RtnValue) (*Object, error) {
	if rtn.value == nil {
		return nil, newJSError(rtn.error)
	}
	return &Object{&Value{rtn.value, ctx}}, nil
}
