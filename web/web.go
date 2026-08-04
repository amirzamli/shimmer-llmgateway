// Package web embeds the single-page HTML UI (§6.1): one index.html with
// vanilla JS/CSS, no build step. The gateway serves it at GET /.
package web

import "embed"

//go:embed index.html
var FS embed.FS
