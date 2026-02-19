# AIBrix Chat

A multi-modal chat portal for interacting with LLM models deployed on AIBrix. Supports text chat with SSE streaming, with image and video generation planned.

```
┌─────────────┐       ┌─────────────────┐      ┌──────────────────┐
│  Frontend   │ SSE   │   BFF (FastAPI) │ HTTP │  AIBrix Gateway  │
│  React SPA  ├──────►│   apps/chat/api ├─────►│  + Model Pods    │
│  :5173      │       │   :8000         │      │  :8888           │
└─────────────┘       └─────────────────┘      └──────────────────┘
```

## Prerequisites

- Node.js 18+
- Python 3.10+
- (Optional) An AIBrix gateway running at `http://localhost:8888`

## Quick Start

### 1. Start the API server

```bash
cd apps/chat/api
pip install -r requirements.txt
python -m uvicorn main:app --reload --port 8000
```

The API server runs at http://localhost:8000. Interactive docs at http://localhost:8000/api/docs.

### 2. Start the frontend

```bash
cd apps/chat/web
npm install
npm run dev
```

The frontend runs at http://localhost:5173.

### 3. (Optional) Point to an AIBrix gateway

```bash
# Set the gateway URL before starting the API server
export AIBRIX_GATEWAY_URL=http://<your-gateway>:8888
export API_KEY=<your-api-key>  # if auth is required
python -m uvicorn main:app --reload --port 8000
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/health` | Health check |
| `GET` | `/api/models` | List available models |
| `POST` | `/api/conversations` | Create conversation |
| `GET` | `/api/conversations` | List conversations |
| `GET` | `/api/conversations/{id}` | Get conversation with messages |
| `PATCH` | `/api/conversations/{id}` | Update title |
| `DELETE` | `/api/conversations/{id}` | Delete conversation |
| `POST` | `/api/conversations/{id}/completions` | Send message + stream response (SSE) |
| `POST` | `/api/image/generate` | Image generation (stub) |
| `POST` | `/api/video/generate` | Video generation (stub) |

## Configuration

Environment variables for the API server:

| Variable | Description | Default |
|---|---|---|
| `AIBRIX_GATEWAY_URL` | AIBrix gateway base URL | `http://localhost:8888` |
| `API_KEY` | API key for gateway auth | (empty) |
| `CORS_ORIGINS` | Allowed CORS origins (comma-separated) | `http://localhost:5173` |

## Project Structure

```
apps/chat/
├── api/                     # Python FastAPI backend (BFF)
│   ├── main.py              # App entry point
│   ├── config.py            # Settings
│   ├── models/schemas.py    # Pydantic models
│   ├── routers/             # API route handlers
│   │   ├── chat.py          # SSE streaming completions
│   │   ├── conversations.py # Conversation CRUD
│   │   ├── models.py        # Model discovery
│   │   ├── images.py        # Image generation (stub)
│   │   └── video.py         # Video generation (stub)
│   └── services/            # Business logic
│       ├── conversation.py  # In-memory conversation store
│       └── gateway.py       # AIBrix gateway HTTP client
├── web/                     # React + Vite + Tailwind frontend
│   ├── src/app/components/  # UI components
│   └── package.json
└── API_SPEC.md              # Full API specification
```

## Guidelines

See [web/GUIDELINES.md](./web/GUIDELINES.md) for AI and design system guidelines.

## Attributions

This project includes components from [shadcn/ui](https://ui.shadcn.com/) under [MIT license](https://github.com/shadcn-ui/ui/blob/main/LICENSE.md).

This project includes photos from [Unsplash](https://unsplash.com) under [Unsplash license](https://unsplash.com/license).
