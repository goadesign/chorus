// This file embeds the Chorus dashboard (chorus/ui) and serves it: the page
// skeleton at GET / and its scripts and styles at GET /ui/<file>. Every file
// is served from the same origin under a Content-Security-Policy that allows
// no inline scripts or styles and no external hosts, so text that agents put
// in messages can never run as code and the page cannot load or send data
// anywhere else.

package chorus

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	goahttp "goa.design/goa/v3/http"
)

const (
	// uiContentSecurityPolicy restricts the dashboard to same-origin scripts,
	// styles, and connections, allows inline SVG/data images only, forbids
	// plugins and <base> rewriting, and stops other sites from framing the
	// page (clickjacking).
	uiContentSecurityPolicy = "default-src 'self'; img-src 'self' data:; object-src 'none'; " +
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
)

var (
	// uiFiles holds the dashboard files: index.html plus the scripts and
	// styles it references under /ui/.
	//
	//go:embed ui
	uiFiles embed.FS

	// uiRoot is uiFiles rooted at the ui directory, so request paths map
	// directly to file names.
	uiRoot = mustSub(uiFiles, "ui")
)

// mountUI registers the dashboard on mux: GET / serves index.html and
// GET /ui/{file} serves the other embedded files. Unknown paths return 404.
func mountUI(mux goahttp.Muxer) {
	mux.Handle(http.MethodGet, "/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveUIFile(w, r, "index.html")
	})
	mux.Handle(http.MethodGet, "/ui/{*file}", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/ui/")
		if name == "" || name == "index.html" || !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		serveUIFile(w, r, name)
	})
}

// serveUIFile writes the embedded file name with the dashboard's security
// headers, or 404 when it does not exist. Responses are not cached so a
// rebuilt server is picked up on the next page load.
func serveUIFile(w http.ResponseWriter, r *http.Request, name string) {
	if _, err := fs.Stat(uiRoot, name); err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", uiContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	http.ServeFileFS(w, r, uiRoot, name)
}

// mustSub returns the sub-filesystem of fsys rooted at dir. It panics when dir
// is not a valid path, which can only happen if the embed directive above is
// changed incorrectly.
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
