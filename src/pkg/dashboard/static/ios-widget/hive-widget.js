// Variables used by Scriptable.
// These must be at the very top of the file. Do not edit.
// icon-color: yellow; icon-glyph: bug;
//
// Hive iPhone widget (Scriptable) — see src/docs/ios-widget.md
//
// Shows, for one hive: total open issues, total open PRs, actionable issues
// and actionable PRs, each with a sparkline. Tapping the widget opens the
// hive dashboard (the same mobile page you get from "Add to Home Screen").
//
// Setup: run the script once inside Scriptable. It asks for the hive URL and
// a GitHub token, stores the token in the iOS Keychain (never in the widget
// parameter), and shows a preview. Then add a Scriptable widget to the Home
// Screen, pick this script, and set the widget parameter to the hive URL.
//
// Auth: the hub's auth-check accepts `Authorization: Bearer <GitHub token>`
// for any user already allowed on the hive, so no server-side change is
// needed. Use a fine-grained token with no repository permissions — it is
// only used to prove who you are.

const VERSION = "1";
const KEYCHAIN_TOKEN_KEY = "hive.widget.github-token";
const KEYCHAIN_URL_KEY = "hive.widget.default-url";
const HISTORY_DAYS = 7;
const HISTORY_BUCKET_MIN = 15; // one locally kept sample per 15 minutes
const SPARK_POINTS = 48;      // points drawn per sparkline
const REFRESH_MIN = 15;
const FETCH_TIMEOUT_S = 12;

// Hive dashboard palette (tokens.css).
const C = {
  bg: new Color("#0d1117"),
  card: new Color("#161b22"),
  text: new Color("#e6edf3"),
  muted: new Color("#8b949e"),
  amber: new Color("#f5c542"),
  green: new Color("#3fb950"),
  blue: new Color("#58a6ff"),
  red: new Color("#f85149"),
};

const METRICS = [
  { key: "issues", label: "ISSUES", color: C.blue },
  { key: "prs", label: "PRS", color: C.green },
  { key: "actIssues", label: "YOUR TURN · ISSUES", short: "ACT ISSUES", color: C.amber },
  { key: "actPrs", label: "YOUR TURN · PRS", short: "ACT PRS", color: C.amber },
];

// ---------------------------------------------------------------- config

function normalizeURL(u) {
  u = (u || "").trim();
  if (!u) return "";
  if (!/^https?:\/\//i.test(u)) u = "https://" + u;
  return u.replace(/\/+$/, "");
}

function hiveURL() {
  const fromParam = normalizeURL(args.widgetParameter);
  if (fromParam) return fromParam;
  if (Keychain.contains(KEYCHAIN_URL_KEY)) return normalizeURL(Keychain.get(KEYCHAIN_URL_KEY));
  return "";
}

function token() {
  return Keychain.contains(KEYCHAIN_TOKEN_KEY) ? Keychain.get(KEYCHAIN_TOKEN_KEY) : "";
}

async function setup() {
  const a = new Alert();
  a.title = "Hive widget setup";
  a.message = "Hive dashboard URL and a GitHub token (fine-grained, no repo permissions). The token is stored in the iOS Keychain.";
  a.addTextField("https://<hive>.hive.hivecommons.dev", hiveURL());
  a.addSecureTextField("GitHub token", token());
  a.addAction("Save");
  a.addCancelAction("Cancel");
  if ((await a.present()) === -1) return false;
  const url = normalizeURL(a.textFieldValue(0));
  const tok = a.textFieldValue(1).trim();
  if (!url || !tok) {
    const e = new Alert(); e.title = "Both fields are required"; e.addAction("OK"); await e.present();
    return false;
  }
  Keychain.set(KEYCHAIN_URL_KEY, url);
  Keychain.set(KEYCHAIN_TOKEN_KEY, tok);
  return true;
}

// ------------------------------------------------------------------ data

async function getJSON(base, path) {
  const req = new Request(base + path);
  req.timeoutInterval = FETCH_TIMEOUT_S;
  req.headers = { Authorization: "Bearer " + token(), Accept: "application/json" };
  const body = await req.loadString();
  const status = req.response ? req.response.statusCode : 0;
  if (status !== 200) throw new Error("HTTP " + status + " " + path);
  try { return JSON.parse(body); } catch (e) { throw new Error("not JSON (login page?) " + path); }
}

// One eval-history point -> the four numbers.
function pointToSample(p) {
  let issues = 0, prs = 0;
  const repos = p.repos || {};
  for (const name in repos) {
    issues += Number(repos[name].issues || 0);
    prs += Number(repos[name].prs || 0);
  }
  return {
    t: Number(p.t),
    issues,
    prs,
    actIssues: Number(p.govIssues || 0),
    actPrs: Number(p.govPrs || 0),
  };
}

function historyFile(base) {
  const fm = FileManager.local();
  const dir = fm.joinPath(fm.documentsDirectory(), "hive-widget");
  if (!fm.fileExists(dir)) fm.createDirectory(dir, true);
  const slug = base.replace(/^https?:\/\//, "").replace(/[^a-z0-9.-]/gi, "_");
  return { fm, path: fm.joinPath(dir, slug + ".v" + VERSION + ".json") };
}

function loadLocal(base) {
  const { fm, path } = historyFile(base);
  if (!fm.fileExists(path)) return { samples: [], meta: {} };
  try { return JSON.parse(fm.readString(path)); } catch (e) { return { samples: [], meta: {} }; }
}

function saveLocal(base, state) {
  const { fm, path } = historyFile(base);
  fm.writeString(path, JSON.stringify(state));
}

// Merge server history into the local rolling file: one sample per
// HISTORY_BUCKET_MIN, newest wins, keep HISTORY_DAYS.
function mergeSamples(existing, incoming) {
  const bucketMs = HISTORY_BUCKET_MIN * 60 * 1000;
  const byBucket = new Map();
  for (const s of existing) byBucket.set(Math.floor(s.t / bucketMs), s);
  for (const s of incoming) byBucket.set(Math.floor(s.t / bucketMs), s);
  const cutoff = Date.now() - HISTORY_DAYS * 24 * 3600 * 1000;
  return [...byBucket.values()].filter((s) => s.t >= cutoff).sort((a, b) => a.t - b.t);
}

async function loadData(base) {
  const state = loadLocal(base);
  let live = null, err = null;
  try {
    const [hist, summary] = await Promise.all([
      getJSON(base, "/api/history?hours=168"),
      getJSON(base, "/api/status/summary"),
    ]);
    const incoming = (Array.isArray(hist) ? hist : []).map(pointToSample).filter((s) => s.t > 0);
    state.samples = mergeSamples(state.samples || [], incoming);
    const agents = Array.isArray(summary.agents) ? summary.agents : [];
    state.meta = {
      hiveId: summary.hiveId || base.replace(/^https?:\/\//, "").split(".")[0],
      agentsRunning: agents.filter((a) => a.state === "running").length,
      agentsTotal: agents.length,
      fetchedAt: Date.now(),
    };
    saveLocal(base, state);
    live = true;
  } catch (e) {
    err = String(e.message || e);
  }
  return { state, live, err };
}

// ---------------------------------------------------------------- drawing

function series(samples, key) {
  const vals = samples.map((s) => Number(s[key] || 0));
  if (vals.length <= SPARK_POINTS) return vals;
  // Downsample evenly so the sparkline spans the whole window.
  const out = [];
  for (let i = 0; i < SPARK_POINTS; i++) {
    out.push(vals[Math.floor((i * (vals.length - 1)) / (SPARK_POINTS - 1))]);
  }
  return out;
}

function sparkline(vals, color, w, h) {
  const ctx = new DrawContext();
  ctx.size = new Size(w, h);
  ctx.opaque = false;
  ctx.respectScreenScale = true;
  if (vals.length < 2) return ctx.getImage();
  let min = Math.min(...vals), max = Math.max(...vals);
  if (max === min) { max += 1; min -= 1; }
  const pad = 2;
  const x = (i) => pad + (i * (w - 2 * pad)) / (vals.length - 1);
  const y = (v) => h - pad - ((v - min) * (h - 2 * pad)) / (max - min);

  const fill = new Path();
  fill.move(new Point(x(0), h));
  vals.forEach((v, i) => fill.addLine(new Point(x(i), y(v))));
  fill.addLine(new Point(x(vals.length - 1), h));
  fill.closeSubpath();
  const faint = new Color(color.hex, 0.18);
  ctx.setFillColor(faint);
  ctx.addPath(fill);
  ctx.fillPath();

  const line = new Path();
  vals.forEach((v, i) => (i === 0 ? line.move(new Point(x(i), y(v))) : line.addLine(new Point(x(i), y(v)))));
  ctx.setStrokeColor(color);
  ctx.setLineWidth(1.5);
  ctx.addPath(line);
  ctx.strokePath();

  ctx.setFillColor(color);
  const lx = x(vals.length - 1), ly = y(vals[vals.length - 1]);
  ctx.fillEllipse(new Rect(lx - 2, ly - 2, 4, 4));
  return ctx.getImage();
}

function delta(vals) {
  if (vals.length < 2) return 0;
  return vals[vals.length - 1] - vals[0];
}

function metricCell(parent, m, samples, opts) {
  const vals = series(samples, m.key);
  const cur = vals.length ? vals[vals.length - 1] : 0;
  const d = delta(vals);

  const cell = parent.addStack();
  cell.layoutVertically();
  cell.backgroundColor = C.card;
  cell.cornerRadius = 8;
  cell.setPadding(6, 8, 6, 8);

  const head = cell.addStack();
  head.centerAlignContent();
  const label = head.addText(opts.compact ? (m.short || m.label) : m.label);
  label.font = Font.semiboldSystemFont(opts.compact ? 8 : 9);
  label.textColor = C.muted;
  label.lineLimit = 1;
  label.minimumScaleFactor = 0.7;
  head.addSpacer();
  if (d !== 0) {
    const dt = head.addText((d > 0 ? "▲" : "▼") + Math.abs(d));
    dt.font = Font.mediumSystemFont(opts.compact ? 8 : 9);
    // For "your turn" and open counts, down is good.
    dt.textColor = d > 0 ? C.red : C.green;
  }

  const row = cell.addStack();
  row.bottomAlignContent();
  const num = row.addText(String(cur));
  num.font = Font.boldRoundedSystemFont(opts.compact ? 20 : 24);
  num.textColor = m.color;
  num.minimumScaleFactor = 0.6;
  row.addSpacer(6);
  const img = row.addImage(sparkline(vals, m.color, opts.sparkW, opts.sparkH));
  img.imageSize = new Size(opts.sparkW, opts.sparkH);
  img.resizable = false;
}

function ago(ms) {
  if (!ms) return "";
  const m = Math.round((Date.now() - ms) / 60000);
  if (m < 1) return "just now";
  if (m < 60) return m + "m ago";
  return Math.round(m / 60) + "h ago";
}

function buildWidget(base, data, family) {
  const w = new ListWidget();
  w.backgroundColor = C.bg;
  w.url = base;
  w.refreshAfterDate = new Date(Date.now() + REFRESH_MIN * 60 * 1000);

  const compact = family === "small";
  const samples = data.state.samples || [];
  const meta = data.state.meta || {};

  if (family === "accessoryRectangular" || family === "accessoryInline") {
    const last = samples[samples.length - 1] || {};
    const t = w.addText(`🐝 ${last.issues || 0}i ${last.prs || 0}pr · turn ${last.actIssues || 0}/${last.actPrs || 0}`);
    t.font = Font.mediumSystemFont(12);
    return w;
  }

  w.setPadding(10, 10, 8, 10);
  const header = w.addStack();
  header.centerAlignContent();
  const title = header.addText("🐝 " + (meta.hiveId || base.replace(/^https?:\/\//, "")));
  title.font = Font.boldSystemFont(compact ? 10 : 12);
  title.textColor = C.text;
  title.lineLimit = 1;
  title.minimumScaleFactor = 0.6;
  header.addSpacer();
  if (!compact && meta.agentsTotal) {
    const ag = header.addText(`${meta.agentsRunning}/${meta.agentsTotal} agents`);
    ag.font = Font.mediumSystemFont(9);
    ag.textColor = C.muted;
  }
  w.addSpacer(6);

  if (!samples.length) {
    const t = w.addText(data.err ? "Can't reach hive\n" + data.err : "No data yet");
    t.font = Font.mediumSystemFont(11);
    t.textColor = C.red;
    return w;
  }

  const opts = compact
    ? { compact: true, sparkW: 40, sparkH: 18 }
    : { compact: false, sparkW: 64, sparkH: 24 };

  const grid = w.addStack();
  grid.layoutVertically();
  for (let r = 0; r < 2; r++) {
    const row = grid.addStack();
    row.layoutHorizontally();
    metricCell(row, METRICS[r * 2], samples, opts);
    row.addSpacer(6);
    metricCell(row, METRICS[r * 2 + 1], samples, opts);
    if (r === 0) grid.addSpacer(6);
  }

  w.addSpacer(4);
  const foot = w.addStack();
  const span = samples.length > 1 ? Math.round((samples[samples.length - 1].t - samples[0].t) / 3600000) : 0;
  const f = foot.addText((span >= 48 ? Math.round(span / 24) + "d" : span + "h") + " · " + (data.live ? "updated " : "stale · ") + ago(meta.fetchedAt));
  f.font = Font.regularSystemFont(8);
  f.textColor = data.live ? C.muted : C.red;
  return w;
}

// ------------------------------------------------------------------ main

async function main() {
  // Launched from the hive dashboard's "iPhone widget" menu item as
  // scriptable:///run/Hive?url=<hive>: adopt that hive as the default so the
  // setup form only needs the token.
  const q = args.queryParameters || {};
  const handedURL = normalizeURL(q.url);
  if (handedURL && !config.runsInWidget) Keychain.set(KEYCHAIN_URL_KEY, handedURL);

  let base = hiveURL();
  if (!config.runsInWidget && (!base || !token() || q.setup)) {
    if (!(await setup())) { Script.complete(); return; }
    base = hiveURL();
  }
  if (!base || !token()) {
    const w = new ListWidget();
    w.backgroundColor = C.bg;
    const t = w.addText("🐝 Hive\nOpen Scriptable and run this script once to set the hive URL and token.");
    t.font = Font.mediumSystemFont(11);
    t.textColor = C.text;
    Script.setWidget(w);
    Script.complete();
    return;
  }

  const data = await loadData(base);
  const family = config.widgetFamily || "medium";
  const w = buildWidget(base, data, family);

  if (config.runsInWidget) {
    Script.setWidget(w);
  } else {
    await w.presentMedium();
  }
  Script.complete();
}

await main();
