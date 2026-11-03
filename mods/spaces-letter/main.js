import { View, div } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { agents, panes, spaces, terminals, run, theme } from "herdr";
import { PR, summarize } from "./summary.js";
import { observeStatus, statusPaths, watchPaneRoots } from "./status.js";

const MARK = { ansi6: "#22a99a", ansi3: "#e3814f", ansi4: "#5b8def", ansi5: "#8b7cf6", ansi2: "#3fae6a", ansi1: "#d9649a" };
function prMark(state, count) {
  const { glyph, color } = PR[state];
  return h_flex().flex_none().gap(2).items_center().text_color(color)
    .tooltip(`${count} ${state} pull request${count === 1 ? "" : "s"}`)
    .child(div().font_family("Symbols Nerd Font").text_size(12).child(glyph))
    .child(div().text_size(11).font_medium().child(String(count)));
}

function attentionMark(t, summary) {
  const count = summary.blocked + summary.done;
  const color = summary.blocked ? t.blocked : t.done;
  const details = [summary.blocked && `${summary.blocked} blocked`, summary.done && `${summary.done} finished`].filter(Boolean).join(", ");
  return h_flex().flex_none().gap(3).items_center().text_color(color)
    .tooltip(`${count} agent${count === 1 ? " needs" : "s need"} attention: ${details}`)
    .child(div().size(14).rounded(4).bg(color + "26").text_size(10.5).font_bold().flex().items_center().justify_center().child("!"))
    .child(div().text_size(11).font_medium().child(String(count)));
}

// Status dot color for a space's most urgent agent status.
function statusDotColor(t, status) {
  if (status === "working") return t.working ?? t.accent;
  if (status === "blocked") return t.blocked ?? "#e06c75";
  if (status === "done") return t.done ?? "#3fae6a";
  if (status === "idle") return t.idle ?? t.dim;
  return null;
}

// Show only the last segment of a branch name (after the last /).
function shortBranch(branch) {
  if (!branch) return null;
  const parts = branch.split("/");
  return parts[parts.length - 1];
}

export default class Spaces extends View {
  init(props, cx) {
    this.prs = {};
    this.roots = watchPaneRoots(cx);
    observeStatus(statusPaths(props).statusFile, cx, (status, cx) => {
      if (JSON.stringify(status.prs) === JSON.stringify(this.prs)) return;
      this.prs = status.prs;
      cx.notify();
    });
  }

  render() {
    const t = theme();
    const sp = spaces();
    const summaries = summarize(sp, agents(), panes(), terminals(), this.prs ?? {}, this.roots);
    if (!this.colors) this.colors = {};
    const palette = ["ansi6", "ansi3", "ansi4", "ansi5", "ansi2", "ansi1"];
    return v_flex().size_full().min_h(0).bg(t.chrome).text_size(13)
      .child(h_flex().h(31).pl(16).pr(10).items_center().justify_between().text_size(11.5).text_color(t.dim)
        .child(div().child("SPACES"))
        .child(div().id("space-new").p(4).rounded(5).hover(s => s.bg(t.hover).text_color(t.foreground)).on_click(() => run("space.new")).child("+")))
      .child(v_flex().flex_1().min_h(0).px(8).pb(8).overflow_y_scrollbar().gap(2).children(sp.map((w, i) => {
        if (!this.colors[w.id]) this.colors[w.id] = palette[i % palette.length];
        const fill = MARK[this.colors[w.id]] ?? t.ansi6;
        const summary = summaries.get(w.id);
        const hasAttention = summary.blocked + summary.done > 0;
        const rowH = 48;
        const dotColor = statusDotColor(t, w.status);
        const branch = shortBranch(w.branch);
        return h_flex().id(`space-${w.id}`).h(rowH).px(0).flex_none().items_center().gap(0).rounded(8).cursor_pointer()
          .when(w.active, (el) => el.bg(t.selected))
          .hover((s) => s.bg(t.hover))
          .on_click(() => run("space.select", { id: w.id }))
          // Colored left bar — stretches to row height with 6px vertical margin.
          .child(div().w(3).h(rowH - 12).flex_none().rounded(2).bg(fill).ml(4))
          // Two-line content.
          .child(
            v_flex().flex_1().min_w(0).gap(1).justify_center().pl(8).pr(8)
              // Line 1: name + PR counts + attention.
              .child(
                h_flex().gap(6).items_center()
                  .child(div().flex_1().min_w(0).truncate().text_size(13).text_color(t.foreground).tooltip(w.name).child(w.name))
                  .children(Object.keys(PR).filter((state) => summary.prs[state]).map((state) => prMark(state, summary.prs[state])))
                  .when(hasAttention, (el) => el.child(attentionMark(t, summary)))
              )
              // Line 2: status dot + branch name.
              .child(
                h_flex().gap(5).items_center().h(16)
                  .when(Boolean(dotColor), (el) => el.child(div().size(8).flex_none().rounded(4).bg(dotColor)))
                  .when(Boolean(branch), (el) => el.child(div().text_size(11).text_color(t.dim).truncate().tooltip(w.branch).child(branch)))
              )
          );
      })));
  }
}
