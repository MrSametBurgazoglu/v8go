// Copyright 2020 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"errors"
	"runtime"
)

// PropertyAttribute are the attribute flags for a property on an Object.
// Typical usage when setting an Object or TemplateObject property, and
// can also be validated when accessing a property.
type PropertyAttribute uint8

// These are V8's own values, and they have to be: they are passed through to
// v8::PropertyAttribute unchanged.
//
// They were 2, 4 and 8 — `1 << iota` inside a const block whose first line
// already consumed iota 0. Every flag was therefore one position too high:
// asking for ReadOnly set DontEnum, asking for DontEnum set DontDelete, and
// DontDelete was 8, outside V8's three-bit mask, where it did nothing at all.
// A property asked to be non-configurable was configurable, and a test that
// deleted one found it gone.
const (
	// None.
	None PropertyAttribute = 0
	// ReadOnly, ie. not writable.
	ReadOnly PropertyAttribute = 1 << 0
	// DontEnum, ie. not enumerable.
	DontEnum PropertyAttribute = 1 << 1
	// DontDelete, ie. not configurable.
	DontDelete PropertyAttribute = 1 << 2
)

// ObjectTemplate is used to create objects at runtime.
// Properties added to an ObjectTemplate are added to each object created from the ObjectTemplate.
type ObjectTemplate struct {
	*template
}

// NewObjectTemplate creates a new ObjectTemplate.
// The *ObjectTemplate can be used as a v8go.ContextOption to create a global object in a Context.
func NewObjectTemplate(iso *Isolate) *ObjectTemplate {
	if iso == nil {
		panic("nil Isolate argument not supported")
	}

	tmpl := &template{
		ptr: C.NewObjectTemplate(iso.ptr),
		iso: iso,
	}
	runtime.SetFinalizer(tmpl, (*template).finalizer)
	return &ObjectTemplate{tmpl}
}

// NewInstance creates a new Object based on the template.
func (o *ObjectTemplate) NewInstance(ctx *Context) (*Object, error) {
	if ctx == nil {
		return nil, errors.New("v8go: Context cannot be <nil>")
	}

	rtn := C.ObjectTemplateNewInstance(o.ptr, ctx.ptr)
	runtime.KeepAlive(o)
	return objectResult(ctx, rtn)
}

// SetInternalFieldCount sets the number of internal fields that instances of this
// template will have.
func (o *ObjectTemplate) SetInternalFieldCount(fieldCount uint32) {
	C.ObjectTemplateSetInternalFieldCount(o.ptr, C.int(fieldCount))
}

// InternalFieldCount returns the number of internal fields that instances of this
// template will have.
func (o *ObjectTemplate) InternalFieldCount() uint32 {
	return uint32(C.ObjectTemplateInternalFieldCount(o.ptr))
}

// MarkAsUndetectable makes instances of this template answer `typeof` with
// "undefined", test as false in a boolean context, and compare loosely equal to
// null and undefined — while still being a real object that `===` distinguishes
// from undefined.
//
// This is the [[IsHTMLDDA]] slot the HTML specification carves out for exactly
// one object, `document.all`, so that the feature sniffs written for a
// twenty-year-old browser keep answering "no". An embedder implementing that
// object needs it; nothing else should.
//
// V8 requires an undetectable template to also be callable, and enforces it
// with a CHECK when the first instance is created — so pair this with
// SetCallAsFunctionHandler, which the one object that needs this wants anyway
// (`document.all(name)` is the legacy spelling of namedItem).
func (o *ObjectTemplate) MarkAsUndetectable() {
	C.ObjectTemplateMarkAsUndetectable(o.ptr)
	runtime.KeepAlive(o)
}

// SetCallAsFunctionHandler makes instances of this template callable, running
// callback for both `obj(…)` and `new obj(…)`.
func (o *ObjectTemplate) SetCallAsFunctionHandler(callback FunctionCallback) {
	if callback == nil {
		panic("nil FunctionCallback argument not supported")
	}
	cbref := o.iso.registerCallback(callback)
	o.cbrefs = append(o.cbrefs, cbref)
	C.ObjectTemplateSetCallAsFunctionHandler(o.ptr, C.int(cbref))
	runtime.KeepAlive(o)
}

func (o *ObjectTemplate) apply(opts *contextOptions) {
	opts.gTmpl = o
}
