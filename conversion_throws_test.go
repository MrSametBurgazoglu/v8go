package v8go_test

import (
	"math"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A numeric conversion can throw: ToNumber refuses a BigInt and a Symbol,
// and an object's valueOf may throw anything. The conversions used
// ToChecked(), which is V8's fatal check -- the process aborted on
// `history.go(1n)` in the engine, since every native reads its arguments
// through these. A conversion that throws now answers NaN (Number) or 0 (the
// integer forms), and the throw goes no further than the TryCatch the call
// already opened. Has and Delete through a Proxy trap that throws are the
// same shape.
func TestConversionsThatThrowDoNotAbort(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	for _, source := range []string{"1n", "Symbol()", "({valueOf() { throw new Error('no') }})"} {
		value, err := ctx.RunScript(source, "value.js")
		if err != nil {
			t.Fatal(err)
		}
		if got := value.Number(); !math.IsNaN(got) {
			t.Errorf("Number(%s) = %v, want NaN", source, got)
		}
		if got := value.Integer(); got != 0 {
			t.Errorf("Integer(%s) = %v, want 0", source, got)
		}
		if got := value.Int32(); got != 0 {
			t.Errorf("Int32(%s) = %v, want 0", source, got)
		}
		if got := value.Uint32(); got != 0 {
			t.Errorf("Uint32(%s) = %v, want 0", source, got)
		}
	}

	value, err := ctx.RunScript(`new Proxy({}, {has() { throw 1 }, deleteProperty() { throw 1 }})`, "proxy.js")
	if err != nil {
		t.Fatal(err)
	}
	obj, err := value.AsObject()
	if err != nil {
		t.Fatal(err)
	}
	if obj.Has("x") || obj.HasIdx(0) || obj.Delete("x") || obj.DeleteIdx(0) {
		t.Error("a throwing trap answered true")
	}
	// The context is still usable: nothing was left pending.
	if v, err := ctx.RunScript("1 + 1", "after.js"); err != nil || v.Integer() != 2 {
		t.Errorf("after the throws: %v, %v", v, err)
	}
}
