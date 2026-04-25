// Package addons exposes the embedded clusterbox addon directory tree.
//
// The actual addon definitions live in this directory as sibling subfolders
// (e.g. ./gha-runner-scale-set/). They are pulled into the binary at compile
// time via //go:embed. The catalog logic that parses these bytes lives in
// internal/addon — this package is intentionally a thin embed shim so the
// embed pattern is rooted at the addons tree.
package addons

import "embed"

// FS holds every file under each addon subdirectory, captured at build time.
//
// The "all:" prefix tells the embed compiler to include dotfiles (e.g.
// .gitkeep), which lets a stubbed-out manifests/ directory still ship.
//
//go:embed all:*
var FS embed.FS

// Root is the path inside FS that contains the addon subdirectories. Because
// the embed pattern is rooted at this directory, addon dirs live at the FS
// root itself.
const Root = "."
