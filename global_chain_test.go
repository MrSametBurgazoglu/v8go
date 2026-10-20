package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A [Global] interface's chain built from templates, as WebIDL has it for
// Window: global -> Window.prototype -> the named properties object (an
// interceptor on a prototype template) -> EventTarget.prototype, every link
// fixed at creation and immutable from then on.
func TestGlobalPrototypeChainFromTemplates(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	et := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value { return nil })
	et.PrototypeTemplate().SetImmutableProto()
	wp := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value { return nil })
	wp.Inherit(et)
	wp.PrototypeTemplate().SetImmutableProto()
	wp.PrototypeTemplate().SetNamedPropertyInterceptor(v8.PropertyInterceptor{
		Getter: func(info *v8.FunctionCallbackInfo) *v8.Value {
			if info.Args()[0].String() == "magic" {
				v, _ := v8.NewValue(iso, "named!")
				return v
			}
			return nil
		},
		Flags: v8.HandlerOnlyInterceptStrings,
	})
	win := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value { return nil })
	win.Inherit(wp)
	win.PrototypeTemplate().SetImmutableProto()
	g := win.InstanceTemplate()
	g.SetImmutableProto()
	ctx := v8.NewContext(iso, g)
	defer ctx.Close()
	etFn := et.GetFunction(ctx)
	wpFn := wp.GetFunction(ctx)
	winFn := win.GetFunction(ctx)
	ctx.Global().Set("ETP", mustGet(t, etFn.Object(), "prototype"))
	ctx.Global().Set("WPP", mustGet(t, wpFn.Object(), "prototype"))
	ctx.Global().Set("WINP", mustGet(t, winFn.Object(), "prototype"))
	out, err := ctx.RunScript(`[
	  Object.getPrototypeOf(globalThis) === WINP,
	  Object.getPrototypeOf(WINP) === WPP,
	  Object.getPrototypeOf(WPP) === ETP,
	  Object.getPrototypeOf(ETP) === Object.prototype,
	  magic,
	  Reflect.setPrototypeOf(globalThis, {}),
	  Reflect.setPrototypeOf(globalThis, WINP),
	  Reflect.setPrototypeOf(WINP, {}),
	  Reflect.setPrototypeOf(ETP, {}),
	  (() => { var x = 1; return typeof undefinedThing; })(),
	].join()`, "t.js")
	if err != nil {
		t.Fatal(err)
	}
	if want := "true,true,true,true,named!,false,true,false,false,undefined"; out.String() != want {
		t.Errorf("got %s, want %s", out.String(), want)
	}
}

func mustGet(t *testing.T, o *v8.Object, k string) *v8.Value {
	v, err := o.Get(k)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
