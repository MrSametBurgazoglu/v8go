package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A context is in use exactly while something on the stack is working in it:
// a script entered into it, or a Go callback one of its functions dispatched —
// including when the call came from ANOTHER context's script. An embedder that
// tears a context down from inside script asks this first; closing one that is
// in use leaves the frames below writing through freed memory.
func TestContextInUse(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	outer := v8.NewContext(iso)
	defer outer.Close()
	inner := v8.NewContext(iso)
	defer inner.Close()
	// One origin, so the outer script may call the inner context's function.
	inner.SetSecurityToken(outer.SecurityToken())

	var sawOuter, sawInner []bool
	probe := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		sawOuter = append(sawOuter, outer.InUse())
		sawInner = append(sawInner, inner.InUse())
		return nil
	})
	fn, err := probe.GetFunction(inner).AsObject()
	if err != nil {
		t.Fatal(err)
	}
	if err := outer.Global().Set("probe", fn); err != nil {
		t.Fatal(err)
	}

	if outer.InUse() || inner.InUse() {
		t.Fatal("a context nothing is running in reads as in use")
	}
	// The inner context's function, called by the outer context's script: the
	// outer is entered, and the inner is the callback's own.
	if _, err := outer.RunScript("probe()", "probe.js"); err != nil {
		t.Fatal(err)
	}
	if len(sawOuter) != 1 || !sawOuter[0] || !sawInner[0] {
		t.Fatalf("inside the callback: outer in use %v, inner in use %v; want both", sawOuter, sawInner)
	}
	if outer.InUse() || inner.InUse() {
		t.Fatal("a context reads as in use after the script returned")
	}

	closed := v8.NewContext(iso)
	closed.Close()
	if closed.InUse() {
		t.Fatal("a closed context reads as in use")
	}
}

// Freeing a context from inside a callback it dispatched must not free the
// struct the dispatch is still using: the free is deferred to the last frame
// working in it. The embedder should never do this (ContextInUse is how it
// knows), but a mistake is then a leaked struct rather than a write into freed
// memory.
func TestContextFreedInUseOutlivesTheFrame(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	closeIt := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		info.Context().Close()
		return nil
	})
	fn := closeIt.GetFunction(ctx)
	if err := ctx.Global().Set("closeIt", fn); err != nil {
		t.Fatal(err)
	}
	global := ctx.Global()
	fnVal, err := global.Get("closeIt")
	if err != nil {
		t.Fatal(err)
	}
	f, err := fnVal.AsFunction()
	if err != nil {
		t.Fatal(err)
	}
	// Called from Go so no script of the context is left to unwind into.
	_, _ = f.Call(v8.Undefined(iso))
	if ctx.InUse() {
		t.Fatal("a context freed in use still reads as in use")
	}
}
