# GitHub Spaces for Herdr

A reusable Herdr layout with GitHub-aware spaces and agents. Colored bars mark spaces, which show PR totals and agents needing attention. All agents remain visible, with the selected space's agents first; rows show PR numbers, CI counts, and review decisions. The bottom bar shows machine, branch, global agent counts, unique open PR totals, and watcher health. Hover over watcher health for errors and startup guidance. PR rows show green `ready` or grey `draft` when GitHub has no decisive review, and red `cancelled` for closed, unmerged PRs. An open PR always takes precedence over closed PRs on the same branch.

![GitHub Spaces](preview.png)

[RECIPE.md](RECIPE.md) describes how to rebuild the layout from an empty folder. The screenshot has all workspace names, paths, task text, and terminal contents removed. No personal configuration is bundled.

## Install

Validated with Herdr 0.1.12 on macOS. Read your installation's `AGENTS.md` and the recipe first. Set `HERDR_APP_HOME` to its configuration directory for the commands below.

1. Copy `layouts/github-spaces.json` into its `layouts/` folder.
2. Copy `mods/spaces-letter`, `mods/agents-space`, `mods/github-spaces-status`, and `mods/space-status` into its `mods/` folder. Keep the bundled font and upstream license together.
3. Keep Herdr's shipped chrome, tabs, pane header, Files/Changes panels, menus, and icons installed. The status bar has its own name to avoid overwriting a shipped mod.
4. Never overwrite an existing layout or mod. Rename conflicting copies and update references. Use the configuration below if the helper or cache moves.

```sh
herdr-app check "$HERDR_APP_HOME/mods/spaces-letter"
herdr-app check "$HERDR_APP_HOME/mods/agents-space"
herdr-app check "$HERDR_APP_HOME/mods/github-spaces-status"
herdr-app do layout.switch '{"name":"github-spaces"}'
```

The layout remains usable without the helper. After the helper is built, the agents mod resolves the active local pane's working directory to a Git repository, falling back to the space's saved folder. An explicit `repo` prop overrides inference. Invalid or unavailable folders are retried during health checks. PR cache files stay under the active Herdr configuration directory.

## Configure PR updates

Requires Go 1.27+, Git, `hc`, and a Unix-like environment. Build the helper in the installation directory:

```sh
cd "$HERDR_APP_HOME/mods/space-status"
go test ./...
go build -trimpath -buildvcs=false -o space-status .
```

The `agents-space` mod starts a missing helper for the inferred local repository. You can set `content.props.repo` to an absolute path to override inference; the distributable layout leaves it empty. With no usable local repository, no daemon starts. The UI launches the helper with `--discover-repos`: each refresh includes the repositories used by live local Herdr panes, even in other spaces. Each repository is queried using its own origin owner/name. Notification delivery remains scoped to the original repository and its worktrees.

| Mod prop | Default | Purpose |
| --- | --- | --- |
| `repo` | Empty; inferred from the active local pane, then space folder | Initial repository override; agents mod only |
| `githubAccounts` | `{}` | Map GitHub owners or `OWNER/REPO` to saved `gh` logins; agents mod only |
| `helperDir` | Active Herdr home + `/mods/space-status` | Helper binary and runtime state/PID/log directory |
| `statusFile` | `helperDir` + `/status.json` | PR cache output/read path |
| `pidFile` | `helperDir` + `/pr_watch.pid` | Watcher PID used by the launcher and footer |

All three mods accept `helperDir`, `statusFile`, and `pidFile`. When changing these paths, set the same values in `content.props` on the `agents`, `spaces`, and `status` blocks. Moving the binary/runtime directory does not move the bundled UI modules. Paths with spaces or apostrophes are supported; relative paths and shell expansions are not accepted by layout props. Reload the layout after changing props. The helper's lock prevents concurrent watcher instances from running if multiple mod instances attempt startup.

For repositories without a login mapping, the helper resolves authentication internally from `--token`, then `GITHUB_TOKEN`, then `gh auth token`. For a `githubAccounts` mapping, it requests that saved login explicitly, removing inherited GitHub token variables only from the credential subprocess. An exact `OWNER/REPO` mapping takes precedence over an owner mapping. The active global `gh` login and parent environment are unchanged. Prefer the environment or authenticated `gh` so credentials do not appear in command arguments. Only configured login names are stored in the layout; never tokens. The distributable layout contains no personal login mappings. The helper does not fetch the authenticated user's profile.

The agent mod checks helper health immediately, after its first three-second tick, and roughly every 30 seconds thereafter. The helper normally polls every 60 seconds. It reads repository and terminal metadata, contacts GitHub, writes local runtime files, and can send PR-change messages to matching terminals after establishing a baseline. Working/blocked terminals hold messages. The helper itself does not post to GitHub or modify repository code.

The lists resolve local pane directories to worktree roots, so PR badges also work in subdirectories. Root results are cached while those directories remain in use. All panels watch the cache and poll every 15 seconds as a fallback; failed watches retry on that timer. Missing or malformed caches leave ordinary navigation usable.

## Run the helper directly

Both refresh/watch commands require an explicit `--repo`. GitHub owner/repository is inferred from that checkout's origin, or supplied through `--github-repo OWNER/NAME`.

```text
space-status refresh --repo PATH [--github-repo OWNER/NAME] [--output PATH] [--discover-repos] [--github-account KEY=LOGIN ...] [--once]
space-status pr-watch --repo PATH [--github-repo OWNER/NAME] [--state PATH] [--status PATH] [--pid PATH] [--skip ADDRESS ...] [--discover-repos] [--github-account KEY=LOGIN ...]
space-status trigger [--pid PATH]
```

Runtime files default beside the helper executable; flags override every output/state/PID path. With custom paths, point all three UI mods at the resulting cache and the launcher/footer at the same PID file. Without `--discover-repos`, only the explicit local repository and its worktrees are scanned. With it, badge data also covers other live local Herdr repositories. PR-change messages remain scoped to the explicit repository. Runtime caches contain live repository/PR data and should remain local.

## Validate

```sh
node scripts/sync-ui.mjs --check
node --experimental-vm-modules --test tests/*.test.mjs
cd mods/space-status
go test ./...
```

Herdr confines imports to each mod directory. Edit shared UI behavior in `mods/space-status/ui.js`, then run `node scripts/sync-ui.mjs` to update the generated `status.js` in each UI mod. The sync check rejects stale copies.

The package includes the layout, three UI mods, helper source/tests, font with required upstream attribution, recipe, and preview. It excludes personal settings, key bindings, credentials, runtime caches/logs, and compiled binaries.
