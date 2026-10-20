package v8go_test

import (
	"errors"
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// A script placed in its resource by line and column offsets reports the
// resource's line in an error's location and its stack, both for one run
// directly and for one compiled unbound: an inline <script> on line 30 of a
// document throws "at line 36", not "at line 7".
func TestScriptLineOffsets(t *testing.T) {
	t.Parallel()
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	src := "\n  var x = 1;\n  null.oops();\n"
	_, err := ctx.RunScriptAt(src, "page.html", 28, 10)
	var je *v8.JSError
	if !errors.As(err, &je) {
		t.Fatalf("RunScriptAt error = %v", err)
	}
	if !strings.HasPrefix(je.Location, "page.html:31") {
		t.Errorf("RunScriptAt location = %q, want page.html:31...", je.Location)
	}

	_, err = ctx.RunScriptAt("syntax error here", "page.html", 33, 9)
	if !errors.As(err, &je) || !strings.HasPrefix(je.Location, "page.html:34") {
		t.Errorf("syntax error location = %v", err)
	}

	script, err := iso.CompileUnboundScript(src, "page.html", v8.CompileOptions{LineOffset: 28})
	if err != nil {
		t.Fatal(err)
	}
	_, err = script.Run(ctx)
	if !errors.As(err, &je) || !strings.HasPrefix(je.Location, "page.html:31") {
		t.Errorf("unbound location = %v (%q)", err, je.Location)
	}
	if !strings.Contains(je.StackTrace, "page.html:31") {
		t.Errorf("unbound stack = %q", je.StackTrace)
	}

	// The column offset applies to the first line only.
	val, err := ctx.RunScriptAt(`new Error().stack.split('\n')[1]`, "col.html", 4, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); !strings.Contains(got, "col.html:5:21") {
		t.Errorf("first-line frame = %q, want col.html:5:21", got)
	}
}
