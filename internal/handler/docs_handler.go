package handler

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"time"
)

// openAPISpec is the machine readable description of /api/v1.
//
// Embedded rather than read from disk, for the same reason the templates are:
// the container carries only the binary, and a docs page that 404s in
// production because a file was not copied is worse than no docs page.
//
// Hand written. The previous revision of this service shipped a generated
// Swagger file that described an API two versions old, which is the failure
// mode of generated specs: nothing fails when they drift.
//
//go:embed openapi.yaml
var openAPISpec []byte

// specETag identifies this build's spec, so an unchanged one answers 304.
//
// Derived from the content rather than from a version string: two builds with
// the same spec should not force a re-download, and a spec edited without a
// version bump must not be served as unchanged.
var specETag = func() string {
	sum := sha256.Sum256(openAPISpec)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}()

// specModTime is the process start.
//
// The embedded file has no modification time of its own, and ServeContent needs
// one for If-Modified-Since. Process start is the honest answer: the content
// cannot change while the process runs.
var specModTime = time.Now()

// DocsHandler serves the API reference.
type DocsHandler struct{}

// NewDocsHandler wires the handler.
func NewDocsHandler() *DocsHandler { return &DocsHandler{} }

// Spec serves the OpenAPI document.
//
// A real file at a stable path, so Postman, Insomnia, a client generator or
// somebody's CI can consume it without scraping the page that renders it.
func (h *DocsHandler) Spec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	// no-cache means "revalidate", not "do not store". A fixed max-age was the
	// first attempt and it was wrong: the spec changes when the binary does, so
	// after a deploy the browser serves the previous one and the reference shows
	// an API that is no longer there. The document is small and the ETag makes
	// an unchanged one a 304.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", specETag)
	http.ServeContent(w, r, "openapi.yaml", specModTime, bytes.NewReader(openAPISpec))
}

// Page renders the reference.
//
// Scalar rather than Swagger UI: it reads the same OpenAPI document, and the
// try-it panel sends real requests with the credential the reader pastes in,
// which is the part that makes a reference worth opening.
//
// It renders outside the application layout on purpose. Scalar takes the whole
// viewport and brings its own styling; wrapping it in the console's chrome
// produces two scrollbars and two visual languages arguing with each other.
func (h *DocsHandler) Page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsPage))
}

// docsPage is a literal rather than a template: it has nothing to interpolate,
// and putting it in the template set would drag the layout in with it.
//
// The CDN host matches the Content-Security-Policy in internal/middleware. A
// script from anywhere else is blocked, so adding a different renderer means
// changing that policy deliberately rather than by accident.
const docsPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex, nofollow">
  <title>API &middot; Cronsole</title>
  <style>
    body { margin: 0; }
    /* A way back. Scalar fills the viewport and has no notion of the console
     * around it, so without this the page is a dead end. */
    .back {
      position: fixed; top: .75rem; right: .75rem; z-index: 10;
      font: 500 13px/1 ui-sans-serif, system-ui, sans-serif;
      padding: .5rem .75rem; border-radius: 8px; text-decoration: none;
      background: #3d5afe; color: #fff;
      box-shadow: 0 2px 6px rgba(16, 24, 40, .2);
    }
  </style>
</head>
<body>
  <a class="back" href="/">&larr; Console</a>
  <script id="api-reference" data-url="/openapi.yaml"></script>
  <script>
    var configuration = {
      theme: 'default',
      layout: 'modern',
      hideDownloadButton: false,
      /* The console is served over the reader's own domain, so the try-it panel
       * should default to it rather than to an example host. */
      servers: [{ url: window.location.origin + '/api/v1', description: 'This deployment' }],
    };
    document.getElementById('api-reference').dataset.configuration = JSON.stringify(configuration);
  </script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.25.0"></script>
</body>
</html>`
