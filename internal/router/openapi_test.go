package router

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v2"

	"github.com/mstgnz/cronsole/v2/pkg/auth"
)

// specPath is the OpenAPI document, which lives beside the handler that embeds
// and serves it.
const specPath = "../handler/openapi.yaml"

// loadSpec parses the document and returns its paths.
//
// Into a generic map rather than a typed struct: the point is to parse the
// WHOLE document the way a client will, not to model the parts this test reads.
// A struct would silently ignore a mangled section it has no field for.
func loadSpec(t *testing.T) map[string]any {
	t.Helper()

	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading %s: %v", specPath, err)
	}

	// Strict, which is the whole point of parsing it here: it rejects duplicate
	// keys. The failure that motivated this is not exotic: an unquoted
	// description inside a flow mapping,
	//   { type: string, description: Code, name or URL }
	// splits on the comma and produces exactly that. The document still reads
	// fine to a human and the reference renders blank in the browser, which is a
	// long way from the change that caused it.
	var doc map[string]any
	if err := yaml.UnmarshalStrict(raw, &doc); err != nil {
		t.Fatalf("the OpenAPI document does not parse: %v", err)
	}

	paths, ok := doc["paths"].(map[any]any)
	if !ok || len(paths) == 0 {
		t.Fatal("the OpenAPI document declares no paths")
	}

	out := make(map[string]any, len(paths))
	for k, v := range paths {
		name, ok := k.(string)
		if !ok {
			t.Fatalf("a path key is not a string: %v", k)
		}
		out[name] = v
	}
	return out
}

// apiRoutes walks the real route table and returns every path under /api/v1,
// with chi's {param} placeholders intact.
//
// The handlers are zero values. router.New only takes method values off them to
// register; nothing is called, so nil receivers are safe here and the test needs
// no database, no services and no wiring.
func apiRoutes(t *testing.T) map[string]struct{} {
	t.Helper()

	mux, ok := New(Handlers{}, nil, auth.TrustedProxy{}, nil).(chi.Routes)
	if !ok {
		t.Fatal("the router is not walkable")
	}

	out := map[string]struct{}{}
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1") {
			return nil
		}
		// chi reports a trailing slash on a subrouter's index route; the spec
		// writes it without.
		route = strings.TrimSuffix(route, "/")
		out[method+" "+strings.TrimPrefix(route, "/api/v1")] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("walking routes: %v", err)
	}
	return out
}

func TestOpenAPIParses(t *testing.T) {
	loadSpec(t)
}

func TestOpenAPIDescribesEveryAPIRoute(t *testing.T) {
	// The reason this test exists: a generated spec rots silently, and so does a
	// hand written one. The previous revision of this service shipped a Swagger
	// file describing an API two versions old. Comparing the document against
	// the route table is what makes "hand written" safe, because a route added
	// without a line in the spec fails here rather than in somebody's client.
	spec := loadSpec(t)
	routes := apiRoutes(t)

	documented := map[string]struct{}{}
	for path, item := range spec {
		ops, ok := item.(map[any]any)
		if !ok {
			t.Fatalf("path %q is not a mapping", path)
		}
		for key := range ops {
			method, ok := key.(string)
			if !ok {
				continue
			}
			switch strings.ToLower(method) {
			case "get", "post", "put", "patch", "delete", "head", "options":
				documented[strings.ToUpper(method)+" "+path] = struct{}{}
			}
		}
	}

	var missing, extra []string
	for route := range routes {
		if _, ok := documented[route]; !ok {
			missing = append(missing, route)
		}
	}
	for route := range documented {
		if _, ok := routes[route]; !ok {
			extra = append(extra, route)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("%d API route(s) the document does not describe:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("%d documented path(s) that no route serves:\n  %s",
			len(extra), strings.Join(extra, "\n  "))
	}
}
