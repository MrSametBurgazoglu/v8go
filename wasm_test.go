// Copyright 2026 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"strconv"
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// wasmAddModuleBytes is the canonical 41-byte WebAssembly module exporting
// add(i32, i32) -> i32. Sections: magic+version, type (i32,i32)->i32, function,
// export "add", code (local.get 0; local.get 1; i32.add; end). A single source
// of truth for both the validate gate and the compile/instantiate/call path.
var wasmAddModuleBytes = []byte{
	// magic \0asm + version 1
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// type section: (i32, i32) -> i32
	0x01, 0x07, 0x01, 0x60, 0x02, 0x7f, 0x7f, 0x01, 0x7f,
	// function section: function 0 = type 0
	0x03, 0x02, 0x01, 0x00,
	// export section: "add" = function 0
	0x07, 0x07, 0x01, 0x03, 0x61, 0x64, 0x64, 0x00, 0x00,
	// code section: local.get 0; local.get 1; i32.add; end
	0x0a, 0x09, 0x01, 0x07, 0x00, 0x20, 0x00, 0x20, 0x01, 0x6a, 0x0b,
}

// jsByteArrayLiteral renders b as the inside of a JS Array/Uint8Array literal,
// e.g. [0,97,115,...]. Kept as the single place bytes are formatted into JS.
func jsByteArrayLiteral(b []byte) string {
	parts := make([]string, len(b))
	for i, by := range b {
		parts[i] = strconv.Itoa(int(by))
	}
	return strings.Join(parts, ",")
}

// TestWebAssemblyAvailable proves the WebAssembly global is exposed on a bare
// v8.NewContext. V8 enables WebAssembly by default; this asserts the embedder's
// GN build did not pass v8_enable_wasm=false. If this fails, WASM is disabled at
// build time and no JS-side wasm code can run — a GN/libv8.a rebuild is required.
func TestWebAssemblyAvailable(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	val, err := ctx.RunScript("typeof WebAssembly", "wasm_typeof.js")
	if err != nil {
		t.Fatalf("typeof WebAssembly RunScript failed: %v", err)
	}
	if got := val.String(); got != "object" {
		t.Fatalf("WebAssembly global missing or wrong type: got %q, want %q", got, "object")
	}
}

// TestWebAssemblyCompileAndCall runs the full pipeline in JS: validate the
// hardcoded bytes, compile to a Module, instantiate, and call add(2, 3).
// The validate gate runs first in its own RunScript so a wrong byte sequence
// fails with a validate message rather than a confusing compile error.
func TestWebAssemblyCompileAndCall(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	valid, err := ctx.RunScript(
		"WebAssembly.validate(new Uint8Array(["+jsByteArrayLiteral(wasmAddModuleBytes)+"]))",
		"wasm_validate.js",
	)
	if err != nil {
		t.Fatalf("validate RunScript failed: %v", err)
	}
	if !valid.Boolean() {
		t.Fatal("WebAssembly.validate rejected the hardcoded add module bytes; byte sequence is wrong")
	}

	sum, err := ctx.RunScript(
		"const b = new Uint8Array(["+jsByteArrayLiteral(wasmAddModuleBytes)+"]);\n"+
			"const m = new WebAssembly.Module(b);\n"+
			"const i = new WebAssembly.Instance(m);\n"+
			"i.exports.add(2, 3)",
		"wasm_compile_call.js",
	)
	if err != nil {
		t.Fatalf("compile/instantiate/call RunScript failed: %v", err)
	}
	if got := sum.Integer(); got != 5 {
		t.Fatalf("add(2, 3) = %d, want 5", got)
	}
}

// TestWebAssemblyValidateRejects confirms WebAssembly.validate distinguishes
// valid from malformed modules: a non-wasm byte sequence must return false
// (not throw), proving the validate path is wired through end-to-end.
func TestWebAssemblyValidateRejects(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	valid, err := ctx.RunScript(
		"WebAssembly.validate(new Uint8Array([1, 2, 3]))",
		"wasm_validate_bad.js",
	)
	if err != nil {
		t.Fatalf("validate(bad) RunScript failed: %v", err)
	}
	if valid.Boolean() {
		t.Fatal("WebAssembly.validate accepted [1,2,3] as a valid module; expected rejection")
	}
}
