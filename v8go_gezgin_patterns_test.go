// Copyright 2026 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"crypto/sha256"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// TestGezginPatterns is the capstone "ready gate" for the v8go fork. It proves
// the binding can reproduce GezginWebEngineGo's four core JavaScript-engine
// patterns end to end, using ONLY the APIs built in todos 2-7, with ZERO
// base64 round-trips for binary transfer. If any of the underlying APIs were
// missing this file would fail to compile (loud failure, never a silent skip);
// all four are present on master.
func TestGezginPatterns(t *testing.T) {
	// Scenario 1 — CRYPTO: a Go callback reads a JS Uint8Array zero-copy
	// (TypedArrayBytes), hashes it with crypto/sha256, and returns the digest
	// as a new ArrayBuffer (NewArrayBuffer). Mirrors Gezgin's
	// crypto.subtle.digest path — Go↔JS binary without btoa/atob.
	t.Run("Crypto_NoBase64", func(t *testing.T) {
		t.Parallel()

		iso := v8.NewIsolate()
		defer iso.Dispose()

		digestFn := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
			arg := info.Args()[0]
			in, _, _, release, err := arg.TypedArrayBytes()
			if err != nil {
				return nil
			}
			hash := sha256.Sum256(in) // Sum256 copies; safe to release after
			release()
			return v8.NewArrayBuffer(info.Context().Isolate(), hash[:])
		})

		global := v8.NewObjectTemplate(iso)
		global.Set("digest", digestFn)
		ctx := v8.NewContext(iso, global)
		defer ctx.Close()

		// sha256("abc") = ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
		result, err := ctx.RunScript(`digest(new Uint8Array([97, 98, 99]))`, "crypto.js")
		if err != nil {
			t.Fatalf("digest RunScript failed: %v", err)
		}
		if !result.IsArrayBuffer() {
			t.Fatalf("digest() returned %s, want ArrayBuffer", result.DetailString())
		}

		out, releaseOut, err := result.ArrayBufferBytes()
		if err != nil {
			t.Fatalf("ArrayBufferBytes on digest result: %v", err)
		}
		defer releaseOut()

		if got := len(out); got != 32 {
			t.Fatalf("digest byteLength = %d, want 32", got)
		}
		if out[0] != 0xba {
			t.Errorf("digest[0] = %#x, want 0xba (sha256(\"abc\")[0])", out[0])
		}
	})

	// Scenario 2 — PROMISE: the promise-reject callback fires for an unhandled
	// rejection with the correct event kind and message. Mirrors the gap closed
	// at Gezgin's missing.go:14.
	t.Run("Promise_RejectCallback", func(t *testing.T) {
		t.Parallel()

		iso := v8.NewIsolate()
		defer iso.Dispose()
		ctx := v8.NewContext(iso)
		defer ctx.Close()

		var gotEvent v8.PromiseRejectEvent
		var gotMessage string
		iso.SetPromiseRejectCallback(func(event v8.PromiseRejectEvent, rejection *v8.Value) {
			gotEvent = event
			if rejection != nil {
				if msgVal, err := rejection.Object().Get("message"); err == nil && msgVal != nil {
					gotMessage = msgVal.String()
				}
			}
		})

		if _, err := ctx.RunScript(
			`new Promise((_, reject) => reject(new Error('capstone-boom')))`, "promise.js",
		); err != nil {
			t.Fatalf("RunScript failed: %v", err)
		}
		iso.PerformMicrotaskCheckpoint()

		if gotEvent != v8.PromiseRejectWithNoHandler {
			t.Fatalf("expected event PromiseRejectWithNoHandler (%d), got %d",
				v8.PromiseRejectWithNoHandler, gotEvent)
		}
		if gotMessage != "capstone-boom" {
			t.Fatalf("expected rejection .message %q, got %q", "capstone-boom", gotMessage)
		}
	})

	// Scenario 3 — WASM: compile, instantiate, and call the canonical
	// add(i32,i32)->i32 module through RunScript. Mirrors modern Gezgin WASM
	// pages exercising the WebAssembly global end to end.
	t.Run("Wasm_CompileAndCall", func(t *testing.T) {
		t.Parallel()

		iso := v8.NewIsolate()
		defer iso.Dispose()
		ctx := v8.NewContext(iso)
		defer ctx.Close()

		sum, err := ctx.RunScript(
			"const b = new Uint8Array(["+jsByteArrayLiteral(wasmAddModuleBytes)+"]);\n"+
				"const m = new WebAssembly.Module(b);\n"+
				"const i = new WebAssembly.Instance(m);\n"+
				"i.exports.add(2, 3)",
			"capstone_wasm.js",
		)
		if err != nil {
			t.Fatalf("wasm RunScript failed: %v", err)
		}
		if got := sum.Integer(); got != 5 {
			t.Fatalf("add(2, 3) = %d, want 5", got)
		}
	})

	// Scenario 4 — METHOD: a FunctionTemplate ("Element") gains a prototype
	// method ("getId") whose Go callback reads the receiver's own property.
	// Mirrors Gezgin's bindings.go DOM-binding pattern.
	t.Run("Method_PrototypeBinding", func(t *testing.T) {
		t.Parallel()

		iso := v8.NewIsolate()
		defer iso.Dispose()

		element := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value {
			return nil
		})
		element.PrototypeMethod("getId", func(info *v8.FunctionCallbackInfo) *v8.Value {
			idVal, _ := info.This().Get("id")
			return idVal
		})

		global := v8.NewObjectTemplate(iso)
		global.Set("Element", element)
		ctx := v8.NewContext(iso, global)
		defer ctx.Close()

		if _, err := ctx.RunScript(`var el = new Element(); el.id = "main"`, "element_setup.js"); err != nil {
			t.Fatal(err)
		}
		val, err := ctx.RunScript(`el.getId()`, "element_call.js")
		if err != nil {
			t.Fatal(err)
		}
		if got := val.String(); got != "main" {
			t.Fatalf("el.getId() = %q, want %q", got, "main")
		}
	})
}
