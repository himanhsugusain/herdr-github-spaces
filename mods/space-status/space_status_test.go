package main

import (
	"testing"

	"github.com/google/go-github/v72/github"
)

func ptr(s string) *string { return &s }

func intPtr(n int) *int { return &n }

func TestGroupAgentsByPR(t *testing.T) {
	agents := []watchAgent{
		{Address: "terminal-one"},
		{Address: "terminal-two"},
		{Address: "terminal-three"},
	}
	branches := map[string]string{
		"terminal-one":   "feature-a",
		"terminal-two":   "feature-b",
		"terminal-three": "feature-c",
	}
	prs := map[string]*github.PullRequest{
		"feature-a": {Number: intPtr(42)},
		"feature-b": {Number: intPtr(42)},
		"feature-c": {Number: intPtr(77)},
	}

	groups := groupAgentsByPR(agents, branches, prs)
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if groups[0].number != 42 || len(groups[0].agents) != 2 {
		t.Fatalf("first group = PR #%d with %d agents, want PR #42 with 2 agents", groups[0].number, len(groups[0].agents))
	}
	if groups[1].number != 77 || len(groups[1].agents) != 1 {
		t.Fatalf("second group = PR #%d with %d agents, want PR #77 with 1 agent", groups[1].number, len(groups[1].agents))
	}
}

func TestGroupUpdateLinesDeduplicatesSharedPRChanges(t *testing.T) {
	previous := &prSnapshot{Fingerprint: snapshotFP{
		Head: "before", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	current := &prSnapshot{Fingerprint: snapshotFP{
		Head: "after", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	agents := []watchAgent{{Address: "one"}, {Address: "two"}}
	state := map[string]watchSlot{
		"one": {Snapshot: previous},
		"two": {Snapshot: previous},
	}

	lines, hasBaseline := groupUpdateLines(agents, state, current)
	if !hasBaseline || len(lines) != 1 || lines[0] != "- Head moved to after." {
		t.Fatalf("groupUpdateLines() = (%v, %v), want one shared head-change line", lines, hasBaseline)
	}
}

func TestDeliverToFirstAvailableUsesAtMostOneTerminal(t *testing.T) {
	agents := []watchAgent{{Address: "busy"}, {Address: "available"}, {Address: "also-available"}}
	var attempted []string
	busy := func(address string) bool { return address == "busy" }
	send := func(address string) bool {
		attempted = append(attempted, address)
		return true
	}

	address, delivered := deliverToFirstAvailable(agents, busy, send)
	if !delivered || address != "available" {
		t.Fatalf("delivery = (%q, %v), want (available, true)", address, delivered)
	}
	if len(attempted) != 1 || attempted[0] != "available" {
		t.Fatalf("send attempted for %v, want only available", attempted)
	}
}

func TestDeliverToFirstAvailableHoldsWhenAllBusy(t *testing.T) {
	agents := []watchAgent{{Address: "one"}, {Address: "two"}}
	called := false
	address, delivered := deliverToFirstAvailable(agents, func(string) bool { return true }, func(string) bool {
		called = true
		return true
	})
	if delivered || address != "" || called {
		t.Fatalf("got (%q, %v), send called=%v; want no delivery", address, delivered, called)
	}
}

func TestParseGitHubURLHTTPS(t *testing.T) {
	owner, repo, err := parseGitHubURL("https://github.com/example-owner/example-repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "example-owner" || repo != "example-repo" {
		t.Fatalf("got %s/%s, want example-owner/example-repo", owner, repo)
	}
}

func TestParseGitHubURLSSH(t *testing.T) {
	owner, repo, err := parseGitHubURL("git@github.com:example-owner/example-repo.git")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "example-owner" || repo != "example-repo" {
		t.Fatalf("got %s/%s, want example-owner/example-repo", owner, repo)
	}
}

func TestParseGitHubURLPlain(t *testing.T) {
	owner, repo, err := parseGitHubURL("https://github.com/example-owner/example-repo")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "example-owner" || repo != "example-repo" {
		t.Fatalf("got %s/%s, want example-owner/example-repo", owner, repo)
	}
}

func TestComputeDecisionApproved(t *testing.T) {
	reviews := []*github.PullRequestReview{
		{User: &github.User{Login: ptr("reviewer-one")}, State: ptr("APPROVED")},
	}
	if got := computeDecision(reviews); got != "APPROVED" {
		t.Fatalf("got %q, want APPROVED", got)
	}
}

func TestComputeDecisionChangesRequested(t *testing.T) {
	reviews := []*github.PullRequestReview{
		{User: &github.User{Login: ptr("reviewer-one")}, State: ptr("APPROVED")},
		{User: &github.User{Login: ptr("reviewer-two")}, State: ptr("CHANGES_REQUESTED")},
	}
	if got := computeDecision(reviews); got != "CHANGES_REQUESTED" {
		t.Fatalf("got %q, want CHANGES_REQUESTED", got)
	}
}

func TestComputeDecisionDismissedClearsVote(t *testing.T) {
	reviews := []*github.PullRequestReview{
		{User: &github.User{Login: ptr("reviewer-one")}, State: ptr("CHANGES_REQUESTED")},
		{User: &github.User{Login: ptr("reviewer-one")}, State: ptr("DISMISSED")},
	}
	if got := computeDecision(reviews); got != "" {
		t.Fatalf("got %q, want empty after dismissal", got)
	}
}

func TestComputeDecisionLatestWins(t *testing.T) {
	reviews := []*github.PullRequestReview{
		{User: &github.User{Login: ptr("reviewer-one")}, State: ptr("CHANGES_REQUESTED")},
		{User: &github.User{Login: ptr("reviewer-one")}, State: ptr("APPROVED")},
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
		Comments: []noteRecord{{ID: "1", Author: "reviewer-two", Body: "LGTM"}},
		Reviews:  []noteRecord{}, Inline: []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 1 || lines[0] != "- Comment from reviewer-two: LGTM" {
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

func TestPreserveKnownMergeable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous *prSnapshot
		current  prSnapshot
		want     string
	}{
		{
			name:     "unknown during recomputation",
			previous: &prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "blocked"}},
			current:  prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "unknown"}},
			want:     "blocked",
		},
		{
			name:     "empty during recomputation",
			previous: &prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "blocked"}},
			current:  prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa"}},
			want:     "blocked",
		},
		{
			name:     "real change",
			previous: &prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "blocked"}},
			current:  prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "clean"}},
			want:     "clean",
		},
		{
			name:     "new head",
			previous: &prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "blocked"}},
			current:  prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "bbb", Mergeable: "unknown"}},
			want:     "unknown",
		},
		{
			name:     "different PR",
			previous: &prSnapshot{Number: 41, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "blocked"}},
			current:  prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "unknown"}},
			want:     "unknown",
		},
		{
			name:    "no baseline",
			current: prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "unknown"}},
			want:    "unknown",
		},
		{
			name:     "unknown baseline",
			previous: &prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "unknown"}},
			current:  prSnapshot{Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "unknown"}},
			want:     "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := []watchAgent{{Address: "new"}, {Address: "existing"}}
			state := map[string]watchSlot{"existing": {Snapshot: tc.previous}}
			preserveKnownMergeable(agents, state, &tc.current)
			if got := tc.current.Fingerprint.Mergeable; got != tc.want {
				t.Fatalf("mergeable = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMergeableRecomputationAcrossPolls(t *testing.T) {
	agents := []watchAgent{{Address: "one"}}
	state := map[string]watchSlot{"one": {Snapshot: &prSnapshot{
		Number: 42, Fingerprint: snapshotFP{Head: "aaa", Mergeable: "blocked"},
	}}}
	for i, poll := range []struct {
		mergeable string
		passed    int
		want      string
	}{
		{mergeable: "unknown"},
		{mergeable: "blocked"},
		{mergeable: "unknown", passed: 1, want: "- CI: failed [none]; pending 0; passed 1."},
		{mergeable: "unknown", passed: 1},
		{mergeable: "blocked", passed: 1},
		{mergeable: "clean", passed: 1, want: "- Mergeable status changed to clean."},
		{mergeable: "unknown", passed: 1},
		{mergeable: "clean", passed: 1},
		{mergeable: "blocked", passed: 1, want: "- Mergeable status changed to blocked."},
	} {
		current := &prSnapshot{Number: 42, Fingerprint: snapshotFP{
			Head: "aaa", Mergeable: poll.mergeable, Checks: ciDetail{Passed: poll.passed},
		}}
		preserveKnownMergeable(agents, state, current)
		lines, hasBaseline := groupUpdateLines(agents, state, current)
		if !hasBaseline || (poll.want == "" && len(lines) != 0) ||
			(poll.want != "" && (len(lines) != 1 || lines[0] != poll.want)) {
			t.Fatalf("poll %d: lines = %v, baseline = %v; want %q", i, lines, hasBaseline, poll.want)
		}
		state["one"] = watchSlot{Snapshot: current}
	}
}

func TestMakeMessageCIFailed(t *testing.T) {
	current := &prSnapshot{
		Number: 42, Title: "Fix bug", URL: "https://github.com/example-owner/example-repo/pull/42",
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
		Number: 42, Title: "Fix bug", URL: "https://github.com/example-owner/example-repo/pull/42",
		Fingerprint: snapshotFP{
			Head: "abcdef1234", State: "OPEN", Decision: "APPROVED",
			Checks: ciDetail{Failed: []string{}, Passed: 10},
		},
	}
	lines := []string{"- Review from reviewer-one (APPROVED): Looks good!"}
	msg := makeMessage(current, lines)
	for _, want := range []string{"Review Approved", "ask the user", "Handle this only"} {
		if !containsStr(msg, want) {
			t.Fatalf("makeMessage() missing %q, got:\n%s", want, msg)
		}
	}
}

func TestMakeMessageMerged(t *testing.T) {
	current := &prSnapshot{
		Number: 42, Title: "Fix bug", URL: "https://github.com/example-owner/example-repo/pull/42",
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

func TestDescribeCommentedReviewFilteredFromInline(t *testing.T) {
	// A "COMMENTED" review with empty body is just a container for inline
	// comments. After the snapshot filters it out, describe should only
	// report the inline comment, not a duplicate "Review" line.
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{},
		Reviews:  []noteRecord{}, // COMMENTED review already filtered by snapshot
		Inline:   []noteRecord{{ID: "100", Author: "reviewer-two", Path: "main.go", Body: "fix this"}},
	}}
	lines := describe(prev, curr)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d: %v", len(lines), lines)
	}
	if !stringContains(lines[0], "Inline comment") {
		t.Fatalf("expected inline comment line, got %q", lines[0])
	}
}

func TestDescribeAgentReviewFiltered(t *testing.T) {
	// Agent-authored reviews (prefixed [Herdr]:) should be filtered from
	// snapshots so the agent is not notified about its own actions.
	prev := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{}, Reviews: []noteRecord{}, Inline: []noteRecord{},
	}}
	curr := &prSnapshot{Fingerprint: snapshotFP{
		Head: "aaa", Checks: ciDetail{Failed: []string{}},
		Comments: []noteRecord{},
		Reviews:  []noteRecord{}, // agent review already filtered by snapshot
		Inline:   []noteRecord{},
	}}
	lines := describe(prev, curr)
	if len(lines) != 0 {
		t.Fatalf("expected 0 lines after filtering agent review, got %d: %v", len(lines), lines)
	}
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
