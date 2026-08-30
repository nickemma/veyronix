package api

const swaggerHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Plinth API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => window.ui = SwaggerUIBundle({url: '/openapi.yaml', dom_id: '#swagger-ui', deepLinking: true});
  </script>
</body>
</html>`

const playgroundHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Plinth Playground</title>
  <style>
    :root { color-scheme: light dark; font-family: system-ui, sans-serif; }
    body { max-width: 1000px; margin: 2rem auto; padding: 0 1rem; }
    textarea { width: 100%; min-height: 260px; font: 14px ui-monospace, monospace; }
    button { margin: .25rem; padding: .5rem .8rem; cursor: pointer; }
    pre { padding: 1rem; overflow: auto; background: #222; color: #eee; border-radius: 6px; min-height: 160px; }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(130px, 1fr)); gap: .5rem; }
    .hint { opacity: .8; }
  </style>
</head>
<body>
  <h1>Plinth API Playground</h1>
  <p class="hint">A small test client for the same API used by the CLI. It is not a production dashboard.</p>
  <label for="manifest"><strong>Manifest JSON</strong></label>
  <textarea id="manifest">{
  "name": "tessera-gateway",
  "image": "ghcr.io/nickemma/tessera:v0.4.1",
  "port": 8080,
  "replicas": 3,
  "env": {"LOG_LEVEL": "info"},
  "secrets": ["DATABASE_URL"],
  "resources": {"cpu": "500m", "memory": "512Mi"}
}</textarea>
  <div class="grid">
    <button onclick="applyManifest()">Apply manifest</button>
    <button onclick="listServices()">List services</button>
    <button onclick="serviceAction('events')">Events</button>
    <button onclick="serviceAction('logs')">Logs</button>
    <button onclick="serviceAction('test/drift', {kind:'Deployment'})">Simulate drift</button>
    <button onclick="serviceAction('rollback', {})">Rollback</button>
    <button onclick="serviceAction('pause')">Pause</button>
    <button onclick="serviceAction('resume')">Resume</button>
    <button onclick="serviceAction('destroy')">Destroy</button>
  </div>
  <pre id="output">Ready. Apply the example manifest to begin.</pre>
  <script>
    const output = document.getElementById('output');
    const manifest = document.getElementById('manifest');
    const show = value => output.textContent = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
    const request = async (path, options = {}) => {
      const response = await fetch('/api/v1' + path, {headers: {'Content-Type': 'application/json'}, ...options});
      const value = await response.json();
      if (!response.ok) throw new Error(value.error || response.statusText);
      return value;
    };
    const currentName = () => JSON.parse(manifest.value).name;
    async function applyManifest() { try { show(await request('/services', {method: 'POST', body: manifest.value})); } catch (e) { show(e.message); } }
    async function listServices() { try { show(await request('/services')); } catch (e) { show(e.message); } }
    async function serviceAction(action, body) {
      try { const readOnly = action === 'events' || action === 'logs'; const suffix = '/' + action; show(await request('/services/' + encodeURIComponent(currentName()) + suffix, {method: readOnly ? 'GET' : 'POST', body: readOnly ? undefined : JSON.stringify(body || {})})); }
      catch (e) { show(e.message); }
    }
  </script>
</body>
</html>`

const openAPIFallback = `openapi: 3.0.3
info:
  title: Plinth API
  version: 0.1.0
paths: {}
`
