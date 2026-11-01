package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/google/go-github/v72/github"
)

const (
	watchInterval = 60 * time.Second
	bodyLimit     = 700
)

// ---------- flag helpers ----------

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// ---------- typed snapshot structs ----------

type prSnapshot struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	Fingerprint snapshotFP `json:"fingerprint"`
}

type snapshotFP struct {
	Head      string       `json:"head"`
	State     string       `json:"state"`
	Mergeable string       `json:"mergeable"`
	Decision  string       `json:"decision"`
	Draft     bool         `json:"draft"`
	Checks    ciDetail     `json:"checks"`
	Comments  []noteRecord `json:"comments"`
	Reviews   []noteRecord `json:"reviews"`
	Inline    []noteRecord `json:"inline"`
}

type ciDetail struct {
	Failed  []string `json:"failed"`
	Pending int      `json:"pending"`
	Passed  int      `json:"passed"`
}

type noteRecord struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	At     string `json:"at"`
	State  string `json:"state,omitempty"`
	Path   string `json:"path,omitempty"`
	Body   string `json:"body"`
}

// ---------- watcher types ----------

type watchAgent struct {
	Address string
	CWD     string
	Where   string
}

type watchSlot struct {
	PR       string      `json:"pr"`
	Snapshot *prSnapshot `json:"snapshot"`
	Pending  []string    `json:"pending"`
}

type watcher struct {
	root      string
	owner     string
	repo      string
	statePath string
	skip      map[string]bool
	gh        *github.Client
	username  string
}

// ---------- entry point ----------

func runPRWatch(args []string) error {
	fs := newFlagSet("pr-watch")
	repoFlag := fs.String("repo", ".", "Local repository/worktree root used for Git")
	githubRepoFlag := fs.String("github-repo", "", "GitHub repository as OWNER/NAME (inferred from git remote when omitted)")
	tokenFlag := fs.String("token", "", "GitHub personal-access token (default: GITHUB_TOKEN env)")
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

	token, err := resolveToken(*tokenFlag)
	if err != nil {
		return err
	}
	gh := newGitHubClient(token)

	owner, repo := "", ""
	if r := strings.TrimSpace(*githubRepoFlag); r != "" {
		parts := strings.SplitN(r, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid --github-repo %q: expected OWNER/NAME", r)
		}
		owner, repo = parts[0], parts[1]
	} else {
		owner, repo, err = repoFromRemote(root)
		if err != nil {
			return err
		}
	}

	ctx := context.Background()
	username, err := githubUser(ctx, gh)
	if err != nil {
		return err
	}

	ignored := make(map[string]bool)
	for _, address := range skip {
		ignored[address] = true
	}
	if address := os.Getenv("HC_TERMINAL_ID"); address != "" {
		ignored[address] = true
	}

	w := &watcher{
		root: root, owner: owner, repo: repo,
		statePath: statePath, skip: ignored,
		gh: gh, username: username,
	}

	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		started := time.Now()
		if err := w.once(sctx); err != nil {
			fmt.Fprintf(os.Stderr, "pr-watch: %v\n", err)
		}
		wait := watchInterval - time.Since(started)
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-sctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// ---------- poll loop body ----------

func (w *watcher) once(ctx context.Context) error {
	state, err := w.loadState()
	if err != nil {
		return err
	}
	agents, err := w.liveAgents(ctx)
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
		number := pr.GetNumber()

		current, err := w.snapshot(ctx, number)
		if err != nil {
			return err
		}
		slot, exists := state[agent.Address]
		if !exists || slot.Snapshot == nil {
			state[agent.Address] = watchSlot{PR: fmt.Sprint(number), Snapshot: current}
			fmt.Printf("pr-watch: baseline %s PR #%d\n", agent.Where, number)
			continue
		}

		lines := describe(slot.Snapshot, current)
		if len(lines) == 0 && len(slot.Pending) > 0 {
			lines = slot.Pending
		}
		if len(lines) == 0 {
			state[agent.Address] = watchSlot{PR: fmt.Sprint(number), Snapshot: current}
			continue
		}

		if w.deliver(ctx, agent.Address, makeMessage(current, lines)) {
			state[agent.Address] = watchSlot{PR: fmt.Sprint(number), Snapshot: current}
			fmt.Printf("pr-watch: sent %s PR #%d (%d lines)\n", agent.Address, number, len(lines))
		} else {
			state[agent.Address] = watchSlot{PR: fmt.Sprint(number), Snapshot: slot.Snapshot, Pending: lines}
		}
	}

	for address := range state {
		if !seen[address] && !w.skip[address] {
			delete(state, address)
		}
	}
	return w.saveState(state)
}

// ---------- state persistence ----------

func (w *watcher) loadState() (map[string]watchSlot, error) {
	state := make(map[string]watchSlot)
	data, err := os.ReadFile(w.statePath)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	// Silently discard state if the format changed (e.g. after an upgrade).
	if decodeJSON(data, &state) != nil {
		return make(map[string]watchSlot), nil
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

// ---------- Herdr agent discovery (hc CLI) ----------

func (w *watcher) liveAgents(ctx context.Context) ([]watchAgent, error) {
	args := []string{"hc", "ls", "--json"}
	stdout, stderr, err := command(ctx, "", 45*time.Second, args...)
	if err != nil {
		return nil, commandError(args, stdout, stderr, err)
	}
	var response struct {
		Terminals []struct {
			Address string `json:"address"`
			CWD     string `json:"cwd"`
			State   string `json:"state"`
			Where   string `json:"where"`
		} `json:"terminals"`
	}
	if err := decodeJSON(stdout, &response); err != nil {
		return nil, err
	}
	var agents []watchAgent
	for _, t := range response.Terminals {
		if t.State != "live" || t.Address == "" || t.CWD == "" || w.skip[t.Address] {
			continue
		}
		where := t.Where
		if where == "" {
			where = t.Address
		}
		agents = append(agents, watchAgent{Address: t.Address, CWD: t.CWD, Where: where})
	}
	return agents, nil
}

func (w *watcher) busy(ctx context.Context, address string) bool {
	args := []string{"hc", "get", "--json", address}
	stdout, _, err := command(ctx, "", 15*time.Second, args...)
	if err != nil {
		return true
	}
	var response struct {
		Status string `json:"status"`
	}
	if decodeJSON(stdout, &response) != nil {
		return false
	}
	return response.Status == "working" || response.Status == "blocked"
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

// ---------- GitHub data (SDK) ----------

func (w *watcher) openPRs(ctx context.Context) (map[string]*github.PullRequest, error) {
	prs, err := listPRsByAuthor(ctx, w.gh, w.owner, w.repo, w.username, "open", 40)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*github.PullRequest, len(prs))
	for _, pr := range prs {
		result[pr.GetHead().GetRef()] = pr
	}
	return result, nil
}

func (w *watcher) snapshot(ctx context.Context, number int) (*prSnapshot, error) {
	pr, _, err := w.gh.PullRequests.Get(ctx, w.owner, w.repo, number)
	if err != nil {
		return nil, fmt.Errorf("get PR #%d: %w", number, err)
	}

	// Conversation comments.
	issueComments := paginate(func(opts *github.ListOptions) ([]*github.IssueComment, *github.Response, error) {
		return w.gh.Issues.ListComments(ctx, w.owner, w.repo, number, &github.IssueListCommentsOptions{ListOptions: *opts})
	})
	comments := make([]noteRecord, 0, len(issueComments))
	for _, c := range issueComments {
		if isAgentComment(c.GetBody()) {
			continue
		}
		comments = append(comments, noteRecord{
			ID: fmt.Sprint(c.GetID()), Author: loginOrUnknown(c.GetUser()),
			At: formatTimestamp(c.UpdatedAt), Body: clip(c.GetBody()),
		})
	}

	// Reviews.
	prReviews := paginate(func(opts *github.ListOptions) ([]*github.PullRequestReview, *github.Response, error) {
		return w.gh.PullRequests.ListReviews(ctx, w.owner, w.repo, number, opts)
	})
	reviews := make([]noteRecord, 0, len(prReviews))
	for _, r := range prReviews {
		reviews = append(reviews, noteRecord{
			ID: fmt.Sprint(r.GetID()), Author: loginOrUnknown(r.GetUser()),
			State: r.GetState(), At: formatTimestamp(r.SubmittedAt),
			Body: clip(r.GetBody()),
		})
	}

	// Inline review comments (paginated).
	prComments := paginate(func(opts *github.ListOptions) ([]*github.PullRequestComment, *github.Response, error) {
		return w.gh.PullRequests.ListComments(ctx, w.owner, w.repo, number, &github.PullRequestListCommentsOptions{ListOptions: *opts})
	})
	inline := make([]noteRecord, 0, len(prComments))
	for _, c := range prComments {
		if isAgentComment(c.GetBody()) {
			continue
		}
		inline = append(inline, noteRecord{
			ID: fmt.Sprint(c.GetID()), Author: loginOrUnknown(c.GetUser()),
			At: formatTimestamp(c.UpdatedAt), Path: c.GetPath(),
			Body: clip(c.GetBody()),
		})
	}

	// CI.
	ref := pr.GetHead().GetSHA()
	ci := fetchCI(ctx, w.gh, w.owner, w.repo, ref)

	// Review decision (approximated).
	decision := computeDecision(prReviews)

	state := prStateName(pr)
	mergeable := pr.GetMergeableState()
	if mergeable == "" {
		mergeable = "unknown"
	}

	return &prSnapshot{
		Number: pr.GetNumber(),
		Title:  pr.GetTitle(),
		URL:    pr.GetHTMLURL(),
		Fingerprint: snapshotFP{
			Head: pr.GetHead().GetSHA(), State: state, Mergeable: mergeable,
			Decision: decision, Draft: pr.GetDraft(),
			Checks:   ciDetail{Failed: ci.FailedNames, Pending: ci.Pending, Passed: ci.Passed},
			Comments: comments, Reviews: reviews, Inline: inline,
		},
	}, nil
}

// paginate collects all pages from a GitHub list endpoint.
func paginate[T any](fn func(opts *github.ListOptions) ([]T, *github.Response, error)) []T {
	var all []T
	opts := &github.ListOptions{PerPage: 100}
	for {
		items, resp, err := fn(opts)
		if err != nil || len(items) == 0 {
			break
		}
		all = append(all, items...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all
}

func loginOrUnknown(u *github.User) string {
	if login := u.GetLogin(); login != "" {
		return login
	}
	return "unknown"
}

// ---------- snapshot diff ----------

// isAgentComment returns true if the body was posted by this agent.
func isAgentComment(body string) bool {
	trimmed := strings.TrimSpace(body)
	return strings.HasPrefix(trimmed, "[Herdr]:")
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

func noteMap(items []noteRecord) map[string]noteRecord {
	m := make(map[string]noteRecord, len(items))
	for _, item := range items {
		m[item.ID] = item
	}
	return m
}

func describe(previous, current *prSnapshot) []string {
	if previous == nil || current == nil {
		return nil
	}
	old, next := &previous.Fingerprint, &current.Fingerprint
	var lines []string

	// Merge / close / reopen detection.
	if old.State != next.State {
		switch next.State {
		case "MERGED":
			lines = append(lines, "- PR was merged.")
		case "CLOSED":
			lines = append(lines, "- PR was closed without merging.")
		case "OPEN":
			if old.State == "CLOSED" {
				lines = append(lines, "- PR was reopened.")
			}
		}
	}

	// Mergeable status change.
	if old.Mergeable != next.Mergeable && next.Mergeable != "unknown" {
		lines = append(lines, fmt.Sprintf("- Mergeable status changed to %s.", next.Mergeable))
	}

	if old.Head != next.Head {
		head := next.Head
		if len(head) > 10 {
			head = head[:10]
		}
		lines = append(lines, fmt.Sprintf("- Head moved to %s.", head))
	}

	if old.Decision != next.Decision || old.Draft != next.Draft {
		decision := next.Decision
		if decision == "" {
			decision = "none"
		}
		draft := ""
		if next.Draft {
			draft = " draft"
		}
		lines = append(lines, fmt.Sprintf("- Review decision is now %s%s.", decision, draft))
	}

	if !reflect.DeepEqual(old.Checks, next.Checks) {
		failedText := strings.Join(next.Checks.Failed, ", ")
		if failedText == "" {
			failedText = "none"
		}
		lines = append(lines, fmt.Sprintf("- CI: failed [%s]; pending %d; passed %d.",
			failedText, next.Checks.Pending, next.Checks.Passed))
	}

	for _, group := range []struct {
		label string
		old   []noteRecord
		next  []noteRecord
	}{
		{"Comment", old.Comments, next.Comments},
		{"Review", old.Reviews, next.Reviews},
		{"Inline comment", old.Inline, next.Inline},
	} {
		before := noteMap(group.old)
		for _, item := range group.next {
			if prev, exists := before[item.ID]; exists && reflect.DeepEqual(prev, item) {
				continue
			}
			where := ""
			if item.Path != "" {
				where = " on " + item.Path
			}
			state := ""
			if item.State != "" {
				state = " (" + item.State + ")"
			}
			body := item.Body
			if body == "" {
				body = "(no body)"
			}
			lines = append(lines, fmt.Sprintf("- %s from %s%s%s: %s",
				group.label, item.Author, state, where, body))
		}
	}
	return lines
}

func makeMessage(current *prSnapshot, lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## PR #%d update: %s\n%s\n\n", current.Number, current.Title, current.URL)

	// Categorise diff lines into buckets for actionable prompts.
	var (
		stateLine    []string // merged, closed, reopened
		mergeable    []string
		commitLines  []string
		decisionLine []string
		ciLines      []string
		reviews      []string
		comments     []string
		inline       []string
	)
	for _, line := range lines {
		switch {
		case strings.Contains(line, "PR was merged"), strings.Contains(line, "PR was closed"), strings.Contains(line, "PR was reopened"):
			stateLine = append(stateLine, line)
		case strings.Contains(line, "Mergeable status"):
			mergeable = append(mergeable, line)
		case strings.Contains(line, "Head moved"):
			commitLines = append(commitLines, line)
		case strings.Contains(line, "Review decision"):
			decisionLine = append(decisionLine, line)
		case strings.HasPrefix(line, "- CI:"):
			ciLines = append(ciLines, line)
		case strings.HasPrefix(line, "- Review from"):
			reviews = append(reviews, line)
		case strings.HasPrefix(line, "- Inline comment"):
			inline = append(inline, line)
		case strings.HasPrefix(line, "- Comment from"):
			comments = append(comments, line)
		default:
			comments = append(comments, line)
		}
	}

	// Emit sections with action instructions.
	if len(stateLine) > 0 {
		for _, l := range stateLine {
			switch {
			case strings.Contains(l, "merged"):
				fmt.Fprintf(&b, "### ✅ PR Merged\n%s\n→ PR is already merged. No further action needed.\n\n", l)
			case strings.Contains(l, "closed"):
				fmt.Fprintf(&b, "### 🚫 PR Closed\n%s\n→ No action needed unless you want to reopen.\n\n", l)
			case strings.Contains(l, "reopened"):
				fmt.Fprintf(&b, "### 🔄 PR Reopened\n%s\n→ Check outstanding review comments and CI status.\n\n", l)
			}
		}
	}

	if len(ciLines) > 0 {
		fp := &current.Fingerprint
		if len(fp.Checks.Failed) > 0 {
			fmt.Fprintf(&b, "### 🔴 CI Failed\n%s\n→ Run `gh pr checks %d` to identify failures. Read the logs, diagnose and fix.\n\n",
				strings.Join(ciLines, "\n"), current.Number)
		} else if fp.Checks.Pending > 0 {
			fmt.Fprintf(&b, "### ⏳ CI Pending\n%s\n→ Checks are still running. Wait for results.\n\n", strings.Join(ciLines, "\n"))
		} else {
			fmt.Fprintf(&b, "### ✅ CI Passed\n%s\n→ All checks green. If reviews are also approved, ask the user whether to merge.\n\n", strings.Join(ciLines, "\n"))
		}
	}

	if len(reviews) > 0 {
		for _, r := range reviews {
			switch {
			case strings.Contains(r, "CHANGES_REQUESTED"):
				fmt.Fprintf(&b, "### ❌ Changes Requested\n%s\n→ Read each review comment carefully. Address the feedback, push fixes.\n\n", r)
			case strings.Contains(r, "APPROVED"):
				fmt.Fprintf(&b, "### ✅ Review Approved\n%s\n→ Check if all required reviews are met and CI is green — if so, ask the user whether to merge.\n\n", r)
			default:
				fmt.Fprintf(&b, "### 💬 Review Comment\n%s\n→ Read the review and determine if action is needed.\n\n", r)
			}
		}
	}

	if len(inline) > 0 {
		fmt.Fprintf(&b, "### 💬 Inline Review Comments\n")
		for _, l := range inline {
			fmt.Fprintf(&b, "%s\n", l)
		}
		fmt.Fprintf(&b, "→ Read each comment in context of the file. Fix or respond to each one.\n\n")
	}

	if len(comments) > 0 {
		fmt.Fprintf(&b, "### 💬 PR Comments\n")
		for _, l := range comments {
			fmt.Fprintf(&b, "%s\n", l)
		}
		fmt.Fprintf(&b, "→ Determine if any comment requires a code change or reply.\n\n")
	}

	if len(commitLines) > 0 {
		fmt.Fprintf(&b, "### 🔨 New Commits\n%s\n→ Check if this resolves outstanding review comments.\n\n", strings.Join(commitLines, "\n"))
	}

	if len(decisionLine) > 0 {
		for _, l := range decisionLine {
			switch {
			case strings.Contains(l, "APPROVED"):
				fmt.Fprintf(&b, "### ✅ All Reviews Approved\n%s\n→ If CI is green, ask the user whether to merge.\n\n", l)
			case strings.Contains(l, "CHANGES_REQUESTED"):
				fmt.Fprintf(&b, "### ❌ Changes Requested\n%s\n→ Address all review feedback before proceeding.\n\n", l)
			default:
				fmt.Fprintf(&b, "### 📋 Review Status\n%s\n\n", l)
			}
		}
	}

	if len(mergeable) > 0 {
		for _, l := range mergeable {
			if strings.Contains(l, "dirty") || strings.Contains(l, "behind") {
				fmt.Fprintf(&b, "### ⚠️ Merge Conflict\n%s\n→ Rebase or merge the base branch to resolve conflicts.\n\n", l)
			} else {
				fmt.Fprintf(&b, "### 📋 Mergeable Status\n%s\n\n", l)
			}
		}
	}

	// Current state summary.
	fp := &current.Fingerprint
	failedNames := "none"
	if len(fp.Checks.Failed) > 0 {
		failedNames = strings.Join(fp.Checks.Failed, ", ")
	}
	head := fp.Head
	if len(head) > 10 {
		head = head[:10]
	}
	fmt.Fprintf(&b, "---\n**Current state**: %s | review=%s | CI: failed=[%s] pending=%d passed=%d | mergeable=%s | head=%s\n\n",
		fp.State, fp.Decision, failedNames, fp.Checks.Pending, fp.Checks.Passed, fp.Mergeable, head)

	b.WriteString("When posting PR comments or replies, prefix with `[Herdr]:` followed by a newline, then the markdown body. This identifies messages as agent-authored so they are not re-processed. Use markdown formatting for all comments and responses (GitHub supports it).\n\n")
	b.WriteString("Handle this only on this branch. Do not start other agents. If it needs no code or reply change, stop without extra work.")

	return b.String()
}
