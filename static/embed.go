// Package static embeds the built-in frontend test UI assets as a fallback
// when no external static directory is present on disk.
package static

import "embed"

//go:embed index.html styles.css app.js logo.svg
var FS embed.FS
