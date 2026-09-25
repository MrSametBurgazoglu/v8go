package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A callback reached from another realm's function, and from a microtask,
// names the realm that started the call chain rather than the callee's.
func TestEnteredOrMicrotaskContextRef(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	var seen []int
	record := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		seen = append(seen, iso.EnteredOrMicrotaskContextRef())
		return nil
	})
	calleeGlobal := v8.NewObjectTemplate(iso)
	_ = calleeGlobal.Set("record", record)
	callee := v8.NewContext(iso, calleeGlobal)
	defer callee.Close()
	caller := v8.NewContext(iso)
	defer caller.Close()

	fn, err := callee.RunScript(`(function () { record(); })`, "callee.js")
	if err != nil {
		t.Fatal(err)
	}
	if err := caller.Global().Set("f", fn); err != nil {
		t.Fatal(err)
	}
	if _, err := caller.RunScript(`f(); Promise.resolve().then(() => f());`, "caller.js"); err != nil {
		t.Fatal(err)
	}
	caller.PerformMicrotaskCheckpoint()
	if len(seen) != 2 || seen[0] != caller.Ref() || seen[1] != caller.Ref() {
		t.Errorf("refs = %v, want the caller's %d twice (callee is %d)", seen, caller.Ref(), callee.Ref())
	}
}
