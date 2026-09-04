package v8go_test

import (
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A generated WebIDL interface is a FunctionTemplate whose prototype carries
// accessor pairs and methods, whose instances carry an internal field, and
// which inherits from its parent interface's template. These pin the pieces
// that shape, because each of them is observable from a page and none of them
// could be built before.

func TestFunctionTemplateInheritBuildsThePrototypeChain(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()

	parent := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value { return nil })
	parent.SetClassName("Node")
	child := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value { return nil })
	child.SetClassName("Element")
	child.Inherit(parent)

	global := v8.NewObjectTemplate(iso)
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()

	if err := ctx.Global().Set("Node", parent.GetFunction(ctx).Value); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Global().Set("Element", child.GetFunction(ctx).Value); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ expr, want string }{
		{"Object.getPrototypeOf(Element.prototype) === Node.prototype", "true"},
		{"new Element() instanceof Node", "true"},
		{"Element.prototype.constructor.name", "Element"},
	} {
		val, err := ctx.RunScript(c.expr, "t.js")
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := val.String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}
}

// An attribute is an accessor pair, not a data property holding a function.
// A page can tell: it reads the descriptor, replaces the getter, and calls
// through it from a subclass.
func TestAccessorPropertyIsARealAccessor(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()

	iface := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value { return nil })
	iface.SetClassName("Thing")

	getter := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		val, _ := v8.NewValue(iso, "read")
		return val
	})
	var wrote string
	setter := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		if len(info.Args()) > 0 {
			wrote = info.Args()[0].String()
		}
		return nil
	})
	iface.PrototypeTemplate().SetAccessorProperty("name", getter, setter, v8.None)

	readonlyGetter := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		val, _ := v8.NewValue(iso, int32(7))
		return val
	})
	iface.PrototypeTemplate().SetAccessorProperty("count", readonlyGetter, nil, v8.None)

	ctx := v8.NewContext(iso, v8.NewObjectTemplate(iso))
	defer ctx.Close()
	if err := ctx.Global().Set("Thing", iface.GetFunction(ctx).Value); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ expr, want string }{
		{"typeof Object.getOwnPropertyDescriptor(Thing.prototype, 'name').get", "function"},
		{"typeof Object.getOwnPropertyDescriptor(Thing.prototype, 'name').set", "function"},
		{"'value' in Object.getOwnPropertyDescriptor(Thing.prototype, 'name')", "false"},
		{"new Thing().name", "read"},
		{"new Thing().count", "7"},
		{"typeof Object.getOwnPropertyDescriptor(Thing.prototype, 'count').set", "undefined"},
		// The accessor lives on the prototype, not on the instance: a page
		// that enumerates an instance's own properties must see nothing.
		{"Object.getOwnPropertyNames(new Thing()).length", "0"},
	} {
		val, err := ctx.RunScript(c.expr, "t.js")
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := val.String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}

	if _, err := ctx.RunScript(`var t = new Thing(); t.name = 'written';`, "t.js"); err != nil {
		t.Fatal(err)
	}
	if wrote != "written" {
		t.Errorf("the setter received %q, want \"written\"", wrote)
	}
}

// Without Symbol.toStringTag every wrapper answers "[object Object]", which is
// how a great deal of library code decides what it has been handed.
func TestSymbolToStringTag(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()

	iface := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value { return nil })
	iface.SetClassName("Widget")
	tag, err := v8.NewValue(iso, "Widget")
	if err != nil {
		t.Fatal(err)
	}
	iface.PrototypeTemplate().SetSymbolValue("toStringTag", tag, v8.ReadOnly|v8.DontEnum)

	ctx := v8.NewContext(iso, v8.NewObjectTemplate(iso))
	defer ctx.Close()
	if err := ctx.Global().Set("Widget", iface.GetFunction(ctx).Value); err != nil {
		t.Fatal(err)
	}
	val, err := ctx.RunScript(`Object.prototype.toString.call(new Widget())`, "t.js")
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); got != "[object Widget]" {
		t.Errorf("toString gives %q, want \"[object Widget]\"", got)
	}
	val, err = ctx.RunScript(`Object.keys(Widget.prototype).length`, "t.js")
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); got != "0" {
		t.Errorf("the tag is enumerable: Object.keys gives %s", got)
	}
}

// The instance template is where internal fields and unforgeable own
// properties live; the prototype template is what instances inherit. Mixing
// them up is the difference between a member a page can delete and one it
// cannot.
func TestInstanceAndPrototypeTemplatesAreDifferentObjects(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()

	iface := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value { return nil })
	iface.SetClassName("Holder")
	iface.InstanceTemplate().SetInternalFieldCount(1)
	iface.SetLength(2)
	iface.ReadOnlyPrototype()

	unforgeable := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value {
		val, _ := v8.NewValue(iso, "own")
		return val
	})
	iface.InstanceTemplate().SetAccessorProperty("origin", unforgeable, nil, v8.DontDelete)

	ctx := v8.NewContext(iso, v8.NewObjectTemplate(iso))
	defer ctx.Close()
	if err := ctx.Global().Set("Holder", iface.GetFunction(ctx).Value); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ expr, want string }{
		{"Holder.length", "2"},
		{"new Holder().origin", "own"},
		{"Object.getOwnPropertyNames(new Holder()).join(',')", "origin"},
		{"'origin' in Holder.prototype", "false"},
		{"(function(){ var h = new Holder(); delete h.origin; return h.origin; })()", "own"},
	} {
		val, err := ctx.RunScript(c.expr, "t.js")
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := val.String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}

	obj, err := ctx.RunScript(`new Holder()`, "t.js")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := obj.AsObject()
	if err != nil {
		t.Fatal(err)
	}
	if n := instance.InternalFieldCount(); n != 1 {
		t.Errorf("instance has %d internal fields, want 1", n)
	}
}

// Releasing a template must unregister the callbacks its accessors were built
// from, and must not unregister them while an accessor is still reachable —
// Isolate.cbs growing without bound is a live leak, and releasing early is a
// getter that answers undefined.
func TestReleaseCoversAccessorCallbacks(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()

	before := iso.CallbackCount()
	iface := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value { return nil })
	getter := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value { return nil })
	setter := v8.NewFunctionTemplate(iso, func(*v8.FunctionCallbackInfo) *v8.Value { return nil })
	proto := iface.PrototypeTemplate()
	proto.SetAccessorProperty("x", getter, setter, v8.None)
	if iso.CallbackCount() <= before {
		t.Fatal("no callbacks were registered; the test cannot see a leak")
	}

	proto.Release()
	iface.Release()
	if got := iso.CallbackCount(); got > before {
		t.Errorf("%d callbacks remain registered after Release, was %d before", got, before)
	}
}

func TestSetSymbolValueIgnoresAnUnknownSymbol(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	tmpl := v8.NewObjectTemplate(iso)
	val, _ := v8.NewValue(iso, "x")
	// Must not panic or corrupt the template.
	tmpl.SetSymbolValue("notASymbol", val, v8.None)
	ctx := v8.NewContext(iso, tmpl)
	defer ctx.Close()
	out, err := ctx.RunScript(`Object.getOwnPropertySymbols(globalThis).length`, "t.js")
	if err != nil || !strings.Contains(out.String(), "0") {
		t.Errorf("unknown symbol left a mark: %v %v", out, err)
	}
}

// Each attribute flag must mean what V8 means by it. The constants were
// `1 << iota` in a block whose first line consumed iota 0, so every flag was
// one position too high: ReadOnly set DontEnum, DontEnum set DontDelete, and
// DontDelete was 8 — outside V8's three-bit mask, where it did nothing.
func TestPropertyAttributesMeanWhatTheySay(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()

	tmpl := v8.NewObjectTemplate(iso)
	for _, c := range []struct {
		name string
		attr v8.PropertyAttribute
	}{
		{"plain", v8.None},
		{"readonly", v8.ReadOnly},
		{"hidden", v8.DontEnum},
		{"permanent", v8.DontDelete},
	} {
		val, err := v8.NewValue(iso, "v")
		if err != nil {
			t.Fatal(err)
		}
		if err := tmpl.Set(c.name, val, c.attr); err != nil {
			t.Fatal(err)
		}
	}

	ctx := v8.NewContext(iso, tmpl)
	defer ctx.Close()

	for _, c := range []struct{ expr, want string }{
		{"Object.getOwnPropertyDescriptor(globalThis,'plain').writable", "true"},
		{"Object.getOwnPropertyDescriptor(globalThis,'plain').enumerable", "true"},
		{"Object.getOwnPropertyDescriptor(globalThis,'plain').configurable", "true"},

		{"Object.getOwnPropertyDescriptor(globalThis,'readonly').writable", "false"},
		{"Object.getOwnPropertyDescriptor(globalThis,'readonly').enumerable", "true"},

		{"Object.getOwnPropertyDescriptor(globalThis,'hidden').enumerable", "false"},
		{"Object.getOwnPropertyDescriptor(globalThis,'hidden').writable", "true"},

		{"Object.getOwnPropertyDescriptor(globalThis,'permanent').configurable", "false"},
		{"Object.getOwnPropertyDescriptor(globalThis,'permanent').writable", "true"},
	} {
		val, err := ctx.RunScript(c.expr, "t.js")
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := val.String(); got != c.want {
			t.Errorf("%s = %q, want %q", c.expr, got, c.want)
		}
	}
}
