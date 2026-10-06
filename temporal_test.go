// Copyright 2026 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go_test

import (
	"testing"

	v8 "github.com/MrSametBurgazoglu/v8go"
)

// Temporal exists only in an archive built with v8_enable_temporal_support
// (deps/build.py's default since 2026-10-06; upstream's release archives
// leave it out). This fails on such an archive, which is the point: see
// deps/BUILDING-LIBV8.md.
func TestTemporal(t *testing.T) {
	v8.SetFlags("--harmony-temporal")
	iso := v8.NewIsolate()
	defer iso.Dispose()
	ctx := v8.NewContext(iso)
	defer ctx.Close()

	val, err := ctx.RunScript(`typeof Temporal === "object" ?
		Temporal.PlainDate.from("2026-01-31").add({ months: 1 }).toString() : "no Temporal"`, "temporal.js")
	if err != nil {
		t.Fatal(err)
	}
	// Month arithmetic constrains to the last day of the month.
	if got, want := val.String(), "2026-02-28"; got != want {
		t.Fatalf("Temporal.PlainDate arithmetic = %q, want %q (archive built without Temporal?)", got, want)
	}
}
