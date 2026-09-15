package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
	"github.com/atlas-platform-tcc/atlas-api/internal/provisioner"
	"github.com/atlas-platform-tcc/atlas-api/internal/store"
)

// fakeProvisioner records the call and returns a canned Result or an error.
type fakeProvisioner struct {
	res provisioner.Result
	err error
}

func (f fakeProvisioner) Provision(_ context.Context, _ domain.Service) (provisioner.Result, error) {
	return f.res, f.err
}

func newTestServer() http.Handler {
	return NewServer(store.NewMemory()).Handler()
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateService_Created(t *testing.T) {
	h := newTestServer()
	rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go","replicas":2}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var svc domain.Service
	if err := json.Unmarshal(rec.Body.Bytes(), &svc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if svc.Name != "payments" || svc.Replicas != 2 || svc.Status != domain.StatusRegistered {
		t.Errorf("unexpected service: %+v", svc)
	}
}

func TestCreateService_ValidationFailed(t *testing.T) {
	h := newTestServer()
	rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"python"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "validation_failed" || len(body.Fields) == 0 {
		t.Errorf("unexpected error body: %+v", body)
	}
}

func TestCreateService_InvalidJSON(t *testing.T) {
	h := newTestServer()
	rec := do(t, h, http.MethodPost, "/services", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCreateService_Conflict(t *testing.T) {
	h := newTestServer()
	payload := `{"name":"payments","language":"go"}`
	_ = do(t, h, http.MethodPost, "/services", payload)
	rec := do(t, h, http.MethodPost, "/services", payload)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestGetService(t *testing.T) {
	h := newTestServer()
	_ = do(t, h, http.MethodPost, "/services", `{"name":"orders","language":"go"}`)

	rec := do(t, h, http.MethodGet, "/services/orders", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	rec = do(t, h, http.MethodGet, "/services/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCreateService_Provisioned(t *testing.T) {
	st := store.NewMemory()
	h := NewServer(st).WithProvisioner(fakeProvisioner{res: provisioner.Result{
		RepoURL:     "https://github.com/atlas-platform-tcc/payments",
		InfraPRURL:  "https://github.com/atlas-platform-tcc/atlas-infra/pull/1",
		GitopsPRURL: "https://github.com/atlas-platform-tcc/atlas-gitops/pull/1",
	}}).Handler()

	rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var svc domain.Service
	if err := json.Unmarshal(rec.Body.Bytes(), &svc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if svc.RepoURL == "" || svc.InfraPRURL == "" || svc.GitopsPRURL == "" || svc.Status != domain.StatusProvisioned {
		t.Errorf("expected repoUrl, infraPrUrl, gitopsPrUrl and status provisioned, got %+v", svc)
	}
	// catalog reflects the provisioned state
	got, _ := st.Get(context.Background(), "payments")
	if got.Status != domain.StatusProvisioned || got.RepoURL == "" || got.InfraPRURL == "" || got.GitopsPRURL == "" {
		t.Errorf("catalog not updated: %+v", got)
	}
}

func TestCreateService_ProvisioningFailed(t *testing.T) {
	h := NewServer(store.NewMemory()).
		WithProvisioner(fakeProvisioner{err: errors.New("boom")}).Handler()

	rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestListServices(t *testing.T) {
	h := newTestServer()
	_ = do(t, h, http.MethodPost, "/services", `{"name":"a","language":"go"}`)
	_ = do(t, h, http.MethodPost, "/services", `{"name":"b","language":"go"}`)

	rec := do(t, h, http.MethodGet, "/services", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var services []domain.Service
	if err := json.Unmarshal(rec.Body.Bytes(), &services); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(services) != 2 {
		t.Errorf("len = %d, want 2", len(services))
	}
}
