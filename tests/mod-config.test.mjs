import assert from 'node:assert/strict';
import { test } from 'node:test';
import { loadMod } from './mod-harness.mjs';
const loadAgentMod = (overrides) => loadMod('agents-space', overrides);

test('an empty repo disables watcher startup while reading the selected app home', async () => {
  const h = await loadAgentMod();
  h.instance.init({}, h.cx);
  await h.flush();
  assert.ok(h.reads.includes('/custom-herdr/mods/space-status/status.json'));
  assert.ok(h.reads.includes('/custom-herdr/mods/space-status/pr_watch.pid'));
  assert.deepEqual(h.instance.prs, {});
  assert.deepEqual(h.commands, []);
  assert.deepEqual(h.timers.map((x) => x.ms), [3000, 15000, 3000, 30000]);
});

test('the active local space folder is inferred when repo is not configured', async () => {
  const h = await loadAgentMod({
    spaces: () => [{ active: true, machine: null, folder: '/tmp/active-repo', tabs: [] }],
    exec: async (command, args = []) => {
      h.commands.push({ command, args });
      if (command === 'git') return { code: 0, stdout: args[0] === 'rev-parse' ? '/tmp/active-repo' : 'https://github.com/example/project.git', stderr: '' };
      return { code: 0, stdout: '', stderr: '' };
    },
  });
  h.instance.init({}, h.cx);
  await h.flush();
  const launch = h.commands.find((c) => c.command === 'sh');
  assert.ok(launch, 'expected watcher launch for inferred repo');
  assert.equal(launch.args[4], '/tmp/active-repo');
});

test('configured repo starts the helper with literal positional paths', async () => {
  const repo = "/tmp/repo with 'quote";
  const h = await loadAgentMod({
    exec: async (command, args = []) => {
      h.commands.push({ command, args });
      if (command === 'git') return { code: 0, stdout: args[0] === 'rev-parse' ? repo : 'https://github.com/example/project.git', stderr: '' };
      return { code: 0, stdout: '', stderr: '' };
    },
  });
  h.instance.init({ repo }, h.cx);
  await h.flush();
  const launch = h.commands.find((c) => c.command === 'sh');
  assert.ok(launch, 'expected watcher launch');
  assert.deepEqual(launch.args.slice(0, 3), ['-c', launch.args[1], 'herdr-space-status']);
  assert.match(launch.args[1], /umask 077/);
  assert.match(launch.args[1], /nohup/);
  assert.equal(launch.args[4], repo);
  assert.equal(launch.args[5], '/custom-herdr/mods/space-status/pr_watch_state.json');
  assert.equal(launch.args[6], '/custom-herdr/mods/space-status/status.json');
});

test('a live watcher PID prevents another launch', async () => {
  const h = await loadAgentMod({
    readTextFile: async (file) => {
      h.reads.push(file);
      if (file.endsWith('/pr_watch.pid')) return '2345';
      throw new Error('missing status cache');
    },
    exec: async (command, args = []) => {
      h.commands.push({ command, args });
      return { code: command === 'kill' ? 0 : 1, stdout: '', stderr: '' };
    },
  });
  h.instance.init({ repo: '/tmp/repo' }, h.cx);
  await h.flush();
  assert.deepEqual(h.commands, [{ command: 'kill', args: ['-0', '2345'] }]);
});

test('malformed cache data leaves the ordinary agent list usable', async () => {
  for (const raw of ['null', '[]', 'false', '{invalid', '{"prs": []}', '{"prs": 7}']) {
    const h = await loadAgentMod({ readTextFile: async () => raw });
    h.instance.init({}, h.cx);
    await h.flush();
    assert.deepEqual(h.instance.prs, {});
  }
});
