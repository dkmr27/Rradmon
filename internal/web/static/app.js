"use strict";
// Rradmon UI. Plain JS, no dependencies; talks to /api/* and /api/stream.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const short = (h, n = 8) => (h ? h.slice(0, n) : "");
const ms = (t) => new Date(t).getTime();
const pad = (n) => String(n).padStart(2, "0");
const clock = (t) => { const d = new Date(t); return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`; };
const num = (v, d = 1) => (v == null ? "–" : Number(v).toFixed(d));
function ago(t) {
  const s = Math.max(0, (Date.now() - ms(t)) / 1000);
  if (s < 60) return `${Math.round(s)} s ago`;
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${(s / 3600).toFixed(1)} h ago`;
  return `${(s / 86400).toFixed(1)} d ago`;
}
function dur(msv) {
  if (msv < 1000) return `${msv} ms`;
  if (msv < 60000) return `${(msv / 1000).toFixed(1)} s`;
  return `${Math.floor(msv / 60000)} min ${Math.round((msv % 60000) / 1000)} s`;
}
async function api(path) {
  const r = await fetch(path, { headers: { Accept: "application/json" } });
  if (!r.ok) throw new Error(`${path}: ${r.status}`);
  return r.json();
}
function store(key, val) { try { if (val === undefined) return localStorage.getItem(key); localStorage.setItem(key, val); } catch { return null; } }

// ---------- theme ----------
(function theme() {
  const saved = store("rradmon.theme");
  if (saved) document.documentElement.dataset.theme = saved;
  $("#themeBtn").onclick = () => {
    const dark = document.documentElement.dataset.theme
      ? document.documentElement.dataset.theme === "dark"
      : matchMedia("(prefers-color-scheme: dark)").matches;
    document.documentElement.dataset.theme = dark ? "light" : "dark";
    store("rradmon.theme", document.documentElement.dataset.theme);
    redrawCharts();
  };
})();

// ---------- tabs ----------
let currentTab = "live";
function showTab(name) {
  currentTab = name;
  const nav = name === "node" ? "nodes" : name;
  $$(".tabs button").forEach((b) => b.setAttribute("aria-selected", b.dataset.tab === nav));
  $$(".tab").forEach((s) => (s.hidden = s.id !== `tab-${name}`));
  if (name === "nodes") loadNodes();
  if (name === "channel") loadChannel();
  if (name === "observers") loadObservers();
  if (name === "map") loadMap();
  if (name !== "node") store("rradmon.tab", name);
}
$$(".tabs button").forEach((b) => (b.onclick = () => {
  if (location.hash.startsWith("#node=")) history.replaceState(null, "", location.pathname);
  showTab(b.dataset.tab);
}));

// ---------- stats strip ----------
let observers = [];
async function loadSummary() {
  const s = await api("/api/summary");
  observers = s.observers || [];
  const obs = observers[0];
  const stat = (k, v, sub = "") => `<div class="stat"><div class="k">${k}</div><div class="v">${v}</div><div class="sub">${sub}</div></div>`;
  $("#stats").innerHTML =
    stat("Packets, last hour", s.packets_1h, `${s.packets} stored`) +
    stat("Nodes", s.nodes, `${s.destinations} destinations`) +
    stat("Orphaned halves, last hour", s.orphans_1h, "split packets heard incomplete") +
    (obs
      ? stat("Channel utilisation", obs.util_s == null ? "–" : `${num(obs.util_s)} %`, `long-term ${num(obs.util_l, 2)} % · ${esc(obs.ext_id)}`) +
        stat("Noise floor", obs.noise_floor == null ? "–" : `${num(obs.noise_floor, 0)} dBm`, `observer last heard ${ago(obs.last_seen)}`)
      : "");
  const sel = $("#obsSelect");
  if (sel.options.length !== observers.length) {
    sel.innerHTML = observers.map((o) => `<option value="${o.id}">${esc(o.ext_id)}</option>`).join("");
  }
}

// ---------- live feed ----------
// One row group per packet: a summary line showing the first complete
// observation, and (expanded) one line per observation from every observer.
// Unmatched orphan groups are row groups of their own.
const feed = { rows: new Map(), newest: 0, oldest: Infinity, paused: false, open: new Set(), search: "" };
const MAX_ROWS = 600;

function typeFilter() { return new Set($$("#typeFilter input:checked").map((i) => i.value)); }
const rowKey = (r) => `${r.kind}:${r.id}`;
const rowType = (r) => (r.kind === "orphan" ? "orphan" : r.type);
// S: one LoRa frame, D: both frames of a split packet, O: one half heard alone
const framesCode = (r) => (r.kind === "orphan" ? "O" : r.frames === 2 ? "D" : "S");
const FRAMES_TITLE = { S: "Single LoRa frame", D: "Double: both frames of a split packet", O: "Orphan: one half of a split packet heard alone" };

function destCell(r) {
  const label = r.dest_name || r.app_name;
  const name = label ? ` <span class="muted">${esc(label)}</span>` : "";
  if (r.kind === "orphan") {
    // A first half carries its packet's header, so say what it was
    if (r.dest) return `<span class="muted">${esc(r.orphan_kind)} of ${esc(r.type)} to</span> <span class="mono">${short(r.dest, 12)}</span>${name}`;
    return `<span class="muted">${esc(r.orphan_kind)}</span>`;
  }
  return `<span class="mono">${short(r.dest, 12)}</span>${name} <span class="muted">${esc(r.dest_type)}</span>`;
}
function fromCell(tx, name) {
  return tx ? `<a class="node" href="#node=${tx}">${esc(name) || `<span class="mono">${short(tx)}</span>`}</a>` : "";
}
function hashCell(r) {
  if (!r.full_hash) return '<span class="muted">–</span>';
  return `<button class="hash mono" data-hash="${r.full_hash}" title="Packet hash (as RNS shows it): ${r.full_hash}. Click to copy.">${r.full_hash.slice(0, 8)}</button>`;
}
function notes(r) {
  const t = [];
  // What an announce says about itself: name, interface, radio, position known
  if (r.info) t.push(`<span class="ann-info">${esc(r.info)}</span>`);
  if (r.kind === "orphan") {
    t.push(r.link_id
      ? `<span class="tag warn" title="Likely link: the first proof of an unheard packet within 15 s">never heard whole; delivered, proof ${dur(r.link_delay_ms)} later</span>`
      : `<span class="tag bad">never heard whole</span>`);
  }
  if (r.proves_unheard) {
    t.push(r.link_id
      ? `<span class="tag warn" title="Likely link: the latest partly heard transmission within 15 s before">proves a packet only partly heard, ${dur(r.link_delay_ms)} earlier</span>`
      : `<span class="tag warn">proves a packet no observer heard</span>`);
  }
  if (r.partials) t.push(`<span class="tag warn">${r.partials} half${r.partials > 1 ? "ves" : ""} heard alone</span>`);
  if (r.max_delay_ms) t.push(`last copy after ${dur(r.max_delay_ms)}`);
  if (r.repeat) t.push(`<span class="tag">repeat</span>`);
  if (r.via) t.push(`via <span class="mono">${short(r.via)}</span>`);
  return t.join(" ");
}
function toggleCell(r) {
  const more = r.receptions - 1;
  if (more < 1) return "";
  const who = r.observers > 1 ? `, ${r.observers} obs` : "";
  return `<button class="expand" aria-expanded="${feed.open.has(rowKey(r))}" title="Show every observation">▸ +${more}${who}</button>`;
}
function summaryRow(r) {
  const tr = document.createElement("tr");
  tr.className = "summary";
  const obs = r.kind === "orphan" && r.observers > 1 ? `${esc(r.observer)} <span class="muted">+${r.observers - 1}</span>` : esc(r.observer);
  const code = framesCode(r);
  tr.innerHTML = `<td class="tog">${toggleCell(r)}</td>
    <td class="mono">${clock(r.t)}</td>
    <td class="obs">${obs}</td>
    <td class="fr"><span class="frames ${code}" title="${FRAMES_TITLE[code]}">${code}</span></td>
    <td><span class="type ${rowType(r)}">${r.kind === "orphan" ? "ORPHAN" : r.type}</span></td>
    <td>${destCell(r)}</td>
    <td class="num">${r.hops ?? "–"}</td>
    <td>${fromCell(r.tx, r.tx_name)}</td>
    <td class="num">${num(r.rssi, 0)}</td>
    <td class="num">${num(r.snr, 2)}</td>
    <td class="num">${r.len}</td>
    <td>${hashCell(r)}</td>
    <td class="notes">${notes(r)}</td>`;
  return tr;
}
function observationRow(o) {
  const tr = document.createElement("tr");
  tr.className = "obsrow";
  const code = o.frames;
  const extra = [];
  if (o.part) extra.push(esc(o.part) + " only");
  if (o.via) extra.push(`via <span class="mono">${short(o.via)}</span>`);
  tr.innerHTML = `<td></td>
    <td class="mono">${clock(o.t)}${o.delay_ms ? ` <span class="muted">+${dur(o.delay_ms)}</span>` : ""}</td>
    <td class="obs">${esc(o.observer)}</td>
    <td class="fr"><span class="frames ${code}" title="${FRAMES_TITLE[code]}">${code}</span></td>
    <td></td><td></td>
    <td class="num">${o.hops ?? "–"}</td>
    <td>${fromCell(o.tx, o.tx_name)}</td>
    <td class="num">${num(o.rssi, 0)}</td>
    <td class="num">${num(o.snr, 2)}</td>
    <td></td><td></td>
    <td class="notes">${extra.join(" ")}</td>`;
  return tr;
}
// A row group is a <tbody>: summary line first, then any observation lines
function buildGroup(r) {
  const tb = document.createElement("tbody");
  tb.dataset.key = rowKey(r);
  tb.dataset.type = rowType(r);
  tb.dataset.t = ms(r.t);
  tb.append(summaryRow(r));
  tb.hidden = !typeFilter().has(tb.dataset.type);
  return tb;
}
async function loadObservations(tb, r) {
  const url = r.kind === "orphan" ? `/api/orphans/${r.id}/observations` : `/api/packets/${r.id}/observations`;
  const list = await api(url);
  [...tb.querySelectorAll("tr.obsrow")].forEach((x) => x.remove());
  // The summary line already shows the first observation
  for (const o of list) if (o.id !== r.first_obs) tb.append(observationRow(o));
}
async function toggleGroup(tb) {
  const key = tb.dataset.key;
  const r = feed.rows.get(key);
  if (!r) return;
  const open = !feed.open.has(key);
  open ? feed.open.add(key) : feed.open.delete(key);
  tb.querySelector("button.expand")?.setAttribute("aria-expanded", open);
  if (open) await loadObservations(tb, r);
  else [...tb.querySelectorAll("tr.obsrow")].forEach((x) => x.remove());
}
// Insert keeping newest-first order by first observation time
function insertGroup(body, tb) {
  const t = Number(tb.dataset.t);
  for (const other of body.children) {
    if (Number(other.dataset.t) < t) { body.insertBefore(tb, other); return; }
  }
  body.append(tb);
}
function upsert(r, flash) {
  const table = $("#feedTable");
  const key = rowKey(r);
  // Orphans heard before their whole packet arrived are now part of it
  for (const id of r.absorbed || []) {
    table.querySelector(`tbody[data-key="orphan:${id}"]`)?.remove();
    feed.rows.delete(`orphan:${id}`);
  }
  // A proof that links back to an orphan group on screen: update that row
  if (r.proves_unheard && r.link_id) {
    const o = feed.rows.get(`orphan:${r.link_id}`);
    if (o && !o.link_id) { o.link_id = r.id; o.link_delay_ms = r.link_delay_ms; upsert({ ...o }, false); }
  }
  const old = table.querySelector(`tbody[data-key="${key}"]`);
  feed.rows.set(key, r);
  // Catch-up overlaps by 1 ms, so the latest row comes back unchanged: only
  // redraw (and flash) when something about the row actually changed
  const sig = [r.last_seen, r.receptions, r.observers, r.partials, r.link_id, r.max_delay_ms].join("|");
  if (old) {
    if (old.dataset.sig !== sig) {
      old.dataset.sig = sig;
      const tr = summaryRow(r);
      old.querySelector("tr.summary").replaceWith(tr);
      if (flash) tr.classList.add("new");
      if (feed.open.has(key)) loadObservations(old, r);
    }
  } else {
    const tb = buildGroup(r);
    tb.dataset.sig = sig;
    if (flash) tb.firstChild.classList.add("new");
    insertGroup(table, tb);
  }
  feed.newest = Math.max(feed.newest, ms(r.last_seen || r.t));
  feed.oldest = Math.min(feed.oldest, ms(r.t));
}
function trim() {
  const table = $("#feedTable");
  const groups = table.querySelectorAll("tbody[data-key]");
  for (let i = groups.length - 1; i >= MAX_ROWS; i--) {
    feed.rows.delete(groups[i].dataset.key);
    groups[i].remove();
  }
  const left = [...feed.rows.values()];
  feed.oldest = left.length ? Math.min(...left.map((r) => ms(r.t))) : Infinity;
}
function applyFilter() {
  const f = typeFilter();
  $$("#feedTable tbody[data-key]").forEach((tb) => (tb.hidden = !f.has(tb.dataset.type)));
}
function clearFeed() {
  $$("#feedTable tbody[data-key]").forEach((tb) => tb.remove());
  feed.rows.clear();
  feed.newest = 0;
  feed.oldest = Infinity;
  $("#olderBtn").disabled = false;
}
async function loadFeed() {
  const q = feed.search ? `&q=${encodeURIComponent(feed.search)}` : "";
  const rows = await api(`/api/feed?limit=150${q}`);
  rows.forEach((r) => upsert(r, false));
  $("#searchNote").textContent = feed.search
    ? rows.length ? `${rows.length} packet${rows.length > 1 ? "s" : ""} with hash ${feed.search}…` : `No packet with hash ${feed.search}…`
    : "";
}
async function catchUp() {
  if (feed.paused || feed.search) return;
  // Rows with any activity since the newest we have: new packets, and new
  // observations of packets already shown (relayed copies, other observers)
  const rows = await api(`/api/feed?after=${feed.newest - 1}&limit=500`);
  rows.forEach((r) => upsert(r, true));
  trim();
}
$("#olderBtn").onclick = async () => {
  if (!isFinite(feed.oldest)) return;
  const q = feed.search ? `&q=${encodeURIComponent(feed.search)}` : "";
  const rows = await api(`/api/feed?before=${feed.oldest}&limit=150${q}`);
  rows.forEach((r) => upsert(r, false));
  if (!rows.length) $("#olderBtn").disabled = true;
};
$("#pauseBtn").onclick = () => {
  feed.paused = !feed.paused;
  $("#pauseBtn").textContent = feed.paused ? "Resume" : "Pause";
  $("#liveDot").classList.toggle("paused", feed.paused);
  if (!feed.paused) catchUp();
};
$("#typeFilter").onchange = applyFilter;
$("#feedTable").addEventListener("click", async (e) => {
  const hash = e.target.closest("button.hash");
  if (hash) {
    try { await navigator.clipboard.writeText(hash.dataset.hash); hash.classList.add("copied"); setTimeout(() => hash.classList.remove("copied"), 1200); } catch {}
    return;
  }
  const tog = e.target.closest("button.expand");
  if (tog) toggleGroup(tog.closest("tbody"));
});
let hashTimer;
$("#hashSearch").oninput = () => {
  clearTimeout(hashTimer);
  hashTimer = setTimeout(() => {
    feed.search = $("#hashSearch").value.trim().toLowerCase().replace(/[^0-9a-f]/g, "");
    clearFeed();
    loadFeed().catch(console.error);
  }, 300);
};

// ---------- live stream ----------
let lastNodesRefresh = 0;
function connect() {
  const es = new EventSource("/api/stream");
  es.onopen = () => { $("#liveDot").classList.add("on"); catchUp(); };
  es.onerror = () => $("#liveDot").classList.remove("on");
  es.addEventListener("data", () => {
    catchUp().catch(console.error);
    throttledRefresh();
  });
}
let refreshTimer = null;
function throttledRefresh() {
  if (refreshTimer) return;
  refreshTimer = setTimeout(() => {
    refreshTimer = null;
    loadSummary().catch(console.error);
    if (currentTab === "nodes" && Date.now() - lastNodesRefresh > 5000) loadNodes();
    if (currentTab === "node" && currentNode && Date.now() - lastNodesRefresh > 5000) loadNodePage(currentNode);
    if (currentTab === "channel") loadChannel();
    if (currentTab === "observers") loadObservers();
    if (currentTab === "map") loadMap();
  }, 2000);
}

// ---------- nodes ----------
// The Nodes tab is a list; a node opens on a page of its own (#node=<id>).
let currentNode = null;
let returnTab = "nodes";
function perHour(n24) {
  if (!n24) return "–";
  const r = n24 / 24;
  return r < 0.1 ? "&lt;0.1" : r.toFixed(1);
}
async function loadNodes() {
  lastNodesRefresh = Date.now();
  const q = $("#nodeSearch").value;
  const rows = await api(`/api/nodes?q=${encodeURIComponent(q)}`);
  $("#nodesEmpty").hidden = rows.length > 0;
  $("#nodesBody").innerHTML = rows
    .map((n) => {
      const name = n.name ? esc(n.name) : `<span class="mono">${short(n.identity, 12)}</span>`;
      const apps = (n.apps || []).map((a) => `<span class="tag">${esc(a.replace(/^rnstransport\./, ""))}</span>`).join("");
      return `<tr data-id="${n.identity}">
        <td>${name}<div class="muted mono">${short(n.identity, 16)}</div></td>
        <td>${apps || '<span class="muted">relay only</span>'}</td>
        <td title="${new Date(n.last_seen).toLocaleString()}">${ago(n.last_seen)}</td>
        <td class="num">${num(n.last_rssi, 0)}</td>
        <td class="num">${num(n.last_snr, 2)}</td>
        <td class="num">${num(n.avg_rssi_24h, 0)}</td>
        <td class="num" title="${n.announces_24h} announce${n.announces_24h === 1 ? "" : "s"} in the last 24 h">${perHour(n.announces_24h)}</td>
        <td class="num">${n.announces}</td>
        <td class="num">${n.min_hops ?? "–"}</td></tr>`;
    })
    .join("");
}
let searchTimer;
$("#nodeSearch").oninput = () => { clearTimeout(searchTimer); searchTimer = setTimeout(loadNodes, 200); };
$("#nodesBody").addEventListener("click", (e) => {
  const tr = e.target.closest("tr[data-id]");
  if (tr) openNode(tr.dataset.id);
});

// openNode goes to the node's page; the browser's Back button returns.
function openNode(id) {
  if (location.hash === `#node=${id}`) showNodePage(id);
  else location.hash = `node=${id}`;
}
function showNodePage(id) {
  if (currentTab !== "node") returnTab = currentTab;
  currentNode = id;
  showTab("node");
  scrollTo(0, 0);
  loadNodePage(id);
}
$("#nodeBack").onclick = () => {
  if (history.length > 1 && location.hash.startsWith("#node=")) history.back();
  else showTab(returnTab);
};

const nodeCharts = {};
const OBS_COLORS = ["--s1", "--s2", "--s3"]; // categorical slots, fixed order
async function loadNodePage(id) {
  let d;
  try { d = await api(`/api/nodes/${id}`); } catch {
    $("#nodePage").innerHTML = `<p class="empty">No node with identity <span class="mono">${esc(id)}</span>.</p>`;
    return;
  }
  if (currentNode !== id) return;
  lastNodesRefresh = Date.now();
  // A refresh rebuilds the page and its charts are drawn just after, so for a
  // moment it is shorter and the browser would move the scroll position. Hold
  // the old height while it rebuilds, then put the scroll position back.
  const page = $("#nodePage");
  const refresh = page.dataset.node === id;
  const scrollY = window.scrollY;
  if (refresh) page.style.minHeight = `${page.offsetHeight}px`;
  page.dataset.node = id;
  const kv = (k, v, title = "") => `<div${title ? ` title="${esc(title)}"` : ""}><div class="k">${k}</div><div class="v">${v}</div></div>`;
  const placed = (d.dests || []).find((x) => x.lat != null);
  const apps = (d.apps || []).map((a) => `<span class="tag">${esc(a.replace(/^rnstransport\./, ""))}</span>`).join("");
  page.innerHTML = `
    <header class="node-head">
      <h2>${d.name ? esc(d.name) : "Unnamed node"}</h2>
      <div class="mono muted">${d.identity}</div>
      <div>${apps || '<span class="muted">seen only as a relay</span>'}</div>
      ${placed ? `<div class="small">${num(placed.lat, 5)}, ${num(placed.lon, 5)}${placed.height != null ? ` · ${num(placed.height, 0)} m` : ""}
        <button class="link" data-map="1" data-lat="${placed.lat}" data-lon="${placed.lon}">show on map</button></div>` : ""}
    </header>
    <div class="kv kv4">
      ${kv("Last heard", ago(d.last_seen), new Date(d.last_seen).toLocaleString())}
      ${kv("Last RSSI / SNR", `${num(d.last_rssi, 0)} dBm / ${num(d.last_snr, 2)} dB`)}
      ${kv("24 h average", `${num(d.avg_rssi_24h, 0)} dBm / ${num(d.avg_snr_24h, 1)} dB`)}
      ${kv("Announces per hour", perHour(d.announces_24h), `${d.announces_24h} in the last 24 h`)}
      ${kv("Announces, all", d.announces)}
      ${kv("Fewest hops", d.min_hops ?? "–")}
      ${kv("Receptions attributed", d.heard_direct)}
      ${kv("Announces relayed", d.relayed, "Other nodes' announces this one rebroadcast")}
    </div>
    <div class="node-grid">
      <section class="card"><h3>Announces per hour, last 24 h</h3><div class="chart" id="annChart"></div></section>
      <section class="card"><h3>How each observer hears it, last 24 h</h3>
        <div class="table-wrap"><table><thead><tr><th>Observer</th><th class="num">Receptions</th>
          <th class="num">RSSI avg</th><th class="num">min</th><th class="num">max</th><th class="num">SNR avg</th><th class="num">min</th><th>Last</th></tr></thead>
        <tbody>${(d.per_observer || []).map((o) => `<tr><td>${esc(o.observer)}</td><td class="num">${o.count}</td>
          <td class="num">${num(o.rssi_avg, 0)}</td><td class="num">${num(o.rssi_min, 0)}</td><td class="num">${num(o.rssi_max, 0)}</td>
          <td class="num">${num(o.snr_avg, 1)}</td><td class="num">${num(o.snr_min, 1)}</td><td>${ago(o.last)}</td></tr>`).join("") ||
          '<tr><td colspan="8" class="muted">Nothing attributed to this node in the last 24 h.</td></tr>'}</tbody></table></div>
      </section>
    </div>
    <section class="card"><h3>Signal by observer, last 7 days</h3>
      <div class="chart" id="rssiChart"></div>
      <div class="chart" id="snrChart"></div>
    </section>
    <h3>Destinations</h3>
    <div class="table-wrap"><table><thead><tr><th>Hash</th><th>App</th><th>Name</th><th>Announce says</th><th class="num">Announces</th><th class="num">Hops</th><th>Last seen</th></tr></thead>
    <tbody>${(d.dests || []).map((x) => `<tr><td class="mono">${short(x.hash, 16)}</td><td>${esc(x.app)}</td><td>${esc(x.name)}</td><td class="notes">${esc(x.info || "")}</td>
      <td class="num">${x.announces}</td><td class="num">${x.last_hops ?? "–"}</td><td>${ago(x.last_seen)}</td></tr>`).join("") ||
      '<tr><td colspan="7" class="muted">Seen only as a relay (its transport ID); it has not announced a destination here.</td></tr>'}</tbody></table></div>
    <h3>Announce history</h3>
    <div class="table-wrap"><table><thead><tr><th>Time</th><th>App</th><th class="num">Hops</th><th>Signature</th><th class="num">Copies heard</th><th class="num">Last relay after</th></tr></thead>
    <tbody>${(d.announce_history || []).map((a) => `<tr><td>${new Date(a.t).toLocaleString()}</td><td>${esc(a.app.replace(/^rnstransport\./, ""))}</td>
      <td class="num">${a.hops}</td><td>${a.valid === "valid" ? "valid" : `<span class="tag bad">${esc(a.valid)}</span>`}</td>
      <td class="num">${a.copies}</td><td class="num">${a.copies > 1 ? dur(a.max_delay) : "–"}</td></tr>`).join("") ||
      '<tr><td colspan="6" class="muted">No announces from this identity.</td></tr>'}</tbody></table></div>`;
  $$("[data-map]", $("#nodePage")).forEach((b) => (b.onclick = async () => {
    showTab("map");
    await loadMap();
    geo.zoom = 14;
    const p = project(Number(b.dataset.lat), Number(b.dataset.lon), geo.zoom);
    geo.cx = p.x; geo.cy = p.y; geo.fitted = true;
    drawMap();
  }));

  // Charts. Signal: one series per observer, in a fixed colour order; past
  // three, the rest share an "other observers" series.
  const rate = d.announce_rate || [];
  const t0 = rate.length ? ms(rate[0].hour) : Date.now() - 23 * 3600e3;
  nodeCharts.ann = { el: "#annChart", opts: { title: "Announces", unit: "per hour", mode: "bar", yMin: 0, minSpan: 4,
    domain: [t0, t0 + 24 * 3600e3], series: [{ name: "Announces", color: "--s2", pts: rate.map((h) => ({ t: ms(h.hour), v: h.count })) }] } };
  const sig = d.signal || [];
  const obsOrder = [...new Set((d.per_observer || []).map((o) => o.observer).concat(sig.map((p) => p.observer)))];
  const seriesFor = (field) => {
    const top = obsOrder.slice(0, 3).map((o, i) => ({ name: o, color: OBS_COLORS[i],
      pts: sig.filter((p) => p.observer === o).map((p) => ({ t: ms(p.t), v: p[field], note: p.type })) }));
    const rest = sig.filter((p) => !obsOrder.slice(0, 3).includes(p.observer));
    if (rest.length) top.push({ name: "other observers", color: "--text-3", pts: rest.map((p) => ({ t: ms(p.t), v: p[field], note: `${p.type} · ${p.observer}` })) });
    return top;
  };
  nodeCharts.rssi = { el: "#rssiChart", opts: { title: "RSSI", unit: "dBm", mode: "dot", minSpan: 10, series: seriesFor("rssi") } };
  nodeCharts.snr = { el: "#snrChart", opts: { title: "SNR", unit: "dB", mode: "dot", minSpan: 5, series: seriesFor("snr") } };
  Object.values(nodeCharts).forEach(drawChart);
  if (refresh) {
    page.style.minHeight = "";
    window.scrollTo(0, scrollY);
  }
}

// ---------- map ----------
// A small slippy map: Web Mercator, OpenStreetMap tiles (optional), markers
// for nodes with a known position and for observers. No library needed.
const TILE = 256;
const geo = { zoom: 12, cx: 0, cy: 0, fitted: false, data: null, tiles: store("rradmon.tiles") !== "off" };
function project(lat, lon, z) {
  const s = TILE * 2 ** z;
  const sin = Math.sin((Math.max(-85, Math.min(85, lat)) * Math.PI) / 180);
  return { x: ((lon + 180) / 360) * s, y: (0.5 - Math.log((1 + sin) / (1 - sin)) / (4 * Math.PI)) * s };
}
function fitMap() {
  const d = geo.data;
  const pts = [...(d?.nodes || []), ...(d?.observers || [])];
  const el = $("#map");
  if (!pts.length || !el.clientWidth) return;
  for (let z = 17; z >= 2; z--) {
    const ps = pts.map((p) => project(p.lat, p.lon, z));
    const xs = ps.map((p) => p.x), ys = ps.map((p) => p.y);
    const w = Math.max(...xs) - Math.min(...xs), h = Math.max(...ys) - Math.min(...ys);
    if (w < el.clientWidth - 120 && h < el.clientHeight - 120) {
      geo.zoom = Math.min(z, 15);
      const zz = geo.zoom;
      const pz = pts.map((p) => project(p.lat, p.lon, zz));
      geo.cx = (Math.min(...pz.map((p) => p.x)) + Math.max(...pz.map((p) => p.x))) / 2;
      geo.cy = (Math.min(...pz.map((p) => p.y)) + Math.max(...pz.map((p) => p.y))) / 2;
      break;
    }
  }
  geo.fitted = true;
  drawMap();
}
function setZoom(z, ax, ay) {
  // Zoom about a screen point (ax, ay), keeping it fixed
  const el = $("#map");
  z = Math.max(2, Math.min(18, z));
  if (z === geo.zoom) return;
  ax ??= el.clientWidth / 2; ay ??= el.clientHeight / 2;
  const wx = geo.cx - el.clientWidth / 2 + ax, wy = geo.cy - el.clientHeight / 2 + ay;
  const k = 2 ** (z - geo.zoom);
  geo.cx = wx * k - ax + el.clientWidth / 2;
  geo.cy = wy * k - ay + el.clientHeight / 2;
  geo.zoom = z;
  drawMap();
}
function recency(t) {
  const age = Date.now() - ms(t);
  return age < 3600e3 ? "fresh" : age < 86400e3 ? "day" : "old";
}
const RECENCY_TEXT = { fresh: "heard in the last hour", day: "heard in the last day", old: "not heard for over a day" };
function drawMap() {
  const el = $("#map");
  const W = el.clientWidth, H = el.clientHeight;
  if (!W) return;
  const z = geo.zoom, n = 2 ** z;
  const left = geo.cx - W / 2, top = geo.cy - H / 2;
  let html = "";
  if (geo.tiles) {
    const x0 = Math.floor(left / TILE), x1 = Math.floor((left + W) / TILE);
    const y0 = Math.max(0, Math.floor(top / TILE)), y1 = Math.min(n - 1, Math.floor((top + H) / TILE));
    for (let ty = y0; ty <= y1; ty++) {
      for (let tx = x0; tx <= x1; tx++) {
        const wx = ((tx % n) + n) % n;
        html += `<img class="tile" alt="" draggable="false" style="left:${tx * TILE - left}px;top:${ty * TILE - top}px"
          src="https://tile.openstreetmap.org/${z}/${wx}/${ty}.png">`;
      }
    }
  }
  const d = geo.data || { nodes: [], observers: [] };
  for (const o of d.observers) {
    const p = project(o.lat, o.lon, z);
    html += `<div class="mk obs ${esc(o.state)}" style="left:${p.x - left}px;top:${p.y - top}px"
      data-tip="${esc(`Observer ${o.ext_id} · ${o.state}`)}"><span class="lbl">${esc(o.ext_id)}</span></div>`;
  }
  for (const nd of d.nodes) {
    const p = project(nd.lat, nd.lon, z);
    const rec = recency(nd.last_seen);
    const name = nd.name || short(nd.identity, 12);
    const tip = [name, RECENCY_TEXT[rec], nd.last_rssi != null ? `last ${num(nd.last_rssi, 0)} dBm / ${num(nd.last_snr, 1)} dB` : "",
      nd.height != null ? `${num(nd.height, 0)} m` : "", `position from ${nd.pos_source} announce`].filter(Boolean).join(" · ");
    html += `<button class="mk node ${rec}" style="left:${p.x - left}px;top:${p.y - top}px"
      data-id="${nd.identity}" data-tip="${esc(tip)}"><span class="lbl">${esc(name)}</span></button>`;
  }
  $("#mapLayer").innerHTML = html;
  $("#mapZoom").textContent = `zoom ${z}`;
}
async function loadMap() {
  geo.data = await api("/api/map");
  const d = geo.data;
  $("#mapSummary").innerHTML = `${d.nodes.length} node${d.nodes.length === 1 ? "" : "s"} placed` +
    (d.unplaced ? ` · ${d.unplaced} without a known position` : "") +
    ` · ${d.observers.length} observer${d.observers.length === 1 ? "" : "s"} placed`;
  $("#mapList").innerHTML = d.nodes.map((nd) => `<li><button class="link" data-id="${nd.identity}">${esc(nd.name || short(nd.identity, 12))}</button>
    <span class="muted small">${esc(nd.info || "")}</span></li>`).join("") || '<li class="muted">No node has announced a position yet. Interface-discovery announces carry one.</li>';
  if (!geo.fitted) fitMap(); else drawMap();
}
(function mapEvents() {
  const el = $("#map");
  let drag = null;
  el.addEventListener("pointerdown", (e) => {
    if (e.target.closest(".mk, .map-ctl")) return;
    drag = { x: e.clientX, y: e.clientY, cx: geo.cx, cy: geo.cy };
    el.setPointerCapture(e.pointerId);
    el.classList.add("dragging");
  });
  el.addEventListener("pointermove", (e) => {
    if (drag) {
      geo.cx = drag.cx - (e.clientX - drag.x);
      geo.cy = drag.cy - (e.clientY - drag.y);
      drawMap();
      return;
    }
    const mk = e.target.closest(".mk");
    const tip = $("#tip");
    if (mk) {
      tip.textContent = mk.dataset.tip;
      tip.hidden = false;
      tip.style.left = `${Math.min(e.clientX + 14, innerWidth - tip.offsetWidth - 8)}px`;
      tip.style.top = `${e.clientY + 14}px`;
    } else tip.hidden = true;
  });
  const end = () => { drag = null; el.classList.remove("dragging"); };
  el.addEventListener("pointerup", end);
  el.addEventListener("pointercancel", end);
  el.addEventListener("pointerleave", () => ($("#tip").hidden = true));
  el.addEventListener("wheel", (e) => {
    e.preventDefault();
    const r = el.getBoundingClientRect();
    setZoom(geo.zoom + (e.deltaY < 0 ? 1 : -1), e.clientX - r.left, e.clientY - r.top);
  }, { passive: false });
  el.addEventListener("click", (e) => {
    const mk = e.target.closest(".mk.node");
    if (mk) openNode(mk.dataset.id);
  });
  // Offline or blocked tiles: drop them and let the grid show (inline
  // onerror handlers would be blocked by the page's content security policy)
  $("#mapLayer").addEventListener("error", (e) => { if (e.target.tagName === "IMG") e.target.remove(); }, true);
  $("#mapIn").onclick = () => setZoom(geo.zoom + 1);
  $("#mapOut").onclick = () => setZoom(geo.zoom - 1);
  $("#mapFit").onclick = () => fitMap();
  const tiles = $("#mapTiles");
  tiles.checked = geo.tiles;
  tiles.onchange = () => { geo.tiles = tiles.checked; store("rradmon.tiles", geo.tiles ? "on" : "off"); drawMap(); };
  $("#mapList").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-id]");
    if (b) openNode(b.dataset.id);
  });
  addEventListener("resize", () => { if (currentTab === "map") drawMap(); });
})();

// ---------- channel ----------
let chanHours = Number(store("rradmon.chanHours")) || 1;
const chanCharts = {};
$$("#chanRange button").forEach((b) => {
  b.setAttribute("aria-pressed", Number(b.dataset.h) === chanHours);
  b.onclick = () => {
    chanHours = Number(b.dataset.h);
    store("rradmon.chanHours", chanHours);
    $$("#chanRange button").forEach((x) => x.setAttribute("aria-pressed", x === b));
    loadChannel();
  };
});
$("#obsSelect").onchange = loadChannel;

// ---------- observers ----------
function uptime(s) {
  if (s == null) return "–";
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d ? `${d} d ${h} h` : h ? `${h} h ${m} min` : `${m} min`;
}
const STATE_TEXT = { online: "Online", silent: "No data", offline: "Offline" };
async function loadObservers() {
  const list = await api("/api/observers");
  $("#obsEmpty").hidden = list.length > 0;
  $("#obsGrid").innerHTML = list.map((o) => {
    const st = o.status || {};
    const src = o.source === "mqtt" ? "MQTT" : o.source === "rnode-serial" ? "Local RNode" : esc(o.source);
    const kv = (k, v, title = "") => `<div${title ? ` title="${esc(title)}"` : ""}><div class="k">${k}</div><div class="v">${v}</div></div>`;
    const sig = o.avg_rssi_1h == null ? "–" : `${num(o.avg_rssi_1h, 0)} dBm / ${num(o.avg_snr_1h, 1)} dB`;
    const util = o.util_s == null ? "–" : `${num(o.util_s, 1)} % / ${num(o.util_l, 2)} %`;
    const problems = [];
    if (o.orphans_24h) problems.push(`${o.orphans_24h} orphan${o.orphans_24h > 1 ? "s" : ""}`);
    if (o.partial_24h) problems.push(`${o.partial_24h} partial`);
    if (o.mqtt && o.mqtt.seq_gaps) problems.push(`${o.mqtt.seq_gaps} lost over MQTT`);
    if (o.mqtt && o.mqtt.invalid) problems.push(`${o.mqtt.invalid} rejected`);
    if (o.mqtt && o.mqtt.rate_limited) problems.push(`${o.mqtt.rate_limited} rate-limited`);
    if (st.rx_errors) problems.push(`${st.rx_errors} radio errors`);
    if (st.mqtt_dropped) problems.push(`${st.mqtt_dropped} dropped by observer`);
    const extra = [];
    // Below 2.5 V there is no battery fitted (the divider reads near zero on USB power)
    if (st.battery_mv >= 2500) extra.push(kv("Battery", `${(st.battery_mv / 1000).toFixed(2)} V`));
    // A local RNode reports no uptime; show time since Rradmon brought it online
    let up = st.uptime_s;
    if (up == null && o.state === "online" && o.source === "rnode-serial" && o.status_t) up = (Date.now() - ms(o.status_t)) / 1000;
    if (st.rssi_wifi) extra.push(kv("Wi-Fi", `${st.rssi_wifi} dBm`));
    if (o.lat != null && o.lon != null) extra.push(kv("Position", `${num(o.lat, 4)}, ${num(o.lon, 4)}`));
    return `<article class="obs-card">
      <header>
        <span class="state ${esc(o.state)}"></span>
        <h3>${esc(o.ext_id)}</h3>
        <span class="tag">${src}</span>
        <span class="state-text">${STATE_TEXT[o.state] || esc(o.state)}</span>
      </header>
      <div class="muted small">${esc(o.firmware) || "firmware not reported"}${o.software ? " · " + esc(o.software) : ""}</div>
      <div class="kv">
        ${kv("Last heard", ago(o.last_seen), new Date(o.last_seen).toLocaleString())}
        ${kv("Uptime", uptime(up))}
        ${kv("Received 1 h / 24 h", `${o.rx_1h} / ${o.rx_24h}`)}
        ${kv("Only here, 24 h", `${o.only_here_24h} of ${o.packets_24h}`, "Packets no other observer heard")}
        ${kv("Signal, 1 h avg", sig)}
        ${kv("Utilisation short / long", util)}
        ${kv("Noise floor", o.noise_floor == null ? "–" : `${num(o.noise_floor, 0)} dBm`)}
        ${extra.join("")}
      </div>
      <div class="obs-foot">
        <span class="${problems.length ? "" : "muted"}">${problems.length ? problems.join(" · ") : "No losses or errors recorded"}</span>
        ${o.chan_t ? `<button class="link" data-chan="${o.id}">Channel history</button>` : ""}
      </div>
    </article>`;
  }).join("");
  $$("#obsGrid [data-chan]").forEach((b) => (b.onclick = () => {
    $("#obsSelect").value = b.dataset.chan;
    showTab("channel");
  }));
}
async function loadChannel() {
  const id = $("#obsSelect").value || (observers[0] && observers[0].id);
  if (!id) return;
  const pts = await api(`/api/channel/${id}?hours=${chanHours}`);
  const now = Date.now();
  const domain = [now - chanHours * 3600e3, now];
  chanCharts.util = { el: "#utilChart", opts: { title: "Utilisation", unit: "%", mode: "line", yMin: 0, minSpan: 5, domain, series: [
    { name: "Short-term", color: "--s1", pts: pts.map((p) => ({ t: ms(p.t), v: p.util_s })) },
    { name: "Long-term", color: "--s2", pts: pts.map((p) => ({ t: ms(p.t), v: p.util_l })) },
  ] } };
  chanCharts.noise = { el: "#noiseChart", opts: { unit: "dBm", mode: "line", minSpan: 10, domain, series: [
    { name: "Noise floor", color: "--s1", pts: pts.map((p) => ({ t: ms(p.t), v: p.noise_floor })) },
  ] } };
  drawChart(chanCharts.util);
  drawChart(chanCharts.noise);
}

function redrawCharts() {
  Object.values(nodeCharts).concat(Object.values(chanCharts)).forEach((c) => { if ($(c.el)) drawChart(c); });
}
let resizeTimer;
addEventListener("resize", () => { clearTimeout(resizeTimer); resizeTimer = setTimeout(redrawCharts, 150); });

// ---------- chart ----------
// One y-axis, time on x. mode "line" (gaps where data stops) or "dot".
function niceTicks(lo, hi, n = 4) {
  if (lo === hi) { lo -= 1; hi += 1; }
  const raw = (hi - lo) / n;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw);
  const start = Math.floor(lo / step) * step;
  const ticks = [];
  for (let v = start; v <= hi + step * 1e-9; v += step) ticks.push(+v.toFixed(10));
  if (ticks[ticks.length - 1] < hi) ticks.push(ticks[ticks.length - 1] + step);
  return ticks;
}
function timeTicks(t0, t1) {
  const span = t1 - t0;
  const steps = [60e3, 300e3, 600e3, 1800e3, 3600e3, 3 * 3600e3, 6 * 3600e3, 12 * 3600e3, 86400e3];
  const step = steps.find((s) => span / s <= 6) || 86400e3;
  const off = new Date().getTimezoneOffset() * 60e3;
  const ticks = [];
  for (let t = Math.ceil((t0 - off) / step) * step + off; t <= t1; t += step) ticks.push(t);
  const fmt = (t) => {
    const d = new Date(t);
    return step >= 86400e3 ? `${d.getDate()}/${d.getMonth() + 1}` : `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  };
  return ticks.map((t) => ({ t, label: fmt(t) }));
}
function drawChart({ el, opts }) {
  const host = $(el);
  if (!host) return;
  const all = opts.series.flatMap((s) => s.pts.filter((p) => p.v != null));
  const legend = opts.series.length > 1
    ? `<div class="legend">${opts.series.map((s) => `<span style="--c:var(${s.color})">${esc(s.name)}</span>`).join("")}</div>`
    : opts.title ? `<div class="legend"><span style="--c:var(${opts.series[0].color})">${esc(opts.title)} (${esc(opts.unit)})</span></div>` : "";
  if (!all.length) { host.innerHTML = legend + `<div class="nodata">No data in this range yet.</div>`; return; }

  const W = Math.max(280, host.clientWidth), H = 170, m = { l: 46, r: 12, t: 8, b: 22 };
  const [t0, t1] = opts.domain || [Math.min(...all.map((p) => p.t)), Math.max(...all.map((p) => p.t))];
  const pad5 = (t1 - t0) * 0.03 || 60e3;
  const x0 = opts.domain ? t0 : t0 - pad5, x1 = opts.domain ? t1 : t1 + pad5;
  let lo = Math.min(...all.map((p) => p.v)), hi = Math.max(...all.map((p) => p.v));
  if (opts.yMin != null) lo = Math.min(lo, opts.yMin);
  if (opts.minSpan && hi - lo < opts.minSpan) {
    // Keep small changes from looking dramatic: centre a minimum span.
    const mid = (hi + lo) / 2;
    lo = mid - opts.minSpan / 2;
    hi = mid + opts.minSpan / 2;
    if (opts.yMin != null && lo < opts.yMin) { hi += opts.yMin - lo; lo = opts.yMin; }
  }
  const yt = niceTicks(lo, hi);
  const y0 = yt[0], y1 = yt[yt.length - 1];
  const X = (t) => m.l + ((t - x0) / (x1 - x0)) * (W - m.l - m.r);
  const Y = (v) => H - m.b - ((v - y0) / (y1 - y0 || 1)) * (H - m.t - m.b);

  let svg = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="${esc(opts.title || opts.series.map((s) => s.name).join(", "))}">`;
  svg += `<g class="grid">${yt.map((v) => `<line x1="${m.l}" x2="${W - m.r}" y1="${Y(v)}" y2="${Y(v)}"/>`).join("")}</g>`;
  svg += `<g class="axis">${yt.map((v) => `<text x="${m.l - 6}" y="${Y(v) + 4}" text-anchor="end">${v}</text>`).join("")}`;
  svg += timeTicks(x0, x1).map((k) => `<text x="${X(k.t)}" y="${H - 6}" text-anchor="middle">${k.label}</text>`).join("");
  svg += `</g>`;

  for (const s of opts.series) {
    const pts = s.pts.filter((p) => p.v != null).sort((a, b) => a.t - b.t);
    if (opts.mode === "line") {
      // Break the line where reports stop for more than 5x the usual spacing.
      const gaps = pts.slice(1).map((p, i) => p.t - pts[i].t).sort((a, b) => a - b);
      const typical = gaps.length ? gaps[Math.floor(gaps.length / 2)] : 0;
      let d = "";
      pts.forEach((p, i) => {
        const brk = i === 0 || p.t - pts[i - 1].t > Math.max(typical * 5, 10e3);
        d += `${brk ? "M" : "L"}${X(p.t).toFixed(1)},${Y(p.v).toFixed(1)}`;
      });
      svg += `<path d="${d}" fill="none" style="stroke:var(${s.color})" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>`;
    } else if (opts.mode === "bar") {
      // One bar per hour, anchored at zero, with a 2px gap between bars
      const slot = X(x0 + 3600e3) - X(x0);
      svg += pts.filter((p) => p.v > 0).map((p) => {
        const top = Y(p.v), base = Y(0);
        return `<rect x="${(X(p.t) + 1).toFixed(1)}" y="${top.toFixed(1)}" width="${Math.max(1, slot - 2).toFixed(1)}"
          height="${(base - top).toFixed(1)}" rx="2" style="fill:var(${s.color})"/>`;
      }).join("");
    } else {
      svg += pts.map((p) => `<circle cx="${X(p.t).toFixed(1)}" cy="${Y(p.v).toFixed(1)}" r="4" style="fill:var(${s.color});stroke:var(--surface)" stroke-width="2"/>`).join("");
    }
  }
  svg += `<line class="xh" x1="0" x2="0" y1="${m.t}" y2="${H - m.b}" style="stroke:var(--text-3)" stroke-width="1" visibility="hidden"/>`;
  svg += `<rect x="${m.l}" y="${m.t}" width="${W - m.l - m.r}" height="${H - m.t - m.b}" fill="transparent"/></svg>`;
  host.innerHTML = legend + svg;

  // Hover: nearest point in time (lines) or in distance (dots).
  const svgEl = $("svg", host), xh = $(".xh", host), tip = $("#tip");
  const flat = opts.series.flatMap((s) => s.pts.filter((p) => p.v != null).map((p) => ({ ...p, s })));
  svgEl.onmousemove = (e) => {
    const r = svgEl.getBoundingClientRect();
    const k = W / r.width;
    const mx = (e.clientX - r.left) * k, my = (e.clientY - r.top) * k;
    let best = null, bd = Infinity;
    for (const p of flat) {
      const d = opts.mode === "line" ? Math.abs(X(p.t) - mx)
        : opts.mode === "bar" ? Math.abs(X(p.t + 1800e3) - mx)
        : Math.hypot(X(p.t) - mx, Y(p.v) - my);
      if (d < bd) { bd = d; best = p; }
    }
    if (!best || bd > (opts.mode === "line" || opts.mode === "bar" ? 40 : 24)) { tip.hidden = true; xh.setAttribute("visibility", "hidden"); return; }
    xh.setAttribute("x1", X(best.t)); xh.setAttribute("x2", X(best.t)); xh.setAttribute("visibility", "visible");
    const same = opts.mode === "line" ? flat.filter((p) => p.t === best.t) : [best];
    const when = opts.mode === "bar" ? `${new Date(best.t).toLocaleString()} – ${pad(new Date(best.t + 3600e3).getHours())}:00` : new Date(best.t).toLocaleString();
    tip.innerHTML = `<div class="muted">${when}</div>` +
      same.map((p) => `<div><span class="sw" style="background:var(${p.s.color})"></span>${esc(p.s.name)} <b>${num(p.v, opts.mode === "bar" ? 0 : opts.unit === "dB" || opts.unit === "%" ? 2 : 0)} ${esc(opts.unit)}</b>${p.note ? ` <span class="muted">${esc(p.note)}</span>` : ""}</div>`).join("");
    tip.hidden = false;
    const tx = Math.min(e.clientX + 14, innerWidth - tip.offsetWidth - 8);
    tip.style.left = `${tx}px`;
    tip.style.top = `${e.clientY + 14}px`;
  };
  svgEl.onmouseleave = () => { tip.hidden = true; xh.setAttribute("visibility", "hidden"); };
}

// ---------- start ----------
addEventListener("hashchange", () => {
  const m = location.hash.match(/^#node=([0-9a-f]{32})$/);
  if (m) showNodePage(m[1]);
  else if (currentTab === "node") showTab(returnTab);
});
(async function start() {
  await loadSummary().catch(console.error);
  await loadFeed().catch(console.error);
  const m = location.hash.match(/^#node=([0-9a-f]{32})$/);
  if (m) showNodePage(m[1]);
  else showTab(store("rradmon.tab") || "live");
  connect();
  setInterval(() => loadSummary().catch(console.error), 30000);
})();
