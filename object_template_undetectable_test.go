// Copyright 2020 Roger Chapman and the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// An undetectable object is the [[IsHTMLDDA]] slot HTML reserves for
// document.all: typeof says "undefined", it is falsy, it is loosely equal to
// both null and undefined — and `===` still tells it apart from undefined,
// which is what makes it usable as an identity token.
func TestObjectTemplateMarkAsUndetectable(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()

	tmpl := v8.NewObjectTemplate(iso)
	tmpl.MarkAsUndetectable()
	// V8 CHECKs that an undetectable template is callable, so this is not
	// optional decoration — without it the first NewInstance aborts the
	// process.
	tmpl.SetCallAsFunctionHandler(func(info *v8.FunctionCallbackInfo) *v8.Value {
		v, _ := v8.NewValue(iso, "called")
		return v
	})

	global := v8.NewObjectTemplate(iso)
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()

	obj, err := tmpl.NewInstance(ctx)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	if err := ctx.Global().Set("all", obj); err != nil {
		t.Fatalf("Set: %v", err)
	}

	for _, tt := range []struct{ source, want string }{
		{`typeof all`, "undefined"},
		{`all ? "truthy" : "falsy"`, "falsy"},
		{`String(all == undefined)`, "true"},
		{`String(all == null)`, "true"},
		{`String(all === undefined)`, "false"},
		{`String(all === null)`, "false"},
		{`String(all === all)`, "true"},
		{`all()`, "called"},
	} {
		val, err := ctx.RunScript(tt.source, "undetectable.js")
		if err != nil {
			t.Fatalf("%s: %v", tt.source, err)
		}
		if got := val.String(); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.source, got, tt.want)
		}
	}
}

// A plain object template is unaffected: undetectability is opt-in.
func TestObjectTemplateIsDetectableByDefault(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()

	ctx := v8.NewContext(iso, v8.NewObjectTemplate(iso))
	defer ctx.Close()

	obj, err := v8.NewObjectTemplate(iso).NewInstance(ctx)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	if err := ctx.Global().Set("plain", obj); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, err := ctx.RunScript(`typeof plain`, "detectable.js")
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if got := val.String(); got != "object" {
		t.Errorf("typeof a plain template instance = %q, want %q", got, "object")
	}
}
