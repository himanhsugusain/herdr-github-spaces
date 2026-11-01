package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const pollInterval = 60 * time.Second
const commandTimeout = 45 * time.Second

type pullRequest struct {
	Number  int    `json:"number"`
	HeadRef string `json:"headRefName"`
	State   string `json:"state"`
	IsDraft bool   `json:"isDraft"`
}

type statusRecord struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	State      string `json:"state"`
	IsDraft    bool   `json:"isDraft"`
	Failed     int    `json:"failed"`
	Pending    int    `json:"pending"`
	Passed     int    `json:"passed"`
	Skipped    int    `json:"skipped"`
	Total      int    `json:"total"`
}

type checkTally struct {
	Failed  int `json:"failed"`
	Pending int `json:"pending"`
	Passed  int `json:"passed"`
	Skipped int `json:"skipped"`
	Total   int `json:"total"`
}

func runRefresh(args []string) error {
	fs := newFlagSet("refresh")
	repoFlag := fs.String("repo", ".", "Local Git checkout whose worktrees to monitor")
	outputFlag := fs.String("output", filepath.Join(executableDir(), "status.json"), "Status JSON output path")
	once := fs.Bool("once", false, "Refresh once and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := absolutePath(*repoFlag)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("--repo must be an existing local directory: %s", root)
	}
	out, err := absolutePath(*outputFlag)
	if err != nil {
		return err
	}
	refresh := func() error { return refreshOnce(root, out) }
	if *once {
		return refresh()
	}
	for {
		started := time.Now()
		if err := refresh(); err != nil {
			fmt.Fprintf(os.Stderr, "space-status: %v\n", err)
		}
		if wait := pollInterval - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
	}
}

func refreshOnce(root, out string) error {
	paths, err := worktrees(root)
	if err != nil {
		return err
	}
	prs := pullRequests(paths, root)
	payload := map[string]any{"prs": prs}
	data, err := jsonText(payload, false)
	if err != nil {
		return err
	}
	if previous, err := os.ReadFile(out); err == nil && string(previous) == string(data) {
		return nil
	}
	return atomicWrite(out, data)
}

func worktrees(root string) ([]string, error) {
	args := []string{"git", "worktree", "list", "--porcelain"}
	stdout, stderr, err := command(context.Background(), root, commandTimeout, args...)
	if err != nil {
		return nil, commandError(args, stdout, stderr, err)
	}
	var paths []string
	for _, block := range strings.Split(string(stdout), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				paths = append(paths, strings.TrimPrefix(line, "worktree "))
			}
		}
	}
	if len(paths) == 0 {
		paths = []string{root}
	}
	return paths, nil
}

func branch(cwd string) string {
	args := []string{"git", "rev-parse", "--abbrev-ref", "HEAD"}
	stdout, _, err := command(context.Background(), cwd, commandTimeout, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(stdout))
}

func ghJSON(root string, args ...string) ([]byte, error) {
	stdout, stderr, err := command(context.Background(), root, commandTimeout, args...)
	if err != nil {
		return nil, commandError(args, stdout, stderr, err)
	}
	return stdout, nil
}

func pullRequests(paths []string, root string) map[string]any {
	listed, err := ghJSON(root, "gh", "pr", "list", "--author", "@me", "--state", "open", "--limit", "40", "--json", "number,headRefName,state,isDraft")
	if err != nil {
		return map[string]any{}
	}
	var open []pullRequest
	if decodeJSON(listed, &open) != nil {
		return map[string]any{}
	}
	byBranch := make(map[string]pullRequest, len(open))
	for _, pr := range open {
		byBranch[pr.HeadRef] = pr
	}
	if recentData, recentErr := ghJSON(root, "gh", "pr", "list", "--author", "@me", "--state", "all", "--limit", "100", "--json", "number,headRefName,state,isDraft"); recentErr == nil {
		var recent []pullRequest
		if decodeJSON(recentData, &recent) == nil {
			for _, pr := range recent {
				if _, exists := byBranch[pr.HeadRef]; !exists {
					byBranch[pr.HeadRef] = pr
				}
			}
		}
	}
	repoData, err := ghJSON(root, "gh", "repo", "view", "--json", "nameWithOwner")
	if err != nil {
		return map[string]any{}
	}
	var repo struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if decodeJSON(repoData, &repo) != nil {
		return map[string]any{}
	}
	parts := strings.SplitN(repo.NameWithOwner, "/", 2)
	if len(parts) != 2 {
		return map[string]any{}
	}

	type match struct {
		cwd  string
		head string
		pr   *pullRequest
	}
	matches := make([]match, 0, len(paths))
	numbers := make([]int, 0)
	seenNumbers := make(map[int]bool)
	for _, cwd := range paths {
		head := branch(cwd)
		pr, found := byBranch[head]
		var matched *pullRequest
		if found && head != "" {
			copy := pr
			matched = &copy
			if !seenNumbers[pr.Number] {
				seenNumbers[pr.Number] = true
				numbers = append(numbers, pr.Number)
			}
		}
		matches = append(matches, match{cwd: cwd, head: head, pr: matched})
	}
	ci := make(map[int]checkTally)
	if len(numbers) > 0 {
		fields := make([]string, 0, len(numbers))
		for _, number := range numbers {
			fields = append(fields, fmt.Sprintf("n%d: pullRequest(number: %d) { commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 100) { nodes { ... on CheckRun { conclusion status } ... on StatusContext { state } } } } } } } }", number, number))
		}
		query := fmt.Sprintf(`query { repository(owner: %q, name: %q) { %s } }`, parts[0], parts[1], strings.Join(fields, " "))
		if data, queryErr := ghJSON(root, "gh", "api", "graphql", "-f", "query="+query); queryErr == nil {
			var response map[string]any
			if decodeJSON(data, &response) == nil {
				body := object(object(response["data"])["repository"])
				for _, number := range numbers {
					pr := object(body[fmt.Sprintf("n%d", number)])
					commits := list(object(pr["commits"])["nodes"])
					var contexts []any
					if len(commits) > 0 {
						commit := object(commits[0])["commit"]
						contexts = list(object(object(commit)["statusCheckRollup"])["contexts"])
						if len(contexts) == 0 {
							contexts = list(object(object(object(commit)["statusCheckRollup"])["contexts"])["nodes"])
						}
					}
					ci[number] = tallyChecks(contexts)
				}
			}
		}
	}
	result := make(map[string]any, len(matches))
	for _, item := range matches {
		keyBytes, _ := jsonText([]any{nil, item.cwd, item.head}, false)
		key := string(keyBytes)
		if item.pr == nil {
			result[key] = nil
			continue
		}
		counts := ci[item.pr.Number]
		result[key] = statusRecord{
			Repository: repo.NameWithOwner,
			Number:     item.pr.Number,
			State:      item.pr.State,
			IsDraft:    item.pr.IsDraft,
			Failed:     counts.Failed,
			Pending:    counts.Pending,
			Passed:     counts.Passed,
			Skipped:    counts.Skipped,
			Total:      counts.Total,
		}
	}
	return result
}

func tallyChecks(nodes []any) checkTally {
	var tally checkTally
	tally.Total = len(nodes)
	for _, raw := range nodes {
		node := object(raw)
		conclusion := text(node["conclusion"])
		status := text(node["status"])
		if status == "" {
			status = text(node["state"])
		}
		switch {
		case contains([]string{"FAILURE", "CANCELLED", "TIMED_OUT", "STARTUP_FAILURE"}, conclusion) || contains([]string{"FAILURE", "ERROR"}, status):
			tally.Failed++
		case conclusion == "SKIPPED":
			tally.Skipped++
		case contains([]string{"SUCCESS", "NEUTRAL"}, conclusion) || status == "SUCCESS":
			tally.Passed++
		default:
			tally.Pending++
		}
	}
	return tally
}

func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
