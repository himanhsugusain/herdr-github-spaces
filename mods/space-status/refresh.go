package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v72/github"
)

const pollInterval = 60 * time.Second

type statusRecord struct {
	Repository     string `json:"repository"`
	Number         int    `json:"number"`
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	ReviewDecision string `json:"reviewDecision"`
	Failed         int    `json:"failed"`
	Pending        int    `json:"pending"`
	Passed         int    `json:"passed"`
	Skipped        int    `json:"skipped"`
	Total          int    `json:"total"`
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
	repoFlag := fs.String("repo", "", "Local Git checkout whose worktrees to monitor (required)")
	githubRepoFlag := fs.String("github-repo", "", "GitHub repository as OWNER/NAME (inferred from origin when omitted)")
	outputFlag := fs.String("output", filepath.Join(executableDir(), "status.json"), "Status JSON output path")
	tokenFlag := fs.String("token", "", "GitHub token (default: GITHUB_TOKEN or gh authentication)")
	once := fs.Bool("once", false, "Refresh once and exit")
	discover := fs.Bool("discover-repos", false, "Include repositories of all live local Herdr panes")
	var accountFlags stringList
	fs.Var(&accountFlags, "github-account", "GitHub login per OWNER or OWNER/REPO: KEY=LOGIN (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	accounts, err := parseGitHubAccounts(accountFlags)
	if err != nil {
		return err
	}
	root, err := repositoryPath(*repoFlag)
	if err != nil {
		return err
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

	owner, repo, err := repositoryIdentity(root, *githubRepoFlag)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if *once {
		return refreshOnceSync(ctx, root, out, gh, owner, repo, *discover, accounts)
	}
	writer := newAsyncWriter(out)
	defer writer.Stop()
	for {
		started := time.Now()
		if err := refreshOnce(ctx, root, writer, gh, owner, repo, *discover, accounts); err != nil {
			fmt.Fprintf(os.Stderr, "space-status: %v\n", err)
		}
		if wait := pollInterval - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
	}
}

func statusPayload(ctx context.Context, root string, gh *github.Client, owner, repo string, discover bool, accounts map[string]string) ([]byte, error) {
	started := time.Now()
	paths, err := worktrees(root)
	var errs []string
	if err != nil {
		errs = append(errs, err.Error())
	}
	targets := []repositoryTarget{{owner: owner, repo: repo, paths: paths}}
	if discover {
		directories, failures := herdrDirectories(ctx)
		errs = append(errs, failures...)
		targets = discoverTargets(ctx, targets[0], directories)
	}
	prs, failures := collectRepositoryStatus(ctx, gh, targets, accounts)
	errs = append(errs, failures...)
	payload := map[string]any{
		"prs":         prs,
		"lastRefresh": time.Now().UTC().Format(time.RFC3339),
		"durationMs":  int(time.Since(started).Milliseconds()),
	}
	if len(errs) > 0 {
		payload["errors"] = errs
	}
	return jsonText(payload, false)
}

func refreshOnce(ctx context.Context, root string, writer *asyncWriter, gh *github.Client, owner, repo string, discover bool, accounts map[string]string) error {
	data, err := statusPayload(ctx, root, gh, owner, repo, discover, accounts)
	if err != nil {
		return err
	}
	writer.Write(data)
	return nil
}

func refreshOnceSync(ctx context.Context, root, out string, gh *github.Client, owner, repo string, discover bool, accounts map[string]string) error {
	data, err := statusPayload(ctx, root, gh, owner, repo, discover, accounts)
	if err != nil {
		return err
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

// lookupPR finds the most recent PR for a given head branch.
// Uses the Head filter so the API only returns PRs for that branch.
func lookupPR(ctx context.Context, gh *github.Client, owner, repo, branch string) (*github.PullRequest, error) {
	if branch == "master" || branch == "main" {
		return nil, nil
	}
	// Try open PRs first.
	head := owner + ":" + branch
	prs, _, err := gh.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head:        head,
		State:       "open",
		Sort:        "updated",
		ListOptions: github.ListOptions{PerPage: 1},
	})
	if err != nil {
		return nil, fmt.Errorf("list open PRs for %s: %w", branch, err)
	}
	if len(prs) > 0 {
		return prs[0], nil
	}
	// Fall back to closed/merged.
	prs, _, err = gh.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head:        head,
		State:       "closed",
		Sort:        "updated",
		ListOptions: github.ListOptions{PerPage: 1},
	})
	if err != nil {
		return nil, fmt.Errorf("list closed PRs for %s: %w", branch, err)
	}
	if len(prs) > 0 {
		return prs[0], nil
	}
	return nil, nil
}

func pullRequests(ctx context.Context, gh *github.Client, owner, repo string, paths []string) (map[string]any, []string) {
	nameWithOwner := owner + "/" + repo
	var errs []string

	type match struct {
		cwd  string
		head string
		pr   *github.PullRequest
	}

	// Deduplicate branches across worktrees and look up PRs in parallel.
	uniqueBranches := make(map[string]bool)
	var cwdHeads []struct{ cwd, head string }
	for _, cwd := range paths {
		head := branch(cwd)
		cwdHeads = append(cwdHeads, struct{ cwd, head string }{cwd, head})
		if head != "" && head != "HEAD" {
			uniqueBranches[head] = true
		}
	}

	branchPR := make(map[string]*github.PullRequest)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for b := range uniqueBranches {
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			pr, err := lookupPR(ctx, gh, owner, repo, b)
			mu.Lock()
			if err != nil {
				errs = append(errs, fmt.Sprintf("lookup %s: %v", b, err))
			}
			branchPR[b] = pr
			mu.Unlock()
		}(b)
	}
	wg.Wait()

	var matches []match
	for _, ch := range cwdHeads {
		if ch.head == "" || ch.head == "HEAD" {
			matches = append(matches, match{cwd: ch.cwd, head: ch.head})
		} else {
			matches = append(matches, match{cwd: ch.cwd, head: ch.head, pr: branchPR[ch.head]})
		}
	}

	// Collect unique PR numbers for CI + review lookups.
	refs := make(map[int]string)
	for _, m := range matches {
		if m.pr == nil {
			continue
		}
		n := m.pr.GetNumber()
		if _, ok := refs[n]; !ok {
			refs[n] = m.pr.GetHead().GetSHA()
		}
	}

	// Fetch CI and reviews per PR in parallel.
	ci := make(map[int]checkTally)
	decisions := make(map[int]string)
	for n, ref := range refs {
		wg.Add(1)
		go func(n int, ref string) {
			defer wg.Done()
			r, ciErr := fetchCI(ctx, gh, owner, repo, ref)
			revs := paginate(func(opts *github.ListOptions) ([]*github.PullRequestReview, *github.Response, error) {
				return gh.PullRequests.ListReviews(ctx, owner, repo, n, opts)
			})
			mu.Lock()
			if ciErr != nil {
				errs = append(errs, fmt.Sprintf("CI #%d: %v", n, ciErr))
			}
			ci[n] = checkTally{
				Failed: r.Failed, Pending: r.Pending,
				Passed: r.Passed, Skipped: r.Skipped, Total: r.Total,
			}
			decisions[n] = computeDecision(revs)
			mu.Unlock()
		}(n, ref)
	}
	wg.Wait()

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
			Repository:     nameWithOwner,
			Number:         item.pr.GetNumber(),
			State:          prStateName(item.pr),
			IsDraft:        item.pr.GetDraft(),
			ReviewDecision: decisions[item.pr.GetNumber()],
			Failed:         counts.Failed,
			Pending:        counts.Pending,
			Passed:         counts.Passed,
			Skipped:        counts.Skipped,
			Total:          counts.Total,
		}
	}
	return result, errs
}
