package frontend

import "embed"

// Assets contains the compiled, shared browser and desktop interface.
//
//go:embed all:dist
var Assets embed.FS
