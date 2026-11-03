import assert from 'node:assert/strict';
import { test } from 'node:test';
import { loadMod, space, agent, nodes, texts } from './mod-harness.mjs';

const success = (stdout = '') => ({ code: 0, stdout, stderr: '' });
const failure = () => ({ code: 1, stdout: '', stderr: '' });
const pr = { repository: 'owner/repo', number: 42, state: 'OPEN', total: 10, failed: 1, pending: 2, skipped: 3, passed: 4, reviewDecision: 'APPROVED' };
const key = (cwd, branch = 'feature', machine = null) => JSON.stringify([machine, cwd, branch]);

function launcher(commands, valid = () => true) {
  return async (command, args, options) => {
    commands.push({ command, args, options });
    if (command === 'git' && args[0] === 'rev-parse') return valid(options.cwd) ? success(options.cwd.replace(/\/src$/, '')) : failure();
    if (command === 'git') return success('git@github.com:owner/repo.git');
    return success();
  };
}

test('launcher prefers live pane cwd over a home-folder space', async () => {
  const s = { ...space('a'), folder: '/home/person' };
  const commands = [];
  const h = await loadMod('agents-space', {
    spaces: () => [s], panes: () => [{ tab: 'tab-a', focused: true, cwd: '/repos/a/src' }],
    exec: launcher(commands, (cwd) => cwd !== '/home/person'),
  });
  h.instance.init({}, h.cx); await h.flush();
  assert.equal(commands.find((c) => c.command === 'sh').args[4], '/repos/a');
  assert.ok(!commands.some((c) => c.options?.cwd === '/home/person'));
});

test('launcher recovers when an initially missing space becomes available', async () => {
  let spaces = []; const commands = [];
  const h = await loadMod('agents-space', { spaces: () => spaces, exec: launcher(commands) });
  h.instance.init({}, h.cx); await h.flush();
  assert.ok(!commands.some((c) => c.command === 'sh'));
  spaces = [space('b')]; await h.tick(3000);
  assert.equal(commands.find((c) => c.command === 'sh').args[4], '/repos/b');
});

test('launcher recovers from a nonrepository and respects explicit overrides', async () => {
  let spaces = [{ ...space('a'), folder: '/home/person' }]; const commands = [];
  const h = await loadMod('agents-space', { spaces: () => spaces, exec: launcher(commands, (cwd) => cwd.startsWith('/repos/')) });
  h.instance.init({}, h.cx); await h.flush();
  assert.match(h.state.get(h.instance.watcherKey), /not a Git repository/);
  spaces = [space('b')]; await h.tick(3000);
  assert.equal(commands.find((c) => c.command === 'sh').args[4], '/repos/b');
  const explicit = await loadMod('agents-space', { spaces: () => spaces, exec: launcher(commands) });
  explicit.instance.init({ repo: '/repos/override' }, explicit.cx); await explicit.flush();
  assert.equal(commands.filter((c) => c.command === 'sh').at(-1).args[4], '/repos/override');
});

test('a running watcher is not replaced when the selected repository changes', async () => {
  let spaces = [space('a')]; const commands = [];
  const h = await loadMod('agents-space', {
    spaces: () => spaces,
    readTextFile: async (file) => file.endsWith('.pid') ? '2345' : '{}',
    exec: launcher(commands),
  });
  h.instance.init({}, h.cx); await h.flush(); spaces = [space('b')]; await h.tick(3000);
  assert.deepEqual(commands.map((c) => c.command), ['kill', 'kill']);
});

test('remote spaces never launch a local watcher or resolve their pane paths locally', async () => {
  const h = await loadMod('agents-space', {
    spaces: () => [{ ...space('a'), machine: 'server' }],
    panes: () => [{ tab: 'tab-a', focused: true, cwd: '/remote/repo' }],
  });
  h.instance.init({}, h.cx); await h.flush(); await h.tick(3000);
  assert.deepEqual(h.commands, []);
});

test('all three panels read custom cache paths and health checks use the custom PID', async () => {
  const props = { helperDir: "/tmp/helper's dir", statusFile: '/tmp/cache/custom.json', pidFile: '/tmp/runtime/custom.pid' };
  for (const name of ['agents-space', 'spaces-letter', 'github-spaces-status']) {
    const h = await loadMod(name); h.instance.init(props, h.cx); await h.flush();
    assert.ok(h.reads.includes(props.statusFile), name);
    assert.ok(h.watches.includes('/tmp/cache'), name);
    if (name !== 'spaces-letter') assert.ok(h.reads.includes(props.pidFile), name);
    assert.ok(!h.reads.some((file) => file.startsWith('/custom-herdr')), name);
  }
});

test('custom helper paths are passed literally to the launcher', async () => {
  const commands = []; const h = await loadMod('agents-space', { exec: launcher(commands) });
  h.instance.init({ repo: '/repos/a', helperDir: "/tmp/helper's dir", statusFile: '/tmp/cache.json', pidFile: '/tmp/custom.pid' }, h.cx);
  await h.flush();
  assert.deepEqual(commands.find((c) => c.command === 'sh').args.slice(3), [
    "/tmp/helper's dir/space-status", '/repos/a', "/tmp/helper's dir/pr_watch_state.json", '/tmp/cache.json', '/tmp/custom.pid', "/tmp/helper's dir/pr_watch.log",
  ]);
});

test('cache polling updates every panel after a folder-watch failure and retries watching', async () => {
  for (const name of ['agents-space', 'spaces-letter', 'github-spaces-status']) {
    let prs = {};
    const h = await loadMod(name, { readTextFile: async (file) => file.endsWith('.json') ? JSON.stringify({ prs }) : '' });
    h.instance.init({}, h.cx); await h.flush();
    assert.deepEqual(h.instance.prs, {});
    prs = { [key('/repos/a')]: pr }; await h.tick(15000);
    assert.deepEqual(h.instance.prs, prs, name);
    assert.equal(h.watches.length, 2, name);
  }
});

test('agent badges resolve subdirectory roots, invalidate moved paths, and keep branches separate', async () => {
  let cwd = '/repos/a/src'; const a = agent('one', 'a'), commands = [];
  const h = await loadMod('agents-space', {
    spaces: () => [space('a')], agents: () => [a],
    panes: () => [{ pane: a.pane, tab: 'tab-a', cwd, branch: 'feature', focused: true }],
    exec: launcher(commands),
  });
  h.instance.init({}, h.cx); await h.flush(); h.instance.prs = { [key('/repos/a')]: pr };
  const rendered = texts(h.instance.render());
  for (const text of ['#42', '1', '2', '3', '4', 'approved']) assert.ok(rendered.includes(text));
  const exports = await h.exports();
  assert.equal(exports.panePR(h.instance.prs, null, { cwd, branch: 'other' }, h.instance.roots), null);
  assert.equal(exports.panePR(h.instance.prs, 'server', { cwd, branch: 'feature' }, h.instance.roots), null);
  cwd = '/repos/b/src'; await h.tick(3000);
  assert.ok(!h.instance.roots.has('/repos/a/src'));
  assert.ok(!texts(h.instance.render()).includes('#42'));
  const calls = commands.length; await h.tick(3000); assert.equal(commands.length, calls);
});

test('root lookup does not confuse nested repositories or override an explicit no-PR cache entry', async () => {
  const h = await loadMod(); const { panePR } = await h.exports();
  const roots = new Map([['/repos/a/nested/src', '/repos/a/nested']]);
  const pane = { cwd: '/repos/a/nested/src', branch: 'feature' };
  assert.equal(panePR({ [key('/repos/a')]: pr }, null, pane, roots), null);
  assert.equal(panePR({ [key('/repos/a/nested')]: pr, [key(pane.cwd)]: null }, null, pane, roots), null);
});

test('Spaces counts a subdirectory PR once and preserves descendant attention', async () => {
  const a = { ...agent('one', 'a'), status: 'done' };
  const b = agent('two', 'a');
  const panes = [{ pane: a.pane, tab: 'tab-a', cwd: '/repos/a/src', branch: 'feature' }, { pane: b.pane, tab: 'tab-a', cwd: '/repos/a', branch: 'feature' }];
  const h = await loadMod('spaces-letter', { spaces: () => [space('a')], panes: () => panes, exec: launcher([]) });
  h.instance.init({}, h.cx); await h.flush();
  const { summarize } = await h.exports('summary.js');
  const summary = summarize([space('a')], [a, b], panes, [{ address: 'child', parent: a.address, state: 'live', status: 'blocked' }], { [key('/repos/a')]: pr }, h.instance.roots).get('a');
  assert.equal(summary.prs.open, 1); assert.equal(summary.done, 1); assert.equal(summary.blocked, 1);
});

test('Spaces counts only PRs represented in its agent rows', async () => {
  const a = agent('one', 'a');
  const panes = [
    { pane: a.pane, tab: 'tab-b', cwd: '/repos/a', branch: 'feature' },
    { pane: 'shell', tab: 'tab-a', cwd: '/repos/old', branch: 'feature' },
  ];
  const prs = { [key('/repos/a')]: pr, [key('/repos/old')]: { ...pr, number: 9, state: 'CLOSED' } };
  const h = await loadMod('spaces-letter', { spaces: () => [space('a'), space('b', false)], agents: () => [a], panes: () => panes });
  h.instance.init({}, h.cx); await h.flush();
  const { summarize } = await h.exports('summary.js');
  const summaries = summarize([space('a'), space('b', false)], [a], panes, [], prs, h.instance.roots);
  assert.equal(summaries.get('a').prs.open, 1);
  assert.equal(summaries.get('a').prs.closed, 0);
  assert.equal(summaries.get('b').prs.open, 0);
  const agents = await loadMod('agents-space', { spaces: () => [space('a')], agents: () => [a], panes: () => panes });
  agents.instance.init({}, agents.cx); await agents.flush();
  agents.instance.prs = prs;
  assert.ok(texts(agents.instance.render()).includes('#42'));
  assert.ok(!texts(agents.instance.render()).includes('#9'));
});

test('footer deduplicates PRs and CI by repository and number', async () => {
  const h = await loadMod('github-spaces-status'); h.instance.init({}, h.cx); await h.flush();
  h.instance.prs = { a: pr, b: { ...pr }, c: { ...pr, repository: 'owner/other' }, d: { ...pr, number: 9, state: 'CLOSED' } };
  const { openPRs } = await h.exports(); const unique = openPRs(h.instance.prs);
  assert.equal(unique.length, 2); assert.equal(unique.reduce((sum, pr) => sum + pr.failed, 0), 2);
  assert.ok(texts(h.instance.render()).includes('2 PRs'));
});

test('footer exposes launcher diagnostics in the watcher tooltip', async () => {
  const h = await loadMod('github-spaces-status'); h.instance.init({}, h.cx); await h.flush();
  h.state.set(h.instance.watcherKey, 'Selected folder is not a Git repository.');
  assert.ok(nodes(h.instance.render()).some((node) => node.attrs.tooltip?.[0].includes('not a Git repository')));
});

test('Agents retains all spaces with selected-space agents first and focus actions intact', async () => {
  const h = await loadMod('agents-space', { spaces: () => [space('a'), space('b', false)], agents: () => [agent('b', 'b'), agent('a', 'a')] });
  h.instance.init({}, h.cx); await h.flush();
  const rows = nodes(h.instance.render()).filter((node) => node.attrs.id?.[0].startsWith('ag-'));
  assert.deepEqual(rows.map((row) => row.attrs.id[0]), ['ag-a', 'ag-b']);
  rows[1].attrs.on_click[0](); assert.deepEqual(h.actions, [['pane.focus', { pane: 'pane-b' }]]);
});

test('long blocked context shrinks with a tooltip while its status label stays visible', async () => {
  const h = await loadMod(); h.instance.init({}, h.cx); await h.flush();
  const a = { ...agent('one', 'a'), status: 'blocked', title: 'A long task title '.repeat(20) };
  const row = h.instance.statusLine({}, a);
  assert.deepEqual(row.attrs.flex_1, []); assert.deepEqual(row.attrs.min_w, [0]);
  const context = nodes(row).find((node) => node.attrs.tooltip?.[0] === a.title);
  assert.deepEqual(context.attrs.min_w, [0]); assert.deepEqual(context.attrs.truncate, []);
  assert.deepEqual(row.items[0].attrs.flex_none, []);
});

test('launcher passes repository login mappings as literal arguments and enables discovery', async () => {
  const commands = []; const h = await loadMod('agents-space', { exec: launcher(commands) });
  h.instance.init({ repo: '/repos/a', githubAccounts: { 'personal/layout': 'personal-login', 'work-org': 'work-login', 'invalid;key': 'bad-login', valid: '$(bad)' } }, h.cx);
  await h.flush();
  const launch = commands.find((command) => command.command === 'sh');
  assert.match(launch.args[1], /--discover-repos/);
  assert.match(launch.args[1], /"\$@"/);
  assert.deepEqual(launch.args.slice(9), ['--github-account', 'personal/layout=personal-login', '--github-account', 'work-org=work-login']);
});


test('PR rows show an explicit review state when no decision is available', async () => {
  const a = agent('one', 'a');
  const h = await loadMod('agents-space', {
    spaces: () => [space('a')], agents: () => [a],
    panes: () => [{ pane: a.pane, tab: 'tab-a', cwd: '/repos/a', branch: 'feature' }],
  });
  h.instance.init({}, h.cx); await h.flush();
  h.instance.prs = { [key('/repos/a')]: { ...pr, reviewDecision: '' } };
  let label = nodes(h.instance.render()).find((node) => node.items.includes('ready'));
  assert.ok(label);
  assert.deepEqual(label.attrs.text_color, ['#3fae6a']);
  h.instance.prs[key('/repos/a')].isDraft = true;
  label = nodes(h.instance.render()).find((node) => node.items.includes('draft'));
  assert.ok(label);
  assert.deepEqual(label.attrs.text_color, ['#8b949e']);
  delete h.instance.prs[key('/repos/a')].reviewDecision;
  assert.ok(texts(h.instance.render()).includes('review unknown'));
  for (const reviewDecision of ['', 'APPROVED', 'CHANGES_REQUESTED', undefined]) {
    h.instance.prs[key('/repos/a')] = { ...pr, state: 'CLOSED', isDraft: true, reviewDecision };
    const row = h.instance.render();
    label = nodes(row).find((node) => node.items.includes('cancelled'));
    assert.ok(label);
    assert.deepEqual(label.attrs.text_color, ['#e06c75']);
    for (const stale of ['ready', 'draft', 'approved', 'changes requested', 'review unknown']) {
      assert.ok(!texts(row).includes(stale));
    }
  }
  h.instance.prs[key('/repos/a')] = { ...pr, state: 'MERGED' };
  assert.ok(!texts(h.instance.render()).includes('cancelled'));
  assert.ok(texts(h.instance.render()).includes('approved'));
});


test('native Herdr props take precedence over empty View initialization arguments', async () => {
  const config = { repo: '/repos/explicit', helperDir: '/tmp/helper', statusFile: '/tmp/cache.json', pidFile: '/tmp/watcher.pid', githubAccounts: { 'work-org/app': 'work-login' } };
  for (const name of ['agents-space', 'spaces-letter', 'github-spaces-status']) {
    const commands = [];
    const h = await loadMod(name, { props: () => config, exec: launcher(commands) });
    h.instance.init({}, h.cx); await h.flush();
    assert.ok(h.reads.includes(config.statusFile), name);
    if (name === 'agents-space') {
      const launch = commands.find((c) => c.command === 'sh');
      assert.equal(launch.args[4], config.repo);
      assert.deepEqual(launch.args.slice(9), ['--github-account', 'work-org/app=work-login']);
    }
    if (name !== 'spaces-letter') assert.ok(h.reads.includes(config.pidFile), name);
  }
});
