package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

// page is the screen, compiled into the binary.
//
// It is embedded rather than read from disk because it is not packaging: the
// screen is the participant's information set, and a condition the subject can
// edit is not a condition. That is narrower than it sounds — a modified binary
// defeats it, as ADR-015 says of everything else — but it removes a documented,
// unattested knob, and it keeps the screen inside the artefact a digest covers.
//
// There is deliberately no flag that serves it from a directory. If one is ever
// wanted, section 11 decides the rule in advance: it must refuse pilot and
// confirmatory pacing, so a development build cannot produce a journal the
// confirmatory sample could contain.
//
//go:embed web/index.html
var page embed.FS

// screen serves the page at the root, and nothing else: one file, no directory
// listing, and no path a request can walk.
func (s *Server) screen() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		body, err := fs.ReadFile(page, "web/index.html")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, refusal{ReasonRefused, err.Error()})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The same rule the state has: nothing about a session is cached, and
		// the screen is where a stale one would be least visible.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}
