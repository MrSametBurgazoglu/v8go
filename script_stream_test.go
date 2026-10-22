package v8go_test

import (
	"strings"
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

func TestScriptStreamCompilesAndRuns(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	var b strings.Builder
	b.WriteString("var total = 0;\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("function f")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString("_" + string(rune('a'+i%26)) + "() { return 1; }\n")
	}
	b.WriteString("total = 6 * 7; 'é' + total;")
	source := b.String()
	stream := iso.StartScriptStream(source)
	script, err := stream.Compile(source, "streamed.js", v8.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	val, err := script.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); got != "é42" {
		t.Fatalf("streamed script ran to %q, want é42", got)
	}
}

func TestScriptStreamReportsASyntaxError(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	source := "let a = ;"
	stream := iso.StartScriptStream(source)
	if _, err := stream.Compile(source, "bad.js", v8.CompileOptions{}); err == nil {
		t.Fatal("a syntax error compiled")
	}
}

func TestScriptStreamCanBeAbandoned(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	iso.StartScriptStream("var x = 1;").Close()
}

func TestIncrementalScriptStreamParsesAsItArrives(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		b.WriteString("function g")
		b.WriteString(strings.Repeat("y", i%5))
		b.WriteString("() { return 'ç'; }\n")
	}
	b.WriteString("'ğ' + (6 * 7);")
	source := b.String()
	stream := iso.StartIncrementalScriptStream()
	// Odd-sized chunks, one of them splitting a two-byte character.
	data := []byte(source)
	for start := 0; start < len(data); {
		end := start + 1 + (start*7)%5000
		if end > len(data) {
			end = len(data)
		}
		stream.Push(data[start:end])
		start = end
	}
	script, err := stream.Compile(source, "incremental.js", v8.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	val, err := script.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); got != "ğ42" {
		t.Fatalf("incrementally streamed script ran to %q, want ğ42", got)
	}
}

func TestIncrementalScriptStreamCanBeAbandonedUnfinished(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	stream := iso.StartIncrementalScriptStream()
	stream.Push([]byte("var half = "))
	stream.Close()
}

// A streamed script placed in its resource reports the resource's lines, as
// a compile with the same offsets does.
func TestScriptStreamKeepsLineOffsets(t *testing.T) {
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()
	source := "\n\nnew Error('x').stack"
	stream := iso.StartScriptStream(source)
	script, err := stream.Compile(source, "page.html", v8.CompileOptions{LineOffset: 40, ColumnOffset: 3})
	if err != nil {
		t.Fatal(err)
	}
	val, err := script.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := val.String(); !strings.Contains(got, "page.html:43:") {
		t.Fatalf("streamed stack %q, want line 43 of page.html", got)
	}
	if _, err := stream.Compile(source, "page.html", v8.CompileOptions{}); err == nil {
		t.Fatal("a spent stream compiled again")
	}
}
