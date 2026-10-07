package v8go_test

import (
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

func TestAllowCodeGenerationFromStrings(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	if v, err := ctx.RunScript(`eval('1+1')`, "a.js"); err != nil || v.Integer() != 2 {
		t.Fatalf("control: eval before refusing: %v %v", v, err)
	}
	refused := 0
	iso.SetCodeGenerationRefusedHandler(func(c *v8.Context) {
		if c == ctx {
			refused++
		}
	})
	ctx.AllowCodeGenerationFromStrings(false, "refused by policy")
	for _, src := range []string{`eval('1+1')`, `new Function('return 1')()`, `(0, eval)('1')`} {
		if _, err := ctx.RunScript(src, "b.js"); err == nil || !strings.Contains(err.Error(), "refused by policy") {
			t.Errorf("%s: %v", src, err)
		}
	}
	if v, err := ctx.RunScript(`var a = [1]; eval(a) === a`, "e.js"); err != nil || !v.Boolean() {
		t.Errorf("eval of a non-string under refusal should be the identity: %v %v", v, err)
	}
	if refused != 3 {
		t.Errorf("the handler heard %d refusals, want 3", refused)
	}
	if v, err := ctx.RunScript(`1+2`, "c.js"); err != nil || v.Integer() != 3 {
		t.Errorf("ordinary script after refusing: %v %v", v, err)
	}
	ctx.AllowCodeGenerationFromStrings(true, "")
	if v, err := ctx.RunScript(`eval('2+2')`, "d.js"); err != nil || v.Integer() != 4 {
		t.Errorf("eval after allowing again: %v %v", v, err)
	}
}

// SetCodeGenerationCheck decides each string compilation of a refusing
// context; eval of a code-like object, and the Function constructor over
// code-like arguments, compile the text the check names.
func TestCodeGenerationCheck(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	global := v8.NewObjectTemplate(iso)
	// Code-like is a property of an API constructor's instances, so the
	// template is a FunctionTemplate's instance template.
	ctor := v8.NewFunctionTemplate(iso, func(info *v8.FunctionCallbackInfo) *v8.Value { return nil })
	ctor.InstanceTemplate().SetCodeLike()
	ctx := v8.NewContext(iso, global)
	defer ctx.Close()
	obj, err := ctor.GetFunction(ctx).NewInstance()
	if err != nil {
		t.Fatal(err)
	}
	if err := ctx.Global().Set("trusted", obj); err != nil {
		t.Fatal(err)
	}
	var seen []string
	iso.SetCodeGenerationCheck(func(c *v8.Context, source *v8.Value, isCodeLike bool) (bool, string, bool) {
		if source.IsCodeLike() {
			seen = append(seen, "code-like")
			return true, "40+2", true
		}
		if isCodeLike {
			// The Function constructor's source, assembled from code-like
			// arguments: compiled as it is.
			seen = append(seen, "function:"+source.String())
			return true, "", false
		}
		if !source.IsString() {
			seen = append(seen, "object")
			return true, "", false
		}
		text := source.String()
		seen = append(seen, text)
		return text == "1+1", "", false
	})
	// An allowing context compiles strings without asking, and hands any
	// object back: an embedder that wants the check consulted refuses
	// outright and lets the check allow.
	if v, err := ctx.RunScript(`eval('3+3')`, "a.js"); err != nil || v.Integer() != 6 {
		t.Fatalf("an allowing context: %v %v", v, err)
	}
	ctx.AllowCodeGenerationFromStrings(false, "refused by check")
	if v, err := ctx.RunScript(`eval('1+1')`, "b.js"); err != nil || v.Integer() != 2 {
		t.Errorf("an allowed string: %v %v", v, err)
	}
	if _, err := ctx.RunScript(`eval('2+2')`, "c.js"); err == nil || !strings.Contains(err.Error(), "refused by check") {
		t.Errorf("a refused string: %v", err)
	}
	if v, err := ctx.RunScript(`eval(trusted)`, "d.js"); err != nil || v.Integer() != 42 {
		t.Errorf("eval(code-like) in a refusing context: %v %v", v, err)
	}
	if v, err := ctx.RunScript(`var a = [1]; eval(a) === a`, "e.js"); err != nil || !v.Boolean() {
		t.Errorf("eval of a plain object stays the identity: %v %v", v, err)
	}
	if v, err := ctx.RunScript(`Object.getPrototypeOf(trusted).toString = function () { return 'return 7'; }; new Function(trusted)()`, "f.js"); err != nil || v.Integer() != 7 {
		t.Errorf("Function constructor from a code-like argument: %v %v", v, err)
	}
	if got := strings.Join(seen, "|"); !strings.HasPrefix(got, "1+1|2+2|code-like|object|") || !strings.Contains(got, "function:") {
		t.Errorf("the check saw %q", seen)
	}
}
