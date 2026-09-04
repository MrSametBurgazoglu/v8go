// Copyright 2021 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include "v8go.h"
import "C"
import (
	"errors"
	"unsafe"
)

// Function is a JavaScript function.
type Function struct {
	*Value
}

// Call this JavaScript function with the given arguments.
//
// A function whose context has been closed answers an error rather than being
// called: the handle it holds points into a disposed context, and V8 reads
// freed memory from it. That happens whenever a document keeps a callable
// belonging to one that has gone away, which an iframe navigating itself does
// routinely.
func (fn *Function) Call(recv Valuer, args ...Valuer) (*Value, error) {
	if fn == nil || fn.Value == nil || fn.ctx == nil || getContext(fn.ctx.ref) == nil {
		return nil, errors.New("v8go: the function's context has been closed")
	}
	var argptr *C.ValuePtr
	if len(args) > 0 {
		var cArgs = make([]C.ValuePtr, len(args))
		for i, arg := range args {
			cArgs[i] = arg.value().ptr
		}
		argptr = (*C.ValuePtr)(unsafe.Pointer(&cArgs[0]))
	}
	rtn := C.FunctionCall(fn.ptr, recv.value().ptr, C.int(len(args)), argptr)
	return valueResult(fn.ctx, rtn)
}

// Invoke a constructor function to create an object instance.
func (fn *Function) NewInstance(args ...Valuer) (*Object, error) {
	var argptr *C.ValuePtr
	if len(args) > 0 {
		var cArgs = make([]C.ValuePtr, len(args))
		for i, arg := range args {
			cArgs[i] = arg.value().ptr
		}
		argptr = (*C.ValuePtr)(unsafe.Pointer(&cArgs[0]))
	}
	rtn := C.FunctionNewInstance(fn.ptr, C.int(len(args)), argptr)
	return objectResult(fn.ctx, rtn)
}

// Return the source map url for a function.
func (fn *Function) SourceMapUrl() *Value {
	ptr := C.FunctionSourceMapUrl(fn.ptr)
	return &Value{ptr, fn.ctx}
}
