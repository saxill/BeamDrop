package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
)

func init() {
	// Go's table has no entry for .webmanifest, so it went out as
	// text/plain — and a browser ignores a manifest served as text/plain.
	// "Add to Home Screen" then silently falls back to a screenshot for the
	// icon and opens in browser chrome instead of standalone, with nothing
	// anywhere saying why.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// embeddedStatic carries the iPhone-facing page and its scripts inside the
// executable. beamdrop is meant to be one binary you scp to a laptop and
// run; resolving the page off disk instead would make it work only when
// launched from a checkout, and the failure mode is a silent blank page on
// the phone rather than an error on the laptop.
//
// The whole directory is embedded rather than a list of filenames on
// purpose: a pattern that enumerates files drifts the moment someone adds
// one, and drift here is invisible until a phone loads the page.
//
//go:embed static
var embeddedStatic embed.FS

// staticHandler serves dir from disk when it is set — the smoke test points
// it at the checkout, and it lets you edit the page without rebuilding —
// and otherwise serves the embedded copy.
func staticHandler(dir string) http.Handler {
	if dir != "" {
		return http.FileServer(http.Dir(dir))
	}
	sub, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		// Unreachable: the embed directive above guarantees static/ exists,
		// so this can only fire if the constant path is edited wrongly.
		panic("webui: embedded static assets missing: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
