// This file embeds the single-page Chorus Conductor browser dashboard and
// mounts it at GET /.

package chorus

import (
	_ "embed"
	"net/http"

	goahttp "goa.design/goa/v3/http"
)

const (
	// uiContentSecurityPolicy restricts the embedded dashboard to same-origin
	// connections and inline styles/scripts, preventing external asset loads or
	// data exfiltration.
	uiContentSecurityPolicy = "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:"
)

// uiIndexHTML holds the embedded single-page Chorus Conductor dashboard served
// at GET /.
//
//go:embed ui/index.html
var uiIndexHTML []byte

// mountUI registers the embedded Chorus Conductor browser dashboard at GET / on
// mux.
func mountUI(mux goahttp.Muxer) {
	mux.Handle(http.MethodGet, "/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", uiContentSecurityPolicy)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(uiIndexHTML)
	})
}
