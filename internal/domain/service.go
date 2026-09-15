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

	StatusRegistered  = "registered"
	StatusRepoCreated = "repo_created"
	StatusProvisioned = "provisioned"
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
	CreatedAt   string   `json:"createdAt,omitempty"`
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

	// name
	switch {
	case r.Name == "":
		errs = append(errs, FieldError{"name", "is required"})
	case len(r.Name) > MaxNameLen:
		errs = append(errs, FieldError{"name", "must be at most 63 characters"})
	case !dns1123Label.MatchString(r.Name):
		errs = append(errs, FieldError{"name", "must be a valid DNS-1123 label (lowercase alphanumeric and '-')"})
	}

	// language (guardrail)
	switch {
	case r.Language == "":
		errs = append(errs, FieldError{"language", "is required"})
	case r.Language != SupportedLanguage:
		errs = append(errs, FieldError{"language", `must be "` + SupportedLanguage + `"`})
	}

	// replicas (optional, default DefaultReplicas)
	replicas := DefaultReplicas
	if r.Replicas != nil {
		replicas = *r.Replicas
		if replicas < MinReplicas || replicas > MaxReplicas {
			errs = append(errs, FieldError{"replicas", "must be between 1 and 10"})
		}
	}

	// database (optional; when enabled, type is required and must be supported)
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

	// env (optional): each entry needs a valid name; names must be unique.
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
