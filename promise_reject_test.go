// Copyright 2026 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

func TestPromiseRejectUnhandled(t *testing.T) {
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
		`new Promise((_, reject) => reject(new Error('boom')))`, "unhandled.js",
	); err != nil {
		t.Fatalf("RunScript failed: %v", err)
	}
	ctx.PerformMicrotaskCheckpoint()

	if gotEvent != v8.PromiseRejectWithNoHandler {
		t.Fatalf("expected event PromiseRejectWithNoHandler (%d), got %d",
			v8.PromiseRejectWithNoHandler, gotEvent)
	}
	if gotMessage != "boom" {
		t.Fatalf("expected rejection .message %q, got %q", "boom", gotMessage)
	}
}

func TestPromiseRejectHandled(t *testing.T) {
	t.Parallel()

	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	var unhandledCount int
	iso.SetPromiseRejectCallback(func(event v8.PromiseRejectEvent, rejection *v8.Value) {
		if event == v8.PromiseRejectWithNoHandler {
			unhandledCount++
		}
	})

	if _, err := ctx.RunScript(
		`Promise.resolve().then(() => { throw new Error('boom') }).catch(() => {})`,
		"handled.js",
	); err != nil {
		t.Fatalf("RunScript failed: %v", err)
	}
	ctx.PerformMicrotaskCheckpoint()

	if unhandledCount != 0 {
		t.Fatalf("expected 0 kPromiseRejectWithNoHandler events for a handled rejection, got %d",
			unhandledCount)
	}
}
