package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v72/github"
)

var accountMapping = regexp.MustCompile(`^([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)?)=([A-Za-z0-9-]+)$`)

func parseGitHubAccounts(values []string) (map[string]string, error) {
	accounts := make(map[string]string)
	for _, value := range values {
		match := accountMapping.FindStringSubmatch(value)
		if match == nil {
			return nil, fmt.Errorf("--github-account must be OWNER=LOGIN or OWNER/REPO=LOGIN")
		}
		accounts[strings.ToLower(match[1])] = match[2]
	}
	return accounts, nil
}

func repositoryAccount(accounts map[string]string, owner, repo string) string {
	if account := accounts[strings.ToLower(owner+"/"+repo)]; account != "" {
		return account
	}
	return accounts[strings.ToLower(owner)]
}

func accountToken(ctx context.Context, account string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com", "--user", account)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		switch name {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
			continue
		}
		cmd.Env = append(cmd.Env, variable)
	}
	stdout, err := cmd.Output()
	if token := strings.TrimSpace(string(stdout)); err == nil && token != "" {
		return token, nil
	}
	return "", fmt.Errorf("GitHub login %q is unavailable; authenticate that login with gh", account)
}

func repositoryClient(ctx context.Context, fallback *github.Client, accounts map[string]string, owner, repo string) (*github.Client, error) {
	account := repositoryAccount(accounts, owner, repo)
	if account == "" {
		return fallback, nil
	}
	token, err := accountToken(ctx, account)
	if err != nil {
		return nil, err
	}
	return newGitHubClient(token), nil
}

func resolveToken(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token, nil
	}
	stdout, _, err := command(context.Background(), "", commandTimeout, "gh", "auth", "token")
	if token := strings.TrimSpace(string(stdout)); err == nil && token != "" {
		return token, nil
	}
	return "", fmt.Errorf("GitHub authentication unavailable: authenticate gh or set GITHUB_TOKEN")
}

// newGitHubClient creates an authenticated GitHub API client.
func newGitHubClient(token string) *github.Client {
	return github.NewClient(nil).WithAuthToken(token)
}

// repoFromRemote extracts owner/repo from the git remote "origin" URL.
func repoFromRemote(root string) (string, string, error) {
	args := []string{"git", "remote", "get-url", "origin"}
	stdout, stderr, err := command(context.Background(), root, commandTimeout, args...)
	if err != nil {
		return "", "", commandError(args, stdout, stderr, err)
	}
	return parseGitHubURL(strings.TrimSpace(string(stdout)))
}

// parseGitHubURL extracts owner and repo from an HTTPS or SSH remote URL.
func parseGitHubURL(raw string) (string, string, error) {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	// Normalise SSH colon syntax: git@github.com:owner/repo → …/owner/repo
	if !strings.Contains(raw, "://") {
		if i := strings.LastIndex(raw, ":"); i > 0 {
			raw = raw[:i] + "/" + raw[i+1:]
		}
	}
	parts := strings.Split(raw, "/")
	if len(parts) >= 2 {
		owner, repo := parts[len(parts)-2], parts[len(parts)-1]
		if owner != "" && repo != "" {
			return owner, repo, nil
		}
	}
	return "", "", fmt.Errorf("cannot parse GitHub repository from %q", raw)
}

func repositoryIdentity(root, override string) (string, string, error) {
	if override == "" {
		return repoFromRemote(root)
	}
	parts := strings.Split(override, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("--github-repo must be OWNER/NAME")
	}
	return parts[0], parts[1], nil
}

// ciResult holds CI check tallies plus the names of failed checks.
type ciResult struct {
	FailedNames []string
	Failed      int
	Pending     int
	Passed      int
	Skipped     int
	Total       int
}

// fetchCI retrieves check-run and commit-status results for a ref.
// Returns partial results even on error so callers always get what succeeded.
func fetchCI(ctx context.Context, gh *github.Client, owner, repo, ref string) (ciResult, error) {
	r := ciResult{FailedNames: make([]string, 0)}
	var firstErr error

	runs, _, err := gh.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, &github.ListCheckRunsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	})
	if err != nil {
		firstErr = fmt.Errorf("check-runs: %w", err)
	}
	if runs != nil {
		for _, cr := range runs.CheckRuns {
			r.Total++
			name := cr.GetName()
			if name == "" {
				name = "check"
			}
			conclusion := strings.ToLower(cr.GetConclusion())
			switch conclusion {
			case "failure", "cancelled", "timed_out", "startup_failure":
				r.Failed++
				r.FailedNames = append(r.FailedNames, name)
			case "skipped":
				r.Skipped++
			case "success", "neutral":
				r.Passed++
			default:
				r.Pending++
			}
		}
	}

	combined, _, err := gh.Repositories.GetCombinedStatus(ctx, owner, repo, ref, &github.ListOptions{PerPage: 100})
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("combined-status: %w", err)
	}
	if combined != nil {
		for _, s := range combined.Statuses {
			r.Total++
			name := s.GetContext()
			if name == "" {
				name = "check"
			}
			switch strings.ToLower(s.GetState()) {
			case "failure", "error":
				r.Failed++
				r.FailedNames = append(r.FailedNames, name)
			case "success":
				r.Passed++
			default:
				r.Pending++
			}
		}
	}

	sort.Strings(r.FailedNames)
	unique := r.FailedNames[:0]
	for _, name := range r.FailedNames {
		if len(unique) == 0 || unique[len(unique)-1] != name {
			unique = append(unique, name)
		}
	}
	r.FailedNames = unique
	return r, firstErr
}

// prStateName maps a REST-API PullRequest to the uppercase state the JS mods
// expect: "OPEN", "MERGED", or "CLOSED".
func prStateName(pr *github.PullRequest) string {
	if strings.EqualFold(pr.GetState(), "open") {
		return "OPEN"
	}
	if pr.MergedAt != nil {
		return "MERGED"
	}
	return "CLOSED"
}

// computeDecision approximates the GraphQL reviewDecision field from reviews.
func computeDecision(reviews []*github.PullRequestReview) string {
	latest := make(map[string]string)
	for _, r := range reviews {
		user := r.GetUser().GetLogin()
		state := strings.ToUpper(r.GetState())
		switch state {
		case "APPROVED", "CHANGES_REQUESTED":
			latest[user] = state
		case "DISMISSED":
			delete(latest, user)
		}
	}
	hasApproved := false
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return "CHANGES_REQUESTED"
		}
		if state == "APPROVED" {
			hasApproved = true
		}
	}
	if hasApproved {
		return "APPROVED"
	}
	return ""
}

// formatTimestamp formats a GitHub timestamp to RFC 3339, or "" if nil.
func formatTimestamp(t *github.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}
