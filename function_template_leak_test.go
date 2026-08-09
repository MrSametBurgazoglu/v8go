// Copyright 2026 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"fmt"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// TestFunctionTemplateReturnValueNotLeaked guards the callback-return-value
// leak fix in FunctionTemplateCallback.
//
// A Go callback builds its return value with NewValue(iso, ...), which tracks
// the wrapper in the *isolate's internal* context (iso->GetData(0)->vals) —
// freed only at IsolateDispose, never at the per-call Context::Close, and the
// Go *Value has no finalizer. Before the fix every callback invocation leaked
// one strongly-reachable m_value there for the isolate's whole lifetime. A JS
// Proxy that calls a host function on every property access (the deskbot
// msgbridge) turned that into unbounded, un-GC-able old-space growth and a
// "last resort GC frees 0 bytes" CALL_AND_RETRY_LAST OOM on long-running flows.
//
// After the fix the return value is released once V8 copies its Local into the
// return slot, so the internal-context value count stays flat no matter how
// many times the callback is invoked.
func TestFunctionTemplateReturnValueNotLeaked(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()

	global := v8.NewObjectTemplate(iso)
	// Mirrors msgbridge.__msgGet: return a freshly-created value from Go data.
	host := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		v, _ := v8.NewValue(iso, `{"kind":"string","value":"x"}`)
		return v
	})
	global.Set("host", host)

	ctx := v8.NewContext(iso, global)
	defer ctx.Close()

	// Warm up so one-time setup values (the function object, etc.) are already
	// accounted for before we measure the per-call delta.
	if _, err := ctx.RunScript("host();", "warmup.js"); err != nil {
		t.Fatal(err)
	}
	base := iso.InternalContextValueCount()

	const N = 2000
	if _, err := ctx.RunScript(fmt.Sprintf("for (let i=0;i<%d;i++){host();}", N), "loop.js"); err != nil {
		t.Fatal(err)
	}

	delta := iso.InternalContextValueCount() - base
	// Allow a tiny constant slack for any incidental internal-context value;
	// the leak signature is delta ≈ N.
	if delta > 8 {
		t.Fatalf("isolate internal-context tracked values grew by %d over %d host() calls "+
			"(want ~0): FunctionTemplate return values are leaking into the isolate "+
			"internal context", delta, N)
	}
}

// TestFunctionTemplateReturnValueRetained is the other half of the contract
// above: the drop must only claim values the call itself minted.
//
// A callback that returns a handle the Go side still owns is ordinary — a DOM
// binding hands back the same wrapper object for a node on every access so that
// `a === a` holds in JS, and `return v8.Null(iso)` returns a per-isolate
// singleton cached on the *Isolate. An unconditional drop frees both out from
// under Go: the wrapper's next return builds a different JS object and breaks
// identity, and the freed null singleton is a use-after-free that takes the
// process down some calls later.
func TestFunctionTemplateReturnValueRetained(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	// A wrapper object created once and handed out repeatedly, as a DOM binding
	// does for a node.
	tmpl := v8.NewObjectTemplate(iso)
	wrapper, err := tmpl.NewInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	set := func(name string, cb v8.FunctionCallback) {
		if err := ctx.Global().Set(name, v8.NewFunctionTemplate(iso, cb).GetFunction(ctx)); err != nil {
			t.Fatal(err)
		}
	}
	set("wrapper", func(info *v8.FunctionCallbackInfo) *v8.Value { return wrapper.Value })
	set("nul", func(info *v8.FunctionCallbackInfo) *v8.Value { return v8.Null(iso) })

	for _, tc := range []struct{ src, want string }{
		{"typeof wrapper()", "object"},
		{"wrapper() === wrapper()", "true"},
		// Called repeatedly: a freed singleton reads as garbage, not "null".
		{"nul(); nul(); String(nul())", "null"},
		{"nul() === null", "true"},
	} {
		val, err := ctx.RunScript(tc.src, "retained.js")
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		if got := val.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
	}
}
