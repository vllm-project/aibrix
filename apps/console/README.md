# AIBrix Console

Management console for AIBrix. Consists of a React frontend and a Go gRPC/REST backend.

Design: https://www.figma.com/design/8GNiN53l892dYqaGUt4RJ0/aibrix-portal

## Project Structure

```
apps/console/
├── web/                    # React frontend (Vite + Tailwind + Radix UI)
│   └── src/
├── api/                    # Backend service definitions
│   ├── proto/              # Protobuf service definitions
│   ├── gen/                # Generated gRPC/gateway code
│   ├── handler/            # Service handler implementations
│   ├── server/             # gRPC + HTTP gateway server
│   └── store/              # Data store (in-memory)
cmd/console/main.go         # Backend entry point (in repo root)
```

## Running the Frontend

```bash
cd apps/console/web
npm install
npm run dev
```

Opens at http://localhost:3000.

## Running the Backend

Build from the repo root:

```bash
make build-console
```

Run:

```bash
./bin/console
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--grpc-addr` | `:50060` | gRPC server bind address |
| `--http-addr` | `:8090` | HTTP REST gateway bind address |
| `--gateway-endpoint` | `http://localhost:8888` | AIBrix gateway for playground proxy |

The backend exposes both gRPC (`:50060`) and REST (`:8090`) APIs. The REST API is auto-generated from protobuf via grpc-gateway.

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/models` | List model catalog |
| GET | `/api/v1/models/{id}` | Get model details |
| GET | `/api/v1/deployments` | List deployments |
| GET | `/api/v1/deployments/{id}` | Get deployment |
| POST | `/api/v1/deployments` | Create deployment |
| DELETE | `/api/v1/deployments/{id}` | Delete deployment |
| GET | `/api/v1/jobs` | List batch jobs |
| GET | `/api/v1/jobs/{id}` | Get job |
| POST | `/api/v1/jobs` | Create batch job |
| GET | `/api/v1/apikeys` | List API keys |
| POST | `/api/v1/apikeys` | Create API key |
| DELETE | `/api/v1/apikeys/{id}` | Delete API key |
| GET | `/api/v1/secrets` | List secrets |
| POST | `/api/v1/secrets` | Create secret |
| DELETE | `/api/v1/secrets/{id}` | Delete secret |
| GET | `/api/v1/quotas` | List quotas |
| POST | `/api/v1/playground/chat/completions` | Playground chat (SSE proxy) |

## Regenerating Protobuf Code

After modifying `apps/console/api/proto/`:

```bash
make generate-console-proto
```

Requires [buf](https://buf.build/docs/installation).
