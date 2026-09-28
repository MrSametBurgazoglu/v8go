// Copyright 2026 the v8go contributors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package v8go

// #include <stdlib.h>
// #include "v8go.h"
import "C"
import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// Startup snapshots.
//
// An embedder that builds a large JavaScript surface into every context —
// thousands of interface objects, a bootstrap script — pays for it on every
// context it makes. V8 can instead serialise a context after it has been
// built and deserialise it for the next one. The heap is the easy half. The
// hard half is the embedder's native callbacks: a context holds functions
// whose FunctionTemplate data names a Go callback, and in v8go that name is a
// number in a per-isolate registry, handed out in whatever order templates
// happened to be made — while an embedder with several contexts on one
// isolate (a page and its frames) has one set of closures per context.
//
// So the templates that go into a snapshot are made under a CallbackTable:
//
//   - Recording (on a SnapshotCreator's isolate), every callback registered
//     goes into the table at the next index, and the data the template
//     carries is that index, negated — a table reference. The templates, and
//     the Go call site that made each template or registered each callback,
//     are logged in order.
//   - Replaying (on an isolate made from the snapshot), the embedder runs the
//     same construction code. Each template it asks for is the snapshot's
//     next one; each callback goes into ITS table at the same index; nothing
//     is mutated, since the snapshot's templates are already built. Every
//     event is checked against the recorded call site, and the first that
//     differs makes the replay fail rather than bind a closure to the wrong
//     function.
//   - A callback with a table reference is looked up in the table of the
//     context it is called in (Context.SetCallbackTable), so one snapshot's
//     templates serve every context on the isolate, each with its own
//     closures. Templates made outside a recording or replay keep isolate
//     references, which are positive, and behave exactly as before.

// noCallback is the callback reference meaning "none" (kNoCallback in
// v8go.cc). Not -1: that is a callback table's first entry.
const noCallback = -1 << 31

// CallbackTable is the set of callbacks a context's snapshot templates call.
type CallbackTable struct {
	mu      sync.RWMutex
	entries []interface{} // FunctionCallback or FastCallback; index k-1
}

// NewCallbackTable makes an empty table.
func NewCallbackTable() *CallbackTable { return &CallbackTable{} }

func (t *CallbackTable) add(cb interface{}) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = append(t.entries, cb)
	return len(t.entries)
}

func (t *CallbackTable) at(k int) interface{} {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if k < 1 || k > len(t.entries) {
		return nil
	}
	return t.entries[k-1]
}

// Len is the number of callbacks in the table.
func (t *CallbackTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}

// SetCallbackTable names the table this context's snapshot templates call
// into. See CallbackTable.
func (c *Context) SetCallbackTable(t *CallbackTable) { c.callbacks = t }

// lookupCallback resolves a callback reference for a call made in ctx: a
// negative reference is an index into the context's table, a positive one
// the isolate's registry.
func lookupFunctionCallback(ctx *Context, ref int) FunctionCallback {
	if ref < 0 {
		if ctx.callbacks == nil {
			return nil
		}
		cb, _ := ctx.callbacks.at(-ref).(FunctionCallback)
		return cb
	}
	return ctx.iso.getCallback(ref)
}

func lookupFastCallback(ctx *Context, ref int) FastCallback {
	if ref < 0 {
		if ctx.callbacks == nil {
			return nil
		}
		cb, _ := ctx.callbacks.at(-ref).(FastCallback)
		return cb
	}
	return ctx.iso.getFastCallback(ref)
}

// templateEvent is one step of a recording: a template made, or a callback
// registered, and where in the embedder it happened.
type templateEvent struct {
	site       string
	isTemplate bool
	template   *template // the recorded one; nil for a callback, or on a replay
}

type templateLog struct {
	table  *CallbackTable
	replay bool
	events []templateEvent
	// For a replay: the snapshot's templates in recording order, and where
	// the next event is.
	snapshot []*template
	next     int
	nextTmpl int
	err      error
	// derived is a recording's instance and prototype templates, reached
	// through the ones it made; they hold handles too.
	derived []*template
}

// BeginRecording starts logging this isolate's template construction under
// table. Every template made until EndRecording goes into the snapshot.
func (i *Isolate) BeginRecording(table *CallbackTable) {
	i.log = &templateLog{table: table}
}

// Recording is what a recording logged, in order.
type Recording struct {
	Sites     []string
	templates []*template
	// kinds[i] says whether templates[i] is a FunctionTemplate.
	kinds   []bool
	derived []*template
}

// Templates is how many templates the recording made.
func (r *Recording) Templates() int { return len(r.templates) }

// EndRecording stops logging and answers what was logged.
func (i *Isolate) EndRecording() *Recording {
	log := i.log
	i.log = nil
	rec := &Recording{}
	if log == nil {
		return rec
	}
	rec.derived = log.derived
	for _, e := range log.events {
		kind := "C"
		if e.isTemplate {
			kind = "T"
			rec.templates = append(rec.templates, e.template)
			rec.kinds = append(rec.kinds, e.template.function)
		}
		rec.Sites = append(rec.Sites, kind+" "+e.site)
	}
	return rec
}

// BeginReplay makes this isolate answer template construction from its
// snapshot, registering callbacks into table. sites is the recording's.
func (i *Isolate) BeginReplay(table *CallbackTable, sites []string) error {
	if i.snapshotTemplates == nil {
		return errors.New("v8go: this isolate was not made from a snapshot")
	}
	i.log = &templateLog{table: table, replay: true, snapshot: i.snapshotTemplates}
	for _, s := range sites {
		kind, site, _ := strings.Cut(s, " ")
		i.log.events = append(i.log.events, templateEvent{site: site, isTemplate: kind == "T"})
	}
	return nil
}

// EndReplay stops replaying, and reports the first divergence from the
// recording, or a replay that stopped short of it.
func (i *Isolate) EndReplay() error {
	log := i.log
	i.log = nil
	if log == nil {
		return errors.New("v8go: no replay in progress")
	}
	if log.err != nil {
		return log.err
	}
	if log.next != len(log.events) {
		return fmt.Errorf("v8go: replay made %d of the recording's %d templates and callbacks", log.next, len(log.events))
	}
	return nil
}

// callerSite is the first call site outside this package.
func callerSite() string {
	var pcs [24]uintptr
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "github.com/MrSametBurgazoglu/v8go.") {
			return fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		if !more {
			return "?"
		}
	}
}

// step checks the next replayed event against the recording.
func (l *templateLog) step(site string, isTemplate bool) {
	if l.err != nil {
		return
	}
	if l.next >= len(l.events) {
		l.err = fmt.Errorf("v8go: replay went past the recording at %s", site)
		return
	}
	if want := l.events[l.next]; want.site != site || want.isTemplate != isTemplate {
		l.err = fmt.Errorf("v8go: replay event %d at %s (template %v), recorded at %s (template %v)",
			l.next, site, isTemplate, want.site, want.isTemplate)
	}
	l.next++
}

// registerFunction registers a FunctionCallback: into the log's table while
// recording or replaying, into the isolate otherwise.
func (i *Isolate) registerFunction(cb FunctionCallback) int {
	if l := i.log; l != nil {
		site := callerSite()
		if l.replay {
			l.step(site, false)
		} else {
			l.events = append(l.events, templateEvent{site: site})
		}
		return -l.table.add(cb)
	}
	return i.registerCallback(cb)
}

func (i *Isolate) registerFast(cb FastCallback) int {
	if l := i.log; l != nil {
		site := callerSite()
		if l.replay {
			l.step(site, false)
		} else {
			l.events = append(l.events, templateEvent{site: site})
		}
		return -l.table.add(cb)
	}
	return i.registerFastCallback(cb)
}

// newTemplate makes a template, or while replaying hands out the snapshot's
// next one. create is not called on a replay.
func (i *Isolate) newTemplate(create func() C.TemplatePtr, cbref int, function bool) *template {
	l := i.log
	if l == nil {
		t := &template{ptr: create(), iso: i, cbref: cbref, function: function}
		runtime.SetFinalizer(t, (*template).finalizer)
		return t
	}
	site := callerSite()
	if !l.replay {
		t := &template{ptr: create(), iso: i, cbref: cbref, function: function}
		// Kept by the log, not finalised: a recording's templates must all
		// be released before the blob is cut (CreateBlob).
		l.events = append(l.events, templateEvent{site: site, isTemplate: true, template: t})
		return t
	}
	l.step(site, true)
	if l.err != nil || l.nextTmpl >= len(l.snapshot) {
		if l.err == nil {
			l.err = fmt.Errorf("v8go: replay asked for template %d of %d at %s", l.nextTmpl+1, len(l.snapshot), site)
		}
		// The replay is already failed; answer a real template so the
		// caller's construction does not crash on its way to finding out.
		t := &template{ptr: create(), iso: i, cbref: cbref, function: function}
		runtime.SetFinalizer(t, (*template).finalizer)
		return t
	}
	shared := l.snapshot[l.nextTmpl]
	l.nextTmpl++
	return &template{ptr: shared.ptr, iso: i, cbref: cbref, function: function, replayed: true}
}

// SnapshotCreator builds a startup snapshot from a context made on its
// isolate.
type SnapshotCreator struct {
	iso *Isolate
}

// NewSnapshotCreator makes an isolate to build a snapshot on.
func NewSnapshotCreator() *SnapshotCreator {
	initializeIfNecessary()
	iso := &Isolate{
		ptr:     C.NewSnapshotCreatorIsolate(),
		cbs:     make(map[int]FunctionCallback),
		creator: true,
	}
	iso.null = newValueNull(iso)
	iso.undefined = newValueUndefined(iso)
	return &SnapshotCreator{iso: iso}
}

// Isolate is the creator's isolate.
func (s *SnapshotCreator) Isolate() *Isolate { return s.iso }

// CreateBlob serialises ctx, which becomes context 0 of the snapshot, and
// the recording's templates, which an isolate made from the blob answers a
// replay with. Every other handle on the isolate is released first — V8
// refuses to serialise with any alive — so the isolate, the context and
// every value made on it are unusable afterwards; the creator is spent.
func (s *SnapshotCreator) CreateBlob(ctx *Context, rec *Recording) ([]byte, error) {
	iso := s.iso
	if ctx == nil || ctx.iso != iso {
		return nil, errors.New("v8go: the snapshot context must be on the creator's isolate")
	}
	for _, t := range rec.templates {
		if C.SnapshotCreatorAddTemplate(iso.ptr, t.ptr) < 0 {
			return nil, errors.New("v8go: a recorded template could not be added")
		}
	}
	if C.SnapshotCreatorAddContext(iso.ptr, ctx.ptr) < 0 {
		return nil, errors.New("v8go: the context could not be added")
	}
	// Release everything that holds a handle: every context on the isolate
	// with the values tracked against it, and every recorded template.
	for _, c := range contextsOf(iso) {
		c.Close()
	}
	for _, t := range append(append([]*template(nil), rec.templates...), rec.derived...) {
		if t.ptr != nil {
			C.TemplateRelease(t.ptr)
			t.ptr = nil
		}
	}
	var size C.int
	data := C.SnapshotCreatorCreateBlob(iso.ptr, &size)
	iso.ptr = nil
	if data == nil {
		return nil, errors.New("v8go: V8 could not create the blob")
	}
	defer C.free(unsafe.Pointer(data))
	blob := C.GoBytes(unsafe.Pointer(data), size)
	// The template kinds ride in front of the blob: a four-byte count, then
	// one byte each.
	n := len(rec.kinds)
	header := []byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	for _, fn := range rec.kinds {
		if fn {
			header = append(header, 1)
		} else {
			header = append(header, 0)
		}
	}
	return append(header, blob...), nil
}

// NewIsolateFromSnapshot makes an isolate whose contexts can be deserialised
// from blob (NewContextFromSnapshot) and whose template construction can be
// replayed from it (BeginReplay).
func NewIsolateFromSnapshot(blob []byte) (*Isolate, error) {
	initializeIfNecessary()
	if len(blob) < 4 {
		return nil, errors.New("v8go: not a snapshot")
	}
	n := int(blob[0]) | int(blob[1])<<8 | int(blob[2])<<16 | int(blob[3])<<24
	if n < 0 || 4+n > len(blob) {
		return nil, errors.New("v8go: snapshot header is truncated")
	}
	kinds := blob[4 : 4+n]
	v8blob := blob[4+n:]
	cblob := C.CBytes(v8blob)
	iso := &Isolate{
		ptr: C.NewIsolateFromSnapshot((*C.char)(cblob), C.int(len(v8blob))),
		cbs: make(map[int]FunctionCallback),
	}
	iso.null = newValueNull(iso)
	iso.undefined = newValueUndefined(iso)
	iso.snapshotTemplates = make([]*template, 0, len(kinds))
	for k := range kinds {
		ptr := C.IsolateSnapshotTemplate(iso.ptr, C.int(k), C.int(kinds[k]))
		if ptr == nil {
			iso.Dispose()
			return nil, fmt.Errorf("v8go: snapshot template %d is missing", k)
		}
		iso.snapshotTemplates = append(iso.snapshotTemplates,
			&template{ptr: ptr, iso: iso, function: kinds[k] == 1, replayed: true})
	}
	return iso, nil
}

// NewContextFromSnapshot deserialises the snapshot's context 0, around the
// global proxy KeepGlobal parked under adoptFrom when there is one.
func NewContextFromSnapshot(iso *Isolate, adoptFrom int) (*Context, error) {
	if iso == nil || iso.snapshotTemplates == nil {
		return nil, errors.New("v8go: this isolate was not made from a snapshot")
	}
	ctxMutex.Lock()
	ctxSeq++
	ref := ctxSeq
	ctxMutex.Unlock()
	ptr := C.NewContextFromSnapshot(iso.ptr, 0, C.int(ref), C.int(adoptFrom))
	if ptr == nil {
		return nil, errors.New("v8go: the snapshot's context could not be deserialised")
	}
	ctx := &Context{ref: ref, ptr: ptr, iso: iso}
	ctx.register()
	return ctx, nil
}

// contextsOf is every open context on iso.
func contextsOf(iso *Isolate) []*Context {
	ctxMutex.RLock()
	defer ctxMutex.RUnlock()
	var out []*Context
	for _, r := range ctxRegistry {
		if r.ctx != nil && r.ctx.iso == iso && r.ctx.ptr != nil {
			out = append(out, r.ctx)
		}
	}
	return out
}

// adoptLogTable gives a context made while its isolate records or replays
// the log's callback table: the construction being logged is the context's
// own, and its natives are called before the embedder could set one.
func (c *Context) adoptLogTable() {
	if c.iso != nil && c.iso.log != nil {
		c.callbacks = c.iso.log.table
	}
}
