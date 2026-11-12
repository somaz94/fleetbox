// Package web holds the console, a Vite build of the React app in this
// directory. Run `make web` to fill dist; without it the server still builds
// and serves a page saying the console is missing.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built console, rooted at dist.
func Dist() fs.FS {
	sub, _ := fs.Sub(dist, "dist") // "dist" is a valid path, so Sub cannot fail
	return sub
}
