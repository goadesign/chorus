// This file embeds the Chorus dashboard (chorus/ui) and serves it: the page
// skeleton at GET / and its scripts and styles at GET /ui/<file>. The page
// carries the identity of the dashboard files it was built from, and the
// dashboard data responses carry the identity of the files this server
// embeds, so an open page can tell when the server now serves a different
// dashboard and offer a reload. Every file
// is served from the same origin under a Content-Security-Policy that allows
// no inline scripts or styles and no external hosts, so text that agents put
// in messages can never run as code and the page cannot load or send data
// anywhere else.

package chorus

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
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

	// uiBuildHeader is the response header on the dashboard data routes that
	// names the dashboard files this server embeds (see uiBuild).
	uiBuildHeader = "X-Chorus-UI-Build"
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

	// uiBuild identifies the embedded dashboard files: the hex SHA-256 of
	// every file's path and contents (see uiDigest). It changes only when a
	// dashboard file changes, never for a server-only change.
	uiBuild = mustDigest(uiRoot)
)

// mountUI registers the dashboard on mux: GET / serves index.html and
// GET /ui/{file} serves the other embedded files. Unknown paths return 404.
func mountUI(mux goahttp.Muxer) {
	mux.Handle(http.MethodGet, "/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveUIPage(w)
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
	info, err := fs.Stat(uiRoot, name)
	if err != nil || info.IsDir() || name == "brands/SOURCES.md" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", uiContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	http.ServeFileFS(w, r, uiRoot, name)
}

// serveUIPage writes index.html with a <meta name="chorus-ui-build"> element
// naming uiBuild added before </head>, so the loaded page records which
// dashboard files it came from even if the server restarts on new files
// before the page first asks for data. It uses the same security and
// no-store headers as every other dashboard file.
func serveUIPage(w http.ResponseWriter) {
	page, err := fs.ReadFile(uiRoot, "index.html")
	if err != nil {
		http.Error(w, "dashboard page missing from this build", http.StatusInternalServerError)
		return
	}
	meta := []byte(`<meta name="chorus-ui-build" content="` + uiBuild + `">` + "\n</head>")
	page = bytes.Replace(page, []byte("</head>"), meta, 1)
	h := w.Header()
	h.Set("Content-Security-Policy", uiContentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// uiBuildStamp is the dashboard data routes' middleware that sets
// uiBuildHeader on every response, including 304 Not Modified.
func uiBuildStamp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(uiBuildHeader, uiBuild)
		next.ServeHTTP(w, r)
	})
}

// uiDigest returns the hex SHA-256 identity of the regular files in fsys.
// Files are visited in path order, and each contributes its path length,
// path, content length, and contents, so no two different sets of files
// produce the same input (for example "a" holding "bc" and "ab" holding
// "c").
func uiDigest(fsys fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(path)))
		h.Write(size[:])
		h.Write([]byte(path))
		binary.BigEndian.PutUint64(size[:], uint64(len(data)))
		h.Write(size[:])
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// mustDigest returns uiDigest(fsys). It panics when an embedded file cannot
// be read, which can only happen if the embed directive above is broken.
func mustDigest(fsys fs.FS) string {
	digest, err := uiDigest(fsys)
	if err != nil {
		panic(err)
	}
	return digest
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
