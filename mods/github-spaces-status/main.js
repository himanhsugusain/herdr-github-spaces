import { View, div, svg } from "gpui-kit";
import { h_flex } from "gpui-base";
import { spaces, agents, theme, run, window, keys, readTextFile, exec, shared } from "herdr";

import { observeStatus, openPRs, statusPaths } from "./status.js";

export default class WorkspaceStatus extends View {
  init(props, cx) {
    Object.assign(this, statusPaths(props));
    this.prs = {};
    this.watcherKey = `github-spaces.watcher:${this.pidFile}`;
    this.watcher = { running: false, errors: [] };
    this.healthInFlight = false;
    observeStatus(this.statusFile, cx, (status, cx) => {
      const errors = Array.isArray(status.errors) ? status.errors.filter((error) => typeof error === "string") : [];
      if (JSON.stringify(this.prs) !== JSON.stringify(status.prs) || JSON.stringify(this.watcher.errors) !== JSON.stringify(errors)) {
        this.prs = status.prs;
        this.watcher.errors = errors;
        cx.notify();
      }
    });
    this.checkWatcher(cx);
    cx.timer.every(15000, (cx) => this.checkWatcher(cx));
  }

  checkWatcher(cx) {
    if (this.healthInFlight) return;
    this.healthInFlight = true;
    cx.spawn(async (cx) => {
      let running = false;
      try {
        const rawPid = await readTextFile(this.pidFile);
        const pid = Number(rawPid.trim());
        if (Number.isSafeInteger(pid) && pid > 1) {
          const result = await exec("kill", ["-0", String(pid)]);
          running = result.code === 0;
        }
      } catch {}
      finally { this.healthInFlight = false; }
      if (this.watcher.running !== running) {
        this.watcher.running = running;
        cx.notify();
      }
    });
  }

  // The palette key as actually bound, not a hardcoded one; without a
  // binding the hint stays useful ("commands") instead of lying.
  hint(t) {
    const mac = window()?.platform === "macos";
    const mod = { cmd: mac ? "⌘" : "Ctrl+", ctrl: mac ? "⌃" : "Ctrl+", shift: mac ? "⇧" : "Shift+", alt: mac ? "⌥" : "Alt+", option: mac ? "⌥" : "Alt+" };
    const b = keys().find((k) => k.section === "global" && k.action === "app.palette");
    const parts = b && b.keys.endsWith("--") ? [...b.keys.slice(0, -2).split("-"), "-"] : b?.keys.split("-");
    const key = parts && parts.map((p) => mod[p.toLowerCase()] ?? p.toUpperCase()).join("");
    return key ? `${key}  commands` : "commands";
  }

  render() {
    const t = theme();
    const space = spaces().find(s => s.active);
    const list = agents();
    const needs = list.filter(a => a.status === "blocked").length;
    const prs = openPRs(this.prs);
    const tracked = prs.length;
    const failed = prs.reduce((total, pr) => total + (pr.failed || 0), 0);
    const reason = !this.watcher.running && shared(this.watcherKey);
    const watcherTip = [reason, ...this.watcher.errors, `Watcher log: ${this.helperDir}/pr_watch.log`].filter(Boolean).join("\n");
    const label = (icon, text) => h_flex().gap(6).items_center().child(svg(`mods/workspace-chrome/${icon}.svg`).size(13)).child(text);
    return h_flex().size_full().px(14).gap(16).items_center().bg(t.chrome).border_t(1).border_color(t.border).text_size(11.8).text_color(t.muted)
      .child(label(space?.machine ? "server" : "monitor", space?.machine ?? "This computer"))
      .when(Boolean(space?.branch), el => el.child(label("branch", space.branch)))
      .child(h_flex().gap(6).items_center().child(div().size(6).rounded(3).bg(t.dim)).child(`${list.length} agent${list.length === 1 ? "" : "s"}`)
        .when(needs > 0, el => el.child(div().text_color(t.blocked).child(`· ${needs} needs you`))))
      .when(tracked > 0, el => el.child(h_flex().gap(5).items_center()
        .child(div().text_color(failed ? t.blocked : t.ansi2).child("●"))
        .child(div().text_color(t.muted).child(`${tracked} PR${tracked > 1 ? "s" : ""}`))))
      .child(h_flex().gap(5).items_center().tooltip(watcherTip)
        .child(div().text_color(this.watcher.running ? t.ansi2 : t.blocked).child("●"))
        .child(div().text_color(this.watcher.running ? t.muted : t.blocked).child(this.watcher.running ? "PR watcher" : "PR watcher stopped"))
        .when(this.watcher.errors.length > 0, (el) => el.child(div().text_color(t.blocked).child(`${this.watcher.errors.length} error${this.watcher.errors.length > 1 ? "s" : ""}`))))
      .child(div().flex_1())
      .child(div().id("palette").text_color(t.dim).on_click(() => run("app.palette")).child(this.hint(t)));
  }
}
