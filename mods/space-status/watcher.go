package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	watchInterval = 60 * time.Second
	bodyLimit     = 700
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

type watchAgent struct {
	Address string `json:"address"`
	CWD     string `json:"cwd"`
	Where   string `json:"where"`
}

type watchSlot struct {
	PR       string         `json:"pr"`
	Snapshot map[string]any `json:"snapshot"`
	Pending  []string       `json:"pending"`
}

type watcher struct {
	root       string
	githubRepo string
	statePath  string
	skip       map[string]bool
}

func runPRWatch(args []string) error {
	fs := newFlagSet("pr-watch")
	repoFlag := fs.String("repo", ".", "Local repository/worktree root used for Git and gh")
	githubRepoFlag := fs.String("github-repo", "", "GitHub repository as OWNER/NAME (inferred with gh when omitted)")
	stateFlag := fs.String("state", "", "State JSON path (default: ~/.config/herdr-app/mods/space-status/pr_watch_state.json)")
	var skip stringList
	fs.Var(&skip, "skip", "Agent address to skip (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := absolutePath(*repoFlag)
	if err != nil {
		return err
	}
	if *stateFlag == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return homeErr
		}
		*stateFlag = filepath.Join(home, ".config", "herdr-app", "mods", "space-status", "pr_watch_state.json")
	}
	statePath, err := absolutePath(*stateFlag)
	if err != nil {
		return err
	}
	githubRepo := strings.TrimSpace(*githubRepoFlag)
	if githubRepo == "" {
		data, runErr := ghJSON(root, "gh", "repo", "view", "--json", "nameWithOwner")
		if runErr != nil {
			return runErr
		}
		var info struct {
			NameWithOwner string `json:"nameWithOwner"`
		}
		if err := decodeJSON(data, &info); err != nil {
			return err
		}
		githubRepo = info.NameWithOwner
	}
	if len(strings.Split(githubRepo, "/")) != 2 {
		return fmt.Errorf("invalid --github-repo %q: expected OWNER/NAME", githubRepo)
	}
	ignored := make(map[string]bool)
	for _, address := range skip {
		ignored[address] = true
	}
	if address := os.Getenv("HC_TERMINAL_ID"); address != "" {
		ignored[address] = true
	}
	w := &watcher{root: root, githubRepo: githubRepo, statePath: statePath, skip: ignored}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		started := time.Now()
		if err := w.once(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "pr-watch: %v\n", err)
		}
		wait := watchInterval - time.Since(started)
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (w *watcher) once(ctx context.Context) error {
	state, err := w.loadState()
	if err != nil {
		return err
	}
	agents, err := w.piAgents(ctx)
	if err != nil {
		return err
	}
	prs, err := w.openPRs(ctx)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, agent := range agents {
		name := branch(agent.CWD)
		pr, matched := prs[name]
		if !matched || name == "" || name == "HEAD" {
			continue
		}
		seen[agent.Address] = true
		current, err := w.snapshot(ctx, pr.Number)
		if err != nil {
			return err
		}
		slot, exists := state[agent.Address]
		if !exists || slot.Snapshot == nil {
			state[agent.Address] = watchSlot{PR: fmt.Sprint(pr.Number), Snapshot: current}
			fmt.Printf("pr-watch: baseline %s PR #%d\n", agent.Where, pr.Number)
			continue
		}
		lines := describe(slot.Snapshot, current)
		if len(lines) == 0 && len(slot.Pending) > 0 {
			lines = slot.Pending
		}
		if len(lines) == 0 {
			state[agent.Address] = watchSlot{PR: fmt.Sprint(pr.Number), Snapshot: current}
			continue
		}
		if w.deliver(ctx, agent.Address, makeMessage(current, lines)) {
			state[agent.Address] = watchSlot{PR: fmt.Sprint(pr.Number), Snapshot: current}
			fmt.Printf("pr-watch: sent %s PR #%d (%d lines)\n", agent.Address, pr.Number, len(lines))
		} else {
			state[agent.Address] = watchSlot{PR: fmt.Sprint(pr.Number), Snapshot: slot.Snapshot, Pending: lines}
		}
	}
	for address := range state {
		if !seen[address] && !w.skip[address] {
			delete(state, address)
		}
	}
	return w.saveState(state)
}

func (w *watcher) loadState() (map[string]watchSlot, error) {
	state := make(map[string]watchSlot)
	data, err := os.ReadFile(w.statePath)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := decodeJSON(data, &state); err != nil {
		return nil, fmt.Errorf("read watcher state: %w", err)
	}
	return state, nil
}

func (w *watcher) saveState(state map[string]watchSlot) error {
	data, err := jsonText(state, true)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.statePath), 0o755); err != nil {
		return err
	}
	tmp := w.statePath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.statePath)
}

func (w *watcher) piAgents(ctx context.Context) ([]watchAgent, error) {
	args := []string{"hc", "ls", "--json"}
	stdout, stderr, err := command(ctx, "", 45*time.Second, args...)
	if err != nil {
		return nil, commandError(args, stdout, stderr, err)
	}
	var response struct {
		Terminals []map[string]any `json:"terminals"`
	}
	if err := decodeJSON(stdout, &response); err != nil {
		return nil, err
	}
	var agents []watchAgent
	for _, item := range response.Terminals {
		address, cwd := text(item["address"]), text(item["cwd"])
		if text(item["agent"]) != "pi" || text(item["state"]) != "live" || address == "" || cwd == "" || w.skip[address] {
			continue
		}
		where := text(item["where"])
		if where == "" {
			where = address
		}
		agents = append(agents, watchAgent{Address: address, CWD: cwd, Where: where})
	}
	return agents, nil
}

func (w *watcher) openPRs(ctx context.Context) (map[string]pullRequest, error) {
	args := []string{"gh", "pr", "list", "--repo", w.githubRepo, "--author", "@me", "--state", "open", "--limit", "40", "--json", "number,headRefName,title,url"}
	stdout, stderr, err := command(ctx, w.root, 45*time.Second, args...)
	if err != nil {
		return nil, commandError(args, stdout, stderr, err)
	}
	var prs []pullRequest
	if err := decodeJSON(stdout, &prs); err != nil {
		return nil, err
	}
	result := make(map[string]pullRequest, len(prs))
	for _, pr := range prs {
		result[pr.HeadRef] = pr
	}
	return result, nil
}

func (w *watcher) snapshot(ctx context.Context, number int) (map[string]any, error) {
	args := []string{"gh", "pr", "view", fmt.Sprint(number), "--repo", w.githubRepo, "--json", "number,title,url,reviewDecision,isDraft,headRefOid,updatedAt,statusCheckRollup,comments,reviews"}
	stdout, stderr, err := command(ctx, w.root, 60*time.Second, args...)
	if err != nil {
		return nil, commandError(args, stdout, stderr, err)
	}
	var pr map[string]any
	if err := decodeJSON(stdout, &pr); err != nil {
		return nil, err
	}

	inlineArgs := []string{"gh", "api", "--paginate", fmt.Sprintf("repos/%s/pulls/%d/comments", w.githubRepo, number), "--jq", "[.[] | {id: .id, user: .user.login, updated_at: .updated_at, body: .body, path: .path}]"}
	inlineData, _, inlineErr := command(ctx, w.root, 60*time.Second, inlineArgs...)
	var notes []map[string]any
	if inlineErr == nil && len(strings.TrimSpace(string(inlineData))) > 0 {
		_ = decodeJSON(inlineData, &notes)
	}
	comments := make([]map[string]any, 0)
	for _, raw := range list(pr["comments"]) {
		comment := object(raw)
		author := text(object(comment["author"])["login"])
		if author == "" {
			author = "unknown"
		}
		at := comment["updatedAt"]
		if at == nil {
			at = comment["createdAt"]
		}
		comments = append(comments, map[string]any{"id": comment["id"], "author": author, "at": at, "body": clip(text(comment["body"]))})
	}
	reviews := make([]map[string]any, 0)
	for _, raw := range list(pr["reviews"]) {
		review := object(raw)
		author := text(object(review["author"])["login"])
		if author == "" {
			author = "unknown"
		}
		reviews = append(reviews, map[string]any{"id": review["id"], "author": author, "state": review["state"], "at": review["submittedAt"], "body": clip(text(review["body"]))})
	}
	inline := make([]map[string]any, 0, len(notes))
	for _, note := range notes {
		author := text(note["user"])
		if author == "" {
			author = "unknown"
		}
		inline = append(inline, map[string]any{"id": note["id"], "author": author, "at": note["updated_at"], "path": note["path"], "body": clip(text(note["body"]))})
	}
	checks := tallyPRChecks(list(pr["statusCheckRollup"]))
	fingerprint := map[string]any{
		"head":     pr["headRefOid"],
		"decision": stringValue(pr["reviewDecision"]),
		"draft":    pr["isDraft"] == true,
		"checks":   checks,
		"comments": comments,
		"reviews":  reviews,
		"inline":   inline,
	}
	current := map[string]any{
		"number":      pr["number"],
		"title":       stringValue(pr["title"]),
		"url":         stringValue(pr["url"]),
		"fingerprint": fingerprint,
	}
	return normalizeMap(current)
}

func normalizeMap(value map[string]any) (map[string]any, error) {
	data, err := jsonText(value, false)
	if err != nil {
		return nil, err
	}
	var normalized map[string]any
	if err := decodeJSON(data, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func clip(body string) string {
	body = strings.Join(strings.Fields(body), " ")
	if strings.HasPrefix(body, "<!--") {
		if index := strings.LastIndex(body, "-->"); index >= 0 {
			body = strings.TrimSpace(body[index+3:])
		}
	}
	runes := []rune(body)
	if len(runes) <= bodyLimit {
		return body
	}
	return strings.TrimRight(string(runes[:bodyLimit]), " \t\r\n") + "…"
}

func tallyPRChecks(checks []any) map[string]any {
	failed := make([]string, 0)
	pending, passed := 0, 0
	for _, raw := range checks {
		check := object(raw)
		conclusion, status := text(check["conclusion"]), text(check["status"])
		if status == "" {
			status = text(check["state"])
		}
		name := text(check["name"])
		if name == "" {
			name = text(check["context"])
		}
		if name == "" {
			name = "check"
		}
		if contains([]string{"FAILURE", "CANCELLED", "TIMED_OUT", "STARTUP_FAILURE"}, conclusion) || contains([]string{"FAILURE", "ERROR"}, status) {
			failed = append(failed, name)
		} else if contains([]string{"SUCCESS", "NEUTRAL", "SKIPPED"}, conclusion) || status == "SUCCESS" {
			passed++
		} else if status != "COMPLETED" && status != "" || conclusion == "" {
			pending++
		}
	}
	sort.Strings(failed)
	unique := failed[:0]
	for _, name := range failed {
		if len(unique) == 0 || unique[len(unique)-1] != name {
			unique = append(unique, name)
		}
	}
	return map[string]any{"failed": unique, "pending": pending, "passed": passed}
}

func itemMap(items any) map[string]map[string]any {
	result := make(map[string]map[string]any)
	for _, raw := range list(items) {
		item := object(raw)
		if item["id"] != nil {
			result[text(item["id"])] = item
		}
	}
	return result
}

func describe(previous, current map[string]any) []string {
	old, next := object(previous["fingerprint"]), object(current["fingerprint"])
	lines := make([]string, 0)
	if !reflect.DeepEqual(old["head"], next["head"]) {
		head := text(next["head"])
		if len(head) > 10 {
			head = head[:10]
		}
		lines = append(lines, fmt.Sprintf("- Head moved to %s.", head))
	}
	if !reflect.DeepEqual(old["decision"], next["decision"]) || !reflect.DeepEqual(old["draft"], next["draft"]) {
		decision := text(next["decision"])
		if decision == "" {
			decision = "none"
		}
		draft := ""
		if next["draft"] == true {
			draft = " draft"
		}
		lines = append(lines, fmt.Sprintf("- Review decision is now %s%s.", decision, draft))
	}
	oldChecks, newChecks := object(old["checks"]), object(next["checks"])
	if !reflect.DeepEqual(oldChecks, newChecks) {
		failed := list(newChecks["failed"])
		failedNames := make([]string, 0, len(failed))
		for _, name := range failed {
			failedNames = append(failedNames, text(name))
		}
		failedText := strings.Join(failedNames, ", ")
		if failedText == "" {
			failedText = "none"
		}
		lines = append(lines, fmt.Sprintf("- CI: failed [%s]; pending %d; passed %d.", failedText, intValue(newChecks["pending"]), intValue(newChecks["passed"])))
	}
	for _, group := range []struct{ key, label string }{{"comments", "Comment"}, {"reviews", "Review"}, {"inline", "Inline comment"}} {
		before, after := itemMap(old[group.key]), itemMap(next[group.key])
		ids := make([]string, 0, len(after))
		for id := range after {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			item := after[id]
			if reflect.DeepEqual(before[id], item) {
				continue
			}
			where := ""
			if path := text(item["path"]); path != "" {
				where = " on " + path
			}
			state := ""
			if value := text(item["state"]); value != "" {
				state = " (" + value + ")"
			}
			body := text(item["body"])
			if body == "" {
				body = "(no body)"
			}
			lines = append(lines, fmt.Sprintf("- %s from %s%s%s: %s", group.label, text(item["author"]), state, where, body))
		}
	}
	return lines
}

func (w *watcher) busy(ctx context.Context, address string) bool {
	args := []string{"hc", "get", "--json", address}
	stdout, _, err := command(ctx, "", 15*time.Second, args...)
	if err != nil {
		return true
	}
	var response map[string]any
	if decodeJSON(stdout, &response) != nil {
		return false
	}
	status := text(response["status"])
	return status == "working" || status == "blocked"
}

func (w *watcher) deliver(ctx context.Context, address, message string) bool {
	if w.busy(ctx, address) {
		fmt.Printf("pr-watch: held %s: agent is busy\n", address)
		return false
	}
	args := []string{"hc", "send", address, message}
	stdout, stderr, err := command(ctx, "", 20*time.Second, args...)
	if err == nil {
		return true
	}
	failure := strings.TrimSpace(string(stderr))
	if failure == "" {
		failure = strings.TrimSpace(string(stdout))
	}
	fmt.Printf("pr-watch: held %s: %s\n", address, failure)
	return false
}

func makeMessage(current map[string]any, lines []string) string {
	return fmt.Sprintf("PR #%s update: %s\n%s\n%s\nHandle this only on this branch. Do not start other agents. If it needs no code or reply change, stop without extra work.", text(current["number"]), text(current["title"]), text(current["url"]), strings.Join(lines, "\n"))
}
