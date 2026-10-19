// Package httpapi exposes the Atlas API over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atlas-platform-tcc/atlas-api/internal/argocd"
	"github.com/atlas-platform-tcc/atlas-api/internal/conformance"
	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
	"github.com/atlas-platform-tcc/atlas-api/internal/provisioner"
	"github.com/atlas-platform-tcc/atlas-api/internal/store"
)

// Provisioner creates the application repository and opens the infrastructure and
// GitOps Pull Requests, and undoes them on deprovisioning.
type Provisioner interface {
	Provision(ctx context.Context, svc domain.Service) (provisioner.Result, error)
	Deprovision(ctx context.Context, name string, force bool) (provisioner.DeprovisionResult, error)
}

// Observer reports the observed state (sync + health) from Argo CD. When nil, the
// catalog serves only the projected (git) state.
type Observer interface {
	AppStatus(ctx context.Context, name string) (argocd.Status, error)
	AppStatuses(ctx context.Context) (map[string]argocd.Status, error)
}

// Verifier gathers day-2 facts from the Git provider itself (branch protection,
// CI-published commit statuses) instead of trusting self-reported values
// (ADR atlas-api/0019). When nil, those facts stay unverified.
type Verifier interface {
	BranchProtected(ctx context.Context, repo string) (bool, error)
	CommitStatuses(ctx context.Context, repo, sha string) (map[string]domain.CommitStatus, error)
}

// Server wires the HTTP handlers to the catalog store and (optionally) the
// provisioner that creates application repositories and the Argo CD observer.
type Server struct {
	store       store.Store
	provisioner Provisioner
	observer    Observer
	// spawn runs the async provisioning goroutine (ADR atlas-api/0018); tests override it
	// to run inline. inflight guards against two goroutines provisioning the same service.
	spawn    func(func())
	inflight sync.Map
	policy   conformance.Policy // used by the day-2 conformance endpoint (ADR atlas-api/0019)
	verifier Verifier           // verifies day-2 facts in the Git provider (ADR atlas-api/0019)
	// Registry+org an image must carry on day-2; empty falls back to the registry alone.
	approvedImagePrefix string
}

// NewServer returns a Server backed by the given store.
func NewServer(s store.Store) *Server {
	return &Server{store: s, spawn: func(f func()) { go f() }}
}

// syncSpawn makes provisioning run inline (tests only, same package).
func (s *Server) syncSpawn() *Server {
	s.spawn = func(f func()) { f() }
	return s
}

// startProvision claims the in-flight slot for name; false means one is already running.
func (s *Server) startProvision(name string) bool {
	_, loaded := s.inflight.LoadOrStore(name, true)
	return !loaded
}

// isProvisioning reports whether a goroutine currently holds the slot for name. A
// service persisted as "provisioning" without a slot is orphaned (the process crashed
// mid-run) and must stay resumable.
func (s *Server) isProvisioning(name string) bool {
	_, ok := s.inflight.Load(name)
	return ok
}

// WithProvisioner enables repository provisioning on POST /services.
func (s *Server) WithProvisioner(p Provisioner) *Server {
	s.provisioner = p
	return s
}

// WithPolicy provides the conformance policy used by the day-2 check endpoint.
func (s *Server) WithPolicy(p conformance.Policy) *Server {
	s.policy = p
	return s
}

// WithVerifier enables API-side verification of day-2 facts in the conformance check.
func (s *Server) WithVerifier(v Verifier) *Server {
	s.verifier = v
	return s
}

// WithApprovedRegistry sets the registry+org prefix the day-2 approved-registry rule
// requires, matching the create-phase check rather than the registry alone.
func (s *Server) WithApprovedRegistry(prefix string) *Server {
	s.approvedImagePrefix = prefix
	return s
}

// WithObserver enables augmenting reads with the observed Argo CD state.
func (s *Server) WithObserver(o Observer) *Server {
	s.observer = o
	return s
}

// Handler builds the HTTP router (Go 1.22 method+path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /services", s.createService)
	mux.HandleFunc("GET /services", s.listServices)
	mux.HandleFunc("GET /services/{name}", s.getService)
	mux.HandleFunc("DELETE /services/{name}", s.deleteService)
	mux.HandleFunc("GET /services/{name}/events", s.listServiceEvents)
	mux.HandleFunc("POST /services/{name}/events", s.ingestEvent)
	mux.HandleFunc("POST /services/{name}/conformance", s.checkConformance)
	mux.HandleFunc("GET /events", s.listEvents)
	return mux
}

// pinnedTag matches an immutable image tag (a commit SHA / hex digest), not :latest.
var pinnedTag = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func imagePinned(image string) bool {
	if image == "" {
		return false
	}
	if strings.Contains(image, "@sha256:") {
		return true
	}
	i := strings.LastIndex(image, ":")
	if i < 0 {
		return false // no tag at all
	}
	return pinnedTag.MatchString(image[i+1:])
}

// coveragePctRe parses the first integer run in a status description ("72%" or "72").
// The CI controls that text, so the first digit run is the coverage value.
var coveragePctRe = regexp.MustCompile(`\d+`)

func imageSHA(image string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return ""
}

// checkConformance evaluates a proposed day-2 change against the deploy-phase rules of
// the same versioned policy (ADR atlas-api/0019). The pipeline sends only the image and
// replicas: the remaining facts are verified by the API in the Git provider (branch
// protection, CI-published commit statuses), trusted provenance instead of self-reported
// values. A fact it cannot gather stays unverified — best-effort in audit mode, a gate
// failure in enforce mode.
func (s *Server) checkConformance(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	actor := r.Header.Get("X-Atlas-Actor")
	if actor == "" {
		actor = "api"
	}
	var body struct {
		Image    string `json:"image"`
		Replicas *int   `json:"replicas"` // omitted by a deploy that only bumps the image
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errorBody{Error: "invalid_json", Message: "request body is not valid JSON"})
		return
	}
	approvedPrefix := s.approvedImagePrefix
	if approvedPrefix == "" {
		approvedPrefix = provisioner.Registry + "/" // fallback when the org is unknown
	}
	facts := conformance.Facts{
		"image.pinned":           imagePinned(body.Image),
		"image.approvedRegistry": strings.HasPrefix(body.Image, approvedPrefix),
	}
	// A bare image bump would otherwise read as replicas=0 and fail replicas-in-bounds.
	if body.Replicas != nil {
		facts["replicas"] = *body.Replicas
		facts["resources.totalCpuMillis"] = *body.Replicas * provisioner.LimitCPUMillis
	}
	if s.verifier != nil {
		ctx := r.Context()
		if bp, err := s.verifier.BranchProtected(ctx, name); err != nil {
			s.recordEvent(ctx, actor, name, domain.EventConformanceUnreachable, "warn", "branch protection: "+err.Error())
		} else {
			facts["repo.branchProtected"] = bp
		}
		if sha := imageSHA(body.Image); sha != "" {
			if st, err := s.verifier.CommitStatuses(ctx, name, sha); err != nil {
				s.recordEvent(ctx, actor, name, domain.EventConformanceUnreachable, "warn", "commit status: "+err.Error())
			} else {
				if scan, ok := st["atlas/scan"]; ok {
					facts["scan.passed"] = scan.State == "success"
				}
				if lint, ok := st["atlas/lint"]; ok {
					facts["lint.passed"] = lint.State == "success"
				}
				if cov, ok := st["atlas/coverage"]; ok {
					if n := coveragePctRe.FindString(cov.Description); n != "" {
						pct, _ := strconv.Atoi(n)
						facts["tests.coveragePct"] = pct
					}
				}
			}
		}
	}

	pol := s.policy.ForPhase(conformance.PhaseDeploy)
	report := conformance.Evaluate(pol, facts)
	if unv := unverifiedRuleSummary(report); unv != "" {
		s.recordEvent(r.Context(), actor, name, domain.EventConformanceSkipped, "warn", "não verificado: "+unv)
	}
	if ok, _ := pol.Gate(report); !ok {
		s.recordEvent(r.Context(), actor, name, domain.EventGateBlocked, "blocked", "day-2: "+failedRuleSummary(report))
		writeError(w, http.StatusUnprocessableEntity, errorBody{
			Error:   "conformance_gate_failed",
			Message: "day-2 change refused: it does not satisfy the conformance policy",
			Fields:  failedRuleFields(report),
		})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

const eventsLimit = 100

func (s *Server) listServiceEvents(w http.ResponseWriter, r *http.Request) {
	s.writeEvents(w, r, r.PathValue("name"))
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	s.writeEvents(w, r, "")
}

func (s *Server) writeEvents(w http.ResponseWriter, r *http.Request, service string) {
	events, err := s.store.ListEvents(r.Context(), service, eventsLimit)
	if err != nil {
		log.Printf("list events %q: %v", service, err)
		writeError(w, http.StatusInternalServerError, errorBody{Error: "internal_error", Message: "could not list events"})
		return
	}
	if events == nil {
		events = []domain.Event{}
	}
	writeJSON(w, http.StatusOK, events)
}

// ingestEvent lets an external actor report a deploy/infra event the API cannot observe
// by itself. Only allowlisted types are accepted (ADR atlas-api/0017); the endpoint is
// unauthenticated in the MVP.
func (s *Server) ingestEvent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errorBody{Error: "invalid_json", Message: "request body is not valid JSON"})
		return
	}
	if !domain.IngestableEventTypes[body.Type] {
		writeError(w, http.StatusUnprocessableEntity, errorBody{
			Error:   "invalid_event_type",
			Message: "this endpoint only accepts deploy.* and infra.* event types",
		})
		return
	}
	s.record(r, name, body.Type, "ok", body.Detail)
	w.WriteHeader(http.StatusAccepted)
}

// record appends an audit event from an HTTP request (actor from X-Atlas-Actor).
func (s *Server) record(r *http.Request, service, evType, outcome, detail string) {
	actor := r.Header.Get("X-Atlas-Actor")
	if actor == "" {
		actor = "api"
	}
	s.recordEvent(r.Context(), actor, service, evType, outcome, detail)
}

// recordEvent appends an audit event; a store failure never fails the caller.
func (s *Server) recordEvent(ctx context.Context, actor, service, evType, outcome, detail string) {
	ev := domain.Event{
		Service: service, Type: evType, Outcome: outcome, Actor: actor, Detail: detail,
		At: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.store.AppendEvent(ctx, ev); err != nil {
		log.Printf("append event %s/%s: %v", service, evType, err)
	}
}

// deprovisionResponse is the body of an accepted DELETE /services/{name}.
type deprovisionResponse struct {
	Name                string `json:"name"`
	Status              string `json:"status"`
	GitopsTeardownPRURL string `json:"gitopsTeardownPrUrl,omitempty"`
	InfraTeardownPRURL  string `json:"infraTeardownPrUrl,omitempty"`
	RepoArchived        bool   `json:"repoArchived"`
	RepoDeleted         bool   `json:"repoDeleted"`
	PackageDeleted      bool   `json:"packageDeleted"`
	Message             string `json:"message"`
}

// errorBody is the standardized error response.
type errorBody struct {
	Error   string              `json:"error"`
	Message string              `json:"message"`
	Fields  []domain.FieldError `json:"fields,omitempty"`
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateServiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, errorBody{Error: "invalid_json", Message: "request body is not valid JSON"})
		return
	}

	svc, fieldErrs := req.Validate()
	if fieldErrs != nil {
		writeError(w, http.StatusUnprocessableEntity, errorBody{
			Error:   "validation_failed",
			Message: "the request has invalid fields",
			Fields:  fieldErrs,
		})
		return
	}

	// Idempotency keyed by the service name (ADR atlas-api/0009): an identical spec is a
	// safe replay, a different one under the same name is a conflict.
	existed := false
	switch existing, err := s.store.Get(r.Context(), svc.Name); {
	case err == nil:
		existed = true
		if !existing.SameSpec(svc) {
			s.record(r, svc.Name, domain.EventConflict, "error", "specificação divergente para um nome existente")
			writeError(w, http.StatusConflict, errorBody{
				Error:   "already_exists",
				Message: "a service with this name already exists with a different specification",
			})
			return
		}
		switch existing.Status {
		case domain.StatusProvisioned:
			s.record(r, svc.Name, domain.EventReplayed, "ok", "replay idempotente (serviço já provisionado)")
			writeJSON(w, http.StatusOK, existing)
			return
		case domain.StatusProvisioning:
			// Accept without starting a second run. With no slot held the process crashed
			// mid-provision, so fall through and resume it.
			if s.isProvisioning(svc.Name) {
				writeJSON(w, http.StatusAccepted, existing)
				return
			}
		}
		// Resume, preserving the original record and any progress.
		svc.CreatedAt = existing.CreatedAt
		svc.RepoURL, svc.InfraPRURL, svc.GitopsPRURL = existing.RepoURL, existing.InfraPRURL, existing.GitopsPRURL
	case errors.Is(err, store.ErrNotFound):
		svc.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.store.Create(r.Context(), svc); err != nil {
			log.Printf("create service %q: %v", svc.Name, err)
			writeError(w, http.StatusInternalServerError, errorBody{Error: "internal_error", Message: "could not register the service"})
			return
		}
		s.record(r, svc.Name, domain.EventRegistered, "ok", "")
	default:
		log.Printf("get service %q: %v", svc.Name, err)
		writeError(w, http.StatusInternalServerError, errorBody{Error: "internal_error", Message: "could not read the service"})
		return
	}

	// Without a provisioner the API is register-only.
	if s.provisioner == nil {
		status := http.StatusCreated
		if existed {
			status = http.StatusOK
		}
		writeJSON(w, status, svc)
		return
	}

	// Async provisioning (ADR atlas-api/0018): return 202 immediately while a goroutine
	// opens the PRs and updates the catalog, observed by polling GET /services. A gate
	// refusal in Provision ends the service in the "blocked" state.
	actor := r.Header.Get("X-Atlas-Actor")
	if actor == "" {
		actor = "api"
	}
	svc.Status = domain.StatusProvisioning
	if err := s.store.Update(r.Context(), svc); err != nil {
		log.Printf("mark provisioning %q: %v", svc.Name, err)
	}
	if s.startProvision(svc.Name) {
		s.spawn(func() { s.provisionAsync(svc, actor) })
	}
	writeJSON(w, http.StatusAccepted, svc)
}

// provisionAsync runs the provisioning off the request path (ADR atlas-api/0018),
// owning the in-flight slot claimed by startProvision.
func (s *Server) provisionAsync(svc domain.Service, actor string) {
	defer s.inflight.Delete(svc.Name)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t0 := time.Now()
	res, err := s.provisioner.Provision(ctx, svc)
	svc.RepoURL, svc.InfraPRURL, svc.GitopsPRURL = res.RepoURL, res.InfraPRURL, res.GitopsPRURL
	svc.URL = res.URL
	svc.ProvisionMs = time.Since(t0).Milliseconds()
	if res.Conformance.Max > 0 {
		report := res.Conformance
		svc.Conformance = &report
	}

	if err != nil {
		log.Printf("provision service %q: %v", svc.Name, err)
		switch {
		case errors.Is(err, provisioner.ErrConformanceGate):
			svc.Status = domain.StatusBlocked
			s.recordEvent(ctx, actor, svc.Name, domain.EventGateBlocked, "blocked", "regras reprovadas: "+failedRuleSummary(res.Conformance))
		default:
			// A failure before the repo returns the service to "registered", so a re-POST
			// resumes instead of leaving it stuck.
			if svc.RepoURL != "" {
				svc.Status = domain.StatusRepoCreated
			} else {
				svc.Status = domain.StatusRegistered
			}
			s.recordEvent(ctx, actor, svc.Name, domain.EventProvisionFailed, "error", "provisionamento não concluído")
		}
		if uerr := s.store.Update(ctx, svc); uerr != nil {
			log.Printf("update service %q after failed provisioning: %v", svc.Name, uerr)
		}
		return
	}

	svc.Status = domain.StatusProvisioned
	if uerr := s.store.Update(ctx, svc); uerr != nil {
		log.Printf("update service %q after provisioning: %v", svc.Name, uerr)
	}
	s.recordEvent(ctx, actor, svc.Name, domain.EventConformance, "ok", fmt.Sprintf("score %d%%", res.Conformance.Percent))
	s.recordEvent(ctx, actor, svc.Name, domain.EventProvisioned, "ok", "gitops PR: "+res.GitopsPRURL)
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	services, err := s.store.List(r.Context())
	if err != nil {
		log.Printf("list services: %v", err)
		writeError(w, http.StatusInternalServerError, errorBody{Error: "internal_error", Message: "could not list services"})
		return
	}
	if services == nil {
		services = []domain.Service{}
	}
	// One Argo CD call for the whole listing; on failure the projected state is still
	// served (ADR atlas-api/0012).
	if s.observer != nil {
		if statuses, err := s.observer.AppStatuses(r.Context()); err != nil {
			log.Printf("observe listing: %v", err)
		} else {
			for i := range services {
				if st, ok := statuses[services[i].Name]; ok {
					services[i].Sync, services[i].Health = st.Sync, st.Health
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, services)
}

func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	svc, err := s.store.Get(r.Context(), name)
	switch {
	case err == nil:
		if s.observer != nil {
			if st, oerr := s.observer.AppStatus(r.Context(), name); oerr != nil {
				log.Printf("observe %q: %v", name, oerr)
			} else {
				svc.Sync, svc.Health = st.Sync, st.Health
			}
		}
		writeJSON(w, http.StatusOK, svc)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errorBody{Error: "not_found", Message: "service not found"})
	default:
		log.Printf("get service %q: %v", name, err)
		writeError(w, http.StatusInternalServerError, errorBody{Error: "internal_error", Message: "could not read the service"})
	}
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if _, err := s.store.Get(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, errorBody{Error: "not_found", Message: "service not found"})
			return
		}
		log.Printf("get service %q: %v", name, err)
		writeError(w, http.StatusInternalServerError, errorBody{Error: "internal_error", Message: "could not read the service"})
		return
	}

	if s.provisioner == nil {
		writeError(w, http.StatusNotImplemented, errorBody{
			Error:   "provisioning_disabled",
			Message: "deprovisioning requires a configured Git provider (set GITHUB_TOKEN)",
		})
		return
	}

	// force permanently deletes the repository instead of archiving it (testing/cleanup).
	force := r.URL.Query().Get("force") == "true"

	res, err := s.provisioner.Deprovision(r.Context(), name, force)
	if err != nil {
		log.Printf("deprovision service %q (force=%t): %v", name, force, err)
		s.record(r, name, domain.EventDeprovisionFail, "error", "teardown incompleto")
		writeError(w, http.StatusBadGateway, errorBody{
			Error:   "deprovisioning_failed",
			Message: "the teardown Pull Requests could not be fully opened",
		})
		return
	}
	disposal := "repositório arquivado"
	if force {
		disposal = "repositório e pacote deletados (force)"
	}
	s.record(r, name, domain.EventDeprovisioned, "ok", disposal)

	// The catalog is a projection: dropping the row frees the name for a recreate, and
	// the audit trail stays in Git (teardown PRs and the archived repo).
	if err := s.store.Delete(r.Context(), name); err != nil {
		log.Printf("delete service %q from catalog: %v", name, err)
	}

	msg := "teardown Pull Requests opened; merge them to complete the removal"
	if force {
		msg = "teardown Pull Requests opened (force: repository and package deleted); merge them to complete the removal"
	}
	writeJSON(w, http.StatusAccepted, deprovisionResponse{
		Name:                name,
		Status:              domain.StatusDeprovisioning,
		GitopsTeardownPRURL: res.GitopsPRURL,
		InfraTeardownPRURL:  res.InfraPRURL,
		RepoArchived:        res.RepoArchived,
		RepoDeleted:         res.RepoDeleted,
		PackageDeleted:      res.PackageDeleted,
		Message:             msg,
	})
}

// failedRuleFields turns the genuinely failing rules into standardized field errors;
// unverified ones are reported separately.
func failedRuleFields(report domain.ConformanceReport) []domain.FieldError {
	var fields []domain.FieldError
	for _, r := range report.Results {
		if !r.Pass && !r.Unverified {
			fields = append(fields, domain.FieldError{Field: r.ID, Reason: r.Description})
		}
	}
	return fields
}

// failedRuleSummary is a compact "id1, id2" of the genuinely failing rules, for the
// event detail.
func failedRuleSummary(report domain.ConformanceReport) string {
	var ids []string
	for _, r := range report.Results {
		if !r.Pass && !r.Unverified {
			ids = append(ids, r.ID)
		}
	}
	return strings.Join(ids, ", ")
}

// unverifiedRuleSummary lists rules that applied but could not be verified.
func unverifiedRuleSummary(report domain.ConformanceReport) string {
	var ids []string
	for _, r := range report.Results {
		if r.Unverified {
			ids = append(ids, r.ID)
		}
	}
	return strings.Join(ids, ", ")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, body errorBody) {
	writeJSON(w, status, body)
}
