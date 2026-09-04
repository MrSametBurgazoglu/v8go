// Copyright 2021 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"unsafe"
)

type template struct {
	ptr C.TemplatePtr
	iso *Isolate
	// cbref is the callback a FunctionTemplate registered with its isolate,
	// zero for an ObjectTemplate; cbrefs are any more the template registered
	// since — a call-as-function handler, a prototype method's. Release
	// unregisters them all.
	cbref  int
	cbrefs []int
}

// Release frees the template now, on a thread that owns the isolate: the
// Persistent handle behind it is reset while the isolate is alive, and a
// FunctionTemplate's Go callback is unregistered from the isolate.
//
// The finalizer cannot do either — it runs on the collector's goroutine, and
// the isolate may already be gone — so it leaks both on purpose, and an
// embedder that builds templates per document on a long-lived isolate grows
// without a ceiling until it calls this. Call it only once every context that
// instantiated the template has been closed: a function from a released
// FunctionTemplate that is still reachable answers undefined when called.
func (t *template) Release() {
	if t == nil || t.ptr == nil {
		return
	}
	runtime.SetFinalizer(t, nil)
	if t.iso != nil {
		if t.cbref != 0 {
			t.iso.unregisterCallback(t.cbref)
		}
		for _, ref := range t.cbrefs {
			t.iso.unregisterCallback(ref)
		}
	}
	t.cbref, t.cbrefs = 0, nil
	if t.iso != nil && t.iso.ptr != nil {
		C.TemplateRelease(t.ptr)
	} else {
		C.TemplateFreeWrapper(t.ptr)
	}
	t.ptr = nil
}

// Set adds a property to each instance created by this template.
// The property must be defined either as a primitive value, or a template.
// If the value passed is a Go supported primitive (string, int32, uint32, int64, uint64, float64, big.Int)
// then a value will be created and set as the value property.
func (t *template) Set(name string, val interface{}, attributes ...PropertyAttribute) error {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))

	var attrs PropertyAttribute
	for _, a := range attributes {
		attrs |= a
	}

	switch v := val.(type) {
	case string, int32, uint32, int64, uint64, float64, bool, *big.Int:
		newVal, err := NewValue(t.iso, v)
		if err != nil {
			return fmt.Errorf("v8go: unable to create new value: %v", err)
		}
		C.TemplateSetValue(t.ptr, cname, newVal.ptr, C.int(attrs))
	case *ObjectTemplate:
		C.TemplateSetTemplate(t.ptr, cname, v.ptr, C.int(attrs))
		runtime.KeepAlive(v)
	case *FunctionTemplate:
		C.TemplateSetTemplate(t.ptr, cname, v.ptr, C.int(attrs))
		runtime.KeepAlive(v)
	case *Value:
		if v.IsObject() || v.IsExternal() {
			return errors.New("v8go: unsupported property: value type must be a primitive or use a template")
		}
		C.TemplateSetValue(t.ptr, cname, v.ptr, C.int(attrs))
	default:
		return fmt.Errorf("v8go: unsupported property type `%T`, must be one of string, int32, uint32, int64, uint64, float64, *big.Int, *v8go.Value, *v8go.ObjectTemplate or *v8go.FunctionTemplate", v)
	}
	runtime.KeepAlive(t)

	return nil
}

func (t *template) finalizer() {
	// Using v8::PersistentBase::Reset() wouldn't be thread-safe to do from
	// this finalizer goroutine so just free the wrapper and let the template
	// itself get cleaned up when the isolate is disposed.
	C.TemplateFreeWrapper(t.ptr)
	t.ptr = nil
}

// SetAccessorProperty installs a real accessor pair on the template. Either
// side may be nil for absent, which is how a readonly attribute is spelled.
//
// This is the shape a WebIDL attribute has, and it is not the same thing as a
// data property holding a function:
// `Object.getOwnPropertyDescriptor(proto, name).get` answers a function, a
// page can replace it, and a subclass can call through it. An embedder that
// installs data properties instead is observably different from every browser.
func (t *template) SetAccessorProperty(name string, getter, setter *FunctionTemplate, attributes PropertyAttribute) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	// The accessor pair is installed from the two templates' own callback
	// registrations, and this template takes responsibility for releasing
	// them: an accessor outlives the FunctionTemplate the caller built it
	// from, and unregistering the callback under a live accessor is a getter
	// that answers undefined.
	getterRef, setterRef := C.int(-1), C.int(-1)
	if getter != nil {
		getterRef = C.int(getter.cbref)
		t.cbrefs = append(t.cbrefs, getter.cbref)
	}
	if setter != nil {
		setterRef = C.int(setter.cbref)
		t.cbrefs = append(t.cbrefs, setter.cbref)
	}
	C.TemplateSetAccessorProperty(t.ptr, cname, getterRef, setterRef, C.int(attributes))
	runtime.KeepAlive(t)
	runtime.KeepAlive(getter)
	runtime.KeepAlive(setter)
}

// SetSymbolValue sets a well-known-symbol-keyed property: "toStringTag",
// "iterator", "asyncIterator", "hasInstance", "toPrimitive", "unscopables".
//
// Symbol.toStringTag is why this exists. Without it every wrapper answers
// "[object Object]" to Object.prototype.toString, which is how a great deal of
// library code decides what it has been handed.
func (t *template) SetSymbolValue(symbol string, value *Value, attributes PropertyAttribute) {
	csymbol := C.CString(symbol)
	defer C.free(unsafe.Pointer(csymbol))
	C.TemplateSetSymbolValue(t.ptr, csymbol, value.ptr, C.int(attributes))
	runtime.KeepAlive(t)
	runtime.KeepAlive(value)
}
