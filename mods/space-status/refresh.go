package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v72/github"
)

const pollInterval = 60 * time.Second

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
	tokenFlag := fs.String("token", "", "GitHub personal-access token (default: GITHUB_TOKEN env)")
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

	token, err := resolveToken(*tokenFlag)
	if err != nil {
		return err
	}
	gh := newGitHubClient(token)

	owner, repo, err := repoFromRemote(root)
	if err != nil {
		return err
	}

	ctx := context.Background()
	username, err := githubUser(ctx, gh)
	if err != nil {
		return err
	}

	refresh := func() error { return refreshOnce(ctx, root, out, gh, owner, repo, username) }
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

func refreshOnce(ctx context.Context, root, out string, gh *github.Client, owner, repo, username string) error {
	paths, err := worktrees(root)
	if err != nil {
		return err
	}
	prs := pullRequests(ctx, gh, owner, repo, username, paths)
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

func pullRequests(ctx context.Context, gh *github.Client, owner, repo, username string, paths []string) map[string]any {
	open, err := listPRsByAuthor(ctx, gh, owner, repo, username, "open", 40)
	if err != nil {
		return map[string]any{}
	}
	byBranch := make(map[string]*github.PullRequest, len(open))
	for _, pr := range open {
		byBranch[pr.GetHead().GetRef()] = pr
	}

	if recent, err := listPRsByAuthor(ctx, gh, owner, repo, username, "all", 100); err == nil {
		for _, pr := range recent {
			ref := pr.GetHead().GetRef()
			if _, exists := byBranch[ref]; !exists {
				byBranch[ref] = pr
			}
		}
	}

	nameWithOwner := owner + "/" + repo

	type match struct {
		cwd  string
		head string
		pr   *github.PullRequest
	}
	matches := make([]match, 0, len(paths))
	refs := make(map[int]string) // PR number → head SHA
	seenNumbers := make(map[int]bool)

	for _, cwd := range paths {
		head := branch(cwd)
		pr := byBranch[head]
		if pr != nil && head != "" {
			n := pr.GetNumber()
			if !seenNumbers[n] {
				seenNumbers[n] = true
				refs[n] = pr.GetHead().GetSHA()
			}
		}
		matches = append(matches, match{cwd: cwd, head: head, pr: pr})
	}

	ci := make(map[int]checkTally)
	for n, ref := range refs {
		r := fetchCI(ctx, gh, owner, repo, ref)
		ci[n] = checkTally{
			Failed: r.Failed, Pending: r.Pending,
			Passed: r.Passed, Skipped: r.Skipped, Total: r.Total,
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
		counts := ci[item.pr.GetNumber()]
		result[key] = statusRecord{
			Repository: nameWithOwner,
			Number:     item.pr.GetNumber(),
			State:      prStateName(item.pr),
			IsDraft:    item.pr.GetDraft(),
			Failed:     counts.Failed,
			Pending:    counts.Pending,
			Passed:     counts.Passed,
			Skipped:    counts.Skipped,
			Total:      counts.Total,
		}
	}
	return result
}
