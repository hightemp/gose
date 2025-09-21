#!/usr/bin/env node
// Fast concurrent enqueuer using Node.js stdlib (http/https), no external deps.

const fs = require('fs');
const readline = require('readline');
const http = require('http');
const https = require('https');
const { URL } = require('url');

// -----------------------------
// Env helpers
// -----------------------------
function envStr(name, def) {
  const v = process.env[name];
  return v && v.trim() !== '' ? v : def;
}
function envInt(name, def) {
  const v = process.env[name];
  if (!v || v.trim() === '') return def;
  const n = Number(v);
  return Number.isFinite(n) ? n : def;
}
function envBool(name, def = false) {
  const v = process.env[name];
  if (v == null) return def;
  const s = String(v).trim().toLowerCase();
  return s === '1' || s === 'true' || s === 'yes' || s === 'on';
}

// -----------------------------
// Args parsing (simple)
// -----------------------------
function parseArgs() {
  const args = process.argv.slice(2);
  const out = {};
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    if (a.startsWith('--')) {
      const m = a.match(/^--([^=]+)(?:=(.*))?$/);
      if (!m) continue;
      const key = m[1];
      let val = m[2];
      if (val == null) {
        // try next token if not an option
        if (i + 1 < args.length && !args[i + 1].startsWith('--')) {
          val = args[++i];
        } else {
          // boolean flag
          val = 'true';
        }
      }
      out[key] = val;
    }
  }
  return out;
}

function toInt(v, def) {
  if (v == null || v === '') return def;
  const n = Number(v);
  return Number.isFinite(n) ? n : def;
}
function toBool(v, def = false) {
  if (v == null) return def;
  const s = String(v).trim().toLowerCase();
  return s === '1' || s === 'true' || s === 'yes' || s === 'on';
}

// -----------------------------
// Config
// -----------------------------
function getConfig() {
  const a = parseArgs();
  const cfg = {
    api: a.api ?? envStr('API', 'http://localhost:18082'),
    endpoint: a.endpoint ?? envStr('ENDPOINT', ''),
    file: a.file ?? a.f ?? envStr('FILE', 'tmp/top-1000000-domains/top-1000000-domains'),
    scheme: a.scheme ?? envStr('SCHEME', 'https'),
    concurrency: toInt(a.concurrency ?? a.c, envInt('CONCURRENCY', 128)),
    priority: (process.env.PRIORITY != null && process.env.PRIORITY !== '') ? Number(process.env.PRIORITY)
              : (a.priority != null ? Number(a.priority) : null),
    limit: toInt(a.limit, envInt('LIMIT', 0)),
    startAt: toInt(a['start-at'], envInt('START_AT', 0)),
    timeoutMs: Number(a.timeout ?? envInt('TIMEOUT', 8)) * 1000,
    retries: toInt(a.retries, envInt('RETRIES', 6)),
    retryDelayMs: toInt(a['retry-delay-ms'], envInt('RETRY_DELAY_MS', 200)),
    retryJitterMs: toInt(a['retry-jitter-ms'], envInt('RETRY_JITTER_MS', 400)),
    sleepMs: toInt(a['sleep-ms'], envInt('SLEEP_MS', 0)),
    logEvery: toInt(a['log-every'], envInt('LOG_EVERY', 5000)),
    failedFile: a['failed-file'] ?? envStr('FAILED_FILE', 'scripts/enqueue_failed.txt'),
    clearFailed: toBool(a['clear-failed'], envBool('CLEAR_FAILED', true)),
    connectionClose: toBool(a['connection-close'], envBool('CONNECTION_CLOSE', false)),
    debug: toBool(a['debug'], envBool('DEBUG', false)),
  };
  if (!cfg.endpoint || cfg.endpoint.trim() === '') {
    cfg.endpoint = String(cfg.api).replace(/\/+$/, '') + '/api/enqueue';
  }
  if (cfg.scheme !== 'http' && cfg.scheme !== 'https') {
    throw new Error('--scheme must be http or https');
  }
  return cfg;
}

// -----------------------------
// Domain parsing
// -----------------------------
const DOMAIN_RE = /^[A-Za-z0-9][A-Za-z0-9\.-]*[A-Za-z0-9]$/;

function parseDomainLine(line) {
  let s = String(line).trim();
  if (!s || s.startsWith('#')) return '';
  if (s.includes(',')) {
    const parts = s.split(',');
    s = parts[parts.length - 1].trim();
  }
  s = s.replace(/^[\s"']+|[\s"']+$/g, '');
  if (!s) return '';
  if (!DOMAIN_RE.test(s)) return '';
  return s.toLowerCase();
}

// -----------------------------
// Endpoint parsing
// -----------------------------
function parseEndpoint(endpoint) {
  const u = new URL(endpoint);
  const isHttps = u.protocol === 'https:';
  let port = u.port ? Number(u.port) : (isHttps ? 443 : 80);
  return {
    isHttps,
    hostname: u.hostname || 'localhost',
    port,
    path: u.pathname && u.pathname !== '' ? u.pathname : '/api/enqueue',
  };
}

// -----------------------------
// HTTP client with Agent (keep-alive)
// -----------------------------
function makeHttpClient(ep, keepAlive, maxSockets) {
  const agentOpts = {
    keepAlive,
    maxSockets: Math.max(16, maxSockets || 256),
    scheduling: 'lifo',
  };
  const agent = ep.isHttps ? new https.Agent(agentOpts) : new http.Agent(agentOpts);
  const lib = ep.isHttps ? https : http;
  return { lib, agent };
}

function postJSON({ lib, agent, ep, body, headers, timeoutMs }) {
  return new Promise((resolve, reject) => {
    const opts = {
      hostname: ep.hostname,
      port: ep.port,
      path: ep.path,
      method: 'POST',
      agent,
      headers: {
        'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(body),
        'Accept': 'application/json',
        ...headers,
      },
    };

    const req = lib.request(opts, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const buf = Buffer.concat(chunks);
        resolve({ status: res.statusCode || 0, body: buf });
      });
    });
    req.on('error', (err) => reject(err));
    if (timeoutMs && timeoutMs > 0) {
      req.setTimeout(timeoutMs, () => {
        req.destroy(new Error('timeout'));
      });
    }
    req.write(body);
    req.end();
  });
}

// -----------------------------
// Semaphore
// -----------------------------
class Semaphore {
  constructor(max) {
    this.max = max;
    this.cur = 0;
    this.q = [];
  }
  acquire() {
    if (this.cur < this.max) {
      this.cur++;
      return Promise.resolve();
    }
    return new Promise((res) => this.q.push(res));
  }
  release() {
    this.cur--;
    const next = this.q.shift();
    if (next) {
      this.cur++;
      next();
    }
  }
}

// -----------------------------
// Main logic
// -----------------------------
async function main() {
  const cfg = getConfig();
  const ep = parseEndpoint(cfg.endpoint);

  // failed file
  let failedStream = null;
  try {
    if (cfg.failedFile) {
      if (cfg.clearFailed && fs.existsSync(cfg.failedFile)) {
        fs.writeFileSync(cfg.failedFile, '');
      }
      failedStream = fs.createWriteStream(cfg.failedFile, { flags: 'a' });
      console.error(`[init] failedFile=${cfg.failedFile} clear=${cfg.clearFailed}`);
    }
  } catch (e) {
    console.error(`failed to open failed file ${cfg.failedFile}: ${e}`);
  }

  const { lib, agent } = makeHttpClient(ep, !cfg.connectionClose, cfg.concurrency * 2);
  console.error(`[start] endpoint=${cfg.endpoint} file=${cfg.file} scheme=${cfg.scheme} priority=${Number.isInteger(cfg.priority)?cfg.priority:'none'}`);
  console.error(`[start] concurrency=${cfg.concurrency} timeoutMs=${cfg.timeoutMs} retries=${cfg.retries} keepAlive=${!cfg.connectionClose} connectionClose=${cfg.connectionClose} debug=${cfg.debug}`);
  console.error(`[http] target host=${ep.hostname}:${ep.port} path=${ep.path} https=${ep.isHttps}`);
  
  const sem = new Semaphore(Math.max(1, cfg.concurrency));
  const counters = {
    done: 0,
    ok: 0,
    fail: 0,
    t0: Date.now(),
    logEvery: Math.max(1, cfg.logEvery),
  };

  function progressMaybe() {
    if (counters.done % counters.logEvery === 0) {
      const elapsed = (Date.now() - counters.t0) / 1000;
      const rate = elapsed > 0 ? (counters.done / elapsed) : 0;
      console.error(`progress: done=${counters.done} ok=${counters.ok} fail=${counters.fail} rate=${rate.toFixed(1)}/s`);
    }
  }

  async function enqueueDomain(domain) {
    if (cfg.sleepMs > 0) {
      await new Promise((r) => setTimeout(r, cfg.sleepMs));
    }
    const url = `${cfg.scheme}://${domain}/`;
    const payload = { url };
    if (Number.isInteger(cfg.priority)) payload.priority = cfg.priority;
    const body = Buffer.from(JSON.stringify(payload), 'utf8');

    if (cfg.debug) {
      console.error(`[req] start domain=${domain}`);
    }

    let attempt = 0;
    while (attempt <= cfg.retries) {
      try {
        const headers = cfg.connectionClose ? { 'Connection': 'close' } : {};
        const res = await postJSON({
          lib,
          agent,
          ep,
          body,
          headers,
          timeoutMs: cfg.timeoutMs,
        });
        if (res.status === 200) {
          if (cfg.debug) {
            console.error(`[req] ok domain=${domain} attempt=${attempt+1}`);
          }
          counters.ok++;
          return;
        } else {
          if (cfg.debug) {
            let snippet = '';
            try {
              const s = res.body.toString('utf8');
              snippet = s.length > 200 ? s.slice(0, 200) + '...' : s;
            } catch {}
            console.error(`[req] non-200 domain=${domain} status=${res.status} attempt=${attempt+1} body="${snippet}"`);
          }
        }
      } catch (e) {
        if (cfg.debug) {
          console.error(`[req] error domain=${domain} attempt=${attempt+1} err=${e && e.message ? e.message : e}`);
        }
      }
      attempt++;
      if (attempt <= cfg.retries) {
        let delay = cfg.retryDelayMs * Math.pow(2, attempt - 1);
        if (cfg.retryJitterMs > 0) {
          delay += Math.floor(Math.random() * (cfg.retryJitterMs + 1));
        }
        if (cfg.debug) {
          console.error(`[req] retry domain=${domain} next_delay_ms=${delay}`);
        }
        await new Promise((r) => setTimeout(r, delay));
      }
    }
    counters.fail++;
    if (!cfg.debug) {
      // В режиме без debug тоже отметим явный провал домена
      console.error(`[req] failed domain=${domain} after retries=${cfg.retries}`);
    }
    if (failedStream) {
      try { failedStream.write(domain + '\n'); } catch {}
    }
  }

  // Input stream setup
  let input;
  if (cfg.file === '-') {
    input = process.stdin;
  } else {
    if (!fs.existsSync(cfg.file)) {
      console.error(`Input file not found: ${cfg.file}`);
      process.exit(1);
    }
    input = fs.createReadStream(cfg.file, { encoding: 'utf8' });
  }
  const rl = readline.createInterface({ input, crlfDelay: Infinity });
  console.error(`[read] from=${cfg.file} startAt=${cfg.startAt} limit=${cfg.limit}`);
  
  let idx = 0;
  let sent = 0;
  const runners = [];

  for await (const rawLine of rl) {
    idx++;
    if (cfg.startAt && idx <= cfg.startAt) continue;
    const d = parseDomainLine(rawLine);
    if (!d) continue;

    if (cfg.limit > 0 && sent >= cfg.limit) break;
    sent++;
    if (sent % cfg.logEvery === 0) { console.error(`queued: ${sent}`); }
    
    await sem.acquire();
    const p = enqueueDomain(d)
      .catch(() => {}) // safety
      .finally(() => {
        counters.done++;
        progressMaybe();
        sem.release();
      });
    runners.push(p);
  }

  // wait all
  await Promise.all(runners);

  if (failedStream) {
    await new Promise((r) => failedStream.end(r));
  }

  // destroy agent to close sockets
  if (agent && typeof agent.destroy === 'function') {
    agent.destroy();
  }

  const elapsed = (Date.now() - counters.t0) / 1000;
  const rate = elapsed > 0 ? (counters.done / elapsed) : 0;
  console.error(`done: queued=${sent} processed=${counters.done} ok=${counters.ok} fail=${counters.fail} elapsed=${elapsed.toFixed(1)}s rate=${rate.toFixed(1)}/s`);

  process.exit(counters.fail === 0 ? 0 : 2);
}

// Entry
main().catch((e) => {
  console.error(e?.stack || String(e));
  process.exit(1);
});