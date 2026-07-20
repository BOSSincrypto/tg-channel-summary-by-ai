// Package webapp contains the static Telegram Mini App assets.
package webapp

import (
	"embed"
	"io/fs"
)

// Files contains the SPA assets embedded into the bot binary.
// The HTTP server exposes them below /webapp/.
//
//go:embed index.html app.js style.css offline.html sw.js
var Files embed.FS

// StaticFS returns the embedded asset filesystem without exposing the
// package-level embed implementation to the HTTP server.
func StaticFS() (fs.FS, error) {
	return fs.Sub(Files, ".")
}
