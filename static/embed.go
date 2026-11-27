// Package static embeds the built-in homepage and standalone test console assets
// as a fallback when no external static directory is present on disk.
package static

import "embed"

//go:embed index.html console.html home.css home.js styles.css app.js logo.svg zlibrary-book-source.json
var FS embed.FS
