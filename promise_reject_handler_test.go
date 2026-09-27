package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// SetPromiseRejectHandler hands over the promise itself and the context it
// was created in, for both the unhandled and the handled-later event.
func TestPromiseRejectHandlerCarriesPromise(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	var events []v8.PromiseRejectEvent
	var sameCtx []bool
	var identical []bool
	check, err := ctx.RunScript(`(function (p) { return p === globalThis.p; })`, "check.js")
	if err != nil {
		t.Fatal(err)
	}
	fn, err := check.AsFunction()
	if err != nil {
		t.Fatal(err)
	}
	iso.SetPromiseRejectHandler(func(msg v8.PromiseRejectMessage) {
		events = append(events, msg.Event)
		sameCtx = append(sameCtx, msg.Context == ctx)
		if msg.Event == v8.PromiseRejectWithNoHandler && msg.Value.String() != "boom" {
			t.Errorf("reason = %q, want boom", msg.Value.String())
		}
		if msg.Event == v8.PromiseHandlerAddedAfterReject && !msg.Value.IsUndefined() {
			t.Errorf("handler-added value should be undefined")
		}
		// Promise.reject in the script below assigns p after the event, so
		// identity is checked on the handler-added event only.
		if msg.Event == v8.PromiseHandlerAddedAfterReject {
			r, err := fn.Call(ctx.Global(), msg.Promise)
			identical = append(identical, err == nil && r.Boolean())
		}
	})
	if _, err := ctx.RunScript(`var p = Promise.reject("boom");`, "a.js"); err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.RunScript(`p.catch(function () {});`, "b.js"); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0] != v8.PromiseRejectWithNoHandler || events[1] != v8.PromiseHandlerAddedAfterReject {
		t.Fatalf("events = %v", events)
	}
	for i, ok := range sameCtx {
		if !ok {
			t.Errorf("event %d: context was not the promise's", i)
		}
	}
	if len(identical) != 1 || !identical[0] {
		t.Errorf("handler-added promise was not the page's promise: %v", identical)
	}
}
