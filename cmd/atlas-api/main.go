// Command atlas-api starts the Atlas HTTP API.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/atlas-platform-tcc/atlas-api/internal/argocd"
	"github.com/atlas-platform-tcc/atlas-api/internal/config"
	"github.com/atlas-platform-tcc/atlas-api/internal/conformance"
	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
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

	// One policy for both the creation gate and the day-2 check endpoint.
	policy, err := conformance.Load(os.Getenv("ATLAS_POLICY_FILE"))
	if err != nil {
		log.Fatalf("load conformance policy: %v", err)
	}
	if mode := os.Getenv("ATLAS_POLICY_MODE"); mode != "" {
		policy.Mode = mode // "audit" (default) | "enforce"
	}
	srv := httpapi.NewServer(st).WithPolicy(policy)

	// Provisioning (repository creation) is enabled when a token is provided.
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		org := getenv("ATLAS_ORG", "atlas-platform-tcc")
		templatesRepo := getenv("ATLAS_TEMPLATES_REPO", "atlas-templates")
		templatesRef := getenv("ATLAS_TEMPLATES_REF", "main")
		gitopsRepo := getenv("ATLAS_GITOPS_REPO", "atlas-gitops")
		gitClient := &gitprovider.GitHub{Org: org, Token: token}
		srv = srv.WithProvisioner(&provisioner.Provisioner{
			Templates: &templatesrc.GitHub{
				Owner:  org,
				Repo:   templatesRepo,
				Ref:    templatesRef,
				SubDir: getenv("ATLAS_TEMPLATE_DIR", "go-service-template"),
				Token:  token,
			},
			Git:        gitClient,
			InfraRepo:  getenv("ATLAS_INFRA_REPO", "atlas-infra"),
			GitopsRepo: gitopsRepo,
			AppCfg: generator.AppConfig{
				Org:           org,
				TemplatesRepo: templatesRepo,
				TemplatesRef:  templatesRef,
				ChartPath:     getenv("ATLAS_CHART_PATH", "charts/base"),
				GitopsRepo:    gitopsRepo,
				GitopsRef:     getenv("ATLAS_GITOPS_REF", "main"),
				IngressDomain: getenv("ATLAS_INGRESS_DOMAIN", "127.0.0.1.nip.io"),
			},
			Policy: policy,
		})
		srv = srv.WithVerifier(gitClient).WithApprovedRegistry(provisioner.ApprovedImagePrefix(org))
		log.Printf("provisioning enabled (org %s, policy mode %s)", org, policyMode(policy.Mode))
	} else {
		log.Printf("GITHUB_TOKEN not set — provisioning disabled (register-only)")
	}

	// Optional: without Argo CD, the catalog serves the projected (git) state only
	// (ADR atlas-api/0012).
	if addr := os.Getenv("ARGOCD_ADDR"); addr != "" {
		if token := os.Getenv("ARGOCD_TOKEN"); token != "" {
			insecure := getenv("ARGOCD_INSECURE", "true") == "true"
			client := argocd.New(addr, token, insecure)
			srv = srv.WithObserver(client)
			log.Printf("observed state enabled (argocd %s)", addr)

			// Turns Argo CD status transitions into audit events (ADR atlas-api/0017).
			interval, err := time.ParseDuration(getenv("ATLAS_OBSERVE_INTERVAL", "30s"))
			if err != nil {
				interval = 30 * time.Second
			}
			poller := argocd.NewPoller(client.AppStatuses, func(service, evType, outcome, detail string) {
				_ = st.AppendEvent(context.Background(), domain.Event{
					Service: service, Type: evType, Outcome: outcome, Actor: "argocd-observer",
					Detail: detail, At: time.Now().UTC().Format(time.RFC3339),
				})
			}, interval)
			go poller.Run(context.Background())
			log.Printf("argocd event poller running (every %s)", interval)
		} else {
			log.Printf("ARGOCD_ADDR set but ARGOCD_TOKEN missing — observed state disabled")
		}
	}

	addr := ":" + getenv("PORT", "8081")
	log.Printf("atlas-api listening on %s (catalog: %s)", addr, dsn)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func policyMode(m string) string {
	if m == "" {
		return "audit"
	}
	return m
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
