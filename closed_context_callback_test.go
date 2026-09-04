// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A native function outliving its context, called from a context that is still
// open, must not take the process with it.
//
// V8 enters an API function's own creation context before invoking it, so the
// ctx_ref the callback recovers from embedder data is the CLOSED context's.
// goContext answers null for a context no longer in the registry — and every
// line after the lookup used to dereference it, starting with tracked_value
// writing ctx->nextValId 160 bytes past null. The crash landed inside cgo,
// below the last Go frame, where no recover reaches.
//
// An embedder hits this the moment a document keeps a reference into an iframe
// it then navigates: five lines of page script were enough to kill a browser
// built on this library.
func TestCallbackFromClosedContextDoesNotCrash(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()

	called := 0
	global := v8.NewObjectTemplate(iso)
	probe := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		called++
		v, _ := v8.NewValue(iso, "ran")
		return v
	})
	fatalIf(t, global.Set("probe", probe, v8.ReadOnly))

	doomed := v8.NewContext(iso, global)
	live := v8.NewContext(iso)
	defer live.Close()

	// Hand the function to the surviving context BEFORE the owning one goes,
	// which is how a page comes to hold one: it reads the reference while the
	// document is still there. The live global holds a real V8 reference, so
	// the function object survives its context's m_values being freed.
	fn, err := doomed.Global().Get("probe")
	fatalIf(t, err)
	fatalIf(t, live.Global().Set("probe", fn))

	// Called while the owner is open: the ordinary path, and the control that
	// keeps the assertion below from passing vacuously.
	val, err := live.RunScript("probe()", "before.js")
	fatalIf(t, err)
	if called != 1 || val.String() != "ran" {
		t.Fatalf("before close: called=%d value=%q, want 1 and %q", called, val.String(), "ran")
	}

	doomed.Close()

	// And after. The call answers undefined rather than running the Go
	// callback: the context that owned it is gone, and so is everything the
	// callback would have been handed.
	val, err = live.RunScript("probe()", "after.js")
	fatalIf(t, err)
	if called != 1 {
		t.Errorf("the callback ran %d times; a closed context's callback must not run", called-1)
	}
	if !val.IsUndefined() {
		t.Errorf("call into a closed context = %q, want undefined", val.String())
	}
}

// A property interceptor is NOT the same case, and the difference is worth a
// test of its own: V8 enters an API function's creation context before calling
// it, but it does not switch context for an interceptor. The context an
// interceptor recovers is therefore the CALLER's, which is by construction
// still open — so reading a name off an object whose own context has closed
// keeps working where calling a method on it does not.
//
// The null-context fence in interceptorContext is defence rather than the fix
// for a reachable crash, and this test says which: it fails if the fence ever
// starts firing on a live caller, which would silently turn every intercepted
// name on a detached object into undefined.
func TestInterceptorRunsInTheCallersContext(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()

	called := 0
	global := v8.NewObjectTemplate(iso)
	collection := v8.NewObjectTemplate(iso)
	collection.SetNamedPropertyHandler(func(info *v8.FunctionCallbackInfo) *v8.Value {
		called++
		v, _ := v8.NewValue(iso, "intercepted")
		return v
	}, nil, v8.HandlerNone)
	fatalIf(t, global.Set("bag", collection))

	doomed := v8.NewContext(iso, global)
	live := v8.NewContext(iso)
	defer live.Close()

	bag, err := doomed.Global().Get("bag")
	fatalIf(t, err)
	fatalIf(t, live.Global().Set("bag", bag))

	val, err := live.RunScript("bag.anything", "before.js")
	fatalIf(t, err)
	if called != 1 || val.String() != "intercepted" {
		t.Fatalf("before close: called=%d value=%q", called, val.String())
	}

	doomed.Close()

	val, err = live.RunScript("bag.anything", "after.js")
	fatalIf(t, err)
	if called != 2 {
		t.Errorf("the interceptor ran %d times in total; the caller's context is open, so it must still run", called)
	}
	if val.String() != "intercepted" {
		t.Errorf("interception after the holder's context closed = %q, want %q", val.String(), "intercepted")
	}
}
