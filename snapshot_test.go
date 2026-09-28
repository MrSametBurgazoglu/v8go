package v8go

import (
	"testing"
)

// buildSnapshotSurface is the construction both the recording and the replay
// run: the same call sites, in the same order.
func buildSnapshotSurface(iso *Isolate, name string) *ObjectTemplate {
	global := NewObjectTemplate(iso)
	hello := NewFunctionTemplate(iso, func(info *FunctionCallbackInfo) *Value {
		v, _ := NewValue(iso, "hello from "+name)
		return v
	})
	_ = global.Set("hello", hello)
	fast := NewFastFunctionTemplate(iso, "s", func(info *FastCallbackInfo) {
		info.ReturnString(name + ":" + info.String(0))
	})
	_ = global.Set("fast", fast)
	iface := NewFunctionTemplate(iso, func(info *FunctionCallbackInfo) *Value { return nil })
	iface.SetClassName("Widget")
	getter := NewFunctionTemplate(iso, func(info *FunctionCallbackInfo) *Value {
		v, _ := NewValue(iso, name+" getter")
		return v
	})
	iface.PrototypeTemplate().SetAccessorProperty("who", getter, nil, None)
	_ = global.Set("Widget", iface)
	return global
}

func TestSnapshotRecordAndReplay(t *testing.T) {
	creator := NewSnapshotCreator()
	iso := creator.Isolate()
	table := NewCallbackTable()
	iso.BeginRecording(table)
	global := buildSnapshotSurface(iso, "creator")
	rec := iso.EndRecording()
	ctx := NewContext(iso, global)
	ctx.SetCallbackTable(table)
	if v, err := ctx.RunScript(`globalThis.state = { n: 41, list: [1,2,3] }; class Local { get x() { return 7; } }; globalThis.L = new Local(); hello()`, "boot.js"); err != nil || v.String() != "hello from creator" {
		t.Fatalf("creator context: %v %v", v, err)
	}
	blob, err := creator.CreateBlob(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recording: %d events, %d templates; blob %d bytes", len(rec.Sites), rec.Templates(), len(blob))

	iso2, err := NewIsolateFromSnapshot(blob)
	if err != nil {
		t.Fatal(err)
	}
	defer iso2.Dispose()
	var contexts []*Context
	for _, who := range []string{"page", "frame"} {
		table := NewCallbackTable()
		if err := iso2.BeginReplay(table, rec.Sites); err != nil {
			t.Fatal(err)
		}
		buildSnapshotSurface(iso2, who)
		if err := iso2.EndReplay(); err != nil {
			t.Fatalf("replay for %s: %v", who, err)
		}
		c, err := NewContextFromSnapshot(iso2, 0)
		if err != nil {
			t.Fatal(err)
		}
		c.SetCallbackTable(table)
		contexts = append(contexts, c)
	}
	// The shape of the surface is the one a context built without a snapshot
	// has.
	live := NewIsolate()
	defer live.Dispose()
	liveCtx := NewContext(live, buildSnapshotSurface(live, "live"))
	shape, err := liveCtx.RunScript(`Object.getOwnPropertyNames(Widget.prototype).join()`, "shape.js")
	if err != nil {
		t.Fatal(err)
	}
	liveShape := shape.String()
	liveCtx.Close()
	for i, who := range []string{"page", "frame"} {
		v, err := contexts[i].RunScript(`[hello(), fast('x'), new Widget().who, state.n + 1, L.x, Object.getOwnPropertyNames(Widget.prototype).join()].join('|')`, "use.js")
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
		if want := "hello from " + who + "|" + who + ":x|" + who + " getter|42|7|" + liveShape; v.String() != want {
			t.Errorf("%s answered %q, want %q", who, v.String(), want)
		}
	}
	for _, c := range contexts {
		c.Close()
	}
}

// A replay that does not follow the recording fails instead of binding a
// closure to another function's slot.
func TestSnapshotReplayDivergenceFails(t *testing.T) {
	creator := NewSnapshotCreator()
	iso := creator.Isolate()
	table := NewCallbackTable()
	iso.BeginRecording(table)
	global := buildSnapshotSurface(iso, "creator")
	rec := iso.EndRecording()
	ctx := NewContext(iso, global)
	blob, err := creator.CreateBlob(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	iso2, err := NewIsolateFromSnapshot(blob)
	if err != nil {
		t.Fatal(err)
	}
	defer iso2.Dispose()
	if err := iso2.BeginReplay(NewCallbackTable(), rec.Sites); err != nil {
		t.Fatal(err)
	}
	NewObjectTemplate(iso2) // not where the recording made its first template
	if err := iso2.EndReplay(); err == nil {
		t.Fatal("a divergent replay succeeded")
	}
}

// A wrapper's integer internal field — an embedder's key for the native
// object behind it — survives the snapshot; V8's serialiser takes a small
// integer for an aligned pointer and would otherwise hand it to a callback
// nobody installed.
func TestSnapshotKeepsIntegerInternalFields(t *testing.T) {
	creator := NewSnapshotCreator()
	iso := creator.Isolate()
	table := NewCallbackTable()
	iso.BeginRecording(table)
	wrapper := NewObjectTemplate(iso)
	wrapper.SetInternalFieldCount(1)
	rec := iso.EndRecording()
	ctx := NewContext(iso)
	obj, err := wrapper.NewInstance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := NewValue(iso, int32(4242))
	if err := obj.SetInternalField(0, id); err != nil {
		t.Fatal(err)
	}
	if err := ctx.Global().Set("keyed", obj); err != nil {
		t.Fatal(err)
	}
	blob, err := creator.CreateBlob(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	iso2, err := NewIsolateFromSnapshot(blob)
	if err != nil {
		t.Fatal(err)
	}
	defer iso2.Dispose()
	ctx2, err := NewContextFromSnapshot(iso2, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx2.Close()
	v, err := ctx2.Global().Get("keyed")
	if err != nil {
		t.Fatal(err)
	}
	o, err := v.AsObject()
	if err != nil {
		t.Fatal(err)
	}
	if f := o.GetInternalField(0); f == nil || f.Int32() != 4242 {
		t.Errorf("internal field after the snapshot: %v, want 4242", f)
	}
}
