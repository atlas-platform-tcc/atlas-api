# atlas-api

Core of the Atlas Golden Path. Receives a service creation intent over an HTTP/JSON API, validates
it against the contract and platform guardrails, and registers it in a queryable catalog. The
catalog is a projection — **not** the source of truth for the desired state (that is `atlas-gitops`).

When a `GITHUB_TOKEN` is configured, a valid `POST /services` also **provisions**. It:

1. creates the application repository (template from `atlas-templates`, initial commit, guardrails baked in);
2. **opens a Pull Request in `atlas-infra`** with the service's Terraform (namespace, plus PostgreSQL when `database.enabled`);
3. **opens a Pull Request in `atlas-gitops`** with the Argo CD `Application` (multi-source: base chart from `atlas-templates` + per-service values via `$values`) and a `values.yaml` that **pins the base image** (built from the service's initial commit, `tag` = commit SHA) and the requested `replicaCount` — so merging brings up the base version. Later releases go through the day-2 Deploy job.

The API never merges or applies — human review of each PR is the gate. The `Application` does not create
the namespace (owned by `atlas-infra`), keeping the infra/GitOps merge order independent. The response and
catalog expose `repoUrl`, `infraPrUrl` and `gitopsPrUrl`. Without a token the API runs in register-only mode.

`DELETE /services/{name}` **deprovisions** a service — the inverse of provisioning. It opens a Pull
Request removing `apps/<name>/` from `atlas-gitops` (once merged, Argo CD prunes the `Application`, whose
cascade-deletion finalizer removes the workloads) and another removing `services/svc-<name>.tf` from
`atlas-infra` (Terraform destroys the namespace, the `ghcr-pull` secret and the database), then **archives**
the application repository (reversible; history preserved) and **deletes the container package** from GHCR
(so an orphan cannot block recreating the service; needs `delete:packages` on the token). It returns
`202 Accepted` with the teardown PR URLs and drops the catalog row so the name can be recreated — the audit
trail lives in Git. Merge order is indifferent; the human merge is the gate. `DELETE /services/{name}?force=true`
**permanently deletes** the repository instead of archiving it (testing/cleanup; needs `delete_repo` on the token).

## API

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/services` | Validate the intent and register the service |
| `GET` | `/services` | List services in the catalog |
| `GET` | `/services/{name}` | Get one service |
| `DELETE` | `/services/{name}` | Deprovision: teardown PRs + delete package, archive the repo (`202`; `?force=true` deletes the repo) |
| `GET` | `/healthz` | Liveness |

### Contract (`POST /services`)

```json
{
  "name": "payments",
  "language": "go",
  "replicas": 2,
  "database": { "enabled": true, "type": "postgres" },
  "env": [
    { "name": "LOG_LEVEL", "value": "info" },
    { "name": "FEATURE_X", "value": "true" }
  ]
}
```

Guardrails: `name` is a DNS-1123 label; `language` must be `go`; `replicas` is 1–10 (default 1);
`database.type` must be `postgres` when `database.enabled` is true; each `env[].name` is a valid
environment variable name and unique. The provided `env` flows into the generated repo's
`deploy/values.yaml` (plus `DB_TYPE` when a database is enabled). Invalid input returns `422` with a
list of `{ field, reason }`.

**Idempotency** (keyed by name): re-posting an identical spec is a safe replay — `200` (no-op) if the
service is already provisioned, or a resume if a previous run failed partway. The same name with a
*different* spec returns `409`. Git operations are idempotent too (an existing repo/PR is reused, not
duplicated), so the Golden Path can be re-run without diverging.

## Stack

- Go 1.26 (`net/http`)
- SQLite via `modernc.org/sqlite` (pure Go, no CGO)

## Run

Configuration comes from environment variables, optionally loaded from a `.env` file at startup
(real environment variables take precedence; `.env` is gitignored). Copy `.env.example` to `.env` and
set `GITHUB_TOKEN`:

```bash
cp .env.example .env      # then edit .env and set GITHUB_TOKEN

go run ./cmd/atlas-api    # loads .env; provisioning enabled when GITHUB_TOKEN is set

curl -s -X POST localhost:8081/services \
  -d '{"name":"payments","language":"go","replicas":2}'
curl -s localhost:8081/services
```

Without a `GITHUB_TOKEN` the API runs in register-only mode. Variables: `GITHUB_TOKEN`,
`PORT` (8081), `ATLAS_DB` (`atlas.db`), `ATLAS_ORG` (`atlas-platform-tcc`),
`ATLAS_TEMPLATES_REPO`/`ATLAS_TEMPLATES_REF`/`ATLAS_TEMPLATE_DIR`, `ATLAS_INFRA_REPO` (`atlas-infra`),
`ATLAS_GITOPS_REPO` (`atlas-gitops`), `ATLAS_GITOPS_REF` (`main`), `ATLAS_CHART_PATH` (`charts/base`),
`ATLAS_INGRESS_DOMAIN` (`127.0.0.1.nip.io` — the wildcard domain each service host is derived from),
`ATLAS_ENV_FILE` (`.env`).

Optional observed state (ADR atlas-api/0012): set `ARGOCD_ADDR` (e.g. `https://localhost:8083`) and
`ARGOCD_TOKEN` (a read-only Argo CD token) to have `GET /services` and `GET /services/{name}` attach
each service's `sync`/`health`. `ARGOCD_INSECURE` (`true`) skips TLS verification for the default
self-signed Argo CD server. Unset → the catalog serves the projected (git) state only.

## Estado observável (Argo CD)

A API pode anexar aos serviços o **status do Argo CD** (`sync`/`health`), ao lado do status da
aplicação mantido no catálogo. É **opcional e somente-leitura** — o Git continua a fonte da verdade
(ADR atlas-api/0012). Passos para habilitar num cluster local:

A API precisa apenas de leitura. Em vez de emitir um token na conta `admin`, cria-se uma
conta dedicada `atlas` com RBAC somente-leitura sobre as `Applications`:

```bash
# 1. Habilitar a conta 'atlas' com acesso via apiKey (argocd-cm):
kubectl -n argocd patch configmap argocd-cm --type merge \
  -p '{"data":{"accounts.atlas":"apiKey"}}'

# 2. Conceder RBAC somente-leitura à conta (argocd-rbac-cm):
kubectl -n argocd patch configmap argocd-rbac-cm --type merge -p '{"data":{"policy.csv":"p, role:atlas-ro, applications, get, */*, allow\np, role:atlas-ro, applications, list, */*, allow\ng, atlas, role:atlas-ro"}}'

# 3. Reiniciar o argocd-server para aplicar as mudanças de conta/RBAC:
kubectl -n argocd rollout restart deploy/argocd-server
kubectl -n argocd rollout status  deploy/argocd-server

# 4. Expor a API do Argo CD (que roda dentro do kind). Deixe rodando em outro terminal:
kubectl -n argocd port-forward svc/argocd-server 8083:443

# 5. Autenticar como admin e gerar um token para a conta 'atlas':
#    (--expires-in aceita uma duração; 8760h = 1 ano; 0 = nunca expira)
PW=$(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)
argocd login localhost:8083 --insecure --username admin --password "$PW"
TOKEN=$(argocd account generate-token --account atlas --expires-in 8760h)
```

```bash
# 6. Configurar o .env (gitignored — nunca commitar o token):
#    ARGOCD_ADDR=https://localhost:8083
#    ARGOCD_TOKEN=<valor de $TOKEN>
#    ARGOCD_INSECURE=true
# 7. Reiniciar a API; o log confirma: "observed state enabled (argocd https://localhost:8083)".
# 8. Verificar (serviço cuja Application já existe no cluster, i.e. após o merge do PR gitops):
curl -s localhost:8081/services/<name> | jq '{sync,health}'   # → {"sync":"Synced","health":"Healthy"}
```

Notas: `sync`/`health` só aparecem **após o merge do PR gitops** (antes a `Application` não existe →
404 → campos vazios, sem erro); mantenha o `port-forward` de pé (se cair, a API degrada e serve só
o estado projetado); o token da conta `atlas` expira conforme `--expires-in` (aqui, 1 ano).

## Test

```bash
go mod tidy   # first time, to fetch dependencies
go test ./...
```
