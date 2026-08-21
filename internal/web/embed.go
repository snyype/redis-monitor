// Package web carries the single-page UI, compiled into the binary so the monitor
// is one file to deploy with no asset pipeline and no CDN.
package web

import _ "embed"

// Index is the whole UI: one self-contained HTML document with inline CSS and
// script, no external requests. Everything it draws comes from /api/redis-monitor.
//
//go:embed index.html
var Index []byte
