package generator

import (
	"strings"
	"testing"

	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
)

func templateFixture() Files {
	return Files{
		"main.go":                  []byte("package main\n"),
		"go.mod":                   []byte("module hello-service\n\ngo 1.26\n"),
		"Dockerfile":               []byte("FROM golang:1.26\n"),
		"deploy/values.yaml":       []byte("replicaCount: 2\n"),
		".github/workflows/ci.yml": []byte("name: ci\n"),
	}
}

func TestRender_RegeneratesGoModAndValues(t *testing.T) {
	svc := domain.Service{Name: "payments", Language: "go", Replicas: 3}
	out := Render(templateFixture(), svc)

	if got := string(out["go.mod"]); !strings.Contains(got, "module payments") {
		t.Errorf("go.mod not parametrized: %q", got)
	}
	if got := string(out["deploy/values.yaml"]); !strings.Contains(got, "replicaCount: 3") {
		t.Errorf("values not parametrized: %q", got)
	}
}

func TestRender_CopiesGuardrailsVerbatim(t *testing.T) {
	out := Render(templateFixture(), domain.Service{Name: "svc", Replicas: 1})
	for _, path := range []string{"main.go", "Dockerfile", ".github/workflows/ci.yml"} {
		if _, ok := out[path]; !ok {
			t.Errorf("expected %s to be copied into the generated repo", path)
		}
	}
	if string(out["Dockerfile"]) != "FROM golang:1.26\n" {
		t.Error("Dockerfile should be copied verbatim")
	}
}

func TestRender_UserEnv(t *testing.T) {
	svc := domain.Service{
		Name: "svc", Replicas: 1,
		Env: []domain.EnvVar{{Name: "LOG_LEVEL", Value: "debug"}, {Name: "FEATURE_X", Value: "true"}},
	}
	got := string(Render(templateFixture(), svc)["deploy/values.yaml"])
	if !strings.Contains(got, "LOG_LEVEL") || !strings.Contains(got, "debug") || !strings.Contains(got, "FEATURE_X") {
		t.Errorf("expected user env in values, got: %q", got)
	}
}

func TestRenderServiceTF(t *testing.T) {
	withDB := RenderServiceTF(domain.Service{Name: "orders", Database: domain.Database{Enabled: true, Type: "postgres"}})
	for _, want := range []string{`module "orders"`, `../modules/service`, `name             = "orders"`, `database_enabled = true`, `database_type    = "postgres"`, `ghcr_username    = var.ghcr_username`, `ghcr_token       = var.ghcr_token`} {
		if !strings.Contains(withDB, want) {
			t.Errorf("expected %q in tf, got:\n%s", want, withDB)
		}
	}
	noDB := RenderServiceTF(domain.Service{Name: "gateway"})
	if !strings.Contains(noDB, "database_enabled = false") {
		t.Errorf("expected database_enabled = false, got:\n%s", noDB)
	}
	if strings.Contains(noDB, "database_type") {
		t.Errorf("database_type should be omitted when disabled, got:\n%s", noDB)
	}
}

func TestRender_NoEnv(t *testing.T) {
	got := string(Render(templateFixture(), domain.Service{Name: "svc", Replicas: 1})["deploy/values.yaml"])
	if !strings.Contains(got, "env: []") {
		t.Errorf("expected empty env list, got: %q", got)
	}
}

func TestRender_DatabaseEnv(t *testing.T) {
	svc := domain.Service{Name: "billing", Replicas: 1, Database: domain.Database{Enabled: true, Type: "postgres"}}
	got := string(Render(templateFixture(), svc)["deploy/values.yaml"])
	if !strings.Contains(got, "DB_TYPE") || !strings.Contains(got, "postgres") {
		t.Errorf("expected DB env when database enabled, got: %q", got)
	}
}

func TestRender_DockerComposeNoDatabase(t *testing.T) {
	out := Render(templateFixture(), domain.Service{Name: "gateway", Replicas: 1})
	compose, ok := out["docker-compose.yml"]
	if !ok {
		t.Fatal("expected a docker-compose.yml in the generated repo")
	}
	got := string(compose)
	for _, want := range []string{"services:", "app:", "build: .", `"8080:8080"`, `PORT: "8080"`} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in compose, got:\n%s", want, got)
		}
	}
	// No database asked for: the compose must not spin up Postgres nor inject a connection.
	for _, absent := range []string{"postgres:", "DB_HOST", "POSTGRES_", "depends_on"} {
		if strings.Contains(got, absent) {
			t.Errorf("did not expect %q in a no-database compose, got:\n%s", absent, got)
		}
	}
	// The local .env.example is only for services that have a database.
	if _, ok := out[".env.example"]; ok {
		t.Error(".env.example should not be generated when there is no database")
	}
}

func TestRender_DockerComposeWithDatabase(t *testing.T) {
	svc := domain.Service{
		Name: "billing", Replicas: 1,
		Database: domain.Database{Enabled: true, Type: "postgres"},
		Env:      []domain.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
	}
	out := Render(templateFixture(), svc)
	got := string(out["docker-compose.yml"])
	// Mirrors the cluster runtime wiring so /dbz works locally, and orders app after Postgres.
	for _, want := range []string{
		"postgres:",
		"image: postgres:16-alpine",
		"DB_HOST: postgres",
		`DB_PORT: "5432"`,
		"${POSTGRES_DB:-billing}",
		"depends_on:",
		"condition: service_healthy",
		"healthcheck:",
		"LOG_LEVEL",
		"DB_TYPE", // parity with the deploy values (envFor adds it)
		"pgdata:/var/lib/postgresql/data",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in db compose, got:\n%s", want, got)
		}
	}
	// The database intent must produce a local .env.example (documented, git-ignored real .env).
	env, ok := out[".env.example"]
	if !ok {
		t.Fatal("expected a .env.example when the service has a database")
	}
	for _, want := range []string{"POSTGRES_DB=billing", "POSTGRES_USER=postgres", "POSTGRES_PASSWORD=postgres"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("expected %q in .env.example, got:\n%s", want, string(env))
		}
	}
}

func testAppCfg() AppConfig {
	return AppConfig{
		Org:           "atlas-platform-tcc",
		TemplatesRepo: "atlas-templates",
		TemplatesRef:  "main",
		ChartPath:     "charts/base",
		GitopsRepo:    "atlas-gitops",
		GitopsRef:     "main",
	}
}

func TestRenderApplication(t *testing.T) {
	got := RenderApplication(domain.Service{Name: "payments", Replicas: 3}, testAppCfg())
	for _, want := range []string{
		"kind: Application",
		"name: payments",
		"namespace: payments",
		"https://github.com/atlas-platform-tcc/atlas-templates.git",
		"path: charts/base",
		"$values/apps/payments/values.yaml",
		"https://github.com/atlas-platform-tcc/atlas-gitops.git",
		"ref: values",
		"selfHeal: true",
		"resources-finalizer.argocd.argoproj.io",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in Application, got:\n%s", want, got)
		}
	}
	// The namespace is owned by atlas-infra (Terraform); the Application must NOT
	// create it, so infra/GitOps merge order stays independent.
	if strings.Contains(got, "CreateNamespace") {
		t.Errorf("Application must not create the namespace, got:\n%s", got)
	}
}

func TestRenderGitopsValues_BaseDeploy(t *testing.T) {
	// The base deploy pins the initial-commit image and runs the requested replicas.
	img := Image{Repository: "ghcr.io/atlas-platform-tcc/payments", Tag: "abc123"}
	got := RenderGitopsValues(domain.Service{Name: "payments", Replicas: 3}, img, "127.0.0.1.nip.io")
	for _, want := range []string{
		"replicaCount: 3",
		"image:",
		"repository: ghcr.io/atlas-platform-tcc/payments",
		`tag: "abc123"`,
		"ingress:",
		"host: payments.127.0.0.1.nip.io",
		"env: []",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in gitops values, got:\n%s", want, got)
		}
	}
	// No database asked for: the chart's database wiring must stay off.
	if strings.Contains(got, "database:") {
		t.Errorf("did not expect a database block, got:\n%s", got)
	}
}

func TestRenderGitopsValues_EnvAndDatabase(t *testing.T) {
	svc := domain.Service{
		Name: "orders", Replicas: 2,
		Database: domain.Database{Enabled: true, Type: "postgres"},
		Env:      []domain.EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
	}
	got := RenderGitopsValues(svc, Image{Repository: "ghcr.io/atlas-platform-tcc/orders", Tag: "deadbeef"}, "127.0.0.1.nip.io")
	for _, want := range []string{"LOG_LEVEL", "info", "DB_TYPE", "postgres", "tag: \"deadbeef\"",
		"database:\n  enabled: true", "host: orders.127.0.0.1.nip.io"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in gitops values, got:\n%s", want, got)
		}
	}
}
