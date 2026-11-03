import { readFile } from 'node:fs/promises';
import { SourceTextModule, SyntheticModule } from 'node:vm';

function element() {
  const data = { attrs: {}, children: [] };
  const proxy = new Proxy(data, {
    get(target, name) {
      if (name === 'attrs' || name === 'items') return name === 'items' ? target.children : target.attrs;
      if (name === 'then') return undefined;
      return (...args) => {
        if (name === 'child') target.children.push(args[0]);
        else if (name === 'children') target.children.push(...args[0]);
        else if (name === 'when') { if (args[0]) args[1](proxy); }
        else target.attrs[name] = args;
        return proxy;
      };
    },
  });
  return proxy;
}

export const nodes = (node) => node && typeof node === 'object' ? [node, ...node.items.flatMap(nodes)] : [];
export const texts = (node) => node && typeof node === 'object' ? node.items.flatMap(texts) : [String(node)];
export const space = (id, active = true) => ({ id, active, machine: null, folder: `/repos/${id}`, tabs: [{ id: `tab-${id}`, active: true }], name: id, branch: 'feature', status: 'working' });
export const agent = (id, space) => ({ address: id, space, pane: `pane-${id}`, status: 'working', title: id, program: { name: 'Codex' } });

export async function loadMod(name = 'agents-space', overrides = {}) {
  const reads = [], timers = [], tasks = [], commands = [], watches = [], actions = [], state = new Map();
  const api = {
    agents: () => [], panes: () => [], spaces: () => [], terminals: () => [],
    run: (...args) => actions.push(args), theme: () => ({}), appHome: () => '/custom-herdr',
    props: () => null, keys: () => [], window: () => ({ platform: 'macos' }),
    shared: (key) => state.get(key), setShared: (key, value) => state.set(key, value),
    readTextFile: async (file) => { reads.push(file); throw new Error('missing'); },
    watchFolder: async (file) => { watches.push(file); throw new Error('no watcher'); },
    exec: async (command, args = [], options) => { commands.push({ command, args, options }); return { code: 1, stdout: '', stderr: '' }; },
    ...overrides,
  };
  const stubs = {
    'gpui-kit': { View: class {}, div: element, svg: element },
    'gpui-base': { v_flex: element, h_flex: element },
    herdr: api,
  };
  const modules = new Map();
  function source(url) {
    if (!modules.has(url.href)) modules.set(url.href, readFile(url, 'utf8').then((text) => new SourceTextModule(text, { identifier: url.href })));
    return modules.get(url.href);
  }
  const mod = await source(new URL(`../mods/${name}/main.js`, import.meta.url));
  await mod.link((specifier, parent) => {
    if (!stubs[specifier]) return source(new URL(specifier, parent.identifier));
    if (!modules.has(specifier)) {
      const values = stubs[specifier];
      modules.set(specifier, new SyntheticModule(Object.keys(values), function () {
        for (const [key, value] of Object.entries(values)) this.setExport(key, value);
      }));
    }
    return modules.get(specifier);
  });
  await mod.evaluate();
  const cx = {
    notify() {},
    spawn(fn) { tasks.push(Promise.resolve().then(() => fn(cx))); },
    timer: { every(ms, fn) { timers.push({ ms, fn }); } },
  };
  return {
    instance: new mod.namespace.default(), cx, reads, timers, commands, watches, actions, state,
    async flush() { while (tasks.length) await Promise.all(tasks.splice(0)); },
    async tick(ms) { for (const timer of timers.filter((timer) => timer.ms === ms)) await timer.fn(cx); await this.flush(); },
    async exports(file = 'status.js') { return (await source(new URL(`../mods/${name}/${file}`, import.meta.url))).namespace; },
  };
}
