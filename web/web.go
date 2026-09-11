// Package web embeds the single-page HTML UIs (§6.1): the primary index.html
// and the optional alternate workspace at alt-page1.html. Both are plain
// vanilla JS/CSS with no build step.
package web

import "embed"

//go:embed index.html alt-page1.html
var FS embed.FS
