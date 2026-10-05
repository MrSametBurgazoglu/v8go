package v8go_test

import (
	"errors"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

func opaqueOf(t *testing.T, err error) bool {
	t.Helper()
	var je *v8.JSError
	if !errors.As(err, &je) {
		t.Fatalf("not a JSError: %v", err)
	}
	return je.Opaque
}

// A script compiled with CompileOptions.Opaque marks what it throws: at its
// top level, from its compile, and from a function it defined that runs
// later. A script compiled without it marks nothing, even when the throw
// passes through it.
func TestOpaqueScriptMarksItsExceptions(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	opaque := v8.CompileOptions{Opaque: true}

	script, err := iso.CompileUnboundScript(`throw new Error("top")`, "https://other.example/a.js", opaque)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := script.Run(ctx); err == nil || !opaqueOf(t, err) {
		t.Errorf("top-level throw: %v, want an opaque JSError", err)
	}

	if _, err := iso.CompileUnboundScript(`let a = ;`, "https://other.example/b.js", opaque); err == nil || !opaqueOf(t, err) {
		t.Errorf("syntax error: %v, want an opaque JSError", err)
	}

	script, err = iso.CompileUnboundScript(`function later() { null.x; }`, "https://other.example/c.js", opaque)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := script.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ctx.RunScript(`later()`, "page.js"); err == nil || !opaqueOf(t, err) {
		t.Errorf("a later call into the opaque script's function: %v, want opaque", err)
	}
	fnVal, err := ctx.Global().Get("later")
	if err != nil {
		t.Fatal(err)
	}
	fn, err := fnVal.AsFunction()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn.Call(v8.Undefined(iso)); err == nil || !opaqueOf(t, err) {
		t.Errorf("Function.Call into the opaque script: %v, want opaque", err)
	}

	if _, err := ctx.RunScript(`throw new Error("mine")`, "page.js"); err == nil || opaqueOf(t, err) {
		t.Errorf("a plain script's throw: %v, want a JSError that is not opaque", err)
	}
	script, err = iso.CompileUnboundScript(`throw 1`, "page2.js", v8.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := script.Run(ctx); err == nil || opaqueOf(t, err) {
		t.Errorf("a default compile's throw: %v, want not opaque", err)
	}
	// The opaque bit is about reporting; the error object itself is
	// untouched, as in a browser, where e.stack still names the script.
	val, err := ctx.RunScript(`try { later() } catch (e) { e.stack }`, "page.js")
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); got == "" {
		t.Errorf("e.stack is empty")
	} else {
		t.Logf("stack of an opaque script's error: %q", got)
	}
}
