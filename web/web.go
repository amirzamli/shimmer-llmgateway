// Package web embeds the single-page HTML UI (§6.1). It is plain vanilla
// JS/CSS with no build step.
package web

import "embed"

//go:embed index.html
var FS embed.FS
