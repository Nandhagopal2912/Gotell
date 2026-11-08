// Gotell dashboard. Plain JavaScript, no framework. Every value from the
// API is inserted as text (textContent), never as HTML, so a target URL
// can never inject markup or scripts into the page.
"use strict";

const REFRESH_MS = 30000;
const HOUR = 3600e3;

const state = {
  filter: "all",        // "all" or "problems"
  targets: [],          // from /api/targets/{id}
  runs: new Map(),      // target id -> latest runs
  incidents: [],
  openHour: new Map(),  // target id -> hour (ms) whose details are shown
  selected: null,       // target id shown in "Latest checks"
};

// ---------------------------------------------------------------- helpers

// el creates an element with attributes and children (strings become text).
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value == null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat(Infinity)) {
    if (child == null || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

async function getJSON(path) {
  const res = await fetch(path, { headers: { Accept: "application/json" } });
  if (!res.ok) throw new Error(`${path}: ${res.status} ${res.statusText}`);
  return res.json();
}

// null means "not measured": shown as "–" or "n/a", never as 0.
const fmtPct = (v) => (v == null ? "n/a" : `${v.toFixed(v >= 99.95 || v === 0 ? 0 : 1)}%`);
const fmtMs = (v) => (v == null ? "–" : v >= 1000 ? `${(v / 1000).toFixed(1)} s` : `${Math.round(v)} ms`);
const fmtClock = (d) => new Date(d).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
const fmtWhen = (iso) => {
  if (!iso) return "–";
  const d = new Date(iso);
  const sameDay = d.toDateString() === new Date().toDateString();
  return sameDay ? fmtClock(d) : d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
};

function fmtDuration(sec) {
  const s = Math.max(0, Math.round(sec));
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${s}s`;
}

const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;
const isProblem = (t) => t.status === "down" || t.status === "failing";

// --------------------------------------------------------------- insights

// explainAttempt says, in plain words, what one failed check tells us.
function explainAttempt(a) {
  if (!a) return "";
  const code = a.status_code;
  switch (a.failure_class) {
    case "none":
      return "Healthy answer.";
    case "dns":
      return "The domain name could not be looked up (DNS). Check the URL, or your domain's DNS settings.";
    case "connect":
      return "The server didn't accept the connection. The service may be stopped or restarting.";
    case "tls":
      return "The secure HTTPS connection failed: often an expired or invalid certificate.";
    case "timeout":
      if (a.tls_handshake_ms != null || a.tcp_connect_ms != null)
        return "The server was reachable (connection" + (a.tls_handshake_ms != null ? " and HTTPS handshake" : "") +
          " worked), but the app never answered. The problem is the application, not the network: it may be stuck, crashed, or still starting.";
      return "No answer within the time limit.";
    case "http_5xx":
      return `The app answered with an error (HTTP ${code}). It is running, but failing.`;
    case "http_4xx":
      return `HTTP ${code}: the page wasn't found or access was refused. Check the URL or health path.`;
    case "http_other":
      return `Unexpected answer (HTTP ${code}), for example a redirect that Gotell doesn't follow.`;
    case "body_read":
      return "The connection dropped while the answer was being downloaded.";
    case "cancelled":
      return "Stopped by Gotell (shutting down). Not the target's fault.";
    default:
      return "An unexpected error; see the latest checks for details.";
  }
}

// lastProblemAttempt finds the newest failed attempt in a target's runs.
function lastProblemAttempt(runs) {
  for (const run of runs || []) {
    for (let i = run.attempts.length - 1; i >= 0; i--) {
      const a = run.attempts[i];
      if (a.failure_class !== "none" && a.failure_class !== "cancelled") return a;
    }
  }
  return null;
}

// insight is the one-paragraph, plain-language summary for a target.
function insight(t, runs) {
  const hours = t.hours || [];
  const recovered = hours.reduce((n, h) => n + h.recovered_runs, 0);
  const p50s = hours.map((h) => h.p50_ttfb_ms).filter((v) => v != null);
  const typical = p50s.length ? p50s.sort((a, b) => a - b)[Math.floor(p50s.length / 2)] : null;
  const problem = lastProblemAttempt(runs);
  const notes = [];

  switch (t.status) {
    case "down": {
      const inc = state.incidents.find((i) => i.target_id === t.id && i.open);
      const since = inc ? `Down for ${fmtDuration(inc.duration_sec)} (since ${fmtWhen(inc.opened_at)}).` : "Down.";
      return { tone: "bad", text: `${since} ${explainAttempt(problem)}` };
    }
    case "failing":
      return {
        tone: "warn",
        text: `The latest check failed. ${explainAttempt(problem)} Not an incident yet: Gotell alerts after 3 failures in a row.`,
      };
    case "paused":
      return { tone: "muted", text: "Paused for maintenance: no checks run, and this time doesn't count against uptime." };
    case "unknown":
      return { tone: "muted", text: `No checks yet. The first result will appear within ${t.interval_seconds} seconds.` };
  }

  notes.push(typical != null ? `Healthy. A typical answer takes ${fmtMs(typical)}.` : "Healthy.");
  if (recovered > 0)
    notes.push(`${plural(recovered, "check")} only succeeded after a retry: the app may have been waking from sleep (a suspected cold start).`);
  if (t.uptime_24h_pct != null && t.uptime_24h_pct < 99.5)
    notes.push(`It had problems earlier: ${fmtPct(t.uptime_24h_pct)} uptime over 24 hours.`);
  if (t.missing_runs_24h > 0)
    notes.push(`${plural(t.missing_runs_24h, "expected check")} produced no result.`);
  return { tone: recovered > 0 || (t.uptime_24h_pct ?? 100) < 99.5 ? "warn" : "good", text: notes.join(" ") };
}

// --------------------------------------------------------------- overview

function renderOverview() {
  const ts = state.targets;
  const count = (s) => ts.filter((t) => t.status === s).length;
  const down = count("down"), failing = count("failing"), up = count("up"), paused = count("paused");
  const open = state.incidents.filter((i) => i.open).length;

  let headline;
  if (ts.length === 0) headline = "No targets yet.";
  else if (down + failing === 0) headline = `All ${plural(ts.length - paused, "target")} are healthy.`;
  else {
    const names = ts.filter(isProblem).map((t) => hostOf(t.url));
    headline = `${plural(down + failing, "target")} need${down + failing === 1 ? "s" : ""} attention: ${names.join(", ")}.`;
  }
  const h = document.getElementById("headline");
  h.textContent = headline;
  h.className = `headline ${down ? "bad" : failing ? "warn" : "good"}`;

  const tile = (label, value, tone, help) =>
    el("div", { class: `tile ${tone || ""}`, title: help }, el("span", { class: "tile-value" }, value), el("span", { class: "tile-label" }, label));
  document.getElementById("tiles").replaceChildren(
    tile("watched", String(ts.length), "", "Targets Gotell is checking"),
    tile("up", String(up), up ? "good" : "", "Latest check was healthy"),
    tile("failing", String(failing), failing ? "warn" : "", "Latest check failed, fewer than 3 in a row"),
    tile("down", String(down), down ? "bad" : "", "3 or more failed checks in a row"),
    tile("open incidents", String(open), open ? "bad" : "", "Outages that haven't recovered yet"),
  );
}

function hostOf(url) {
  try { return new URL(url).host; } catch { return url; }
}

// ---------------------------------------------------------------- targets

function barClass(hour) {
  if (!hour) return "b-nodata";
  if (hour.uptime_pct == null) return hour.gotell_off_min >= 59 ? "b-off" : "b-nodata";
  if (hour.uptime_pct >= 99.5) return "b-good";
  if (hour.uptime_pct >= 95) return "b-warn";
  return "b-bad";
}

function renderBars(t) {
  const byHour = new Map((t.hours || []).map((h) => [new Date(h.hour).getTime(), h]));
  // Hours are UTC hours (blueprint §11.1). In a half-hour time zone such as
  // IST (+5:30) a UTC hour starts at :30 local time, so the bars are built on
  // UTC hour boundaries, not local ones.
  const thisHour = Math.floor(Date.now() / HOUR) * HOUR;
  const selected = state.openHour.get(t.id);
  const bars = el("div", { class: "bars", role: "group", "aria-label": "Uptime for each of the last 24 hours" });
  for (let i = 23; i >= 0; i--) {
    const start = thisHour - i * HOUR;
    const hour = byHour.get(start);
    const label = `${fmtClock(start)} to ${fmtClock(start + HOUR)}: ${hour ? `uptime ${fmtPct(hour.uptime_pct)}` : "no data"}`;
    bars.append(el("button", {
      type: "button",
      class: `bar ${barClass(hour)}${selected === start ? " selected" : ""}`,
      title: label,
      "aria-label": label,
      "aria-pressed": String(selected === start),
      onclick: () => {
        state.openHour.get(t.id) === start ? state.openHour.delete(t.id) : state.openHour.set(t.id, start);
        renderTargets();
      },
    }));
  }
  return bars;
}

function renderHourDetail(t) {
  const start = state.openHour.get(t.id);
  if (start == null) return null;
  const h = (t.hours || []).find((x) => new Date(x.hour).getTime() === start);
  const title = `${fmtClock(start)} – ${fmtClock(start + HOUR)}`;
  if (!h) return el("div", { class: "hour-detail" }, el("strong", {}, title), " No data for this hour: Gotell wasn't running, or the target didn't exist yet.");

  const items = [
    ["Uptime", fmtPct(h.uptime_pct)],
    ["Checks", `${h.observed_runs} (${h.success_runs} ok, ${h.recovered_runs} recovered, ${h.failed_runs} failed${h.cancelled_runs ? `, ${h.cancelled_runs} cancelled` : ""})`],
    ["Typical / p95", `${fmtMs(h.p50_ttfb_ms)} / ${fmtMs(h.p95_ttfb_ms)}`],
    ["No answer", plural(h.no_response_attempts, "attempt")],
    ["Expected / missing", `${h.expected_runs} / ${h.missing_runs}`],
  ];
  if (h.gotell_off_min >= 1) items.push(["Gotell off", `${Math.round(h.gotell_off_min)} min`]);
  if (h.paused_min >= 1) items.push(["Paused", `${Math.round(h.paused_min)} min`]);
  return el("div", { class: "hour-detail" },
    el("strong", {}, title),
    el("dl", {}, items.map(([k, v]) => el("div", { class: "kv" }, el("dt", {}, k), el("dd", {}, v)))));
}

function renderTarget(t) {
  const runs = state.runs.get(t.id);
  const tip = insight(t, runs);
  return el("article", { class: `card ${t.status}` },
    el("div", { class: "card-head" },
      el("span", { class: `pill ${t.status}` }, t.status),
      el("span", { class: "url" }, t.url)),
    el("p", { class: `insight ${tip.tone}` }, tip.text),
    el("p", { class: "stats" },
      stat("Uptime 24h", fmtPct(t.uptime_24h_pct), "Healthy checks ÷ checks that said up or down"),
      stat("Worst p95", fmtMs(t.p95_ttfb_24h_ms), "The slowest hour's 95th-percentile time to first byte"),
      stat("Missing", String(t.missing_runs_24h), "Expected checks with no result"),
      stat("Gotell off", `${Math.round(t.gotell_off_24h_min)} min`, "Time Gotell wasn't running (not counted as downtime)"),
      stat("Every", `${t.interval_seconds}s`, "How often this target is checked")),
    renderBars(t),
    el("div", { class: "bar-axis", "aria-hidden": "true" }, el("span", {}, "24h ago"), el("span", {}, "now")),
    renderHourDetail(t),
    el("button", { type: "button", class: "link", onclick: () => showRuns(t.id) }, "Latest checks, step by step →"));
}

function stat(label, value, help) {
  return el("span", { title: help }, `${label} `, el("b", {}, value));
}

function renderTargets() {
  const list = document.getElementById("targets");
  const shown = state.filter === "problems" ? state.targets.filter(isProblem) : state.targets;
  if (shown.length === 0) {
    list.replaceChildren(el("p", { class: "empty-card" },
      state.filter === "problems" ? "No problems right now. Every target is healthy or paused." : "No targets yet."));
    return;
  }
  // Problems first, so they are seen first.
  const order = { down: 0, failing: 1, unknown: 2, up: 3, paused: 4 };
  list.replaceChildren(...[...shown].sort((a, b) => order[a.status] - order[b.status] || a.id - b.id).map(renderTarget));
}

// ------------------------------------------------------------ latest checks

// stageBar shows where an attempt's time went. Missing stages (not
// measured, e.g. a reused connection) are simply absent, never 0.
function stageBar(a) {
  const parts = [];
  const add = (cls, label, ms) => { if (ms != null && ms > 0) parts.push({ cls, label, ms }); };
  add("s-dns", "DNS lookup", a.dns_ms);
  add("s-tcp", "connect", a.tcp_connect_ms);
  add("s-tls", "HTTPS handshake", a.tls_handshake_ms);
  if (a.ttfb_ms != null) {
    const setup = (a.dns_ms || 0) + (a.tcp_connect_ms || 0) + (a.tls_handshake_ms || 0);
    add("s-server", "waiting for the app", Math.max(a.ttfb_ms - setup, 0));
    add("s-body", "download", Math.max(a.total_ms - a.ttfb_ms, 0));
  } else {
    const setup = (a.dns_ms || 0) + (a.tcp_connect_ms || 0) + (a.tls_handshake_ms || 0);
    add("s-wait", "waited, no answer", Math.max(a.total_ms - setup, 0));
  }
  const total = parts.reduce((s, p) => s + p.ms, 0) || 1;
  const bar = el("div", { class: "stage-bar" },
    parts.map((p) => {
      const seg = el("span", { class: `stage ${p.cls}`, title: `${p.label}: ${fmtMs(p.ms)}` });
      // Set through the style object: the page's security policy blocks
      // inline style="" attributes, but allows this.
      seg.style.width = `${Math.max((p.ms / total) * 100, 1.5)}%`;
      return seg;
    }));
  const biggest = parts.slice().sort((x, y) => y.ms - x.ms)[0];
  const caption = biggest ? `mostly ${biggest.label} (${fmtMs(biggest.ms)})` : "nothing measured";
  return el("div", { class: "stage-cell" }, bar, el("span", { class: "stage-caption" }, caption));
}

async function showRuns(id) {
  state.selected = id;
  const t = state.targets.find((x) => x.id === id);
  const section = document.getElementById("detail");
  section.hidden = false;
  document.getElementById("detail-url").textContent = t ? t.url : "";
  const body = document.getElementById("runs");
  body.replaceChildren(el("tr", {}, el("td", { class: "empty", colspan: "5" }, "Loading…")));
  section.scrollIntoView({ behavior: "smooth", block: "start" });

  try {
    const runs = await getJSON(`/api/targets/${id}/runs?limit=20`);
    body.replaceChildren();
    if (runs.length === 0) {
      body.append(el("tr", {}, el("td", { class: "empty", colspan: "5" }, "No checks yet.")));
      return;
    }
    for (const run of runs) {
      run.attempts.forEach((a, idx) => {
        const result = a.status_code != null ? `HTTP ${a.status_code}` : "no answer";
        body.append(el("tr", { class: idx === 0 ? "run-first" : "" },
          el("td", {}, idx === 0 ? fmtWhen(run.started_at) : ""),
          el("td", {},
            idx === 0 ? el("span", { class: `tag ${run.outcome}` }, run.outcome) : "",
            run.attempts.length > 1 ? el("span", { class: "attempt" }, ` try ${a.number}`) : ""),
          el("td", { class: "wrap" },
            el("span", { class: `tag ${a.failure_class}` }, a.failure_class === "none" ? result : `${a.failure_class} · ${result}`),
            a.failure_class !== "none" ? el("span", { class: "explain" }, explainAttempt(a)) : ""),
          el("td", {}, stageBar(a)),
          el("td", { class: "num" }, fmtMs(a.total_ms))));
      });
    }
  } catch (err) {
    body.replaceChildren(el("tr", {}, el("td", { class: "empty", colspan: "5" }, `Could not load checks: ${err.message}`)));
  }
}

// ---------------------------------------------------------------- incidents

function renderIncidents() {
  const body = document.getElementById("incidents");
  body.replaceChildren();
  if (state.incidents.length === 0) {
    body.append(el("tr", {}, el("td", { class: "empty", colspan: "5" }, "No incidents: no target has failed 3 checks in a row.")));
    return;
  }
  const codes = { http_5xx: "5xx", http_4xx: "4xx", http_other: "3xx" };
  const explainCause = (c) => explainAttempt({ failure_class: c, status_code: codes[c] });
  for (const i of state.incidents) {
    body.append(el("tr", {},
      el("td", { class: "wrap" }, i.url),
      el("td", {}, fmtWhen(i.opened_at)),
      el("td", {}, i.open ? el("span", { class: "tag open" }, "still down") : fmtWhen(i.closed_at)),
      el("td", { class: "num" }, fmtDuration(i.duration_sec)),
      el("td", { class: "wrap" },
        el("span", { class: `tag ${i.cause}` }, i.cause),
        el("span", { class: "explain" }, `${explainCause(i.cause)} ${plural(i.failed_runs, "failed check")}.`))));
  }
}

// ---------------------------------------------------------------- refresh

async function refresh() {
  const errorBox = document.getElementById("error");
  try {
    const list = await getJSON("/api/targets");
    const [details, runs, incidents] = await Promise.all([
      Promise.all(list.map((t) => getJSON(`/api/targets/${t.id}?hours=24`))),
      Promise.all(list.map((t) => getJSON(`/api/targets/${t.id}/runs?limit=5`))),
      getJSON("/api/incidents?limit=20"),
    ]);
    state.targets = details;
    state.runs = new Map(list.map((t, i) => [t.id, runs[i]]));
    state.incidents = incidents;

    renderOverview();
    renderTargets();
    renderIncidents();
    if (state.selected != null && !document.getElementById("detail").hidden) showRunsQuietly(state.selected);

    errorBox.hidden = true;
    document.getElementById("updated").textContent =
      `Updated ${new Date().toLocaleTimeString()} · refreshes every ${REFRESH_MS / 1000}s`;
  } catch (err) {
    errorBox.textContent = `Could not reach Gotell: ${err.message}. Is it still running?`;
    errorBox.hidden = false;
  }
}

// showRunsQuietly refreshes the open "Latest checks" table without scrolling.
function showRunsQuietly(id) {
  const scroll = window.scrollY;
  showRuns(id).then(() => window.scrollTo({ top: scroll }));
}

document.getElementById("close-detail").addEventListener("click", () => {
  state.selected = null;
  document.getElementById("detail").hidden = true;
});

for (const chip of document.querySelectorAll("[data-filter]")) {
  chip.addEventListener("click", () => {
    state.filter = chip.dataset.filter;
    for (const c of document.querySelectorAll("[data-filter]")) c.setAttribute("aria-pressed", String(c === chip));
    renderTargets();
  });
}

refresh();
setInterval(refresh, REFRESH_MS);
