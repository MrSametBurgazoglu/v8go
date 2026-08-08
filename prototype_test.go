// Copyright 2021 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// TestFunctionTemplatePrototypeMethod proves that a method added via
// PrototypeMethod is invoked with the JS instance as its receiver (this),
// closing the gap rogchap/v8go left where FunctionTemplate only exposed
// free-function callbacks.
func TestFunctionTemplatePrototypeMethod(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()

	person := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value {
		return nil
	})
	person.PrototypeMethod("greet", func(info *v8.FunctionCallbackInfo) *v8.Value {
		nameVal, _ := info.This().Get("name")
		greeting, _ := v8.NewValue(iso, "hello, "+nameVal.String())
		return greeting
	})

	global := v8.NewObjectTemplate(iso)
	global.Set("Person", person)
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()

	if _, err := ctx.RunScript(`var p = new Person(); p.name = "Ada"`, ""); err != nil {
		t.Fatal(err)
	}
	val, err := ctx.RunScript(`p.greet()`, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); got != "hello, Ada" {
		t.Fatalf("expected %q, got %q", "hello, Ada", got)
	}
}

// TestFunctionTemplatePrototypeSetValue proves that a prototype property set
// via PrototypeSet is inherited by every instance created from the template.
func TestFunctionTemplatePrototypeSetValue(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()

	person := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value {
		return nil
	})
	kind, _ := v8.NewValue(iso, "person")
	person.PrototypeSet("kind", kind)

	global := v8.NewObjectTemplate(iso)
	global.Set("Person", person)
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()

	val, err := ctx.RunScript(`(new Person()).kind`, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); got != "person" {
		t.Fatalf("expected %q, got %q", "person", got)
	}
}

// TestFunctionTemplatePrototypeMethod_panic_on_nil_callback guards against
// registering a nil Go callback, mirroring NewFunctionTemplate's guard.
func TestFunctionTemplatePrototypeMethod_panic_on_nil_callback(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()
	person := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value {
		return nil
	})

	defer func() {
		if err := recover(); err == nil {
			t.Error("expected panic")
		}
	}()
	person.PrototypeMethod("noop", nil)
}
