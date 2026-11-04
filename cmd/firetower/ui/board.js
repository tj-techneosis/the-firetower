// The Firetower's built-in board. It reads /api/feed (the whole state, sent
// on every change and as a heartbeat) and draws it; its buttons POST to
// /api/seen|done|rm. A page of your own can do the same (firetower board --ui
// DIR).
//
// The key arrives in the address (?key=, from firetower board --open or
// --url). It is kept in this origin's localStorage, which no other port or
// site can read, sent as a Bearer header (or ?key= where a browser can't add
// headers), and taken out of the address bar. A key in the address replaces
// the saved one only once the board has accepted it.
//
// The page is rebuilt only when what it shows changes. The heartbeat and the
// clock only rewrite the times, in place, so focus, a half-done click or a
// text selection is never taken away.
//
// The address chooses the view (?view=place|tag|attention, and ?tag=T to
// show only what carries a tag), so a bar, a bookmark or a small window can
// open straight to one: ?view=attention is the "needs a look" list.
//
// Everything a check wrote goes in with textContent, never as HTML.
"use strict";

const WORD = { green: "clear", yellow: "smoke", red: "fire", done: "stood down", stale: "no word",
  waiting: "waiting", lost: "out of contact" };
const RANK = { done: 0, green: 1, seen: 1.5, waiting: 2, yellow: 3, stale: 4, lost: 4, red: 5 };
const ORDER = ["red", "lost", "stale", "yellow", "waiting", "seen", "green", "done"];
const COUNTED = { red: "fire", stale: "no word", yellow: "smoke", waiting: "waiting",
  seen: "seen", green: "clear", done: "stood down" };
const COLOR = { green: "--clear", yellow: "--smoke", red: "--fire", stale: "--noword", lost: "--noword",
  waiting: "--waiting", done: "--down", seen: "--waiting", none: "--down" };
const STORE = "firetower-key";
const ARM_FOR = 8000; // how long a first press of Remove waits for the second
const QUIET = 45000; // no word from the feed this long (it beats every 15s): reconnect

let KEY = "";
let storageOK = true;
let state = null;
let shown = ""; // what the page was last built from, minus the clock
let connected = false;
let blocked = false; // turned away: the board shows how to get in, and nothing else
let es = null;
let heard = 0;
const notes = new Map(); // id -> {text, info}: what a button press said, under its row until it changes
const alerted = new Set(); // notes already read out
const inflight = new Set(); // ids with a button press on its way
let armed = null; // the Remove waiting for its second press: its row's key
let armedAt = 0;
let armTimer = 0;
let pendingFocus = null; // where focus goes after an action's redraw
let openRow = null; // the row whose buttons show on a touch screen (its key)
const VIEWS = ["place", "tag", "attention"];
let view = "place"; // how the board is laid out: by district and tower, by tag, or only what needs a look
let tagFilter = ""; // show only lookouts carrying this tag

const $ = (sel) => document.querySelector(sel);
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}
function announce(text) {
  const a = $("#announce");
  a.textContent = "";
  setTimeout(() => { a.textContent = text; }, 50);
}
// done says, on screen too, what a press did: a row it took out of this view
// (seen in Needs a look, say) is otherwise just gone.
let doneTimer = 0;
function done(text) {
  announce(text);
  const d = $("#done");
  d.textContent = text;
  clearTimeout(doneTimer);
  doneTimer = setTimeout(() => { d.textContent = ""; }, 8000);
}
function setText(node, text) {
  // Only when it changed: some of these are live regions.
  if (node.textContent !== text) node.textContent = text;
}

function bucket(v) {
  if (v.contact === "lost") return "lost";
  if (v.seen) return "seen";
  return v.state;
}
const rank = (v) => RANK[bucket(v)] ?? 0;
// A dot's look: a seen smoke or fire keeps its colour, as a ring.
const dotClass = (v) => (v.seen && v.contact !== "lost" ? `${v.state} seen` : bucket(v));

function ago(iso, now) {
  if (!iso) return "never";
  const s = Math.max(0, (now - Date.parse(iso)) / 1000);
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.max(1, Math.floor(s / 60))}m ago`;
  if (s < 48 * 3600) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}
function clock(iso, now) {
  if (!iso) return "";
  const d = new Date(iso);
  const t = d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  if (new Date(now).toDateString() === d.toDateString()) return t;
  if (Math.abs(now - d) < 6 * 86400e3) return d.toLocaleDateString([], { weekday: "short" }) + " " + t;
  return d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + t;
}
// due says when a promised report is due: "due 5:30 PM (in 20m)".
function due(iso, now) {
  if (!iso) return "";
  const s = (Date.parse(iso) - now) / 1000;
  if (s <= 0) return `due ${clock(iso, now)}`;
  const rel = s < 3600 ? `in ${Math.max(1, Math.floor(s / 60))}m` : s < 48 * 3600 ? `in ${Math.floor(s / 3600)}h` : `in ${Math.floor(s / 86400)}d`;
  return `due ${clock(iso, now)} (${rel})`;
}
const sourceOf = (v) => v.origin && (state.sources.find((s) => s.name === v.origin) || {});

// What the Firetower knows that the check doesn't, in one line.
function meta(v, now) {
  const b = bucket(v);
  const parts = [];
  let word = WORD[b] || b;
  switch (b) {
    case "stale":
      // No word, and what it last said: a fire gone quiet is still a fire.
      if (v.status === "red" || v.status === "yellow") word += `, last said ${WORD[v.status]}`;
      if (v.next_by) parts.push(due(v.next_by, now));
      parts.push(v.checked_at ? `last word ${ago(v.checked_at, now)}` : "never reported");
      break;
    case "lost": {
      const src = sourceOf(v);
      parts.push(src && src.since ? `since ${ago(src.since, now)}` : "its machine isn't answering");
      parts.push("no buttons until it's back");
      if (v.checked_at && !(src && src.since && Math.abs(Date.parse(src.since) - Date.parse(v.checked_at)) < 120e3)) {
        parts.push(`its last report ${ago(v.checked_at, now)}`);
      }
      break;
    }
    case "waiting":
      parts.push(v.next_by ? `first report ${due(v.next_by, now)}` : "for a first report");
      break;
    case "seen":
      word = `${WORD[v.state]}, seen ${ago(v.seen_at, now)}`; // what was acknowledged, and when
      if (v.status_since) parts.push(`since ${clock(v.status_since, now)}`);
      parts.push(v.last_clear_at ? `last clear ${ago(v.last_clear_at, now)}` : "never clear");
      break;
    case "red": case "yellow":
      if (v.status_since) parts.push(`since ${clock(v.status_since, now)}`);
      if (v.broken) { parts.push("its file can't be read"); break; }
      parts.push(v.last_clear_at ? `last clear ${ago(v.last_clear_at, now)}` : "never clear");
      break;
    case "done":
      word = "stood down"; // one word for the state, as everywhere
      parts.push(v.done_by === "until" ? `its --until passed ${clock(v.done_at, now)}` : clock(v.done_at, now));
      break;
    default:
      parts.push(`checked ${ago(v.checked_at, now)}`);
  }
  const live = b !== "stale" && b !== "done" && b !== "lost";
  if (live && v.movement === "moved") parts.push(`moved ${ago(v.moved_at, now)}`);
  if (live && v.movement === "still") parts.push(`still since ${clock(v.moved_at, now)}`);
  const src = sourceOf(v);
  if (src && !src.can_act && b !== "lost") parts.push(`read only here: ${v.origin} has no act command`);
  const m = el("div", "meta");
  const rest = parts.filter(Boolean);
  m.append(el("span", "word", word), rest.length ? " · " + rest.join(" · ") : "");
  return m;
}

function isWeb(link) {
  try { const u = new URL(link); return u.protocol === "https:" || u.protocol === "http:"; } catch { return false; }
}
const isFile = (link) => /^(\/|~\/|file:\/\/)/.test(link || "");

// A row's key: the lookout's id, and in By tag the section too, since a
// lookout with two tags is drawn twice. Focus, Remove's arming and an open
// row follow the row; notes and presses follow the lookout.
const keyOf = (v) => (v._group !== undefined ? `${v.id}\u241f${v._group}` : v.id);
const rowSel = (k) => `.row[data-key="${CSS.escape(k)}"]`;

function row(v, now) {
  const b = bucket(v);
  const k = keyOf(v);
  const li = el("li", `row ${b}` + (openRow === k ? " open" : ""));
  li.dataset.id = v.id;
  li.dataset.key = k;
  li.append(el("span", `dot ${dotClass(v)}`));
  const main = el("div", "main");
  const line = el("div", "line");

  let label;
  if (v.link && isWeb(v.link)) {
    label = el("a", "label link", v.label);
    label.href = v.link;
    label.target = "_blank";
    label.rel = "noopener noreferrer";
  } else if (v.link && isFile(v.link) && !v.origin) {
    // A local file: the board shows it as plain text, never runs it.
    label = el("a", "label link", v.label);
    label.href = withKey("/api/link?id=" + encodeURIComponent(v.id));
    label.target = "_blank";
    label.rel = "noopener";
  } else {
    label = el("span", "label", v.label);
    label.tabIndex = -1; // somewhere for focus to land after a button goes
  }
  label.title = [v.label, v.id, v.source && `reported by ${v.source}`, v.link].filter(Boolean).join("\n");
  line.append(label);
  if (v.message) {
    const msg = el("span", "msg", v.message);
    msg.title = v.message;
    line.append(msg);
  }
  if (view !== "place") {
    // Out of its district and tower, a row says where it lives.
    const c = el("span", "chip", `${v.district}/${v.tower}`);
    c.title = "its district and tower";
    line.append(c);
  }
  if (v.home_district) {
    const c = el("span", "chip", `district ${v.home_district}`);
    c.title = `its own district, on ${v.origin}`;
    line.append(c);
  }
  for (const t of v.tags || []) {
    if (view === "tag" && t === v._group) continue; // the section already says it
    const c = el("span", "chip tag", `#${t}`);
    c.title = `tagged ${t}`;
    line.append(c);
  }
  if (v.origin) {
    const c = el("span", "chip", `on ${v.origin}`);
    c.title = `reported on the machine ${v.origin}`;
    line.append(c);
  }
  main.append(line, meta(v, now));
  if (notes.has(v.id)) {
    const { text, info } = notes.get(v.id);
    const n = el("div", "note" + (info ? " info" : ""), text);
    // Read out once, not on every redraw.
    if (!alerted.has(v.id)) { n.setAttribute("role", "alert"); alerted.add(v.id); }
    main.append(n);
  }
  li.append(main);

  const acts = el("div", "acts");
  const src = sourceOf(v);
  const canAct = !v.origin || src.can_act;
  if (canAct && v.contact !== "lost") {
    if ((v.state === "yellow" || v.state === "red") && !v.seen && !v.broken) acts.append(button("Seen", "seen", v));
    if (v.state !== "done" && !v.broken) acts.append(button("Stand down", "done", v));
    if (v.state === "done") acts.append(button("Watch again", "expect", v));
    acts.append(button("Remove", "rm", v, true));
  }
  if (acts.childElementCount) {
    // On a touch screen (or a narrow window) a row's buttons hide behind
    // this, so a long board stays short; with a mouse they show on hover.
    const more = el("button", "more", "⋯");
    more.type = "button";
    more.dataset.verb = "more";
    more.setAttribute("aria-expanded", String(openRow === k));
    more.setAttribute("aria-label", `Actions for ${v.label}`);
    more.addEventListener("click", (e) => { e.stopPropagation(); toggleRow(k); });
    li.append(more);
    // On a touch screen a tap on the row works too; with a mouse, a click
    // there is someone selecting text, and is left alone.
    main.addEventListener("click", (e) => { if (!e.target.closest("a") && touchLayout()) toggleRow(k); });
  }
  li.append(acts);
  return li;
}

const touchLayout = () => matchMedia("(hover: none), (max-width: 520px)").matches;

function toggleRow(id) {
  openRow = openRow === id ? null : id;
  if (armed !== null && armed !== openRow) quietDisarm();
  pendingFocus = touchLayout() ? { id, verb: "more" } : null;
  build();
  // Opened at the bottom of the window: bring its buttons into view.
  if (openRow) document.querySelector(`${rowSel(openRow)} .acts`)?.scrollIntoView({ block: "nearest" });
}

function button(text, verb, v, confirm) {
  const k = keyOf(v);
  const isArmed = confirm && armed === k;
  const others = view === "tag" ? (v.tags || []).filter((t) => t !== v._group) : [];
  const b = el("button", isArmed ? "armed" : null, isArmed ? (others.length ? `Remove? (also from ${others.map((t) => "#" + t).join(", ")})` : "Remove?") : text);
  b.type = "button";
  b.dataset.verb = verb;
  b.disabled = inflight.has(v.id);
  // In By tag a lookout may show under several tags: say Remove takes it from all.
  const many = view === "tag" && (v.tags || []).length > 1 ? ` (from all ${v.tags.length} of its tags)` : "";
  b.setAttribute("aria-label", isArmed ? `Remove? Press again to remove ${v.label}${many}` : `${text}: ${v.label}`);
  if (isArmed && many) b.title = `Removes ${v.label} everywhere it shows${many}`;
  b.addEventListener("click", async (e) => {
    e.stopPropagation();
    // No dialogs: a second press confirms. A double-click or a held Enter
    // is one gesture, not two, so the second press must come a moment later.
    if (confirm && armed !== k) { arm(v); return; }
    if (confirm && Date.now() - armedAt < 500) return;
    clearTimeout(armTimer);
    armed = null;
    // Where focus goes once this row is redrawn, or gone.
    // (Rows of the same lookout under other tags go too, so they're passed over.)
    const rows = [...document.querySelectorAll(".row")].filter((r) => r.dataset.id !== v.id || r.dataset.key === k);
    const i = rows.findIndex((r) => r.dataset.key === k);
    const next = rows.slice(i + 1).find((r) => r.dataset.id !== v.id) || rows.slice(0, i).reverse().find((r) => r.dataset.id !== v.id);
    pendingFocus = verb === "rm" ? { id: next ? next.dataset.key : "top", verb: "label" } : { id: k, verb: "label" };
    const sentTo = pendingFocus.id;
    inflight.add(v.id);
    build();
    let ok = false;
    try {
      const r = await fetch("/api/" + verb, {
        method: "POST",
        headers: { "Content-Type": "application/json", "Authorization": "Bearer " + KEY },
        body: JSON.stringify({ id: v.id }),
      });
      if (!r.ok) {
        const j = await r.json().catch(() => ({}));
        notes.set(v.id, { text: j.error || `${text} didn't work (${r.status})` });
        alerted.delete(v.id);
        stayNear(k, sentTo);
      } else {
        notes.delete(v.id);
        ok = true;
      }
    } catch {
      notes.set(v.id, { text: "The board isn't answering." });
      alerted.delete(v.id);
      stayNear(k, sentTo);
    }
    inflight.delete(v.id);
    if (ok) {
      const what = { seen: "marked seen", done: "stood down", rm: "removed", expect: "watched again" }[verb];
      if (v.origin) {
        // Another machine's change shows once it reports: say so on the row too.
        const msg = `Asked ${v.origin}: ${what}. It shows here once ${v.origin} reports.`;
        notes.set(v.id, { text: msg, info: true });
        done(`${v.label}: ${msg}`);
      } else done(`${v.label} ${what}.`);
    }
    build();
  });
  return b;
}

// stayNear brings focus back to a row whose press failed, unless the person
// has since moved on (a slow ssh act can take a while).
function stayNear(k, sentTo) {
  const a = document.activeElement;
  const at = a?.closest?.(".row")?.dataset.key;
  // Back on the row that failed, unless the person has moved on: focus still
  // there, nowhere, or where the press itself sent it (Remove's next row).
  const moved = at === sentTo || (sentTo === "top" && a === $("h1"));
  if (!a || a === document.body || at === k || moved) pendingFocus = { id: k, verb: "label" };
}

function arm(v) {
  clearTimeout(armTimer);
  const k = keyOf(v);
  armed = k;
  armedAt = Date.now();
  pendingFocus = { id: k, verb: "rm" };
  announce(`Press Remove again to remove ${v.label}. Escape cancels.`);
  build();
  armTimer = setTimeout(() => { if (armed === k) { quietDisarm(); announce("Remove cancelled."); } }, ARM_FOR);
}
// quietDisarm puts the armed button back in place, without a rebuild, so
// whatever was just clicked or focused stays where it is.
function quietDisarm() {
  clearTimeout(armTimer);
  if (armed === null) return;
  const b = document.querySelector(`${rowSel(armed)} button.armed`);
  const label = document.querySelector(`${rowSel(armed)} .label`)?.textContent || "";
  armed = null;
  if (b) {
    b.classList.remove("armed");
    b.textContent = "Remove";
    b.setAttribute("aria-label", `Remove: ${label}`);
  }
}
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  if (armed !== null) { quietDisarm(); announce("Remove cancelled."); return; }
  if (openRow !== null) { const id = openRow; openRow = null; pendingFocus = touchLayout() ? { id, verb: "more" } : null; build(); }
});
document.addEventListener("focusin", (e) => {
  if (armed !== null && e.target.closest?.(".row")?.dataset.key !== armed) { quietDisarm(); announce("Remove cancelled."); }
});
document.addEventListener("click", (e) => {
  // A tap outside an open row closes it.
  if (openRow !== null && !e.target.closest?.(".row")) {
    if (armed !== null) { quietDisarm(); announce("Remove cancelled."); } // a closed row keeps nothing armed
    openRow = null; build();
  }
});

function group(lookouts) {
  const districts = new Map();
  for (const v of lookouts) {
    if (!districts.has(v.district)) districts.set(v.district, new Map());
    const towers = districts.get(v.district);
    if (!towers.has(v.tower)) towers.set(v.tower, []);
    towers.get(v.tower).push(v);
  }
  return districts;
}

// needsLook: what the "needs a look" view keeps, the same lookouts the
// summary counts as needing one.
const needsLook = (v) => ["red", "yellow", "stale", "lost"].includes(bucket(v));

function allTags(lookouts) {
  const tags = new Set();
  for (const v of lookouts) for (const t of v.tags || []) tags.add(t);
  return [...tags].sort();
}

// A tag as typed in an address: "#Payments" is the tag payments.
const asTag = (t) => (t || "").trim().replace(/^#+/, "").toLowerCase();

// fromAddress reads the view from the address, and puts a wrong one right
// (?view=nonsense is the board by place, and says so).
function fromAddress() {
  const params = new URL(location.href).searchParams;
  view = VIEWS.includes(params.get("view")) ? params.get("view") : "place";
  tagFilter = asTag(params.get("tag"));
  writeAddress(false);
}

// writeAddress puts the view in the address (never the key): a new history
// entry when the person changed it, so Back steps back through views.
function writeAddress(push) {
  const url = new URL(location.href);
  for (const k of ["key", "once"]) url.searchParams.delete(k);
  if (view === "place") url.searchParams.delete("view"); else url.searchParams.set("view", view);
  if (tagFilter) url.searchParams.set("tag", tagFilter); else url.searchParams.delete("tag");
  const next = url.pathname + url.search + url.hash;
  if (next === location.pathname + location.search + location.hash) return;
  if (push) history.pushState(null, "", next); else history.replaceState(null, "", next);
}

// setView changes the layout, and the address with it.
function setView(next, tag) {
  view = VIEWS.includes(next) ? next : "place";
  tagFilter = tag ?? tagFilter;
  writeAddress(true);
  openRow = null;
  build();
}
addEventListener("popstate", () => { fromAddress(); openRow = null; build(); });

// controls keeps the view buttons and the tag list in step with the
// lookouts, changing only what changed, so focus on them stays put.
function controls(tags) {
  for (const b of document.querySelectorAll("#views [data-view]")) {
    b.setAttribute("aria-pressed", String(b.dataset.view === view));
    if (b.dataset.view === "tag") b.hidden = tags.length === 0 && view !== "tag";
  }
  const pick = $("#tagpick");
  pick.closest("label").hidden = tags.length === 0 && !tagFilter;
  const want = ["", ...new Set([...tags, ...(tagFilter ? [tagFilter] : [])])];
  const have = [...pick.options].map((o) => o.value);
  if (want.join("\n") !== have.join("\n")) {
    pick.replaceChildren(...want.map((t) => { const o = el("option", null, t ? `#${t}` : "every tag"); o.value = t; return o; }));
  }
  pick.value = tagFilter;
}

// shownNote says what a filtered board is showing, beside the summary,
// which is always the whole board's.
function shownNote(list) {
  const note = $("#shown");
  if (!tagFilter) { setText(note, ""); return; }
  const live = (vs) => vs.filter((v) => v.state !== "done").length;
  const n = live(list);
  setText(note, n || list.length
    ? `Only #${tagFilter}: ${n} of the ${live(state.lookouts)} lookouts on watch carry it. The summary above is the whole board.`
    : `No lookout carries #${tagFilter}. The summary above is the whole board.`);
}

function worst(vs) {
  let w = null;
  for (const v of vs) if (!w || rank(v) > rank(w)) w = v;
  return w ? dotClass(w) : "none";
}

// buildPlaces is the board by district and tower.
function buildPlaces(board, list, now) {
  const districts = group(list);
  const solo = districts.size <= 1;
  for (const [name, towers] of districts) {
    const sec = el("section", "district" + (solo ? " solo" : ""));
    if (!solo) {
      const h = el("h2");
      h.append(el("span", "dot " + worst([...towers.values()].flat())), name);
      sec.append(h);
    }
    for (const [tname, vs] of towers) sec.append(tower(tname, vs, now, solo ? "h2" : "h3"));
    board.append(sec);
  }
}

// buildTags is the board by tag: a lookout shows under each tag it carries,
// whatever its district or machine, and one with none under "no tag".
function buildTags(board, list, now) {
  const byTag = new Map();
  for (const v of list) {
    const ts = (v.tags || []).filter((t) => !tagFilter || t === tagFilter);
    for (const t of ts.length ? ts : [""]) {
      if (!byTag.has(t)) byTag.set(t, []);
      byTag.get(t).push({ ...v, _group: t });
    }
  }
  const names = [...byTag.keys()].sort((a, b) => (a === "") - (b === "") || a.localeCompare(b));
  for (const t of names) {
    const sec = el("section", "district solo");
    sec.append(tower(t ? `#${t}` : "no tag", byRank(byTag.get(t)), now, "h2"));
    board.append(sec);
  }
}

// buildAttention is only what needs a look, worst first, in one list.
function buildAttention(board, list, now) {
  const vs = byRank(list.filter(needsLook));
  if (!vs.length) return;
  const sec = el("section", "district solo attention");
  // Its count is its rows. The summary's also counts each machine out of
  // contact once, and a machine with no lookouts has a banner, not a row.
  // A machine out of contact with no lookouts here has a banner, not a row;
  // the summary counts it, so the count says it too.
  const shown = new Set(vs.map((v) => v.origin).filter(Boolean));
  const bare = tagFilter ? 0 : state.sources.filter((x) => x.contact === "lost" && !shown.has(x.name)).length;
  let count = `${vs.length} lookout${vs.length === 1 ? "" : "s"}`;
  if (bare) count += ` · and ${bare} machine${bare === 1 ? "" : "s"} out of contact with none, above`;
  const t = tower("Needs a look", vs, now, "h2", count);
  sec.append(t);
  board.append(sec);
}

const byRank = (vs) => [...vs].sort((a, b) => rank(b) - rank(a) || a.id.localeCompare(b.id));

function tower(name, vs, now, tag, count) {
  const t = el("div", "tower");
  const h = el(tag, "tower-name");
  const live = vs.filter((v) => v.state !== "done").length;
  h.append(el("span", "dot " + worst(vs)), el("span", "name", name), el("span", "count", count || `${live} on watch`));
  const ul = el("ul", "rows");
  // Stood-down lookouts sink to the bottom.
  const sorted = [...vs].sort((a, b) => (a.state === "done") - (b.state === "done"));
  for (const v of sorted) ul.append(row(v, now));
  t.append(h, ul);
  return t;
}

// build draws the whole board from state. Only a change in what it shows
// calls it; the clock calls times().
function build() {
  if (blocked || !state) return;
  const now = Date.now();
  const board = $("#board");
  const active = document.activeElement;
  const activeRow = active?.closest?.(".row");
  const focus = pendingFocus || (activeRow ? { id: activeRow.dataset.key, verb: active.dataset?.verb || "label" } : null);
  pendingFocus = null;
  // Where the focused row was, for when it's gone from this view (seen in
  // Needs a look, say): the row that takes its place gets focus.
  const before = [...board.querySelectorAll(".row")].map((r) => r.dataset.key);
  const was = focus && focus.id !== "top" ? before.indexOf(focus.id) : -1;
  board.replaceChildren();

  const tags = allTags(state.lookouts);
  controls(tags);
  const list = tagFilter ? state.lookouts.filter((v) => (v.tags || []).includes(tagFilter)) : state.lookouts;
  shownNote(list);
  if (view === "attention") buildAttention(board, list, now);
  else if (view === "tag") buildTags(board, list, now);
  else buildPlaces(board, list, now);
  if (!board.childElementCount) {
    if (state.lookouts.length === 0) board.append($("#empty").content.cloneNode(true));
    else {
      const msg = el("section", "empty");
      const noneTagged = tagFilter && list.length === 0;
      msg.append(el("h2", null, noneTagged ? `No lookout carries #${tagFilter}` : "Nothing needs a look"));
      msg.append(el("p", null, noneTagged ? "Pick another tag, or every tag."
        : tagFilter ? `Every lookout carrying #${tagFilter} is clear, waiting, seen or stood down.`
        : "Every lookout is clear, waiting, seen or stood down." + (state.sources.some((x) => x.contact === "lost") ? " A machine out of contact is above." : "")));
      board.append(msg);
    }
  }
  times();

  if (focus && focus.id === "top") $("h1").focus();
  else if (focus && focus.id) {
    // Back to the same kind of thing: never from a name onto an action.
    const sel = rowSel(focus.id);
    let target = (focus.verb === "label" ? board.querySelector(`${sel} .label`)
      : board.querySelector(`${sel} button[data-verb="${focus.verb}"]`)) || board.querySelector(`${sel} .label`);
    if (!target && was >= 0) {
      const rows = board.querySelectorAll(".row .label");
      target = rows[Math.min(was, rows.length - 1)] || $("#views [aria-pressed=\"true\"]");
    }
    target?.focus();
  }
}

// times rewrites everything that moves with the clock, in place: each row's
// line of times, the banners, the summary, the title and the icon.
function times() {
  const now = Date.now();
  const banners = $("#banners");
  banners.replaceChildren();
  if (!connected) {
    const b = el("div", "banner");
    b.append(el("b", null, "This board"), " can't reach the Firetower. Retrying…");
    banners.append(b);
  }
  if (!state) {
    if (!connected) setText($("#summary"), "Can't reach the Firetower");
    return;
  }
  for (const r of document.querySelectorAll(".row")) {
    const v = state.lookouts.find((x) => x.id === r.dataset.id);
    const old = r.querySelector(".meta");
    if (!v || !old) continue;
    const fresh = meta(v, now);
    if (fresh.textContent !== old.textContent) old.replaceWith(fresh);
  }
  const lost = state.sources.filter((s) => s.contact === "lost");
  for (const s of lost) {
    const b = el("div", "banner");
    b.append(el("b", null, s.name), ` is out of contact${s.since ? " since " + ago(s.since, now) : ""}`);
    if (s.error) b.append(": " + s.error);
    banners.append(b);
  }

  // The summary, the title and the icon all say the same thing.
  const counts = {};
  let live = 0;
  for (const v of state.lookouts) {
    if (v.contact === "lost") continue; // counted once, per machine
    counts[bucket(v)] = (counts[bucket(v)] || 0) + 1;
    if (v.state !== "done") live++;
  }
  let w = state.lookouts.length ? state.lookouts.reduce((a, v) => (rank(v) > rank(a) ? v : a)) : null;
  w = w ? bucket(w) : "none";
  if (lost.length && (RANK[w] ?? 0) < 4) w = "lost";
  if (!connected) w = "lost";
  const parts = [];
  if (counts.red) parts.push(`${counts.red} fire`);
  if (lost.length) parts.push(`${lost.length} machine${lost.length > 1 ? "s" : ""} out of contact`);
  for (const k of ORDER.slice(2)) if (counts[k]) parts.push(`${counts[k]} ${COUNTED[k]}`);
  let summary;
  if (!connected) summary = "Can't reach the Firetower: these may be out of date";
  else if (state.lookouts.length === 0 && !lost.length) summary = "No lookouts";
  else if (!counts.red && !lost.length && !counts.stale && !counts.yellow && !counts.waiting && !counts.seen) {
    summary = live ? `All clear · ${live} on watch` : "Nothing on watch";
    if (counts.done) summary += ` · ${counts.done} stood down`;
  } else summary = parts.join(" · ");
  const need = (counts.red || 0) + (counts.yellow || 0) + (counts.stale || 0) + lost.length;
  if (connected && need) summary = `${need} need${need === 1 ? "s" : ""} a look: ` + summary;
  setText($("#summary"), summary);
  setText($("#updated"), connected ? "updated " + clock(state.at, now) : "");

  document.title = !connected ? "(!) Firetower" : need ? `(${need}) Firetower` : "Firetower";
  const color = getComputedStyle(document.documentElement).getPropertyValue(COLOR[w] || "--waiting").trim();
  document.documentElement.style.setProperty("--worst", `var(${COLOR[w] || "--waiting"})`);
  setIcon(color);
}

let lastIcon = "";
function setIcon(color) {
  if (color === lastIcon) return;
  lastIcon = color;
  // Binoculars, as in the bars; the lenses carry the worst state's colour.
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path d="M3.2 15 5.1 6.2A1.8 1.8 0 0 1 6.9 4.8h1.4A1.7 1.7 0 0 1 10 6.5V12M20.8 15l-1.9-8.8a1.8 1.8 0 0 0-1.8-1.4h-1.4A1.7 1.7 0 0 0 14 6.5V12M10 8.5h4M10 12h4" fill="none" stroke="#8c959f" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/><circle cx="6.5" cy="15.5" r="3.6" fill="${color}" stroke="#8c959f" stroke-width="1.8"/><circle cx="17.5" cy="15.5" r="3.6" fill="${color}" stroke="#8c959f" stroke-width="1.8"/></svg>`;
  $("#icon").href = "data:image/svg+xml," + encodeURIComponent(svg);
}

function needKey(wrong) {
  blocked = true;
  $("#views").hidden = true;
  state = null;
  armed = null;
  openRow = null;
  clearTimeout(armTimer);
  $("#banners").replaceChildren();
  setText($("#summary"), claimTrouble ? "That address is spent" : wrong ? "Wrong key" : "No key");
  setText($("#updated"), "");
  const msg = el("section", "empty");
  msg.append(el("h2", null, wrong ? "This board didn't accept that key" : "This board opens with its key"));
  if (claimTrouble) msg.append(el("p", null, claimTrouble));
  else if (wrong) msg.append(el("p", null, "The key in the address (or the one this browser saved) isn't this board's: it may be from another board, or an old one."));
  const p = el("p", null, "Open it with ");
  p.append(el("code", null, "firetower board --open"), ", or ", el("code", null, "firetower board --url"), " for the address, then reload.");
  msg.append(p);
  if (!storageOK) msg.append(el("p", null, "This browser won't let the page keep the key, so it's needed again on every reload."));
  $("#board").replaceChildren(msg);
  document.title = "Firetower";
  lastIcon = "";
  setIcon("#8c959f");
}

const withKey = (path) => path + (path.includes("?") ? "&" : "?") + "key=" + encodeURIComponent(KEY);

async function accepted(k) {
  try {
    const r = await fetch("/api/state", { headers: { Authorization: "Bearer " + k } });
    return r.status === 401 ? false : r.ok ? true : null;
  } catch { return null; }
}

// The key: one from the address is tried first and saved only once the
// board has taken it; a wrong one never replaces a saved one that works.
let claimTrouble = ""; // why a one-time address didn't let this page in
async function chooseKey() {
  const url = new URL(location.href);
  let given = url.searchParams.get("key") || "";
  const once = url.searchParams.get("once") || "";
  if (url.searchParams.has("key") || url.searchParams.has("once")) {
    url.searchParams.delete("key");
    url.searchParams.delete("once");
    history.replaceState(null, "", url.pathname + url.search + url.hash);
  }
  // A one-time address (board --open) is traded for the key, once.
  if (once && !given) {
    try {
      const r = await fetch("/api/claim", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ once }) });
      const j = await r.json().catch(() => ({}));
      if (r.ok) given = j.key || "";
      else claimTrouble = j.error || "This address was for one use, and it's spent.";
    } catch {}
  }
  let saved = "";
  try { saved = localStorage.getItem(STORE) || ""; } catch { storageOK = false; }
  let unsure = "";
  for (const k of [given, saved]) {
    if (!k) continue;
    const ok = await accepted(k);
    if (ok === null && !unsure) unsure = k; // the board didn't answer: try it, but don't keep it yet
    if (ok !== true) continue;
    if (k === given) { try { localStorage.setItem(STORE, k); } catch { storageOK = false; } }
    if (k !== given && given) announce("The key in that address was wrong; using the one this browser saved.");
    return { key: k };
  }
  if (unsure) return { key: unsure };
  return { key: "", wrong: !!(given || saved) };
}

function connect() {
  if (es) es.close();
  heard = Date.now();
  es = new EventSource(withKey("/api/feed"));
  es.addEventListener("state", (e) => {
    heard = Date.now();
    const next = JSON.parse(e.data);
    try { localStorage.setItem(STORE, KEY); } catch {}
    // A note about a button stays until its lookout changes, or the board
    // it couldn't reach is back.
    const before = state ? new Map(state.lookouts.map((v) => [v.id, JSON.stringify(v)])) : new Map();
    const notesBefore = notes.size;
    for (const v of next.lookouts) {
      if (before.has(v.id) && before.get(v.id) !== JSON.stringify(v)) {
        notes.delete(v.id); alerted.delete(v.id);
        if (armed !== null && armed.split("\u241f")[0] === v.id) quietDisarm(); // never confirm a Remove on a row that just changed
      }
    }
    for (const [id, n] of notes) if (n.text === "The board isn't answering.") notes.delete(id);
    const notesChanged = notes.size !== notesBefore;
    if (openRow && !next.lookouts.some((v) => v.id === openRow.split("\u241f")[0])) openRow = null;
    const was = connected;
    state = next;
    connected = true;
    // The heartbeat sends the same state again: only the clock moved.
    const what = JSON.stringify({ l: next.lookouts, s: next.sources.map((s) => [s.name, s.contact, s.error, s.can_act]) });
    if (what !== shown || !was || notesChanged) { shown = what; build(); } else times();
  });
  es.onerror = async () => {
    connected = false;
    times();
    const ok = await accepted(KEY);
    if (ok === false) {
      // Turned away. A key from the address the board couldn't check at
      // first may be the wrong one: try the saved one before giving up.
      es.close();
      let saved = "";
      try { saved = localStorage.getItem(STORE) || ""; } catch {}
      if (saved && saved !== KEY && (await accepted(saved)) === true) { KEY = saved; connect(); return; }
      if (saved === KEY) { try { localStorage.removeItem(STORE); } catch {} }
      needKey(true);
    } else if (es.readyState === EventSource.CLOSED) {
      // The browser gave up on the feed (an error page): try again shortly.
      setTimeout(connect, 5000);
    }
  };
}

async function start() {
  for (const b of document.querySelectorAll("#views [data-view]")) b.addEventListener("click", () => setView(b.dataset.view));
  $("#tagpick").addEventListener("change", (e) => setView(view, e.target.value));
  const { key, wrong } = await chooseKey();
  fromAddress(); // only now: the address carried the key, or a one-time token, for chooseKey
  KEY = key;
  if (!KEY) return needKey(wrong);
  connect();
  setInterval(() => {
    if (blocked) return;
    // A feed that went quiet without an error (after a sleep, a dropped
    // network): the heartbeat stopped, so reconnect.
    if (Date.now() - heard > QUIET) { connected = false; connect(); }
    times();
  }, 15000);
}
start();
