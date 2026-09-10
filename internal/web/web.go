// Package web serves the status UI: what is queued, what is running, what was
// left alone and why, and what has been done.
//
// It is read-mostly on purpose. Two things here write anything at all -- "run
// a pass now", and clearing the backoff on a file that failed -- and neither
// touches media directly; they ask the daemon to do the same pass the timer
// would have done anyway. Everything else is a SELECT.
//
// The screen that earns this package is "left alone, and why". This tool
// declines far more files than it touches, by design, and the only way to
// know that the declining is right is to be able to read the reasons. So
// every row carries the reason string decide produced, verbatim -- not a
// status code this package translates back into English, which would be a
// second place for the meaning to drift.
//
// No SPA, no node, no build step. html/template, one vendored copy of HTMX
// for polling, and one stylesheet, all compiled into the binary with embed.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"time"

	"github.com/mjnitz02/media-compressor/internal/config"
	"github.com/mjnitz02/media-compressor/internal/daemon"
	"github.com/mjnitz02/media-compressor/internal/store"
)

//go:embed templates static
var assets embed.FS

// Engine is the running loop, as much of it as this package needs. It is an
// interface so that `serve` can hand over nothing at all: a UI pointed at the
// database of a daemon running elsewhere is a perfectly good read-only view,
// and it simply does not offer the button.
type Engine interface {
	Activity() daemon.Activity
	Trigger() bool
}

// Options are what the server needs to do its job.
type Options struct {
	Store  *store.Store
	Config *config.Config

	// Engine may be nil, in which case the pages report that nothing is
	// running here rather than pretending it is idle.
	Engine Engine

	// Version is stamped into asset URLs so a new binary's stylesheet is not
	// served from a browser cache belonging to the old one.
	Version string

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Server is the whole UI. It is an http.Handler; the caller owns the listener,
// which is what lets main set sensible timeouts on it.
type Server struct {
	opts  Options
	mux   *http.ServeMux
	pages map[string]*template.Template
}

// pageNames are the templates under templates/pages. Each is parsed together
// with the layout into its own template set, because they all define "main"
// and one set could not hold two of those.
var pageNames = []string{"overview", "queue", "skipped", "notes", "blocked", "history"}

// New builds the server, parsing every template up front so that a broken one
// is a startup failure rather than a 500 six months from now.
func New(o Options) (*Server, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("web: a store is required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}

	s := &Server{opts: o, mux: http.NewServeMux(), pages: map[string]*template.Template{}}
	for _, name := range pageNames {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets,
			"templates/layout.html", "templates/partials/*.html", "templates/pages/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("web: parsing %s: %w", name, err)
		}
		s.pages[name] = t
	}

	fragments, err := template.New("fragments").Funcs(funcs).ParseFS(assets, "templates/partials/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parsing partials: %w", err)
	}
	s.pages["_fragments"] = fragments

	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}

	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	s.mux.HandleFunc("GET /{$}", s.handle(s.overview))
	s.mux.HandleFunc("GET /queue", s.handle(s.queue))
	s.mux.HandleFunc("GET /skipped", s.handle(s.skipped))
	s.mux.HandleFunc("GET /notes", s.handle(s.notes))
	s.mux.HandleFunc("GET /blocked", s.handle(s.blocked))
	s.mux.HandleFunc("GET /history", s.handle(s.history))
	s.mux.HandleFunc("GET /partials/activity", s.handle(s.activityFragment))
	s.mux.HandleFunc("POST /pass", s.handle(s.startPass))
	s.mux.HandleFunc("POST /retry", s.handle(s.retry))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// handle wraps a handler that can fail, and guards the two that write.
//
// The guard is the reason this is a wrapper rather than four lines repeated.
// There is no login here -- this is a LAN service, like the stack it replaces
// -- but a page on some other site should still not be able to make the
// operator's browser start an encode. A browser sends Origin on every POST, so
// requiring it to match is enough to stop that, costs nothing, and needs no
// cookie or token machinery.
func (s *Server) handle(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "this request did not come from this page", http.StatusForbidden)
			return
		}
		if err := fn(w, r); err != nil {
			// The message is the operator's, not a user's: there is one
			// person here and hiding the cause from them helps nobody.
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Not a browser, or a browser too old to send it. curl and the
		// container's own health checks land here.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// render writes a page. It renders into a buffer first so that a template
// which fails half way through produces an error rather than half a page and
// a 200.
func (s *Server) render(w http.ResponseWriter, name string, data any) error {
	t, ok := s.pages[name]
	if !ok {
		return fmt.Errorf("web: no page %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		return fmt.Errorf("web: rendering %s: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

// renderFragment writes one partial, which is what HTMX asks for when it
// polls.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) error {
	var buf bytes.Buffer
	if err := s.pages["_fragments"].ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Errorf("web: rendering %s: %w", name, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

func (s *Server) now() time.Time { return s.opts.Now() }
