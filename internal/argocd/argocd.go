// Package argocd is a minimal read-only client for the Argo CD API: it augments the
// catalog with the observed state (sync + health) without ever writing back, so Git
// stays the source of truth (ADR atlas-api/0012).
package argocd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Status is the observed state of a service's Argo CD Application.
type Status struct {
	Sync   string // e.g. "Synced" / "OutOfSync"
	Health string // e.g. "Healthy" / "Progressing" / "Degraded" / "Missing"
}

// Client talks to the Argo CD API server (read-only).
type Client struct {
	addr  string
	token string
	http  *http.Client
}

// New returns a Client for the Argo CD API at addr, authenticating with a bearer
// token. insecure skips TLS verification, required by the self-signed certificate of
// a default local Argo CD server.
func New(addr, token string, insecure bool) *Client {
	tr := &http.Transport{}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		addr:  addr,
		token: token,
		http:  &http.Client{Timeout: 5 * time.Second, Transport: tr},
	}
}

// appPayload is the subset of the Argo CD Application JSON the catalog reads.
type appPayload struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Sync   struct{ Status string } `json:"sync"`
		Health struct{ Status string } `json:"health"`
	} `json:"status"`
}

func (a appPayload) status() Status {
	return Status{Sync: a.Status.Sync.Status, Health: a.Status.Health.Status}
}

// AppStatus returns the observed state of a single Application.
func (c *Client) AppStatus(ctx context.Context, name string) (Status, error) {
	var app appPayload
	if err := c.get(ctx, "/api/v1/applications/"+name, &app); err != nil {
		return Status{}, err
	}
	return app.status(), nil
}

// AppStatuses returns the observed state of every Application, keyed by name, so the
// catalog listing costs one call instead of one request per service.
func (c *Client) AppStatuses(ctx context.Context) (map[string]Status, error) {
	var list struct {
		Items []appPayload `json:"items"`
	}
	if err := c.get(ctx, "/api/v1/applications", &list); err != nil {
		return nil, err
	}
	out := make(map[string]Status, len(list.Items))
	for _, a := range list.Items {
		out[a.Metadata.Name] = a.status()
	}
	return out, nil
}

func (c *Client) get(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.addr+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("argocd %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
