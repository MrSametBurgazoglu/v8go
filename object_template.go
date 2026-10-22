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

	tmpl := iso.newTemplate(func() C.TemplatePtr { return C.NewObjectTemplate(iso.ptr) }, 0, false)
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
	if o.replayed {
		return
	}
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
	if o.replayed {
		return
	}
	C.ObjectTemplateMarkAsUndetectable(o.ptr)
	runtime.KeepAlive(o)
}

// SetCrossOriginHandler makes the global proxy of a context built from this
// global template answer an access from a context with another security token
// through the accessing context's hook (Context.SetCrossOriginHook) instead of
// refusing it outright — HTML's WindowProxy, whose cross-origin [[Get]],
// [[Set]] and [[HasProperty]] let a short allowlist through. Two contexts
// sharing a token never reach the hook, and a context with no hook refuses as
// V8 does by default.
//
// [[GetOwnProperty]], [[OwnPropertyKeys]] (symbols included) and the
// operations V8 has no interceptor for ([[Delete]], [[DefineOwnProperty]],
// [[SetPrototypeOf]], [[PreventExtensions]]) reach the hook too; see the op
// constants in v8go.cc.
//
// On an ordinary (non-global) template the access check refuses every
// context, the instance's own included, so every access to an instance goes
// through the accessing context's hook: an object that is cross-origin to
// whoever holds it, as HTML's cross-origin Location and Window are.
func (o *ObjectTemplate) SetCrossOriginHandler() {
	if o.replayed {
		return
	}
	C.ObjectTemplateSetCrossOriginHandler(o.ptr)
	runtime.KeepAlive(o)
}

// SetCodeLike makes instances of this template "code-like": eval and the
// Function constructor offer such an object to the code-generation check
// (Isolate.SetCodeGenerationCheck) with isCodeLike true, instead of returning
// it unevaluated. Trusted Types' TrustedScript is the object it exists for.
func (o *ObjectTemplate) SetCodeLike() {
	if o.replayed {
		return
	}
	C.ObjectTemplateSetCodeLike(o.ptr)
	runtime.KeepAlive(o)
}

// SetCallAsFunctionHandler makes instances of this template callable, running
// callback for both `obj(…)` and `new obj(…)`.
func (o *ObjectTemplate) SetCallAsFunctionHandler(callback FunctionCallback) {
	if callback == nil {
		panic("nil FunctionCallback argument not supported")
	}
	cbref := o.iso.registerFunction(callback)
	o.cbrefs = append(o.cbrefs, cbref)
	if o.replayed {
		return
	}
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

// PropertyInterceptor is the full set of operations a named or indexed
// interceptor can take over. WebIDL's legacy platform objects need more than a
// getter: a collection's index has a property descriptor
// (Object.getOwnPropertyDescriptor(form, 0)), a named property object refuses
// [[DefineOwnProperty]] and [[Delete]], a <select> has an indexed setter, and
// HTMLFormElement's [LegacyOverrideBuiltIns] named properties fail an expando
// that would shadow them.
//
// Every callback runs with the holder — the object that carries the
// interceptor — as `this`, and the key as its first argument: a string or a
// symbol for a named interceptor, a number for an indexed one. A nil callback
// leaves that operation to V8.
//
//   - Getter(key): the value, or nil/undefined for "not mine" — V8 then looks
//     at the object's own properties and its prototype chain.
//   - Setter(key, value): nil/undefined for "not mine", or a boolean: true
//     when the set was handled, false when it failed. A failure throws a
//     TypeError in strict code and is silent in sloppy code, as [[Set]]
//     returning false is. V8 asks it only when the holder is the receiver.
//   - Query(key): nil/undefined, or the property's attributes as a number
//     (PropertyAttribute bits). V8 uses it, or Descriptor, for
//     [[GetOwnProperty]] and for `in`; set at most one of the two.
//   - Deleter(key): nil/undefined, or a boolean as for Setter.
//   - Enumerator(): an array of the keys, for Object.keys, for..in and
//     Reflect.ownKeys.
//   - Definer(key, descriptor): nil/undefined, or a boolean as for Setter. The
//     descriptor is the object Object.getOwnPropertyDescriptor would give,
//     holding only the fields the definition carried.
//   - Descriptor(key): nil/undefined, or a property descriptor object.
//
// A callback that throws (Isolate.ThrowException) makes the operation throw.
type PropertyInterceptor struct {
	Getter     FunctionCallback
	Setter     FunctionCallback
	Query      FunctionCallback
	Deleter    FunctionCallback
	Enumerator FunctionCallback
	Definer    FunctionCallback
	Descriptor FunctionCallback
	Flags      PropertyHandlerFlags
}

// SetNamedPropertyInterceptor installs h for string- and symbol-keyed
// properties (strings only with HandlerOnlyInterceptStrings). An array index
// is not a name to V8: it reaches the indexed interceptor instead.
func (o *ObjectTemplate) SetNamedPropertyInterceptor(h PropertyInterceptor) {
	o.setPropertyInterceptor(0, h)
}

// SetIndexedPropertyInterceptor installs h for array-index keys (0 to
// 2**32 - 2).
func (o *ObjectTemplate) SetIndexedPropertyInterceptor(h PropertyInterceptor) {
	o.setPropertyInterceptor(1, h)
}

func (o *ObjectTemplate) setPropertyInterceptor(indexed int, h PropertyInterceptor) {
	ref := func(cb FunctionCallback) C.int {
		if cb == nil {
			return noCallback
		}
		r := o.iso.registerFunction(cb)
		o.cbrefs = append(o.cbrefs, r)
		return C.int(r)
	}
	// Registered in operation order whether or not the template is a
	// snapshot's: a replay has to register the same callbacks at the same
	// table indices as the recording did.
	getter, setter, query, deleter := ref(h.Getter), ref(h.Setter), ref(h.Query), ref(h.Deleter)
	enumerator, definer, descriptor := ref(h.Enumerator), ref(h.Definer), ref(h.Descriptor)
	if o.replayed {
		return
	}
	C.ObjectTemplateSetPropertyInterceptor(o.ptr, C.int(indexed),
		getter, setter, query, deleter, enumerator, definer, descriptor, C.int(h.Flags))
	runtime.KeepAlive(o)
}

// SetImmutableProto makes the template's instances immutable prototype exotic
// objects (ECMA-262 10.4.7): setting the prototype to anything other than the
// current one fails — Object.setPrototypeOf throws a TypeError and
// Reflect.setPrototypeOf answers false. WebIDL asks it of every [Global]
// object and of the window's named properties object.
//
// The prototype is fixed when the instance is made: give it by instantiating
// through a FunctionTemplate whose function's "prototype" is set first.
func (o *ObjectTemplate) SetImmutableProto() {
	if o.replayed {
		return
	}
	C.ObjectTemplateSetImmutableProto(o.ptr)
	runtime.KeepAlive(o)
}

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
// be nil; when given it is called with no arguments and returns an array of
// names for Object.keys and for..in.
//
// Mind the flags: with HandlerNone the interceptor is consulted BEFORE the
// object's own properties, which is right for [LegacyOverrideBuiltIns] and
// wrong for everything else. See PropertyHandlerFlags. SetNamedPropertyInterceptor
// takes the other operations too.
func (o *ObjectTemplate) SetNamedPropertyHandler(getter, enumerator FunctionCallback, flags PropertyHandlerFlags) {
	if getter == nil {
		return
	}
	o.SetNamedPropertyInterceptor(PropertyInterceptor{Getter: getter, Enumerator: enumerator, Flags: flags})
}

// SetIndexedPropertyHandler intercepts property reads by index, which is what
// makes collection[3] answer without every index being defined in advance.
//
// getter is called with the index as a number. Returning nil or undefined
// means "not mine". SetIndexedPropertyInterceptor takes the other operations.
func (o *ObjectTemplate) SetIndexedPropertyHandler(getter FunctionCallback, flags PropertyHandlerFlags) {
	if getter == nil {
		return
	}
	o.SetIndexedPropertyInterceptor(PropertyInterceptor{Getter: getter, Flags: flags})
}
