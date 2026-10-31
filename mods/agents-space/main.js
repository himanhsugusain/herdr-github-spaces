import { View, div } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { agents, panes, focused, spaces, run, theme, readTextFile, watchFolder, home } from "herdr";

const prKey = (on, cwd, branch) => JSON.stringify([on ?? null, cwd, branch ?? ""]);

// The PR for the branch checked out in that folder, if gh can see one.
const STATUS = () => `${home()}/.config/herdr-app/mods/space-status/status.json`;

async function readStatus() {
  try {
    const raw = await readTextFile(STATUS());
    return raw ? JSON.parse(raw) : {};
  } catch {
    return {};
  }
}

function ciMark(glyph, n, color, tip) {
  return h_flex().flex_none().gap(2).items_center().tooltip(tip)
    .child(div().font_family("Symbols Nerd Font").text_size(12).flex_none().text_color(color).child(glyph))
    .child(div().text_size(11).font_medium().text_color(color).child(String(n)));
}

function prState(pr) {
  if (pr?.state === "MERGED") return "merged";
  if (pr?.state === "CLOSED") return "closed";
  if (pr?.isDraft) return "draft";
  return "open";
}

function prColor(pr) {
  return { open: "#3fae6a", draft: "#8b949e", merged: "#a371f7", closed: "#e06c75" }[prState(pr)] ?? "#8b949e";
}

function prGlyph(state) {
  return { open: "", draft: "", merged: "", closed: "" }[state] ?? "";
}

const CI = {
  failed: { glyph: "", color: "#e06c75", tip: "failed" },
  pending: { glyph: "", color: "#d29922", tip: "pending" },
  skipped: { glyph: "", color: "#8b949e", tip: "skipped" },
  passed: { glyph: "", color: "#3fae6a", tip: "passed" },
};

function machineOf(agent, spaceById) {
  const space = spaceById.get(agent.space);
  if (space) return space.machine ?? null;
  return agent.address.includes("/") ? agent.address.split("/")[0] : null;
}

// Format elapsed milliseconds as a human-readable duration.
function formatElapsed(ms) {
  if (ms < 0) ms = 0;
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return `${sec}s`;
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m`;
  const hr = Math.floor(min / 60);
  const rm = min % 60;
  return rm > 0 ? `${hr}h ${rm}m` : `${hr}h`;
}

export default class Agents extends View {
  init(_props, cx) {
    this.prs = {};
    // Track when each agent entered its current status: address -> { status, since }.
    this.statusSince = {};
    const apply = async (cx) => {
      const next = (await readStatus()).prs ?? {};
      if (JSON.stringify(next) === JSON.stringify(this.prs)) return;
      this.prs = next;
      cx.notify();
    };
    cx.spawn(async (cx) => {
      await apply(cx);
      for (;;) {
        await watchFolder(STATUS().replace(/\/[^/]+$/, ""));
        await apply(cx);
      }
    });
    // Refresh elapsed times every 30s.
    cx.timer.every(30000, (cx) => cx.notify());
  }

  // Update status tracking and return elapsed ms for the current status.
  elapsed(a) {
    const now = Date.now();
    const prev = this.statusSince[a.address];
    if (!prev || prev.status !== a.status) {
      this.statusSince[a.address] = { status: a.status, since: now };
      return 0;
    }
    return now - prev.since;
  }

  // Status dot: a colored filled circle on the left of the row.
  statusDot(t, a) {
    const color = a.status === "working" ? (t.working ?? t.accent)
      : a.status === "blocked" ? (t.blocked ?? "#e06c75")
      : a.status === "done" ? (t.done ?? "#3fae6a")
      : (t.idle ?? t.dim);
    return div().size(8).flex_none().rounded(4).bg(color);
  }

  // The second line: status label + separator + context.
  statusLine(t, a) {
    const ms = this.elapsed(a);
    let label, labelColor, context;

    if (a.status === "working") {
      label = "working";
      labelColor = t.working ?? t.accent;
      context = formatElapsed(ms);
    } else if (a.status === "blocked") {
      label = "needs you";
      labelColor = t.blocked ?? "#e06c75";
      // Use the terminal title as context when it differs from the program name.
      const progName = a.program?.name || "";
      const titleHint = a.title && a.title.toLowerCase() !== progName.toLowerCase() ? a.title : null;
      context = titleHint || "review";
    } else if (a.status === "done") {
      label = "done";
      labelColor = t.done ?? "#3fae6a";
      context = ms > 0 ? `${formatElapsed(ms)} ago` : "just now";
    } else {
      label = "idle";
      labelColor = t.idle ?? t.dim;
      context = ms > 0 ? `${formatElapsed(ms)} ago` : "just now";
    }

    return h_flex().gap(0).items_center()
      .child(div().text_size(11).text_color(labelColor).child(label))
      .child(div().text_size(11).text_color(t.dim).mx(5).child("\u00b7"))
      .child(div().text_size(11).text_color(t.dim).truncate().child(context));
  }

  agentRow(t, a, paneById, spaceById) {
    const pane = paneById.get(a.pane);
    const pr = pane?.cwd ? (this.prs ?? {})[prKey(machineOf(a, spaceById), pane.cwd, pane.branch)] ?? null : null;
    const state = pr ? prState(pr) : null;
    const color = pr ? prColor(pr) : t.foreground;
    const name = pr?.number ? `#${pr.number}` : (a.title || a.program?.name || a.address);
    const ci = pr?.total ? pr : null;
    return h_flex()
      .id(`ag-${a.address}`)
      .h(52).px(8).flex_none().rounded(8).items_center().gap(10).cursor_pointer()
      .hover((s) => s.bg(t.hover))
      .when(a.focused, (el) => el.bg(t.selected))
      .on_click(() => run("pane.focus", { pane: a.pane }))
      // Colored dot on the left.
      .child(this.statusDot(t, a))
      // Line 1 (PR/name + CI) and line 2 (status label) stacked vertically.
      .child(
        v_flex().flex_1().min_w(0).gap(2).justify_center()
          .child(
            h_flex().gap(6).items_center()
              .when(Boolean(pr), (el) => el.child(div().flex_none().font_family("Symbols Nerd Font").text_size(13).text_color(color).child(prGlyph(state))))
              .child(div().flex_1().min_w(0).truncate().text_size(13).text_color(pr ? color : t.foreground).child(name))
              .when(Boolean(ci?.failed), (el) => el.child(ciMark(CI.failed.glyph, ci.failed, CI.failed.color, `${ci.failed} ${CI.failed.tip}`)))
              .when(Boolean(ci?.pending), (el) => el.child(ciMark(CI.pending.glyph, ci.pending, CI.pending.color, `${ci.pending} ${CI.pending.tip}`)))
              .when(Boolean(ci?.skipped), (el) => el.child(ciMark(CI.skipped.glyph, ci.skipped, CI.skipped.color, `${ci.skipped} ${CI.skipped.tip}`)))
              .when(Boolean(ci?.passed), (el) => el.child(ciMark(CI.passed.glyph, ci.passed, CI.passed.color, `${ci.passed} ${CI.passed.tip}`)))
          )
          .child(this.statusLine(t, a))
      );
  }

  render() {
    const t = theme();
    const spaceId = focused()?.space;
    const list = agents().filter((a) => !spaceId || a.space === spaceId);
    const paneById = new Map(panes().map((p) => [p.pane, p]));
    const spaceById = new Map(spaces().map((s) => [s.id, s]));
    // Clean up stale status tracking.
    const activeAddresses = new Set(list.map((a) => a.address));
    for (const addr of Object.keys(this.statusSince)) {
      if (!activeAddresses.has(addr)) delete this.statusSince[addr];
    }
    return v_flex().size_full().min_h(0).bg(t.chrome).border_t(1).border_color(t.border).text_size(13)
      .child(
        h_flex().h(31).px(16).items_center().justify_between().text_size(11.5).text_color(t.dim)
          .child("AGENTS")
          .when(list.length > 0, (el) => el.child(div().text_color(t.dim).child(String(list.length))))
      )
      .child(v_flex().flex_1().min_h(0).px(8).pb(8).overflow_y_scrollbar().gap(2).children(list.map((a) => this.agentRow(t, a, paneById, spaceById))));
  }
}
