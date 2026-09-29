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
