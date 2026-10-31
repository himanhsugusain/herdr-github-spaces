export const PR = {
  open: { glyph: "", color: "#3fae6a" },
  draft: { glyph: "", color: "#8b949e" },
  merged: { glyph: "", color: "#a371f7" },
  closed: { glyph: "", color: "#e06c75" },
};

function prState(pr) {
  if (pr.state === "MERGED") return "merged";
  if (pr.state === "CLOSED") return "closed";
  if (pr.state === "OPEN") return pr.isDraft ? "draft" : "open";
  return null;
}

export function summarize(spaces, agents, panes, terminals, prs) {
  const summaries = new Map(spaces.map((space) => [space.id, {
    blocked: 0, done: 0, prs: { open: 0, draft: 0, merged: 0, closed: 0 }, seen: new Set(),
  }]));
  const spaceByTab = new Map(spaces.flatMap((space) => space.tabs.map((tab) => [tab.id, space])));
  const spaceByAgent = new Map(agents.map((agent) => [agent.address, agent.space]));
  const terminalByAddress = new Map(terminals.map((terminal) => [terminal.address, terminal]));

  for (const agent of agents) {
    const summary = summaries.get(agent.space);
    if (summary && (agent.status === "blocked" || agent.status === "done")) summary[agent.status]++;
  }

  for (const terminal of terminals) {
    if (spaceByAgent.has(terminal.address) || terminal.state !== "live") continue;
    if (terminal.status !== "blocked" && terminal.status !== "error") continue;
    const visited = new Set([terminal.address]);
    let parent = terminal.parent;
    while (parent && !spaceByAgent.has(parent) && !visited.has(parent)) {
      visited.add(parent);
      parent = terminalByAddress.get(parent)?.parent;
    }
    const summary = summaries.get(spaceByAgent.get(parent));
    if (summary) summary.blocked++;
  }

  for (const pane of panes) {
    const space = spaceByTab.get(pane.tab);
    if (!space || !pane.cwd) continue;
    const key = JSON.stringify([space.machine ?? null, pane.cwd, pane.branch ?? ""]);
    const pr = prs[key];
    if (!pr?.number) continue;
    const state = prState(pr);
    if (!state) continue;
    const summary = summaries.get(space.id);
    const identity = pr.url || JSON.stringify([pr.repository ?? [space.machine ?? null, pane.cwd], pr.number]);
    if (summary.seen.has(identity)) continue;
    summary.seen.add(identity);
    summary.prs[state]++;
  }
  return summaries;
}
