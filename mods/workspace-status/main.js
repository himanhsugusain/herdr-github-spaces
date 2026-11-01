import { View, div, svg } from "gpui-kit";
import { h_flex } from "gpui-base";
import { spaces, agents, theme, run, window, readTextFile, watchFolder, home } from "herdr";

const configDir = () => `${home()}/.config/herdr-app`;
const statusDir = () => `${configDir()}/mods/space-status`;
const activityFile = () => `${configDir()}/activity.json`;

let prStatus = null;
let bgActivity = null;

async function loadPRStatus() {
  try {
    const raw = await readTextFile(`${statusDir()}/status.json`);
    prStatus = raw ? JSON.parse(raw) : null;
  } catch { prStatus = null; }
}

async function loadActivity() {
  try {
    const raw = await readTextFile(activityFile());
    bgActivity = raw ? JSON.parse(raw) : null;
  } catch { bgActivity = null; }
}

function ciSummary() {
  if (!prStatus?.prs) return null;
  let failed = 0, pending = 0, passed = 0;
  for (const pr of Object.values(prStatus.prs)) {
    if (!pr || pr.state !== "OPEN") continue;
    failed += pr.failed || 0;
    pending += pr.pending || 0;
    passed += pr.passed || 0;
  }
  if (failed + pending + passed === 0) return null;
  return { failed, pending, passed };
}

function trackedPRs() {
  if (!prStatus?.prs) return 0;
  return Object.values(prStatus.prs).filter(v => v && v.state === "OPEN").length;
}

// Background extension entries from activity.json
// Entries older than 2 minutes are stale (process was killed without cleanup)
function bgEntries() {
  if (!bgActivity || typeof bgActivity !== "object") return [];
  const now = Date.now();
  return Object.entries(bgActivity)
    .map(([id, v]) => ({ id, ...(v || {}) }))
    .filter(e => e.ext && e.at && (now - new Date(e.at).getTime()) < 120_000);
}

// Braille spinner frames
const SPINNER = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];

export default class WorkspaceStatus extends View {
  constructor() {
    super();
    this.frame = 0;
    this.spinnerTimer = null;
    loadPRStatus();
    loadActivity();
    try { watchFolder(statusDir(), () => { loadPRStatus().then(() => this.notify()); }); } catch {}
    try { watchFolder(configDir(), () => { loadActivity().then(() => this.notify()); }); } catch {}
  }

  render() {
    const t = theme();
    const space = spaces().find(s => s.active);
    const list = agents();
    const needs = list.filter(a => a.status === "blocked").length;
    const working = list.filter(a => a.status === "working").length;
    const label = (icon, text) => h_flex().gap(6).items_center()
      .child(svg(`mods/workspace-chrome/${icon}.svg`).size(13)).child(text);

    const tracked = trackedPRs();
    const ci = ciSummary();
    const bg = bgEntries();
    const bgWorking = bg.filter(e => e.state === "working");
    const bgIdle = bg.filter(e => e.state === "idle");
    const bgError = bg.filter(e => e.state === "error");

    // Animate spinner when background work is happening
    if (bgWorking.length > 0 && !this.spinnerTimer) {
      this.spinnerTimer = setInterval(() => { this.frame = (this.frame + 1) % SPINNER.length; this.notify(); }, 100);
    } else if (bgWorking.length === 0 && this.spinnerTimer) {
      clearInterval(this.spinnerTimer);
      this.spinnerTimer = null;
    }

    const row = h_flex().size_full().px(14).gap(16).items_center()
      .bg(t.chrome).border_t(1).border_color(t.border).text_size(11.8).text_color(t.muted);

    // Machine
    row.child(label(space?.machine ? "server" : "monitor", space?.machine ?? "This computer"));

    // Branch
    if (space?.branch) row.child(label("branch", space.branch));

    // Agents
    row.child(h_flex().gap(6).items_center()
      .child(div().size(6).rounded(3).bg(working > 0 ? t.accent : t.dim))
      .child(`${list.length} agents`)
      .when(working > 0, el => el.child(div().text_color(t.accent).child(`· ${working} working`)))
      .when(needs > 0, el => el.child(div().text_color(t.blocked).child(`· ${needs} needs you`)))
    );

    // PR tracking
    if (tracked > 0) {
      let text = `${tracked} PR${tracked > 1 ? "s" : ""}`;
      if (ci) {
        const parts = [];
        if (ci.failed) parts.push(`${ci.failed}✗`);
        if (ci.pending) parts.push(`${ci.pending}⏳`);
        if (ci.passed) parts.push(`${ci.passed}✓`);
        text += ` (${parts.join(" ")})`;
      }
      const color = ci?.failed ? "#e06c75" : "#3fae6a";
      row.child(h_flex().gap(5).items_center()
        .child(div().text_color(color).child("●"))
        .child(div().text_color(t.muted).child(text))
      );
    }

    // Background extensions — spinner for working, dot for idle
    if (bg.length > 0) {
      const items = h_flex().gap(10).items_center();

      for (const entry of bgWorking) {
        items.child(
          h_flex().gap(4).items_center()
            .child(div().text_color(t.accent).text_size(13).font_family("monospace").child(SPINNER[this.frame]))
            .child(div().text_color(t.accent).child(entry.ext))
            .when(Boolean(entry.status), el => el.child(div().text_color(t.dim).child(entry.status)))
        );
      }

      for (const entry of bgIdle) {
        items.child(
          h_flex().gap(4).items_center().tooltip(entry.status || "idle")
            .child(div().text_color("#3fae6a").child("●"))
            .child(div().text_color(t.dim).child(entry.ext))
        );
      }

      for (const entry of bgError) {
        items.child(
          h_flex().gap(4).items_center().tooltip(entry.status || "error")
            .child(div().text_color("#e06c75").child("●"))
            .child(div().text_color("#e06c75").child(entry.ext))
        );
      }

      row.child(items);
    }

    row.child(div().flex_1());
    row.child(div().id("palette").text_color(t.dim)
      .on_click(() => run("app.palette"))
      .child(`${window()?.platform === "macos" ? "⌘" : "Ctrl"} Shift P  commands`));

    return row;
  }
}
