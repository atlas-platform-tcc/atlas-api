package templatesrc

import (
	"context"
	"os"
	"testing"
)

// Live test against the real atlas-templates repo. Skipped unless ATLAS_LIVE=1.
func TestFetch_Live(t *testing.T) {
	if os.Getenv("ATLAS_LIVE") != "1" {
		t.Skip("set ATLAS_LIVE=1 to run the live GitHub fetch test")
	}
	g := &GitHub{
		Owner:  "atlas-platform-tcc",
		Repo:   "atlas-templates",
		Ref:    "main",
		SubDir: "go-service-template",
		Token:  os.Getenv("GITHUB_TOKEN"), // optional
	}
	files, err := g.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, want := range []string{"main.go", "go.mod", "Dockerfile", "deploy/values.yaml"} {
		if _, ok := files[want]; !ok {
			t.Errorf("expected %s in fetched template; got keys: %v", want, keys(files))
		}
	}
	// ci.yml is only present once the updated atlas-templates has been pushed.
	if _, ok := files[".github/workflows/ci.yml"]; !ok {
		t.Logf("note: .github/workflows/ci.yml not in remote yet — push atlas-templates to include it")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
