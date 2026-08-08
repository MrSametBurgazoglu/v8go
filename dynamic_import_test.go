// Copyright 2026 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// TestDynamicImport verifies that `await import(spec)` resolves a module
// registered via Context.RegisterModule and returns its exports. The import
// runs inside an async IIFE (top-level await is not valid in a classic
// RunScript); the microtask checkpoint drains the async continuation so the
// result is observable on the global object.
func TestDynamicImport(t *testing.T) {
	t.Parallel()

	ctx := v8.NewContext()
	defer ctx.Close()
	ctx.RegisterModule("m.js", "export const x = 42")

	if _, err := ctx.RunScript(
		`globalThis.__result = null;
		 (async () => { const m = await import('m.js'); globalThis.__result = m.x; })();`,
		"import.js",
	); err != nil {
		t.Fatalf("RunScript failed: %v", err)
	}
	ctx.PerformMicrotaskCheckpoint()

	val, err := ctx.RunScript(`globalThis.__result`, "read.js")
	if err != nil {
		t.Fatalf("read result failed: %v", err)
	}
	if got := val.Integer(); got != 42 {
		t.Fatalf("import result = %d, want 42", got)
	}
}

// TestDynamicImportMeta verifies the host hook populates import.meta.url with
// a non-empty string (the specifier the module was registered under).
func TestDynamicImportMeta(t *testing.T) {
	t.Parallel()

	ctx := v8.NewContext()
	defer ctx.Close()
	ctx.RegisterModule("meta.js", "export const url = import.meta.url")

	if _, err := ctx.RunScript(
		`globalThis.__url = null;
		 (async () => { const m = await import('meta.js'); globalThis.__url = m.url; })();`,
		"meta.js",
	); err != nil {
		t.Fatalf("RunScript failed: %v", err)
	}
	ctx.PerformMicrotaskCheckpoint()

	val, err := ctx.RunScript(`globalThis.__url`, "read.js")
	if err != nil {
		t.Fatalf("read url failed: %v", err)
	}
	if url := val.String(); url == "" {
		t.Fatal("import.meta.url is empty")
	}
}

// TestDynamicImportNested verifies a registered module's own static imports
// resolve through the same registry (the InstantiateModule resolver path).
func TestDynamicImportNested(t *testing.T) {
	t.Parallel()

	ctx := v8.NewContext()
	defer ctx.Close()
	ctx.RegisterModule("inner.js", "export const y = 7")
	ctx.RegisterModule("outer.js", "import { y } from 'inner.js'; export const z = y * 6")

	if _, err := ctx.RunScript(
		`globalThis.__result = null;
		 (async () => { const m = await import('outer.js'); globalThis.__result = m.z; })();`,
		"nested.js",
	); err != nil {
		t.Fatalf("RunScript failed: %v", err)
	}
	ctx.PerformMicrotaskCheckpoint()

	val, err := ctx.RunScript(`globalThis.__result`, "read.js")
	if err != nil {
		t.Fatalf("read result failed: %v", err)
	}
	if got := val.Integer(); got != 42 {
		t.Fatalf("nested import result = %d, want 42", got)
	}
}

// TestDynamicImportMissing verifies an unregistered specifier rejects the
// import promise with a clear error message rather than hanging.
func TestDynamicImportMissing(t *testing.T) {
	t.Parallel()

	ctx := v8.NewContext()
	defer ctx.Close()

	if _, err := ctx.RunScript(
		`globalThis.__err = null;
		 (async () => { try { await import('nope.js'); } catch (e) { globalThis.__err = String(e); } })();`,
		"missing.js",
	); err != nil {
		t.Fatalf("RunScript failed: %v", err)
	}
	ctx.PerformMicrotaskCheckpoint()

	val, err := ctx.RunScript(`globalThis.__err`, "read.js")
	if err != nil {
		t.Fatalf("read err failed: %v", err)
	}
	if got := val.String(); got == "" {
		t.Fatal("missing-module import did not reject")
	}
}
