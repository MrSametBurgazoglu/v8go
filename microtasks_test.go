// Copyright 2026 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// TestMicrotasksExplicitDefers proves that under MicrotasksExplicit, a microtask
// queued via Promise.resolve().then(cb) during RunScript does NOT run until the
// embedder explicitly calls PerformMicrotaskCheckpoint. This is the defining
// behavior of kExplicit. (queueMicrotask is a Web API, not a V8 global; a bare
// isolate schedules microtasks through promise reactions.)
func TestMicrotasksExplicitDefers(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	iso.SetMicrotasksPolicy(v8.MicrotasksExplicit)

	if _, err := ctx.RunScript(
		`Promise.resolve().then(() => { globalThis.__ran = true })`, "queue.js",
	); err != nil {
		t.Fatalf("queue RunScript failed: %v", err)
	}

	ranBefore, err := ctx.RunScript(`globalThis.__ran === true`, "before.js")
	if err != nil {
		t.Fatalf("before-check RunScript failed: %v", err)
	}
	if ranBefore.Boolean() {
		t.Fatal("microtask ran before PerformMicrotaskCheckpoint under MicrotasksExplicit")
	}

	iso.PerformMicrotaskCheckpoint()

	ranAfter, err := ctx.RunScript(`globalThis.__ran === true`, "after.js")
	if err != nil {
		t.Fatalf("after-check RunScript failed: %v", err)
	}
	if !ranAfter.Boolean() {
		t.Fatal("microtask did not run after PerformMicrotaskCheckpoint under MicrotasksExplicit")
	}
}

// TestMicrotasksAuto confirms that under MicrotasksAuto (the V8 default), a
// queued microtask is drained automatically at the end of RunScript, so the
// flag is set before any explicit checkpoint. The discriminating assertion is
// in TestMicrotasksExplicitDefers (explicit defers; auto does not).
func TestMicrotasksAuto(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	iso.SetMicrotasksPolicy(v8.MicrotasksAuto)

	if _, err := ctx.RunScript(
		`Promise.resolve().then(() => { globalThis.__ran = true })`, "queue.js",
	); err != nil {
		t.Fatalf("queue RunScript failed: %v", err)
	}

	ranBefore, err := ctx.RunScript(`globalThis.__ran === true`, "before.js")
	if err != nil {
		t.Fatalf("before-check RunScript failed: %v", err)
	}
	if !ranBefore.Boolean() {
		t.Fatal("microtask did not auto-drain at end of RunScript under MicrotasksAuto")
	}

	iso.PerformMicrotaskCheckpoint()
}
