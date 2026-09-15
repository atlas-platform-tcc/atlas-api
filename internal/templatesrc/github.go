// Package templatesrc fetches the service template from the atlas-templates repo
// at runtime, via the GitHub tarball API (no git binary required).
package templatesrc

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/atlas-platform-tcc/atlas-api/internal/generator"
)

// GitHub fetches a subdirectory of a GitHub repository as a set of files.
type GitHub struct {
	Owner  string // e.g. "atlas-platform-tcc"
	Repo   string // e.g. "atlas-templates"
	Ref    string // e.g. "main"
	SubDir string // e.g. "go-service-template"
	Token  string // optional; raises rate limits and allows private repos
	Client *http.Client
}

func (g *GitHub) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return http.DefaultClient
}

// Fetch downloads the repository tarball and returns the files under SubDir,
// keyed by their path relative to SubDir.
func (g *GitHub) Fetch(ctx context.Context) (generator.Files, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/tarball/%s", g.Owner, g.Repo, g.Ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}

	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch tarball: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch tarball: unexpected status %s", resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	files := generator.Files{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel, ok := relativeTo(hdr.Name, g.SubDir)
		if !ok {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", hdr.Name, err)
		}
		files[rel] = content
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files found under %q in %s/%s@%s", g.SubDir, g.Owner, g.Repo, g.Ref)
	}
	return files, nil
}

// relativeTo strips the tarball's top-level directory and the subDir prefix,
// returning the path relative to subDir. Tarball entries look like
// "owner-repo-<sha>/go-service-template/deploy/values.yaml".
func relativeTo(name, subDir string) (string, bool) {
	parts := strings.SplitN(name, "/", 3)
	if len(parts) < 3 {
		return "", false
	}
	if parts[1] != subDir {
		return "", false
	}
	return path.Clean(parts[2]), true
}
