# aiconfigurator GUI

A graphical interface for [aiconfigurator](https://github.com/ai-dynamo/aiconfigurator):
pick a mode, fill in the parameters, and run — view recommended deployment
configurations (parallelism, GPU count, batch size, throughput, latency, memory,
etc.) in the browser without memorizing CLI commands.

This UI is a visual wrapper around the 6 aiconfigurator CLI modes
(default / recommend / estimate / support / generate / exp). For the detailed
functionality and parameter semantics of each mode, see the upstream repository:
<https://github.com/ai-dynamo/aiconfigurator>.

## Python Environment

All computation is done by the aiconfigurator Python SDK; the UI invokes a
Python interpreter to run it:

- **Default**: the repository virtualenv at `<repo>/.venv/bin/python`
- To use a different interpreter, override with the `AIC_PYTHON` environment
  variable:

  ```bash
  AIC_PYTHON=/path/to/python npm start
  ```

## Running

```bash
cd ui
npm start
```

Then open <http://localhost:5199>.

Custom port: `AIC_UI_PORT=8080 npm start`; stop the server with `Ctrl+C`.

## File Layout

```
ui/
├── package.json      # npm start entry point
├── server.js         # Node HTTP server: static pages + /api/run bridge
├── runner.py         # Python bridge: JSON in via stdin -> SDK call -> JSON out via stdout
└── public/           # Frontend (index.html / style.css / app.js)
```

Call chain:

```
Browser ──POST /api/run {mode, params}──▶ server.js
   └─ spawn(python, runner.py); JSON over stdin/stdout
        └─ runner.py calls aiconfigurator.cli SDK functions
```
