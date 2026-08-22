// Copyright 2021 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"runtime"
	"unsafe"
)

// FunctionCallback is a callback that is executed in Go when a function is executed in JS.
type FunctionCallback func(info *FunctionCallbackInfo) *Value

// noInternalField is the thisField0 value meaning "the receiver has no
// internal field 0, or it does not hold a 32-bit integer". It is outside the
// int32 range every readable field value falls in, so no real field collides
// with it.
const noInternalField = int64(-1) << 40

// FunctionCallbackInfo is the argument that is passed to a FunctionCallback.
type FunctionCallbackInfo struct {
	ctx  *Context
	args []*Value
	this *Object
	// thisField0 is the receiver's internal field 0, read on the C++ side
	// while it still had the object. See ThisInternalField.
	thisField0 int64
}

// ThisInternalField reports the receiver's internal field 0 when it holds a
// 32-bit integer, which is the shape an embedder uses to key a native object
// from its JS wrapper.
//
// It costs nothing: the value was read where the call was made, on the C++
// side, rather than fetched back across cgo. Reading it through
// This().GetInternalField(0) instead is four crossings and a handle to
// release, on the busiest path an embedder has.
func (i *FunctionCallbackInfo) ThisInternalField() (int64, bool) {
	if i.thisField0 == noInternalField {
		return 0, false
	}
	return i.thisField0, true
}

// Context is the current context that the callback is being executed in.
func (i *FunctionCallbackInfo) Context() *Context {
	return i.ctx
}

// This returns the receiver object "this".
func (i *FunctionCallbackInfo) This() *Object {
	return i.this
}

// Args returns a slice of the value arguments that are passed to the JS function.
func (i *FunctionCallbackInfo) Args() []*Value {
	return i.args
}

func (i *FunctionCallbackInfo) Release() {
	for _, arg := range i.args {
		arg.Release()
	}
	i.this.Release()
}

// FunctionTemplate is used to create functions at runtime.
// There can only be one function created from a FunctionTemplate in a context.
// The lifetime of the created function is equal to the lifetime of the context.
type FunctionTemplate struct {
	*template
}

// NewFunctionTemplate creates a FunctionTemplate for a given callback.
func NewFunctionTemplate(iso *Isolate, callback FunctionCallback) *FunctionTemplate {
	if iso == nil {
		panic("nil Isolate argument not supported")
	}
	if callback == nil {
		panic("nil FunctionCallback argument not supported")
	}

	cbref := iso.registerCallback(callback)

	tmpl := &template{
		ptr: C.NewFunctionTemplate(iso.ptr, C.int(cbref)),
		iso: iso,
	}
	runtime.SetFinalizer(tmpl, (*template).finalizer)
	return &FunctionTemplate{tmpl}
}

// GetFunction returns an instance of this function template bound to the given context.
func (tmpl *FunctionTemplate) GetFunction(ctx *Context) *Function {
	rtn := C.FunctionTemplateGetFunction(tmpl.ptr, ctx.ptr)
	runtime.KeepAlive(tmpl)
	val, err := valueResult(ctx, rtn)
	if err != nil {
		panic(err) // TODO: Consider returning the error
	}
	return &Function{val}
}

// PrototypeSet adds a property to the prototype of every instance created
// from this FunctionTemplate. The value must be a primitive *Value (a number,
// string, boolean, ...), not a runtime JS object; for method-style callbacks
// use PrototypeMethod.
func (tmpl *FunctionTemplate) PrototypeSet(name string, val *Value) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	C.FunctionTemplatePrototypeSetValue(tmpl.ptr, cname, val.ptr, 0)
	runtime.KeepAlive(tmpl)
	runtime.KeepAlive(val)
}

// PrototypeMethod adds a method to the prototype of every instance created
// from this FunctionTemplate. When JS calls instance.method(), the Go
// callback cb receives the instance as the receiver (FunctionCallbackInfo.This).
// Returns the child FunctionTemplate backing the method so the caller can
// configure it further.
func (tmpl *FunctionTemplate) PrototypeMethod(name string, cb FunctionCallback) *FunctionTemplate {
	if cb == nil {
		panic("nil FunctionCallback argument not supported")
	}
	cbref := tmpl.iso.registerCallback(cb)

	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	child := &template{
		ptr: C.FunctionTemplatePrototypeSetMethod(tmpl.ptr, cname, C.int(cbref)),
		iso: tmpl.iso,
	}
	runtime.SetFinalizer(child, (*template).finalizer)
	return &FunctionTemplate{child}
}

// Note that ideally `thisAndArgs` would be split into two separate arguments, but they were combined
// to workaround an ERROR_COMMITMENT_LIMIT error on windows that was detected in CI circa 2021.
// Windows CI was removed shortly after and the workaround has been preserved conservatively. Now
// that Windows is supported again it is worth re-testing whether the split form still reproduces
// the commitment-limit error on modern runners; see follow-up tracked in the v8go Windows restore.
//
// thisField0 is the receiver's internal field 0 when it holds a 32-bit
// integer, and noInternalField otherwise. It rides along because the C++ side
// is already holding the receiver: an embedder that keys its objects by an id
// in that field would otherwise pay four cgo crossings per call — the field
// count, the field, its value, and the handle's release — to learn a number
// that was one dereference away on the side that made the call.
//
//export goFunctionCallback
func goFunctionCallback(ctxref int, cbref int, thisAndArgs *C.ValuePtr, argsCount int, thisField0 C.int64_t) C.ValuePtr {
	XPCount.Add(1)
	ctx := getContext(ctxref)

	this := *thisAndArgs
	info := &FunctionCallbackInfo{
		ctx:        ctx,
		this:       &Object{&Value{ptr: this, ctx: ctx}},
		args:       make([]*Value, argsCount),
		thisField0: int64(thisField0),
	}

	argv := (*[1 << 30]C.ValuePtr)(unsafe.Pointer(thisAndArgs))[1 : argsCount+1 : argsCount+1]
	for i, v := range argv {
		val := &Value{ptr: v, ctx: ctx}
		info.args[i] = val
	}

	callbackFunc := ctx.iso.getCallback(cbref)
	if val := callbackFunc(info); val != nil {
		return val.ptr
	}
	return nil
}
