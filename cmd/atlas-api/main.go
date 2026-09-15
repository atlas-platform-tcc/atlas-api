// Command atlas-api starts the Atlas HTTP API.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/atlas-platform-tcc/atlas-api/internal/config"
	"github.com/atlas-platform-tcc/atlas-api/internal/generator"
	"github.com/atlas-platform-tcc/atlas-api/internal/gitprovider"
	"github.com/atlas-platform-tcc/atlas-api/internal/httpapi"
	"github.com/atlas-platform-tcc/atlas-api/internal/provisioner"
	"github.com/atlas-platform-tcc/atlas-api/internal/store/sqlite"
	"github.com/atlas-platform-tcc/atlas-api/internal/templatesrc"
)

func main() {
	// Load .env (real environment variables take precedence).
	if err := config.LoadDotenv(getenv("ATLAS_ENV_FILE", ".env")); err != nil {
		log.Printf("load .env: %v", err)
	}

	dsn := getenv("ATLAS_DB", "atlas.db")
	st, err := sqlite.Open(dsn)
	if err != nil {
		log.Fatalf("open catalog: %v", err)
	}
	defer st.Close()

	srv := httpapi.NewServer(st)

	// Provisioning (repository creation) is enabled when a token is provided.
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		org := getenv("ATLAS_ORG", "atlas-platform-tcc")
		templatesRepo := getenv("ATLAS_TEMPLATES_REPO", "atlas-templates")
		templatesRef := getenv("ATLAS_TEMPLATES_REF", "main")
		gitopsRepo := getenv("ATLAS_GITOPS_REPO", "atlas-gitops")
		srv = srv.WithProvisioner(&provisioner.Provisioner{
			Templates: &templatesrc.GitHub{
				Owner:  org,
				Repo:   templatesRepo,
				Ref:    templatesRef,
				SubDir: getenv("ATLAS_TEMPLATE_DIR", "go-service-template"),
				Token:  token,
			},
			Git:        &gitprovider.GitHub{Org: org, Token: token},
			InfraRepo:  getenv("ATLAS_INFRA_REPO", "atlas-infra"),
			GitopsRepo: gitopsRepo,
			AppCfg: generator.AppConfig{
				Org:           org,
				TemplatesRepo: templatesRepo,
				TemplatesRef:  templatesRef,
				ChartPath:     getenv("ATLAS_CHART_PATH", "charts/base"),
				GitopsRepo:    gitopsRepo,
				GitopsRef:     getenv("ATLAS_GITOPS_REF", "main"),
			},
		})
		log.Printf("provisioning enabled (org %s)", org)
	} else {
		log.Printf("GITHUB_TOKEN not set — provisioning disabled (register-only)")
	}

	addr := ":" + getenv("PORT", "8080")
	log.Printf("atlas-api listening on %s (catalog: %s)", addr, dsn)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
