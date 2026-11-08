package zrpc

import (
	_ "embed"
	"net/http"
)

// The explorer is a single self-contained HTML document served by the node. It
// uses no CDN, no external font and no build step, so it works on an air-gapped
// devnet and in the sandboxed preview environment alike. It talks to the same
// origin's JSON-RPC endpoint and never holds keys or write access.
//
//go:embed explorer.html
var explorerHTML []byte

//go:embed explorer.sql
var explorerSchema []byte

// Explorer serves the bundled explorer UI and its data contract.
func (s *Service) Explorer() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// The UI is a static document with an inline script; it must not be framed
		// by a third party, and it must not load anything from elsewhere.
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'")
		_, _ = w.Write(explorerHTML)
	})
	mux.HandleFunc("/explorer.sql", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(explorerSchema)
	})
	return mux
}
