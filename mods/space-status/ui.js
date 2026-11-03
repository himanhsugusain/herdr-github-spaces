import { appHome, exec, panes, props, readTextFile, spaces, watchFolder } from "herdr";

export const absolutePath = (value) => typeof value === "string" && value.startsWith("/") && !value.includes("\0");
const object = (value) => value && typeof value === "object" && !Array.isArray(value);
const key = (machine, cwd, branch) => JSON.stringify([machine ?? null, cwd, branch ?? ""]);

export function viewProps(initial) {
  const value = props() ?? initial;
  return object(value) ? value : {};
}

export function statusPaths(initial) {
  const config = viewProps(initial);
  const helperDir = absolutePath(config.helperDir) ? config.helperDir.replace(/\/+$/, "") : `${appHome()}/mods/space-status`;
  return {
    helperDir,
    statusFile: absolutePath(config.statusFile) ? config.statusFile : `${helperDir}/status.json`,
    pidFile: absolutePath(config.pidFile) ? config.pidFile : `${helperDir}/pr_watch.pid`,
  };
}

export async function readStatus(path) {
  try {
    const value = JSON.parse(await readTextFile(path));
    if (object(value)) return { ...value, prs: object(value.prs) ? value.prs : {} };
  } catch {}
  return { prs: {} };
}

export function observeStatus(path, cx, apply) {
  let watching = false, reading = false;
  const refresh = async (cx) => {
    if (reading) return;
    reading = true;
    try { apply(await readStatus(path), cx); }
    finally { reading = false; }
  };
  const watch = (cx) => {
    if (watching) return;
    watching = true;
    cx.spawn(async (cx) => {
      try {
        for (;;) {
          await watchFolder(path.slice(0, path.lastIndexOf("/")) || "/");
          await refresh(cx);
        }
      } catch {
        // The fallback timer also retries the folder watch.
      } finally { watching = false; }
    });
  };
  watch(cx);
  cx.spawn(refresh);
  cx.timer.every(15000, (cx) => {
    watch(cx);
    cx.spawn(refresh);
  });
}

export function localRepoCandidates() {
  const space = spaces().find((item) => item.active);
  if (!space || space.machine) return [];
  const tabs = new Set(space.tabs.map((tab) => tab.id));
  const activeTab = space.tabs.find((tab) => tab.active)?.id;
  const candidates = panes().filter((pane) => tabs.has(pane.tab) && (pane.focused || pane.tab === activeTab));
  candidates.sort((a, b) => Number(Boolean(b.focused)) - Number(Boolean(a.focused)));
  return [...new Set([...candidates.map((pane) => pane.cwd), space.folder].filter(absolutePath))];
}

export function watchPaneRoots(cx) {
  const roots = new Map(), attempted = new Map();
  let inFlight = false;
  const refresh = (cx) => {
    if (inFlight) return;
    inFlight = true;
    cx.spawn(async (cx) => {
      try {
        const localTabs = new Set(spaces().filter((space) => !space.machine).flatMap((space) => space.tabs.map((tab) => tab.id)));
        const directories = new Set(panes().filter((pane) => localTabs.has(pane.tab)).map((pane) => pane.cwd).filter(absolutePath));
        for (const cwd of attempted.keys()) {
          if (!directories.has(cwd)) { attempted.delete(cwd); roots.delete(cwd); }
        }
        for (const cwd of directories) {
          if (roots.has(cwd) || Date.now() - (attempted.get(cwd) ?? 0) < 15000) continue;
          attempted.set(cwd, Date.now());
          try {
            const result = await exec("git", ["rev-parse", "--show-toplevel"], { cwd });
            const root = result.code === 0 ? result.stdout.trim() : "";
            if (absolutePath(root)) { roots.set(cwd, root); cx.notify(); }
          } catch {}
        }
      } finally { inFlight = false; }
    });
  };
  refresh(cx);
  cx.timer.every(3000, refresh);
  return roots;
}

export function panePR(prs, machine, pane, roots) {
  if (!pane?.cwd) return null;
  const exact = key(machine, pane.cwd, pane.branch);
  if (Object.prototype.hasOwnProperty.call(prs, exact)) return prs[exact];
  const root = !machine && roots?.get(pane.cwd);
  return root ? prs[key(machine, root, pane.branch)] ?? null : null;
}

export function prIdentity(pr, fallback) {
  return pr.url || JSON.stringify([pr.repository ?? fallback, pr.number]);
}

export function openPRs(prs) {
  const unique = new Map();
  for (const [key, pr] of Object.entries(prs)) {
    if (pr?.number && pr.state === "OPEN") unique.set(prIdentity(pr, key), pr);
  }
  return [...unique.values()];
}
