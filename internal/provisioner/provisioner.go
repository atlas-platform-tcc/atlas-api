// Package provisioner turns a validated intent into an application repository and
// an infrastructure Pull Request.
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/atlas-platform-tcc/atlas-api/internal/conformance"
	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
	"github.com/atlas-platform-tcc/atlas-api/internal/generator"
)

// TemplateSource provides the service template files.
type TemplateSource interface {
	Fetch(ctx context.Context) (generator.Files, error)
}

// GitClient creates repositories and opens Pull Requests on the Git provider.
type GitClient interface {
	// CreateRepo creates the repository and returns its URL and the initial commit SHA.
	CreateRepo(ctx context.Context, name string, files generator.Files) (repoURL, commitSHA string, err error)
	// ProtectBranch enables branch protection on the repository's branch (ADR atlas-api/0019).
	ProtectBranch(ctx context.Context, repo, branch string) error
	OpenPR(ctx context.Context, repo, branch string, files generator.Files, title, body, base string) (string, error)
	// OpenDeletePR opens a Pull Request removing the given paths, returning an empty
	// URL (no error) when none of them exist on base (idempotent no-op).
	OpenDeletePR(ctx context.Context, repo, branch string, paths []string, title, body, base string) (string, error)
	// ArchiveRepo marks the repository read-only (reversible, preserves history).
	ArchiveRepo(ctx context.Context, name string) error
	// DeleteRepo permanently deletes the repository (irreversible; force path only).
	DeleteRepo(ctx context.Context, name string) error
	// DeletePackage removes the service's container package so an orphan cannot
	// collide with a future same-named service. A missing package is a no-op.
	DeletePackage(ctx context.Context, name string) error
}

// Registry is the container registry the generated CI pushes to, used to pin the
// base image in the GitOps values (matches the template pipeline: ghcr.io/<org>/<repo>).
const Registry = "ghcr.io"

// Provisioner composes template fetching/rendering, repository creation, the
// infrastructure Pull Request and the GitOps Pull Request.
type Provisioner struct {
	Templates  TemplateSource
	Git        GitClient
	InfraRepo  string              // e.g. "atlas-infra"
	GitopsRepo string              // e.g. "atlas-gitops"
	AppCfg     generator.AppConfig // repository coordinates for the Argo CD Application
	Policy     conformance.Policy  // conformance rule set evaluated at provisioning (ADR atlas-api/0013)
}

// Result carries the URLs produced by a successful provisioning run.
type Result struct {
	RepoURL     string
	InfraPRURL  string
	GitopsPRURL string
	URL         string // service URL once merged, e.g. http://<name>.127.0.0.1.nip.io
	Conformance domain.ConformanceReport
}

// Provision creates the application repository and opens the infrastructure and
// GitOps Pull Requests. None of the PRs is merged automatically — the human
// merge is the gate (see ADR atlas-api/0006).
func (p *Provisioner) Provision(ctx context.Context, svc domain.Service) (Result, error) {
	var res Result

	tmpl, err := p.Templates.Fetch(ctx)
	if err != nil {
		return res, err
	}
	var commitSHA string
	res.RepoURL, commitSHA, err = p.Git.CreateRepo(ctx, svc.Name, generator.Render(tmpl, svc))
	if err != nil {
		return res, err
	}

	// The service is born with a protected main (ADR atlas-api/0019). A permission failure
	// only logs: the day-2 rule catches it, so it must not abort the whole flow.
	if err := p.Git.ProtectBranch(ctx, svc.Name, "main"); err != nil {
		log.Printf("protect branch main for %s: %v (branch protection not enforced — token needs repo admin)", svc.Name, err)
	}

	// Score conformance BEFORE opening any PR, so a non-conformant service is refused at
	// the gate instead of reaching review (ADR atlas-api/0016). Scoring the artifacts
	// actually generated means a generator regression lowers the score (ADR 0013).
	img := generator.Image{
		Repository: fmt.Sprintf("%s/%s/%s", Registry, p.AppCfg.Org, svc.Name),
		Tag:        commitSHA,
	}
	appYAML := generator.RenderApplication(svc, p.AppCfg)
	valuesYAML := generator.RenderGitopsValues(svc, img, p.AppCfg.IngressDomain)
	pol := p.Policy.ForPhase(conformance.PhaseCreate)
	res.Conformance = conformance.Evaluate(pol, p.facts(svc, img, tmpl, appYAML, valuesYAML))
	if ok, reasons := pol.Gate(res.Conformance); !ok {
		return res, fmt.Errorf("%w: %s", ErrConformanceGate, strings.Join(reasons, "; "))
	}

	// Infrastructure PR (namespace + optional database) in atlas-infra.
	infraFiles := generator.Files{
		"services/svc-" + svc.Name + ".tf": []byte(generator.RenderServiceTF(svc)),
	}
	res.InfraPRURL, err = p.Git.OpenPR(ctx, p.InfraRepo, "provision-"+svc.Name, infraFiles,
		"Provision infrastructure for "+svc.Name, infraPRBody(svc), "main")
	if err != nil {
		return res, err
	}

	// GitOps PR (Argo CD Application + per-service values with the base image pinned)
	// in atlas-gitops. The base image is the one CI builds from the initial commit.
	gitopsFiles := generator.Files{
		"apps/" + svc.Name + "/application.yaml": []byte(appYAML),
		"apps/" + svc.Name + "/values.yaml":      []byte(valuesYAML),
	}
	res.GitopsPRURL, err = p.Git.OpenPR(ctx, p.GitopsRepo, "gitops-"+svc.Name, gitopsFiles,
		"Add GitOps skeleton for "+svc.Name, gitopsPRBody(svc, res.Conformance), "main")
	if err != nil {
		return res, err
	}

	res.URL = "http://" + generator.ServiceHost(svc.Name, p.AppCfg.IngressDomain)
	return res, nil
}

// ErrConformanceGate is returned by Provision when the generated service does not
// satisfy the policy gate (ADR atlas-api/0016). No Pull Request is opened.
var ErrConformanceGate = errors.New("conformance gate not satisfied")

// facts extracts the observations the policy asserts over from the generated artifacts.
func (p *Provisioner) facts(svc domain.Service, img generator.Image, tmpl generator.Files, appYAML, valuesYAML string) conformance.Facts {
	_, hasCI := tmpl[".github/workflows/ci.yml"]
	_, hasDeploy := tmpl[".github/workflows/deploy.yml"]
	_, hasReadme := tmpl["README.md"]
	approvedPrefix := ApprovedImagePrefix(p.AppCfg.Org)
	return conformance.Facts{
		"chart.base":                  p.AppCfg.ChartPath == "charts/base",
		"image.pinned":                img.Tag != "",
		"image.approvedRegistry":      strings.HasPrefix(img.Repository, approvedPrefix),
		"pipeline.present":            hasCI && hasDeploy,
		"template.hasReadme":          hasReadme,
		"env.hasSecretLike":           hasSecretLikeEnv(svc.Env),
		"replicas":                    svc.Replicas,
		"resources.totalCpuMillis":    svc.Replicas * limCPUm,
		"application.finalizer":       strings.Contains(appYAML, "resources-finalizer.argocd.argoproj.io"),
		"application.createNamespace": strings.Contains(appYAML, "CreateNamespace"),
		"database.requested":          svc.Database.Enabled,
		"database.wired":              strings.Contains(valuesYAML, "database:\n  enabled: true"),
	}
}

// secretNameRe flags env var names suggesting a secret value, which must never reach Git.
var secretNameRe = regexp.MustCompile(`(?i)(password|secret|token|apikey|api[_-]?key|private[_-]?key|credential)`)

func hasSecretLikeEnv(env []domain.EnvVar) bool {
	for _, e := range env {
		if e.Value == "" {
			continue
		}
		if secretNameRe.MatchString(e.Name) || strings.Contains(e.Value, "-----BEGIN") {
			return true
		}
	}
	return false
}

// DeprovisionResult carries the URLs produced by a successful deprovisioning run.
// A PR URL is empty when that repository had nothing to remove (idempotent no-op).
type DeprovisionResult struct {
	GitopsPRURL    string
	InfraPRURL     string
	RepoArchived   bool
	RepoDeleted    bool
	PackageDeleted bool
}

// Deprovision is the inverse of Provision: it opens PRs removing the service from
// atlas-gitops and atlas-infra, deletes its container package and disposes of its
// repository (archived, or deleted when force is set). No PR is merged automatically —
// the human merge is the gate (ADR atlas-api/0011).
func (p *Provisioner) Deprovision(ctx context.Context, name string, force bool) (DeprovisionResult, error) {
	var res DeprovisionResult

	// Once merged, the app-of-apps prunes the Application, whose cascade-deletion
	// finalizer removes the workloads (ADR atlas-api/0007).
	gitopsPaths := []string{
		"apps/" + name + "/application.yaml",
		"apps/" + name + "/values.yaml",
	}
	gitopsPRURL, err := p.Git.OpenDeletePR(ctx, p.GitopsRepo, "deprovision-"+name, gitopsPaths,
		"Remove GitOps state for "+name, deprovisionGitopsPRBody(name), "main")
	if err != nil {
		return res, err
	}
	res.GitopsPRURL = gitopsPRURL

	// Once merged, Terraform destroys the namespace, the ghcr-pull secret and the database.
	infraPaths := []string{"services/svc-" + name + ".tf"}
	infraPRURL, err := p.Git.OpenDeletePR(ctx, p.InfraRepo, "deprovision-"+name, infraPaths,
		"Remove infrastructure for "+name, deprovisionInfraPRBody(name), "main")
	if err != nil {
		return res, err
	}
	res.InfraPRURL = infraPRURL

	// An orphan package would block recreating the service.
	if err := p.Git.DeletePackage(ctx, name); err != nil {
		return res, err
	}
	res.PackageDeleted = true

	// Archiving is reversible and preserves history; force is for testing/cleanup.
	if force {
		if err := p.Git.DeleteRepo(ctx, name); err != nil {
			return res, err
		}
		res.RepoDeleted = true
	} else {
		if err := p.Git.ArchiveRepo(ctx, name); err != nil {
			return res, err
		}
		res.RepoArchived = true
	}

	return res, nil
}

func deprovisionGitopsPRBody(name string) string {
	return fmt.Sprintf("Remove the GitOps desired state of service **%s** (generated by Atlas).\n\n"+
		"- deletes `apps/%s/` (Argo CD `Application` + per-service values)\n\n"+
		"After merge, the app-of-apps prunes the `Application`; its cascade-deletion finalizer "+
		"removes the workloads from the cluster.", name, name)
}

func deprovisionInfraPRBody(name string) string {
	return fmt.Sprintf("Remove the infrastructure of service **%s** (generated by Atlas).\n\n"+
		"- deletes `services/svc-%s.tf`\n\n"+
		"After merge, Terraform destroys the namespace, the `ghcr-pull` secret and the database (if any).",
		name, name)
}

func infraPRBody(svc domain.Service) string {
	s := fmt.Sprintf("Infrastructure for service **%s** (generated by Atlas).\n\n- namespace `%s`\n", svc.Name, svc.Name)
	if svc.Database.Enabled {
		s += fmt.Sprintf("- PostgreSQL dependency (`%s`)\n", svc.Database.Type)
	}
	s += "\nReview and merge to apply (Terraform runs on the self-hosted runner)."
	return s
}

func gitopsPRBody(svc domain.Service, report domain.ConformanceReport) string {
	return fmt.Sprintf("GitOps desired state for service **%s** (generated by Atlas).\n\n"+
		"- Argo CD `Application` (base chart + per-service values via $values)\n"+
		"- per-service `values.yaml` — **base image pinned to the initial commit, replicaCount %d**\n\n"+
		"After merge, Argo CD reconciles and brings up the base version once the image is built "+
		"and the namespace exists (`atlas-infra`). Later releases go through the day-2 Deploy job.\n\n"+
		"%s",
		svc.Name, svc.Replicas, impactPlan(svc, report))
}

// Base chart resource defaults (mirror atlas-templates/charts/base/values.yaml), used to
// estimate the footprint shown in the PR (ADR atlas-api/0015).
const (
	reqCPUm  = 50
	reqMemMi = 64
	limCPUm  = LimitCPUMillis
	limMemMi = 128
)

// LimitCPUMillis is the per-replica CPU limit (millicores) imposed by the base chart.
// Exported so the day-2 check uses the same budget as the create-phase facts.
const LimitCPUMillis = 200

// ApprovedImagePrefix is the registry+org prefix an image must carry to satisfy the
// approved-registry rule. Shared by the create and day-2 checks so both enforce the
// org, not just the registry.
func ApprovedImagePrefix(org string) string {
	return fmt.Sprintf("%s/%s/", Registry, org)
}

// impactPlan renders the "impact plan" embedded in the provisioning PR: estimated
// resource footprint plus the conformance report (ADR atlas-api/0015).
func impactPlan(svc domain.Service, report domain.ConformanceReport) string {
	var b strings.Builder
	n := svc.Replicas
	b.WriteString("## Plano de impacto (gerado pelo Atlas)\n\n")
	fmt.Fprintf(&b, "**Pegada de recursos** — %d réplica(s):\n", n)
	fmt.Fprintf(&b, "- CPU: requests %dm · limits %dm (total até %dm)\n", reqCPUm, limCPUm, n*limCPUm)
	fmt.Fprintf(&b, "- Memória: requests %dMi · limits %dMi (total até %dMi)\n", reqMemMi, limMemMi, n*limMemMi)
	if svc.Database.Enabled {
		b.WriteString("- Dependência: PostgreSQL dedicado no namespace (provisionado pelo atlas-infra)\n")
	}
	fmt.Fprintf(&b, "\n**Conformidade: %d%%** (%d/%d pontos · %d ok / %d falha[s])\n\n",
		report.Percent, report.Score, report.Max, report.Passed, report.Failed)
	b.WriteString("| Regra | Severidade | OK |\n| --- | --- | --- |\n")
	for _, r := range report.Results {
		mark := "✅"
		if !r.Pass {
			mark = "❌"
		}
		fmt.Fprintf(&b, "| %s — %s | %s | %s |\n", r.ID, r.Description, r.Severity, mark)
	}
	return b.String()
}
