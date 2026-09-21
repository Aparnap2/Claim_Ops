# GCW Emulator — Local Topology

Reproduces Google Cloud Workflows (GCW) durable coordination locally without `docker-compose`.

Repo rule: **no `docker-compose` files**. Use `docker run --network claimops-net`.

## Network

```bash
docker network create claimops-net
# inspect
docker network inspect claimops-net
```

All services join this network so DNS names resolve (`gcw-emulator`, `api`, `agent`, `localgcp`, `claimops-mockoon`).

## GCW Emulator

Image: `ghcr.io/lemonberrylabs/gcw-emulator:latest` (v0.5.0)
Ports: `8787` HTTP (REST), `8788` gRPC

```bash
docker run -d \
  --name gcw-emulator \
  --network claimops-net \
  -p 8787:8787 \
  -p 8788:8788 \
  -v $(pwd)/workflows:/workflows \
  -e WORKFLOWS_DIR=/workflows \
  -e PROJECT=my-project \
  -e LOCATION=us-central1 \
  ghcr.io/lemonberrylabs/gcw-emulator:latest
```

Verify:

```bash
curl -s http://localhost:8787/v1/projects/my-project/locations/us-central1/workflows | jq
docker logs gcw-emulator
```

Stop / remove:

```bash
docker rm -f gcw-emulator
docker network rm claimops-net
```

## Workflow Deployment

The workflow YAML is `workflows/claim-investigation.yaml`.

### Via GCWProvider (Go)

```go
cfg, _ := config.Load()
provider := workflow.NewGCWProvider(cfg.WorkflowsEmulatorHost, cfg.WorkflowsProject, cfg.WorkflowsLocation)
src, _ := os.ReadFile("workflows/claim-investigation.yaml")
_ = provider.DeployWorkflow(ctx, "claim-investigation", string(src))
```

### Via curl (REST)

Deploy (creates or updates):

```bash
curl -s -X POST \
  "http://localhost:8787/v1/projects/my-project/locations/us-central1/workflows?workflowId=claim-investigation" \
  -H "Content-Type: application/json" \
  -d "{\"sourceContents\": $(jq -Rs . < workflows/claim-investigation.yaml)}" | jq
```

List workflows:

```bash
curl -s http://localhost:8787/v1/projects/my-project/locations/us-central1/workflows | jq
```

## Execution

### Start execution

```bash
curl -s -X POST \
  "http://localhost:8787/v1/projects/my-project/locations/us-central1/workflows/claim-investigation/executions" \
  -H "Content-Type: application/json" \
  -d '{"argument": "{\"claim_id\":\"c1\",\"tenant_id\":\"t1\",\"investigation_id\":\"inv1\"}"}' | jq
# -> {"name":"projects/my-project/locations/us-central1/workflows/claim-investigation/executions/<id>"}
```

### Get execution status

```bash
EXEC="projects/my-project/locations/us-central1/workflows/claim-investigation/executions/<id>"
curl -s "http://localhost:8787/v1/${EXEC}" | jq
# states: ACTIVE, SUCCEEDED, FAILED
```

### Callback (HITL)

Workflow waits via `events.await_callback`. The emulator exposes:

```bash
# workflow created callback endpoint; emulator returns callback ID in execution events.
# Simulate human decision:
curl -s -X POST "http://localhost:8787/callbacks/<callbackId>" \
  -H "Content-Type: application/json" \
  -d '{"action":"approve","reason":"verified","actor":"reviewer@example.com"}' | jq
```

Through GCWProvider:

```go
_ = provider.SendCallback(ctx, callbackID, map[string]string{"action":"approve"})
```

## Environment

| Variable | Default | Purpose |
|----------|---------|---------|
| `WORKFLOWS_EMULATOR_HOST` | `""` (provider defaults to `http://localhost:8787` when used) | GCW host; may be `localhost:8787` or `http://gcw-emulator:8787` inside Docker network |
| `WORKFLOWS_PROJECT` | `my-project` | GCW project |
| `WORKFLOWS_LOCATION` | `us-central1` | GCW location |
| `AGENT_URL` | `http://localhost:8081` | Agent service URL (workflow `http.post` to `/v1/investigations`) |
| `API_URL` | `http://localhost:8000` | API URL (workflow `http.post` to `/v1/claims/{id}/decision`) |

Inside containers set `WORKFLOWS_EMULATOR_HOST=gcw-emulator:8787`, `AGENT_URL=http://agent:8081`, `API_URL=http://api:8000`.

Local dev without Docker uses defaults (`localhost`).

## Tests

No real GCW required for unit tests:

```bash
go vet ./...
gofmt -l ./...
go test ./internal/workflow/... -count=1 -v
```

Provider tests use `httptest` mock GCW server.

## Troubleshooting

- `connection refused` → ensure container is on `claimops-net` and port `8787` is published.
- `workflow not found` → deploy first; check `WORKFLOWS_DIR` mount.
- `callback not found` → execution must be in `ACTIVE` waiting state.
