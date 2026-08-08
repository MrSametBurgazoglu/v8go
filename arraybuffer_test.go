// Copyright 2019 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

func TestArrayBufferBytes(t *testing.T) {
	t.Parallel()
	ctx := v8.NewContext(nil)
	defer ctx.Isolate().Dispose()
	defer ctx.Close()

	if _, err := ctx.RunScript("var buf = new ArrayBuffer(16); var view = new Uint8Array(buf)", "setup.js"); err != nil {
		t.Fatalf("RunScript setup: %v", err)
	}

	val, err := ctx.RunScript("buf", "get.js")
	if err != nil {
		t.Fatalf("RunScript buf: %v", err)
	}

	bytes, release, err := val.ArrayBufferBytes()
	if err != nil {
		t.Fatalf("ArrayBufferBytes: %v", err)
	}
	defer release()

	if got, want := len(bytes), 16; got != want {
		t.Fatalf("byte length = %d, want %d", got, want)
	}

	// Zero-copy: a Go-side write must be visible to a JS view of the buffer.
	bytes[0] = 0xFF

	got, err := ctx.RunScript("view[0]", "read.js")
	if err != nil {
		t.Fatalf("RunScript view[0]: %v", err)
	}
	if got == nil || got.Integer() != 255 {
		var n int64
		if got != nil {
			n = got.Integer()
		}
		t.Errorf("view[0] = %d, want 255 (zero-copy mutation not visible)", n)
	}
}

func TestTypedArrayBytes(t *testing.T) {
	t.Parallel()
	ctx := v8.NewContext(nil)
	defer ctx.Isolate().Dispose()
	defer ctx.Close()

	if _, err := ctx.RunScript("var ta = new Uint8Array(new ArrayBuffer(16), 4, 8)", "setup.js"); err != nil {
		t.Fatalf("RunScript setup: %v", err)
	}

	val, err := ctx.RunScript("ta", "get.js")
	if err != nil {
		t.Fatalf("RunScript ta: %v", err)
	}

	bytes, byteOffset, byteLength, release, err := val.TypedArrayBytes()
	if err != nil {
		t.Fatalf("TypedArrayBytes: %v", err)
	}
	defer release()

	if byteOffset != 4 {
		t.Errorf("byteOffset = %d, want 4", byteOffset)
	}
	if byteLength != 8 {
		t.Errorf("byteLength = %d, want 8", byteLength)
	}
	if got := len(bytes); got != 8 {
		t.Errorf("slice len = %d, want 8", got)
	}

	// Zero-copy: the view's [0] (backing-store byte index 4) must land at the
	// slice's [0], proving the offset mapping.
	if _, err := ctx.RunScript("ta[0] = 42", "set.js"); err != nil {
		t.Fatalf("RunScript ta[0]=42: %v", err)
	}
	if bytes[0] != 42 {
		t.Errorf("slice[0] = %d, want 42 (view offset mapping broken)", bytes[0])
	}
}

func TestArrayBufferBytesWrongType(t *testing.T) {
	t.Parallel()
	ctx := v8.NewContext(nil)
	defer ctx.Isolate().Dispose()
	defer ctx.Close()

	for _, src := range []string{"42", `"hello"`} {
		val, err := ctx.RunScript(src, "lit.js")
		if err != nil {
			t.Fatalf("RunScript(%q): %v", src, err)
		}
		if _, _, err := val.ArrayBufferBytes(); err == nil {
			t.Errorf("ArrayBufferBytes() on %q: expected error, got nil", src)
		}
		if _, _, _, _, err := val.TypedArrayBytes(); err == nil {
			t.Errorf("TypedArrayBytes() on %q: expected error, got nil", src)
		}
	}
}

func TestNewArrayBuffer(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()

	source := []byte{1, 2, 3, 4}
	ab := v8.NewArrayBuffer(iso, source)

	ctx := v8.NewContext(iso)
	defer ctx.Close()
	if err := ctx.Global().Set("ab", ab); err != nil {
		t.Fatalf("Global().Set: %v", err)
	}

	lengthVal, err := ctx.RunScript("ab.byteLength", "len.js")
	if err != nil {
		t.Fatalf("RunScript byteLength: %v", err)
	}
	if lengthVal == nil || lengthVal.Integer() != 4 {
		var n int64
		if lengthVal != nil {
			n = lengthVal.Integer()
		}
		t.Errorf("ab.byteLength = %d, want 4", n)
	}

	// Bytes were copied into V8-owned storage: mutating the source slice
	// after NewArrayBuffer returned must not change the JS-side contents.
	source[2] = 99

	elemVal, err := ctx.RunScript("new Uint8Array(ab)[2]", "idx.js")
	if err != nil {
		t.Fatalf("RunScript ab[2]: %v", err)
	}
	if elemVal == nil || elemVal.Integer() != 3 {
		var n int64
		if elemVal != nil {
			n = elemVal.Integer()
		}
		t.Errorf("ab[2] = %d, want 3 (bytes not copied / source aliased)", n)
	}
}

func TestNewArrayBufferEmpty(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()

	ab := v8.NewArrayBuffer(iso, nil)
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	if err := ctx.Global().Set("ab", ab); err != nil {
		t.Fatalf("Global().Set: %v", err)
	}

	lengthVal, err := ctx.RunScript("ab.byteLength", "len.js")
	if err != nil {
		t.Fatalf("RunScript byteLength: %v", err)
	}
	if lengthVal == nil || lengthVal.Integer() != 0 {
		var n int64
		if lengthVal != nil {
			n = lengthVal.Integer()
		}
		t.Errorf("ab.byteLength = %d, want 0", n)
	}
}
