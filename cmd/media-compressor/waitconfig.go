package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/mjnitz02/media-compressor/internal/config"
)

// How often the daemon looks again for a configuration it can use. Short
// enough that fixing the file feels immediate, long enough to be free.
const configPollInterval = 3 * time.Second

// waitForConfig returns the configuration, waiting for one to appear if it has
// to, and serving a page on addr that says what is missing while it waits.
//
// The daemon is a service that is meant to be left alone, and Docker restarts
// a service that exits with an exponential backoff that reaches a minute. So
// exiting here means that the moment the operator fixes the problem is the
// moment the feedback is slowest: they fix the file, watch the container log a
// failure that was recorded before their change, and reasonably conclude the
// fix did not work. That happened on this project's first deployment.
//
// Waiting instead costs nothing -- nothing is scanned, nothing is encoded, no
// database is opened -- and the page is reachable the whole time, so the
// operator can see the real reason and watch it clear on its own.
//
// One-shot commands keep failing loudly: `run`, `scan` and `plan` are typed by
// hand and their exit status is read.
func waitForConfig(ctx context.Context, path, addr string, out io.Writer) (*config.Config, error) {
	cfg, err := loadUsableConfig(path)
	if err == nil {
		return cfg, nil
	}

	fmt.Fprintf(out, "media-compressor: waiting for a usable configuration at %s\n", path)
	fmt.Fprintf(out, "  %v\n", err)
	fmt.Fprintf(out, "  Nothing is being scanned or encoded. This will start on its own\n")
	fmt.Fprintf(out, "  within %s of the file being fixed; there is no need to restart.\n", configPollInterval)

	w := &waitingServer{path: path, err: err}
	if addr != "" {
		if lerr := w.start(addr); lerr != nil {
			fmt.Fprintf(out, "  (the holding page could not start: %v)\n", lerr)
		} else {
			fmt.Fprintf(out, "  why, as a page: http://%s\n", addr)
			defer w.stop()
		}
	}

	ticker := time.NewTicker(configPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			cfg, err := loadUsableConfig(path)
			if err == nil {
				fmt.Fprintf(out, "media-compressor: configuration read from %s; starting\n", path)
				return cfg, nil
			}
			w.setErr(err)
		}
	}
}

// loadUsableConfig is every reason the daemon could not start from this file,
// as one error.
//
// The mount check is in here rather than left to the caller on purpose. Its
// original reason for refusing to start -- that a library whose mount is absent
// would have every file in it swept out of the database as deleted -- is
// satisfied better by waiting than by exiting, because waiting does not open
// the database at all. On a NAS the usual cause is a share that has not come up
// yet, which fixes itself.
func loadUsableConfig(path string) (*config.Config, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("there is no configuration at %s yet", path)
		}
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.CheckPaths(); err != nil {
		return nil, err
	}
	if len(cfg.Libraries) == 0 {
		return nil, errors.New("no libraries are configured, so there is nothing to watch")
	}
	return cfg, nil
}

// waitingServer is the holding page. It is deliberately not internal/web:
// there is no config and no database to build that from, and a page that
// exists to report their absence must not need them.
type waitingServer struct {
	path string

	mu  sync.Mutex
	err error

	srv *http.Server
	ln  net.Listener
}

func (w *waitingServer) setErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = err
}

func (w *waitingServer) reason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		return ""
	}
	return w.err.Error()
}

func (w *waitingServer) start(addr string) error {
	mux := http.NewServeMux()

	// 503 rather than "ok": the container is up but it is not doing its job,
	// and a health check that passes here would report a healthy service that
	// is encoding nothing. Docker marks it unhealthy and leaves it running,
	// which is exactly right -- restarting would not help.
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(rw, "waiting for a usable configuration at %s\n", w.path)
	})

	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.WriteHeader(http.StatusServiceUnavailable)
		_ = waitingPage.Execute(rw, struct{ Path, Reason, Every string }{
			Path: w.path, Reason: w.reason(), Every: configPollInterval.String(),
		})
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "holding page stopped: %v\n", err)
		}
	}()
	w.srv, w.ln = srv, ln
	return nil
}

// stop closes the page down and releases the port.
//
// The explicit ln.Close is not belt and braces. http.Server.Shutdown only
// closes the listeners Serve has registered with it, and Serve does that from
// inside the goroutine above -- so a stop that arrives before the goroutine has
// been scheduled returns with the port still held, and the real web UI, which
// binds that same port a moment later, fails to start. Closing the listener we
// kept is the only way to be sure. Closing it twice is harmless.
func (w *waitingServer) stop() {
	if w.srv == nil {
		return
	}
	_ = shutdownWeb(w.srv)
	_ = w.ln.Close()
}

// Self-contained on purpose -- no stylesheet, no htmx, nothing served from a
// second request that could itself fail. The refresh is a meta tag for the same
// reason: when the config lands this process replaces the page with the real
// UI, and the browser finds it without being touched.
var waitingPage = template.Must(template.New("waiting").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="5">
<title>Waiting for configuration — media-compressor</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 16px/1.6 system-ui, sans-serif; margin: 0; padding: 2rem 1rem; }
  main { max-width: 46rem; margin: 0 auto; }
  h1 { font-size: 1.4rem; margin: 0 0 1rem; }
  code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .9em; }
  pre { padding: .75rem 1rem; border-radius: 6px; overflow-x: auto;
        background: rgba(127,127,127,.14); }
  .muted { opacity: .75; }
</style>
</head>
<body>
<main>
  <h1>Waiting for a configuration</h1>
  <p>Nothing is being scanned and nothing is being encoded. No media has been
     read or written.</p>
  <pre>{{.Reason}}</pre>
  <p>Put a configuration at <code>{{.Path}}</code>. A documented example is
     beside it as <code>config.example.yaml</code>.</p>
  <p class="muted">This page re-checks every {{.Every}} and starts on its own
     once the file is usable. There is no need to restart the container.</p>
</main>
</body>
</html>
`))
