// Package httpapi exposes the Atlas API over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
	"github.com/atlas-platform-tcc/atlas-api/internal/provisioner"
	"github.com/atlas-platform-tcc/atlas-api/internal/store"
)

// Provisioner creates the application repository and opens the infrastructure and
// GitOps Pull Requests.
type Provisioner interface {
	Provision(ctx context.Context, svc domain.Service) (provisioner.Result, error)
}

// Server wires the HTTP handlers to the catalog store and (optionally) the
// provisioner that creates application repositories.
type Server struct {
	store       store.Store
	provisioner Provisioner
}

// NewServer returns a Server backed by the given store.
func NewServer(s store.Store) *Server { return &Server{store: s} }

// WithProvisioner enables repository provisioning on POST /services.
func (s *Server) WithProvisioner(p Provisioner) *Server {
	s.provisioner = p
	return s
}

// Handler builds the HTTP router (Go 1.22 method+path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /services", s.createService)
	mux.HandleFunc("GET /services", s.listServices)
	mux.HandleFunc("GET /services/{name}", s.getService)
	return mux
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

	svc.CreatedAt = time.Now().UTC().Format(time.RFC3339)

	switch err := s.store.Create(r.Context(), svc); {
	case err == nil:
		// registered in the catalog
	case errors.Is(err, store.ErrAlreadyExists):
		writeError(w, http.StatusConflict, errorBody{
			Error:   "already_exists",
			Message: "a service with this name already exists",
		})
		return
	default:
		log.Printf("create service %q: %v", svc.Name, err)
		writeError(w, http.StatusInternalServerError, errorBody{Error: "internal_error", Message: "could not register the service"})
		return
	}

	// Without a provisioner, registration is the whole operation (phase 02 behavior).
	if s.provisioner == nil {
		writeJSON(w, http.StatusCreated, svc)
		return
	}

	// Provision the application repository and the infra + GitOps PRs (phases 03–05).
	res, err := s.provisioner.Provision(r.Context(), svc)
	if err != nil {
		log.Printf("provision service %q: %v", svc.Name, err)
		// Persist whatever partial progress was made before the failure.
		svc.RepoURL = res.RepoURL
		svc.InfraPRURL = res.InfraPRURL
		if res.RepoURL != "" {
			svc.Status = domain.StatusRepoCreated
			_ = s.store.Update(r.Context(), svc)
		}
		writeError(w, http.StatusBadGateway, errorBody{
			Error:   "provisioning_failed",
			Message: "the service was registered but provisioning did not complete",
		})
		return
	}
	svc.RepoURL = res.RepoURL
	svc.InfraPRURL = res.InfraPRURL
	svc.GitopsPRURL = res.GitopsPRURL
	svc.Status = domain.StatusProvisioned
	if err := s.store.Update(r.Context(), svc); err != nil {
		log.Printf("update service %q after provisioning: %v", svc.Name, err)
	}
	writeJSON(w, http.StatusCreated, svc)
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
	writeJSON(w, http.StatusOK, services)
}

func (s *Server) getService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	svc, err := s.store.Get(r.Context(), name)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, svc)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errorBody{Error: "not_found", Message: "service not found"})
	default:
		log.Printf("get service %q: %v", name, err)
		writeError(w, http.StatusInternalServerError, errorBody{Error: "internal_error", Message: "could not read the service"})
	}
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
