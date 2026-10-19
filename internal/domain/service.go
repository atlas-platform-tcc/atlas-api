// Package domain holds the service contract, its guardrails and validation.
package domain

import (
	"fmt"
	"regexp"
)

// Guardrails imposed by the platform (the developer does not choose these).
const (
	SupportedLanguage = "go"
	SupportedDatabase = "postgres"
	MinReplicas       = 1
	MaxReplicas       = 10
	DefaultReplicas   = 1
	MaxNameLen        = 63

	StatusRegistered     = "registered"
	StatusProvisioning   = "provisioning" // async provisioning in flight (ADR atlas-api/0018)
	StatusRepoCreated    = "repo_created"
	StatusProvisioned    = "provisioned"
	StatusBlocked        = "blocked" // refused by the conformance gate (ADR atlas-api/0016)
	StatusDeprovisioning = "deprovisioning"
)

// dns1123Label matches a valid Kubernetes/DNS-1123 label.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// envName matches a valid environment variable name (C identifier).
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvVar is an environment variable injected into the service container.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ConformanceResult is the outcome of a single policy rule (ADR atlas-api/0013).
// Unverified marks a rule that applied but whose fact could not be gathered: best-effort
// in audit mode, a failure in enforce mode (ADR atlas-api/0016).
type ConformanceResult struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Severity    string `json:"severity"` // high | medium | low
	Pass        bool   `json:"pass"`
	Unverified  bool   `json:"unverified,omitempty"`
}

// CommitStatus is a commit status published by the CI. The API reads it to verify
// day-2 facts instead of trusting values self-reported by the deploy pipeline
// (ADR atlas-api/0019).
type CommitStatus struct {
	State       string // success | failure | pending | error
	Description string
}

// ConformanceReport is the evaluation of the platform policy against a service.
// Score/Max are weighted by severity; Percent is the normalized 0..100 result.
type ConformanceReport struct {
	Score   int                 `json:"score"`
	Max     int                 `json:"max"`
	Percent int                 `json:"percent"`
	Passed  int                 `json:"passed"`
	Failed  int                 `json:"failed"`
	Results []ConformanceResult `json:"results"`
}

// Database describes an optional database dependency for a service.
type Database struct {
	Enabled bool   `json:"enabled"`
	Type    string `json:"type,omitempty"`
}

// Service is the catalog entity (the validated intent plus its projected state).
type Service struct {
	Name        string   `json:"name"`
	Language    string   `json:"language"`
	Replicas    int      `json:"replicas"`
	Database    Database `json:"database"`
	Env         []EnvVar `json:"env,omitempty"`
	Status      string   `json:"status,omitempty"`
	RepoURL     string   `json:"repoUrl,omitempty"`
	InfraPRURL  string   `json:"infraPrUrl,omitempty"`
	GitopsPRURL string   `json:"gitopsPrUrl,omitempty"`
	URL         string   `json:"url,omitempty"` // service URL once the GitOps PR is merged
	CreatedAt   string   `json:"createdAt,omitempty"`

	// Read from Argo CD at request time and never persisted: the catalog stays a
	// projection of the desired (git) state (ADR atlas-api/0012).
	Sync   string `json:"sync,omitempty"`
	Health string `json:"health,omitempty"`

	// Golden-path metrics emitted at provisioning (ADRs atlas-api/0013, 0014).
	Conformance *ConformanceReport `json:"conformance,omitempty"`
	ProvisionMs int64              `json:"provisionMs,omitempty"`
}

// SameSpec reports whether two services carry the same intent, ignoring projected
// state. It is the idempotency key: re-posting an identical spec is a safe replay.
func (s Service) SameSpec(o Service) bool {
	if s.Name != o.Name || s.Language != o.Language || s.Replicas != o.Replicas ||
		s.Database != o.Database || len(s.Env) != len(o.Env) {
		return false
	}
	for i := range s.Env {
		if s.Env[i] != o.Env[i] {
			return false
		}
	}
	return true
}

// Event is an append-only audit record of a lifecycle operation (ADR atlas-api/0017).
// It outlives the service it refers to, capturing what the Git history cannot (e.g. a
// gate-blocked provisioning that opened no Pull Request).
type Event struct {
	ID      int64  `json:"id"`
	Service string `json:"service"`
	Type    string `json:"type"`
	Outcome string `json:"outcome"`         // ok | error | blocked
	Actor   string `json:"actor,omitempty"` // X-Atlas-Actor header; "api" by default
	Detail  string `json:"detail,omitempty"`
	At      string `json:"at"` // RFC3339
}

// Event types recorded by the API.
const (
	EventRegistered      = "service.registered"
	EventConformance     = "conformance.evaluated"
	EventProvisioned     = "provision.succeeded"
	EventGateBlocked     = "gate.blocked"
	EventProvisionFailed = "provision.failed"
	EventReplayed        = "create.replayed"
	EventConflict        = "create.conflict"
	EventDeprovisioned   = "deprovision.succeeded"
	EventDeprovisionFail = "deprovision.failed"

	// Ingested from the day-2 pipeline: the deploy runs in CI, not through the API.
	EventDeployPromoted = "deploy.promoted"
	EventDeployRollback = "deploy.rolled_back"

	// Ingested from the Terraform runner after apply.
	EventInfraApplied   = "infra.applied"
	EventInfraDestroyed = "infra.destroyed"

	// Derived by the observer poller on a status transition.
	EventArgoHealthy     = "argocd.healthy"
	EventArgoProgressing = "argocd.progressing"
	EventArgoDegraded    = "argocd.degraded"

	// Transparency (ADR atlas-api/0016): an unverifiable fact is recorded, not silent.
	EventConformanceSkipped     = "conformance.skipped"
	EventConformanceUnreachable = "conformance.unreachable"
)

// IngestableEventTypes are the event types an external actor may POST to the API;
// every other type is recorded by the API itself.
var IngestableEventTypes = map[string]bool{
	EventDeployPromoted: true,
	EventDeployRollback: true,
	EventInfraApplied:   true,
	EventInfraDestroyed: true,
}

// CreateServiceRequest is the JSON contract received by the API. Replicas and
// Database are pointers so an omitted field can be told apart from a zero value.
type CreateServiceRequest struct {
	Name     string    `json:"name"`
	Language string    `json:"language"`
	Replicas *int      `json:"replicas"`
	Database *Database `json:"database"`
	Env      []EnvVar  `json:"env"`
}

// FieldError is a single validation failure, part of the standardized error body.
type FieldError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Validate checks the request against the contract and the platform guardrails,
// accumulating every problem. On success it returns the normalized Service
// (defaults applied) and a nil slice.
func (r CreateServiceRequest) Validate() (Service, []FieldError) {
	var errs []FieldError

	switch {
	case r.Name == "":
		errs = append(errs, FieldError{"name", "is required"})
	case len(r.Name) > MaxNameLen:
		errs = append(errs, FieldError{"name", "must be at most 63 characters"})
	case !dns1123Label.MatchString(r.Name):
		errs = append(errs, FieldError{"name", "must be a valid DNS-1123 label (lowercase alphanumeric and '-')"})
	}

	switch {
	case r.Language == "":
		errs = append(errs, FieldError{"language", "is required"})
	case r.Language != SupportedLanguage:
		errs = append(errs, FieldError{"language", `must be "` + SupportedLanguage + `"`})
	}

	replicas := DefaultReplicas
	if r.Replicas != nil {
		replicas = *r.Replicas
		if replicas < MinReplicas || replicas > MaxReplicas {
			errs = append(errs, FieldError{"replicas", "must be between 1 and 10"})
		}
	}

	var db Database
	if r.Database != nil {
		db.Enabled = r.Database.Enabled
		if db.Enabled {
			switch r.Database.Type {
			case "":
				errs = append(errs, FieldError{"database.type", "is required when database.enabled is true"})
			case SupportedDatabase:
				db.Type = r.Database.Type
			default:
				errs = append(errs, FieldError{"database.type", `must be "` + SupportedDatabase + `"`})
			}
		}
	}

	seen := make(map[string]bool, len(r.Env))
	for i, e := range r.Env {
		field := fmt.Sprintf("env[%d].name", i)
		switch {
		case e.Name == "":
			errs = append(errs, FieldError{field, "is required"})
		case !envName.MatchString(e.Name):
			errs = append(errs, FieldError{field, "must be a valid environment variable name"})
		case seen[e.Name]:
			errs = append(errs, FieldError{field, "is duplicated"})
		default:
			seen[e.Name] = true
		}
	}

	if len(errs) > 0 {
		return Service{}, errs
	}

	return Service{
		Name:     r.Name,
		Language: r.Language,
		Replicas: replicas,
		Database: db,
		Env:      r.Env,
		Status:   StatusRegistered,
	}, nil
}
