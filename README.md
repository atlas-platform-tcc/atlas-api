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

## API

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/services` | Validate the intent and register the service |
| `GET` | `/services` | List services in the catalog |
| `GET` | `/services/{name}` | Get one service |
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
list of `{ field, reason }`; a duplicate name returns `409`.

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

curl -s -X POST localhost:8080/services \
  -d '{"name":"payments","language":"go","replicas":2}'
curl -s localhost:8080/services
```

Without a `GITHUB_TOKEN` the API runs in register-only mode. Variables: `GITHUB_TOKEN`,
`PORT` (8080), `ATLAS_DB` (`atlas.db`), `ATLAS_ORG` (`atlas-platform-tcc`),
`ATLAS_TEMPLATES_REPO`/`ATLAS_TEMPLATES_REF`/`ATLAS_TEMPLATE_DIR`, `ATLAS_INFRA_REPO` (`atlas-infra`),
`ATLAS_GITOPS_REPO` (`atlas-gitops`), `ATLAS_GITOPS_REF` (`main`), `ATLAS_CHART_PATH` (`charts/base`),
`ATLAS_ENV_FILE` (`.env`).

## Test

```bash
go mod tidy   # first time, to fetch dependencies
go test ./...
```
