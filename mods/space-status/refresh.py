#!/usr/bin/env python3
"""Write pull-request and CI status for Herdr's space and agent lists."""

import argparse
import json
import os
import subprocess
import tempfile
import time
from pathlib import Path

INTERVAL = 60


def run(args, cwd):
    return subprocess.run(args, cwd=cwd, text=True, capture_output=True, timeout=45)


def dumps(value):
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False)


def worktrees(root):
    result = run(["git", "worktree", "list", "--porcelain"], root)
    paths = []
    for block in result.stdout.split("\n\n"):
        for line in block.splitlines():
            if line.startswith("worktree "):
                paths.append(line.split(" ", 1)[1])
    return paths or [str(root)]


def branch(cwd):
    head = run(["git", "rev-parse", "--abbrev-ref", "HEAD"], cwd)
    return head.stdout.strip() if head.returncode == 0 else None


def tally(nodes):
    failed = pending = passed = skipped = 0
    for node in nodes or []:
        conclusion = node.get("conclusion") or ""
        status = node.get("status") or node.get("state") or ""
        if conclusion in {"FAILURE", "CANCELLED", "TIMED_OUT", "STARTUP_FAILURE"} or status in {"FAILURE", "ERROR"}:
            failed += 1
        elif conclusion == "SKIPPED":
            skipped += 1
        elif conclusion in {"SUCCESS", "NEUTRAL"} or status == "SUCCESS":
            passed += 1
        else:
            pending += 1
    return {"failed": failed, "pending": pending, "passed": passed, "skipped": skipped, "total": len(nodes or [])}


def pull_requests(paths, root):
    listed = run(["gh", "pr", "list", "--author", "@me", "--state", "open", "--limit", "40", "--json", "number,headRefName,state,isDraft"], root)
    if listed.returncode != 0:
        return {}
    by_branch = {pr["headRefName"]: pr for pr in json.loads(listed.stdout)}
    recent = run(["gh", "pr", "list", "--author", "@me", "--state", "all", "--limit", "100", "--json", "number,headRefName,state,isDraft"], root)
    if recent.returncode == 0:
        for pr in json.loads(recent.stdout):
            by_branch.setdefault(pr["headRefName"], pr)
    repo = run(["gh", "repo", "view", "--json", "nameWithOwner"], root)
    if repo.returncode != 0:
        return {}
    owner, name = json.loads(repo.stdout)["nameWithOwner"].split("/", 1)
    numbers = []
    matched = []
    for cwd in paths:
        head = branch(cwd)
        pr = by_branch.get(head)
        matched.append((cwd, head, pr))
        if pr:
            numbers.append(pr["number"])
    ci = {}
    if numbers:
        fields = " ".join(
            f'n{n}: pullRequest(number: {n}) {{ commits(last: 1) {{ nodes {{ commit {{ statusCheckRollup {{ contexts(first: 100) {{ nodes {{ ... on CheckRun {{ conclusion status }} ... on StatusContext {{ state }} }} }} }} }} }} }} }}'
            for n in dict.fromkeys(numbers)
        )
        query = f'query {{ repository(owner: "{owner}", name: "{name}") {{ {fields} }} }}'
        result = run(["gh", "api", "graphql", "-f", f"query={query}"], root)
        if result.returncode == 0:
            body = (json.loads(result.stdout).get("data") or {}).get("repository") or {}
            for n in dict.fromkeys(numbers):
                commits = ((body.get(f"n{n}") or {}).get("commits") or {}).get("nodes") or []
                commit = (commits[0].get("commit") or {}) if commits else {}
                contexts = (commit.get("statusCheckRollup") or {}).get("contexts") or {}
                nodes = contexts.get("nodes") or []
                ci[n] = tally(nodes)
    prs = {}
    for cwd, head, pr in matched:
        key = dumps([None, cwd, head])
        if not pr:
            prs[key] = None
            continue
        prs[key] = {"repository": f"{owner}/{name}", "number": pr["number"], "state": pr["state"], "isDraft": pr["isDraft"], **ci.get(pr["number"], {"failed": 0, "pending": 0, "passed": 0, "skipped": 0, "total": 0})}
    return prs


def once(root, out):
    paths = worktrees(root)
    payload = {"prs": pull_requests(paths, root)}
    text = json.dumps(payload, separators=(",", ":"))
    if out.exists() and out.read_text() == text:
        return
    out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", dir=out.parent, prefix=".status-", delete=False) as file:
        tmp = Path(file.name)
        file.write(text)
    try:
        tmp.replace(out)
    finally:
        tmp.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, required=True, help="Local Git checkout whose worktrees to monitor")
    parser.add_argument("--output", type=Path, default=Path(__file__).resolve().with_name("status.json"))
    parser.add_argument("--once", action="store_true", help="Refresh once and exit")
    args = parser.parse_args()
    root = args.repo.expanduser().resolve()
    out = args.output.expanduser().resolve()
    if not root.is_dir():
        parser.error("--repo must be an existing local directory")
    if args.once:
        once(root, out)
        return
    try:
        os.nice(10)
    except OSError:
        pass
    while True:
        try:
            once(root, out)
        except Exception as exc:
            print(f"space-status: {exc}", flush=True)
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
