package httpapi

import (
	"bufio"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// openAPIPath is the API contract, relative to this package.
const openAPIPath = "../../../api/openapi.yaml"

var (
	// A path item under paths: is indented by two spaces, and its operations by four.
	openAPIPathItem  = regexp.MustCompile(`^  (/[^:\s]*):\s*$`)
	openAPIOperation = regexp.MustCompile(`^    (get|put|post|delete|options|head|patch|trace):\s*$`)
)

// documentedOperations reads the operations of the contract as "METHOD /path" patterns, the way the router
// registers them. It relies on the file's layout: the paths section starts at a top-level "paths:" and ends at
// the next top-level key.
func documentedOperations(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var (
		ops     []string
		inPaths bool
		path    string
	)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" && line[0] != ' ' && line[0] != '#' {
			inPaths = line == "paths:"
			continue
		}
		if !inPaths {
			continue
		}
		if m := openAPIPathItem.FindStringSubmatch(line); m != nil {
			path = m[1]
		} else if m := openAPIOperation.FindStringSubmatch(line); m != nil {
			ops = append(ops, strings.ToUpper(m[1])+" "+path)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return ops
}

// TestOpenAPIDocumentsEveryRoute keeps the contract and the router in step: every route the router serves is
// documented, with the same path parameter names, and nothing else is.
func TestOpenAPIDocumentsEveryRoute(t *testing.T) {
	t.Parallel()

	_, routes := newMux(testRouterDeps())
	documented := documentedOperations(t)
	if len(documented) < 10 {
		t.Fatalf("found only %d operations in %s; did its layout change?", len(documented), openAPIPath)
	}

	for _, route := range routes {
		if !slices.Contains(documented, route) {
			t.Errorf("route %q is not documented in %s", route, openAPIPath)
		}
	}
	for _, op := range documented {
		if !slices.Contains(routes, op) {
			t.Errorf("%s documents %q, which the router does not serve", openAPIPath, op)
		}
	}
	if dup := len(documented) - len(slices.Compact(slices.Sorted(slices.Values(documented)))); dup != 0 {
		t.Errorf("%s documents %d operations twice", openAPIPath, dup)
	}
}
