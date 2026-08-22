package v8go

import "sync/atomic"

// XPCount is temporary instrumentation; delete with function_template.go's XPCount.Add.
var XPCount atomic.Int64
