// Package web holds the templates and static files of the pgCellar interface.
// They are embedded in the binary, so nothing is read from disk when serving
// and nothing is fetched from another site: HTMX is vendored.
package web

import "embed"

// FS contains templates/*.html and static/*.
//
//go:embed templates/*.html static/*
var FS embed.FS
