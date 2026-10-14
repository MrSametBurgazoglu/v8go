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

// A function called in its creation context reports that context as the
// entered one, though another context holds the handle; Call reports the
// holder's.
func TestFunctionCallInCreationContext(t *testing.T) {
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
	holder := v8.NewContext(iso)
	defer holder.Close()
	if _, err := callee.RunScript(`globalThis.f = function () { record(); return 7; }`, "callee.js"); err != nil {
		t.Fatal(err)
	}
	f, err := callee.Global().Get("f")
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Global().Set("g", f); err != nil {
		t.Fatal(err)
	}
	g, err := holder.Global().Get("g")
	if err != nil {
		t.Fatal(err)
	}
	fn, err := g.AsFunction()
	if err != nil {
		t.Fatal(err)
	}
	undefined := v8.Undefined(iso)
	if _, err := fn.Call(undefined); err != nil {
		t.Fatal(err)
	}
	out, err := fn.CallInCreationContext(undefined)
	if err != nil || out.Integer() != 7 {
		t.Fatalf("CallInCreationContext = %v, %v", out, err)
	}
	if len(seen) != 2 || seen[0] != holder.Ref() || seen[1] != callee.Ref() {
		t.Errorf("refs = %v, want the holder's %d then the callee's %d", seen, holder.Ref(), callee.Ref())
	}
}
