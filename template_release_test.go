package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// Release gives a template back while the isolate lives: the callback count
// falls, and a function kept past its context's close answers undefined
// instead of crashing.
func TestTemplateReleaseUnregistersItsCallback(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	before := iso.CallbackCount()

	tmpl := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		v, _ := v8.NewValue(iso, int32(42))
		return v
	})
	if got := iso.CallbackCount(); got != before+1 {
		t.Fatalf("callbacks after one template: %d, want %d", got, before+1)
	}
	ctx := v8.NewContext(iso)
	fn := tmpl.GetFunction(ctx)
	global := ctx.Global()
	if err := global.Set("answer", fn); err != nil {
		t.Fatal(err)
	}
	if v, err := ctx.RunScript("answer()", "a.js"); err != nil || v.Int32() != 42 {
		t.Fatalf("the function did not answer 42: %v %v", v, err)
	}

	tmpl.Release()
	if got := iso.CallbackCount(); got != before {
		t.Fatalf("callbacks after release: %d, want %d", got, before)
	}
	// The context still holds the function; calling it is undefined now.
	v, err := ctx.RunScript("answer()", "b.js")
	if err != nil {
		t.Fatalf("calling a released template's function threw: %v", err)
	}
	if !v.IsUndefined() {
		t.Fatalf("a released template's function answered %v, want undefined", v)
	}
	global.Release()
	ctx.Close()

	// Releasing twice, and releasing an object template, are no-ops.
	tmpl.Release()
	obj := v8.NewObjectTemplate(iso)
	obj.Release()
	obj.Release()
}
