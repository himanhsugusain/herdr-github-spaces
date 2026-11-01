package main

import (
	"testing"

	"github.com/google/go-github/v72/github"
)

func ptr(s string) *string { return &s }

func TestParseGitHubURLHTTPS(t *testing.T) {
	owner, repo, err := parseGitHubURL("https://github.com/octocat/Hello-World.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "octocat" || repo != "Hello-World" {
		t.Fatalf("got %s/%s, want octocat/Hello-World", owner, repo)
	}
}

func TestParseGitHubURLSSH(t *testing.T) {
	owner, repo, err := parseGitHubURL("git@github.com:octocat/Hello-World.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "octocat" || repo != "Hello-World" {
		t.Fatalf("got %s/%s, want octocat/Hello-World", owner, repo)
	}
}

func TestParseGitHubURLPlain(t *testing.T) {
	owner, repo, err := parseGitHubURL("https://github.com/octocat/Hello-World")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "octocat" || repo != "Hello-World" {
		t.Fatalf("got %s/%s, want octocat/Hello-World", owner, repo)
	}
}

func TestComputeDecisionApproved(t *testing.T) {
	reviews := []*github.PullRequestReview{
		{User: &github.User{Login: ptr("alice")}, State: ptr("APPROVED")},
	}
	if got := computeDecision(reviews); got != "APPROVED" {
		t.Fatalf("got %q, want APPROVED", got)
	}
}

func TestComputeDecisionChangesRequested(t *testing.T) {
	reviews := []*github.PullRequestReview{
		{User: &github.User{Login: ptr("alice")}, State: ptr("APPROVED")},
		{User: &github.User{Login: ptr("bob")}, State: ptr("CHANGES_REQUESTED")},
	}
	if got := computeDecision(reviews); got != "CHANGES_REQUESTED" {
		t.Fatalf("got %q, want CHANGES_REQUESTED", got)
	}
}

func TestComputeDecisionDismissedClearsVote(t *testing.T) {
	reviews := []*github.PullRequestReview{
		{User: &github.User{Login: ptr("alice")}, State: ptr("CHANGES_REQUESTED")},
		{User: &github.User{Login: ptr("alice")}, State: ptr("DISMISSED")},
	}
	if got := computeDecision(reviews); got != "" {
		t.Fatalf("got %q, want empty after dismissal", got)
	}
}

func TestComputeDecisionLatestWins(t *testing.T) {
	reviews := []*github.PullRequestReview{
		{User: &github.User{Login: ptr("alice")}, State: ptr("CHANGES_REQUESTED")},
		{User: &github.User{Login: ptr("alice")}, State: ptr("APPROVED")},
	}
	if got := computeDecision(reviews); got != "APPROVED" {
		t.Fatalf("got %q, want APPROVED", got)
	}
}

func TestDescribeHeadMoved(t *testing.T) {
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "bbbbbbbbbbb", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 1 || lines[0] != "- Head moved to bbbbbbbbbb." {
		t.Fatalf("describe() = %v", lines)
	}
}

func TestDescribeNewComment(t *testing.T) {
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{{ID: "1", Author: "bob", Body: "LGTM"}},
		Reviews:  []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 1 || lines[0] != "- Comment from bob: LGTM" {
		t.Fatalf("describe() = %v", lines)
	}
}

func TestDescribeMerged(t *testing.T) {
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", State: "OPEN", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", State: "MERGED", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 1 || lines[0] != "- PR was merged." {
		t.Fatalf("describe() = %v", lines)
	}
}

func TestDescribeClosed(t *testing.T) {
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", State: "OPEN", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", State: "CLOSED", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 1 || lines[0] != "- PR was closed without merging." {
		t.Fatalf("describe() = %v", lines)
	}
}

func TestDescribeReopened(t *testing.T) {
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", State: "CLOSED", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", State: "OPEN", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 1 || lines[0] != "- PR was reopened." {
		t.Fatalf("describe() = %v", lines)
	}
}

func TestDescribeMergeableChanged(t *testing.T) {
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Mergeable: "clean", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Mergeable: "dirty", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 1 || lines[0] != "- Mergeable status changed to dirty." {
		t.Fatalf("describe() = %v", lines)
	}
}

func TestMakeMessageCIFailed(t *testing.T) {
	current := &prSnapshot{
		Number: 42, Title: "Fix bug", URL: "https://github.com/o/r/pull/42",
		Fingerprint: snapshotFP{
			Head: "abcdef1234", State: "OPEN", Mergeable: "clean",
			Checks: ciDetail{Failed: []string{"lint"}, Pending: 0, Passed: 5},
		},
	}
	lines := []string{"- CI: failed [lint]; pending 0; passed 5."}
	msg := makeMessage(current, lines)
	for _, want := range []string{"CI Failed", "gh pr checks 42", "Handle this only"} {
		if !containsStr(msg, want) {
			t.Fatalf("makeMessage() missing %q, got:\n%s", want, msg)
		}
	}
}

func TestMakeMessageApproved(t *testing.T) {
	current := &prSnapshot{
		Number: 42, Title: "Fix bug", URL: "https://github.com/o/r/pull/42",
		Fingerprint: snapshotFP{
			Head: "abcdef1234", State: "OPEN", Decision: "APPROVED",
			Checks: ciDetail{Failed: []string{}, Passed: 10},
		},
	}
	lines := []string{"- Review from alice (APPROVED): Looks good!"}
	msg := makeMessage(current, lines)
	for _, want := range []string{"Review Approved", "ask the user", "Handle this only"} {
		if !containsStr(msg, want) {
			t.Fatalf("makeMessage() missing %q, got:\n%s", want, msg)
		}
	}
}

func TestMakeMessageMerged(t *testing.T) {
	current := &prSnapshot{
		Number: 42, Title: "Fix bug", URL: "https://github.com/o/r/pull/42",
		Fingerprint: snapshotFP{
			Head: "abcdef1234", State: "MERGED",
			Checks: ciDetail{Failed: []string{}},
		},
	}
	lines := []string{"- PR was merged."}
	msg := makeMessage(current, lines)
	for _, want := range []string{"PR Merged", "No further action", "MERGED"} {
		if !containsStr(msg, want) {
			t.Fatalf("makeMessage() missing %q, got:\n%s", want, msg)
		}
	}
}

func containsStr(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && stringContains(s, substr)
}

func stringContains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestClipCountsUnicodeCharacters(t *testing.T) {
	input := "界"
	for i := 0; i < bodyLimit; i++ {
		input += "界"
	}
	got := clip(input)
	if len([]rune(got)) != bodyLimit+1 || []rune(got)[bodyLimit] != '…' {
		t.Fatalf("clip returned %d runes; want %d including ellipsis", len([]rune(got)), bodyLimit+1)
	}
}

func TestJSONTextMatchesJavaScriptUnicodeEncoding(t *testing.T) {
	got, err := jsonText([]any{"<herdr>", "🌱  "}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `["<herdr>","🌱  "]`
	if string(got) != want {
		t.Fatalf("jsonText() = %q, want %q", got, want)
	}
}
