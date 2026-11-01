package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v72/github"
)

// resolveToken returns a GitHub token from the flag value or GITHUB_TOKEN env.
func resolveToken(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token, nil
	}
	return "", fmt.Errorf("GitHub token required: set GITHUB_TOKEN or pass --token")
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

// githubUser returns the login of the authenticated user.
func githubUser(ctx context.Context, gh *github.Client) (string, error) {
	user, _, err := gh.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("get authenticated user: %w", err)
	}
	return user.GetLogin(), nil
}

// listPRsByAuthor lists pull requests authored by the given user, up to limit.
// state is "open", "closed", or "all".  Results are sorted by last-updated.
func listPRsByAuthor(ctx context.Context, gh *github.Client, owner, repo, author, state string, limit int) ([]*github.PullRequest, error) {
	var result []*github.PullRequest
	opts := &github.PullRequestListOptions{
		State:       state,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for len(result) < limit {
		prs, resp, err := gh.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return result, err
		}
		if len(prs) == 0 {
			break
		}
		for _, pr := range prs {
			if pr.GetUser().GetLogin() == author {
				result = append(result, pr)
				if len(result) >= limit {
					break
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return result, nil
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
func fetchCI(ctx context.Context, gh *github.Client, owner, repo, ref string) ciResult {
	r := ciResult{FailedNames: make([]string, 0)}

	if runs, _, err := gh.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, &github.ListCheckRunsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}); err == nil && runs != nil {
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

	if combined, _, err := gh.Repositories.GetCombinedStatus(ctx, owner, repo, ref, &github.ListOptions{PerPage: 100}); err == nil && combined != nil {
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
	return r
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
