package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atlas-platform-tcc/atlas-api/internal/conformance"
	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
	"github.com/atlas-platform-tcc/atlas-api/internal/provisioner"
	"github.com/atlas-platform-tcc/atlas-api/internal/store"
)

// fakeProvisioner records the call and returns a canned Result or an error.
type fakeProvisioner struct {
	res        provisioner.Result
	err        error
	deconRes   provisioner.DeprovisionResult
	deconErr   error
	provisions *int // optional call counter
}

func (f fakeProvisioner) Provision(_ context.Context, _ domain.Service) (provisioner.Result, error) {
	if f.provisions != nil {
		*f.provisions++
	}
	return f.res, f.err
}

func (f fakeProvisioner) Deprovision(_ context.Context, _ string, force bool) (provisioner.DeprovisionResult, error) {
	res := f.deconRes
	res.RepoDeleted = force
	res.RepoArchived = !force
	return res, f.deconErr
}

func newTestServer() http.Handler {
	return NewServer(store.NewMemory()).Handler()
}

// fakeVerifier returns canned branch-protection and commit-status facts.
type fakeVerifier struct {
	protected bool
	statuses  map[string]domain.CommitStatus
	err       error
}

func (f fakeVerifier) BranchProtected(context.Context, string) (bool, error) {
	return f.protected, f.err
}

func (f fakeVerifier) CommitStatuses(context.Context, string, string) (map[string]domain.CommitStatus, error) {
	return f.statuses, f.err
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

func TestEvents_RegisteredRecorded(t *testing.T) {
	h := newTestServer() // register-only (no provisioner)
	if rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rec.Code)
	}
	rec := do(t, h, http.MethodGet, "/services/payments/events", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d", rec.Code)
	}
	var events []domain.Event
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 1 || events[0].Type != domain.EventRegistered {
		t.Fatalf("expected one %s event, got %+v", domain.EventRegistered, events)
	}
}

func TestIngestEvent_AcceptsDeploy(t *testing.T) {
	h := newTestServer()
	rec := do(t, h, http.MethodPost, "/services/payments/events", `{"type":"deploy.promoted","detail":"image abc123"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/events", "")
	var events []domain.Event
	_ = json.Unmarshal(rec.Body.Bytes(), &events)
	if len(events) != 1 || events[0].Type != domain.EventDeployPromoted || events[0].Actor != "api" {
		t.Fatalf("expected one deploy.promoted event, got %+v", events)
	}
}

func TestIngestEvent_RejectsUnknownType(t *testing.T) {
	h := newTestServer()
	rec := do(t, h, http.MethodPost, "/services/payments/events", `{"type":"provision.succeeded"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (only deploy.*/infra.* ingestable)", rec.Code)
	}
}

func TestCheckConformance_Day2Gate(t *testing.T) {
	pol, _ := conformance.Default() // audit mode

	// No verifier → scan/coverage/branch stay unverified → skipped in audit.
	h := NewServer(store.NewMemory()).WithPolicy(pol).Handler()
	if rec := do(t, h, http.MethodPost, "/services/payments/conformance",
		`{"image":"ghcr.io/atlas-platform-tcc/payments:abc1234","replicas":2}`); rec.Code != http.StatusOK {
		t.Fatalf("pinned image status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// :latest (mutable) → image-pinned (required) fails → 422
	if rec := do(t, h, http.MethodPost, "/services/payments/conformance",
		`{"image":"ghcr.io/atlas-platform-tcc/payments:latest","replicas":2}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf(":latest status = %d, want 422", rec.Code)
	}

	// Verifier reports a FAILED scan → security-scan (required) fails → 422 (even in audit;
	// it is a real failure, not unverified).
	hScanFail := NewServer(store.NewMemory()).WithPolicy(pol).
		WithVerifier(fakeVerifier{protected: true, statuses: map[string]domain.CommitStatus{"atlas/scan": {State: "failure"}}}).Handler()
	if rec := do(t, hScanFail, http.MethodPost, "/services/payments/conformance",
		`{"image":"ghcr.io/atlas-platform-tcc/payments:abc1234","replicas":2}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("failed scan status = %d, want 422", rec.Code)
	}

	// Verifier reports a FAILED static analysis → static-analysis (required) fails → 422.
	hLintFail := NewServer(store.NewMemory()).WithPolicy(pol).
		WithVerifier(fakeVerifier{protected: true, statuses: map[string]domain.CommitStatus{
			"atlas/scan": {State: "success"},
			"atlas/lint": {State: "failure"},
		}}).Handler()
	if rec := do(t, hLintFail, http.MethodPost, "/services/payments/conformance",
		`{"image":"ghcr.io/atlas-platform-tcc/payments:abc1234","replicas":2}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("failed lint status = %d, want 422", rec.Code)
	}

	// Verifier reports scan+lint success + coverage 75% + branch protected → 200.
	hOk := NewServer(store.NewMemory()).WithPolicy(pol).
		WithVerifier(fakeVerifier{protected: true, statuses: map[string]domain.CommitStatus{
			"atlas/scan":     {State: "success"},
			"atlas/lint":     {State: "success"},
			"atlas/coverage": {State: "success", Description: "75%"},
		}}).Handler()
	if rec := do(t, hOk, http.MethodPost, "/services/payments/conformance",
		`{"image":"ghcr.io/atlas-platform-tcc/payments:abc1234","replicas":2}`); rec.Code != http.StatusOK {
		t.Fatalf("verified-ok status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCheckConformance_EnforceBlocksUnverified(t *testing.T) {
	pol, _ := conformance.Default()
	pol.Mode = conformance.ModeEnforce
	// enforce + no verifier → required security-scan is unverified → gate fails (422).
	h := NewServer(store.NewMemory()).WithPolicy(pol).Handler()
	if rec := do(t, h, http.MethodPost, "/services/payments/conformance",
		`{"image":"ghcr.io/atlas-platform-tcc/payments:abc1234","replicas":2}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("enforce/unverified status = %d, want 422", rec.Code)
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

func TestCreateService_ConflictDifferentSpec(t *testing.T) {
	h := newTestServer()
	_ = do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go","replicas":2}`)
	// same name, different spec — not a safe replay
	rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go","replicas":5}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func TestCreateService_IdempotentReplay(t *testing.T) {
	st := store.NewMemory()
	payload := `{"name":"payments","language":"go","replicas":2}`
	// register-only: re-posting the same spec is a no-op replay (200), not a conflict
	h := NewServer(st).Handler()
	if rec := do(t, h, http.MethodPost, "/services", payload); rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/services", payload); rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", rec.Code)
	}
	if list, _ := st.List(context.Background()); len(list) != 1 {
		t.Errorf("expected a single catalog row, got %d", len(list))
	}
}

func TestCreateService_ReplayDoesNotReprovision(t *testing.T) {
	n := 0
	st := store.NewMemory()
	h := NewServer(st).WithProvisioner(fakeProvisioner{
		res: provisioner.Result{
			RepoURL:     "https://github.com/atlas-platform-tcc/payments",
			InfraPRURL:  "https://github.com/atlas-platform-tcc/atlas-infra/pull/1",
			GitopsPRURL: "https://github.com/atlas-platform-tcc/atlas-gitops/pull/1",
		},
		provisions: &n,
	}).syncSpawn().Handler()
	payload := `{"name":"payments","language":"go"}`
	// async provisioning is accepted with 202; syncSpawn ran the goroutine inline
	if rec := do(t, h, http.MethodPost, "/services", payload); rec.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", rec.Code)
	}
	// once provisioned, a replay returns 200 without provisioning again
	if rec := do(t, h, http.MethodPost, "/services", payload); rec.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", rec.Code)
	}
	if n != 1 {
		t.Errorf("Provision called %d times, want 1 (replay must be a no-op)", n)
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
	}}).syncSpawn().Handler()

	// async: POST is accepted (202); the final state is observed in the catalog.
	rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	got, _ := st.Get(context.Background(), "payments")
	if got.Status != domain.StatusProvisioned || got.RepoURL == "" || got.InfraPRURL == "" || got.GitopsPRURL == "" {
		t.Errorf("catalog not provisioned: %+v", got)
	}
}

func TestCreateService_ProvisioningFailed(t *testing.T) {
	st := store.NewMemory()
	h := NewServer(st).
		WithProvisioner(fakeProvisioner{err: errors.New("boom")}).syncSpawn().Handler()

	// async: POST is accepted; the failure lands in the catalog (no repo → back to registered).
	rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	got, _ := st.Get(context.Background(), "payments")
	if got.Status != domain.StatusRegistered {
		t.Errorf("expected status registered after failure with no repo, got %q", got.Status)
	}
}

func TestCreateService_GateBlocked(t *testing.T) {
	st := store.NewMemory()
	report := domain.ConformanceReport{
		Max: 3, Percent: 0, Failed: 1,
		Results: []domain.ConformanceResult{{ID: "image-pinned", Description: "imagem fixada", Severity: "high", Pass: false}},
	}
	h := NewServer(st).WithProvisioner(fakeProvisioner{
		res: provisioner.Result{RepoURL: "https://github.com/atlas-platform-tcc/payments", Conformance: report},
		err: provisioner.ErrConformanceGate,
	}).syncSpawn().Handler()

	if rec := do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	got, _ := st.Get(context.Background(), "payments")
	if got.Status != domain.StatusBlocked {
		t.Errorf("expected status blocked, got %q", got.Status)
	}
	rec := do(t, h, http.MethodGet, "/services/payments/events", "")
	var events []domain.Event
	_ = json.Unmarshal(rec.Body.Bytes(), &events)
	var sawGate bool
	for _, e := range events {
		if e.Type == domain.EventGateBlocked {
			sawGate = true
		}
	}
	if !sawGate {
		t.Errorf("expected a gate.blocked event, got %+v", events)
	}
}

func TestDeleteService_Accepted(t *testing.T) {
	st := store.NewMemory()
	h := NewServer(st).WithProvisioner(fakeProvisioner{deconRes: provisioner.DeprovisionResult{
		GitopsPRURL:    "https://github.com/atlas-platform-tcc/atlas-gitops/pull/9",
		InfraPRURL:     "https://github.com/atlas-platform-tcc/atlas-infra/pull/9",
		RepoArchived:   true,
		PackageDeleted: true,
	}}).Handler()

	_ = do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`)

	rec := do(t, h, http.MethodDelete, "/services/payments", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var body deprovisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != domain.StatusDeprovisioning || body.GitopsTeardownPRURL == "" || body.InfraTeardownPRURL == "" || !body.RepoArchived || !body.PackageDeleted {
		t.Errorf("unexpected response: %+v", body)
	}
	// catalog row is gone, so the name can be recreated
	if _, err := st.Get(context.Background(), "payments"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected service removed from catalog, got err=%v", err)
	}
}

func TestDeleteService_Force(t *testing.T) {
	st := store.NewMemory()
	h := NewServer(st).WithProvisioner(fakeProvisioner{deconRes: provisioner.DeprovisionResult{
		GitopsPRURL:    "https://github.com/atlas-platform-tcc/atlas-gitops/pull/9",
		InfraPRURL:     "https://github.com/atlas-platform-tcc/atlas-infra/pull/9",
		PackageDeleted: true,
	}}).Handler()

	_ = do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`)

	rec := do(t, h, http.MethodDelete, "/services/payments?force=true", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var body deprovisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.RepoDeleted || body.RepoArchived {
		t.Errorf("force should delete (not archive) the repo, got %+v", body)
	}
}

func TestDeleteService_NotFound(t *testing.T) {
	h := NewServer(store.NewMemory()).
		WithProvisioner(fakeProvisioner{}).Handler()
	rec := do(t, h, http.MethodDelete, "/services/missing", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestDeleteService_ProvisioningDisabled(t *testing.T) {
	st := store.NewMemory()
	h := NewServer(st).Handler() // no provisioner
	_ = do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`)

	rec := do(t, h, http.MethodDelete, "/services/payments", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

func TestDeleteService_DeprovisioningFailed(t *testing.T) {
	st := store.NewMemory()
	h := NewServer(st).WithProvisioner(fakeProvisioner{deconErr: errors.New("boom")}).Handler()
	_ = do(t, h, http.MethodPost, "/services", `{"name":"payments","language":"go"}`)

	rec := do(t, h, http.MethodDelete, "/services/payments", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	// on failure the catalog row is kept (removal not confirmed)
	if _, err := st.Get(context.Background(), "payments"); err != nil {
		t.Errorf("expected service kept in catalog on failure, got err=%v", err)
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
