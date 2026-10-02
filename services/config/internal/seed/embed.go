package seed

import (
	"embed"
	"io/fs"
)

// embedded holds the seed tree that ships inside the binary. The image is distroless,
// so there is no seed folder on disk to read at run time; //go:embed is what makes
// `config seed` self-contained.
//
//go:embed seed
var embedded embed.FS

// FS is the embedded seed folder, rooted so that each entry is one namespace
// directory. Load takes an fs.FS rather than reading this directly, so tests can hand
// it an fstest.MapFS with the same shape.
var FS = mustSub(embedded, "seed")

// mustSub re-roots fsys at dir. The embedded tree is fixed at build time, so the only
// possible error is a programming mistake, and failing at package init makes it loud.
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("seed: " + err.Error())
	}
	return sub
}
