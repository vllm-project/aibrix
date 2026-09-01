// aiconfigurator UI server — zero external dependencies (Node built-ins only).
//   npm start          ->  http://localhost:5199
//
// Responsibilities:
//   1. Serve the static frontend from ./public
//   2. Bridge browser requests to the Python SDK via runner.py (JSON over stdio)
//
// Python interpreter resolution:
//   $AIC_PYTHON (custom)  ->  <repo>/.venv/bin/python (default)  ->  python3 on PATH (fallback)

const http = require('http');
const fs = require('fs');
const path = require('path');
const { spawnSync, spawn } = require('child_process');

const PORT = process.env.AIC_UI_PORT ? parseInt(process.env.AIC_UI_PORT, 10) : 5199;
const UI_DIR = __dirname;
const PUBLIC_DIR = path.join(UI_DIR, 'public');
const RUNNER = path.join(UI_DIR, 'runner.py');
const REPO_ROOT = path.join(UI_DIR, '..');
const RUN_TIMEOUT_MS = 10 * 60 * 1000;

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'application/javascript; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
};

function log(msg) {
  console.log(`[aic-ui] ${msg}`);
}

function candidatePythons() {
  const cands = [];
  // 1. Explicit override
  if (process.env.AIC_PYTHON) cands.push(process.env.AIC_PYTHON);
  // 2. Default: repo virtualenv
  cands.push(path.join(REPO_ROOT, '.venv', 'bin', 'python'));
  // 3. Last-resort fallback
  cands.push('python3');
  cands.push('python');
  return cands;
}

function probePython(exe) {
  try {
    const r = spawnSync(exe, ['-c', 'import aiconfigurator.cli; print("ok")'], {
      encoding: 'utf8', timeout: 30000,
    });
    return r.status === 0 && r.stdout.includes('ok');
  } catch (_) {
    return false;
  }
}

let PYTHON = null;
function resolvePython() {
  for (const cand of candidatePythons()) {
    if (probePython(cand)) {
      log(`using python: ${cand}`);
      return cand;
    }
  }
  return null;
}

function sendJson(res, code, obj) {
  const body = JSON.stringify(obj);
  res.writeHead(code, { 'Content-Type': 'application/json; charset=utf-8' });
  res.end(body);
}

function serveStatic(req, res) {
  let urlPath = decodeURIComponent(req.url.split('?')[0]);
  if (urlPath === '/') urlPath = '/index.html';
  const filePath = path.normalize(path.join(PUBLIC_DIR, urlPath));
  if (!filePath.startsWith(PUBLIC_DIR)) {
    res.writeHead(403); res.end('forbidden'); return;
  }
  fs.readFile(filePath, (err, data) => {
    if (err) { res.writeHead(404); res.end('not found'); return; }
    res.writeHead(200, { 'Content-Type': MIME[path.extname(filePath)] || 'application/octet-stream' });
    res.end(data);
  });
}

function runRunner(payload) {
  return new Promise((resolve) => {
    const child = spawn(PYTHON, [RUNNER], { cwd: UI_DIR });
    let stdout = '';
    let stderr = '';
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      resolve({ ok: false, error: `runner timed out after ${RUN_TIMEOUT_MS / 1000}s`, stderr: stderr.slice(-4000) });
    }, RUN_TIMEOUT_MS);

    child.stdout.on('data', (d) => { stdout += d; });
    child.stderr.on('data', (d) => { stderr += d; });
    child.on('close', (code) => {
      clearTimeout(timer);
      if (code !== 0) {
        resolve({ ok: false, error: `python runner exited with code ${code}`, stderr: stderr.slice(-8000) });
        return;
      }
      const marker = stdout.lastIndexOf('\n__AIC_UI_JSON__\n');
      if (marker < 0) {
        resolve({ ok: false, error: 'runner produced no JSON result', stderr: stderr.slice(-4000), stdout: stdout.slice(-4000) });
        return;
      }
      try {
        resolve(JSON.parse(stdout.slice(marker + '__AIC_UI_JSON__\n'.length + 1)));
      } catch (e) {
        resolve({ ok: false, error: `failed to parse runner JSON: ${e.message}`, stdout: stdout.slice(-2000) });
      }
    });
    child.on('error', (e) => {
      clearTimeout(timer);
      resolve({ ok: false, error: `failed to spawn python: ${e.message}`, stderr });
    });
    child.stdin.end(JSON.stringify(payload));
  });
}

const server = http.createServer(async (req, res) => {
  if (req.method === 'GET' && req.url.startsWith('/api/health')) {
    sendJson(res, 200, { ok: true, python: PYTHON, runner: RUNNER });
    return;
  }
  if (req.method === 'POST' && req.url.startsWith('/api/run')) {
    if (!PYTHON) {
      sendJson(res, 500, { ok: false, error: 'No Python interpreter with aiconfigurator found. Set $AIC_PYTHON or install the package.' });
      return;
    }
    let body = '';
    req.on('data', (c) => { body += c; if (body.length > 2 * 1024 * 1024) req.destroy(); });
    req.on('end', async () => {
      let payload;
      try { payload = JSON.parse(body); } catch (e) { sendJson(res, 400, { ok: false, error: `bad JSON: ${e.message}` }); return; }
      log(`run mode=${payload.mode} model=${(payload.params && payload.params.model_path) || '-'}`);
      const result = await runRunner(payload);
      sendJson(res, result.ok === false ? 500 : 200, result);
    });
    return;
  }
  if (req.method === 'GET') serveStatic(req, res);
  else { res.writeHead(405); res.end('method not allowed'); }
});

PYTHON = resolvePython();
if (!PYTHON) log('WARNING: no Python with aiconfigurator.cli found; runs will fail. Set AIC_PYTHON.');
server.listen(PORT, () => {
  log(`aiconfigurator UI ready:  http://localhost:${PORT}`);
  log(`python bridge: ${PYTHON || '(not found)'}`);
});
