import { View, div } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { agents, panes, spaces, run, theme, readTextFile, exec, shared, setShared } from "herdr";

import { absolutePath, localRepoCandidates, observeStatus, panePR, statusPaths, viewProps, watchPaneRoots } from "./status.js";

const LAUNCH_WATCHER = [
  "umask 077",
  'watch_binary=$1; watch_repo=$2; watch_state=$3; watch_status=$4; watch_pid=$5; watch_log=$6; shift 6',
  'nohup "$watch_binary" pr-watch --discover-repos --repo "$watch_repo" --state "$watch_state" --status "$watch_status" --pid "$watch_pid" "$@" >>"$watch_log" 2>&1 </dev/null &',
].join("; ");
const LAUNCH_CHECK_MS = 3000;
const LAUNCH_CHECK_EVERY = 10;
const SPACE_PALETTE = ["#22a99a", "#e3814f", "#5b8def", "#8b7cf6", "#3fae6a", "#d9649a"];

function lighterSpaceColor(space, allSpaces) {
  const index = allSpaces.findIndex((item) => item.id === space?.id);
  if (index < 0) return null;
  const hex = SPACE_PALETTE[index % SPACE_PALETTE.length];
  const rgb = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));
  // Keep the tint subtle so foreground, PR, and status text retain contrast.
  const light = rgb.map((value) => Math.round(value + (255 - value) * 0.03));
  return `#${light.map((value) => value.toString(16).padStart(2, "0")).join("")}`;
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

const REVIEW = {
  APPROVED: { glyph: "✓", color: "#3fae6a", label: "approved" },
  CHANGES_REQUESTED: { glyph: "✗", color: "#e06c75", label: "changes requested" },
  REVIEW_REQUIRED: { glyph: "⊘", color: "#d29922", label: "review required" },
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
  init(props = {}, cx) {
    props = viewProps(props);
    this.prs = {};
    this.statusSince = {};
    this.launchInFlight = false;
    this.healthTick = 0;
    Object.assign(this, statusPaths(props));
    this.repoOverride = absolutePath(props.repo) ? props.repo : "";
    this.accountArgs = [];
    for (const [repository, account] of Object.entries(props.githubAccounts ?? {})) {
      if (/^[A-Za-z0-9_.-]+(?:\/[A-Za-z0-9_.-]+)?$/.test(repository) && typeof account === "string" && /^[A-Za-z0-9-]+$/.test(account)) {
        this.accountArgs.push("--github-account", `${repository}=${account}`);
      }
    }
    this.watcherKey = `github-spaces.watcher:${this.pidFile}`;
    this.roots = watchPaneRoots(cx);
    observeStatus(this.statusFile, cx, (status, cx) => {
      if (JSON.stringify(status.prs) === JSON.stringify(this.prs)) return;
      this.prs = status.prs;
      cx.notify();
    });
    // Check immediately, then every 30s. The first interval fires after 3s.
    cx.timer.every(LAUNCH_CHECK_MS, (cx) => {
      this.healthTick += 1;
      if (this.healthTick === 1 || this.healthTick % LAUNCH_CHECK_EVERY === 0) {
        cx.spawn((cx) => this.ensureWatcher(cx));
      }
    });
    cx.spawn((cx) => this.ensureWatcher(cx));
    // Refresh elapsed times every 30s.
    cx.timer.every(30000, (cx) => cx.notify());
  }

  watcherMessage(message) {
    if (shared(this.watcherKey) !== message) setShared(this.watcherKey, message);
  }

  async ensureWatcher(cx) {
    if (this.launchInFlight) return;
    this.launchInFlight = true;
    try {
      try {
        const raw = await readTextFile(this.pidFile);
        const pid = Number(raw.trim());
        if (Number.isSafeInteger(pid) && pid > 1) {
          const alive = await exec("kill", ["-0", String(pid)]);
          if (alive.code === 0) { this.watcherMessage(""); return; }
        }
      } catch {}

      const candidates = this.repoOverride ? [this.repoOverride] : localRepoCandidates();
      if (!candidates.length) {
        this.watcherMessage("Select a local repository to start PR updates.");
        return;
      }
      let repoRoot = "";
      for (const cwd of candidates) {
        try {
          const repository = await exec("git", ["rev-parse", "--show-toplevel"], { cwd });
          if (repository.code === 0 && absolutePath(repository.stdout.trim())) {
            repoRoot = repository.stdout.trim();
            break;
          }
        } catch {}
      }
      if (!repoRoot) {
        this.watcherMessage("Selected folder is not a Git repository, or Git is unavailable.");
        return;
      }
      const remote = await exec("git", ["remote", "get-url", "origin"], { cwd: repoRoot });
      if (remote.code !== 0 || !remote.stdout.trim()) {
        this.watcherMessage("The repository needs an origin remote for PR updates.");
        return;
      }

      const binary = `${this.helperDir}/space-status`;
      const executable = await exec("test", ["-x", binary]);
      if (executable.code !== 0) {
        this.watcherMessage("Build the space-status helper to enable PR updates.");
        return;
      }
      const result = await exec("sh", [
        "-c", LAUNCH_WATCHER, "herdr-space-status",
        binary, repoRoot, `${this.helperDir}/pr_watch_state.json`, this.statusFile,
        this.pidFile, `${this.helperDir}/pr_watch.log`, ...this.accountArgs,
      ]);
      this.watcherMessage(result.code === 0
        ? "Waiting for the PR watcher. If it stays stopped, check GitHub authentication and the watcher log."
        : "Could not start the PR watcher. Check the helper directory and permissions.");
    } catch {
      this.watcherMessage("Could not check or start the PR watcher. Check Git and helper access.");
    } finally {
      this.launchInFlight = false;
    }
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
    return div().size(6).mr(3).mt(1).flex_none().rounded(3).bg(color);
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

    return h_flex().flex_1().min_w(0).gap(0).items_center()
      .child(div().flex_none().text_size(11).text_color(labelColor).child(label))
      .child(div().flex_none().text_size(11).text_color(t.dim).mx(5).child("\u00b7"))
      .child(div().flex_1().min_w(0).text_size(11).text_color(t.dim).truncate().tooltip(context).child(context));
  }

  agentRow(t, a, paneById, spaceById, allSpaces) {
    const pane = paneById.get(a.pane);
    const space = spaceById.get(a.space);
    const spaceColor = lighterSpaceColor(space, allSpaces);
    const pr = panePR(this.prs, machineOf(a, spaceById), pane, this.roots);
    const state = pr ? prState(pr) : null;
    const color = pr ? prColor(pr) : t.foreground;
    const name = pr?.number ? `#${pr.number}` : (a.title || a.program?.name || a.address);
    const ci = pr?.total ? pr : null;
    const rd = state === "closed" ? { color: "#e06c75", label: "cancelled" }
      : pr ? REVIEW[pr.reviewDecision] ?? (pr.reviewDecision === ""
      ? { color: pr.isDraft ? "#8b949e" : "#3fae6a", label: pr.isDraft ? "draft" : "ready" }
      : { color: t.dim, label: "review unknown" }) : null;
    return h_flex()
      .id(`ag-${a.address}`)
      .h(52).px(8).flex_none().rounded(8).items_center().gap(10).cursor_pointer()
      .when(a.focused, (el) => el.bg(t.selected))
      .hover((s) => s.bg(t.hover))
      .on_click(() => run("pane.focus", { pane: a.pane }))
      // Match the Spaces panel: a narrow space-color bar, not a full-row tint.
      .when(Boolean(spaceColor), (el) => el.child(div().w(3).h(40).flex_none().rounded(2).bg(spaceColor)))
      // Line 1 (PR/name + CI) and line 2 (status + dot) stacked vertically.
      .child(
        v_flex().flex_1().min_w(0).gap(2).justify_center()
          .child(
            h_flex().gap(6).items_center()
              .when(Boolean(pr), (el) => el.child(div().flex_none().font_family("Symbols Nerd Font").text_size(13).text_color(color).child(prGlyph(state))))
              .child(div().flex_1().min_w(0).truncate().text_size(13).text_color(t.foreground).child(name))
              .when(Boolean(ci?.failed), (el) => el.child(ciMark(CI.failed.glyph, ci.failed, CI.failed.color, `${ci.failed} ${CI.failed.tip}`)))
              .when(Boolean(ci?.pending), (el) => el.child(ciMark(CI.pending.glyph, ci.pending, CI.pending.color, `${ci.pending} ${CI.pending.tip}`)))
              .when(Boolean(ci?.skipped), (el) => el.child(ciMark(CI.skipped.glyph, ci.skipped, CI.skipped.color, `${ci.skipped} ${CI.skipped.tip}`)))
              .when(Boolean(ci?.passed), (el) => el.child(ciMark(CI.passed.glyph, ci.passed, CI.passed.color, `${ci.passed} ${CI.passed.tip}`)))
          )
          .child(
            h_flex().gap(0).items_center().min_w(0)
              .child(this.statusDot(t, a))
              .child(this.statusLine(t, a))
              .when(Boolean(rd), (el) => el.child(div().ml(6).text_size(10).flex_none().font_medium().text_color(rd.color).child(rd.label)))
          )
      );
  }

  render() {
    const t = theme();
    const allSpaces = spaces();
    const selectedSpace = allSpaces.find((space) => space.active)?.id;
    // Keep every agent visible, but put the selected space's agents first.
    const list = [...agents()].sort((a, b) =>
      Number(b.space === selectedSpace) - Number(a.space === selectedSpace));
    const paneById = new Map(panes().map((p) => [p.pane, p]));
    const spaceById = new Map(allSpaces.map((s) => [s.id, s]));
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
      .child(v_flex().flex_1().min_h(0).px(8).pb(8).overflow_y_scrollbar().gap(2).children(list.map((a) => this.agentRow(t, a, paneById, spaceById, allSpaces))));
  }
}
