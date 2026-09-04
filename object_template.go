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

// PropertyHandlerFlags configure an interceptor.
type PropertyHandlerFlags int

const (
	// HandlerNone is V8's default, and it masks: the interceptor is consulted
	// FIRST, before the object's own properties and its prototype chain. That
	// is what WebIDL's [LegacyOverrideBuiltIns] describes — a named property
	// on document or on a form wins over a built-in of the same name.
	HandlerNone PropertyHandlerFlags = 0
	// HandlerNonMasking is the other way round: the interceptor is consulted
	// only for names that do not already exist. This is what an interface
	// WITHOUT [LegacyOverrideBuiltIns] needs, which is most of them — a
	// collection whose named getter shadowed `length` or `item` would be
	// unusable.
	//
	// The name is V8's and reads backwards at first: "non-masking" means it
	// does not mask the object's own properties.
	HandlerNonMasking PropertyHandlerFlags = 1 << 0
	// HandlerOnlyInterceptStrings skips symbol keys, so a well-known symbol
	// on the prototype is not shadowed by a named-property lookup.
	HandlerOnlyInterceptStrings PropertyHandlerFlags = 1 << 1
	// HandlerHasNoSideEffect promises the getter is side-effect free, which
	// lets a debugger evaluate through it.
	HandlerHasNoSideEffect PropertyHandlerFlags = 1 << 2
)

// SetNamedPropertyHandler intercepts property reads by name.
//
// The DOM is full of objects whose property names are not known in advance:
// document.forms.myForm, localStorage.token, el.dataset.userId,
// window.someIframeName. Defining each name eagerly is slower than
// intercepting and observably wrong — a name that appears after the object was
// built is simply absent until something redefines it.
//
// getter is called with the name as its only argument and the object as `this`;
// returning nil or undefined means "not mine", and V8 continues the lookup
// through the object's own properties and its prototype chain. enumerator may
// be nil; when given it returns an array of names for Object.keys and for..in.
//
// Mind the flags: with HandlerNone the interceptor is consulted BEFORE the
// object's own properties, which is right for [LegacyOverrideBuiltIns] and
// wrong for everything else. See PropertyHandlerFlags.
func (o *ObjectTemplate) SetNamedPropertyHandler(getter, enumerator FunctionCallback, flags PropertyHandlerFlags) {
	getterRef := C.int(-1)
	if getter != nil {
		ref := o.iso.registerCallback(getter)
		o.cbrefs = append(o.cbrefs, ref)
		getterRef = C.int(ref)
	}
	enumeratorRef := C.int(-1)
	if enumerator != nil {
		ref := o.iso.registerCallback(enumerator)
		o.cbrefs = append(o.cbrefs, ref)
		enumeratorRef = C.int(ref)
	}
	C.ObjectTemplateSetNamedPropertyHandler(o.ptr, getterRef, -1, -1, -1, enumeratorRef, C.int(flags))
	runtime.KeepAlive(o)
}

// SetIndexedPropertyHandler intercepts property reads by index, which is what
// makes collection[3] answer without every index being defined in advance.
//
// getter is called with the index as a number. Returning nil or undefined
// means "not mine".
func (o *ObjectTemplate) SetIndexedPropertyHandler(getter FunctionCallback, flags PropertyHandlerFlags) {
	getterRef := C.int(-1)
	if getter != nil {
		ref := o.iso.registerCallback(getter)
		o.cbrefs = append(o.cbrefs, ref)
		getterRef = C.int(ref)
	}
	C.ObjectTemplateSetIndexedPropertyHandler(o.ptr, getterRef, -1, -1, -1, -1, C.int(flags))
	runtime.KeepAlive(o)
}
