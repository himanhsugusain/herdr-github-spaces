package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
	defaultGH     *github.Client
	accounts      map[string]string
	discoverRepos bool
	root          string
	owner         string
	repo          string
	statePath     string
	statusWriter  *asyncWriter
	stateWriter   *asyncWriter
	skip          map[string]bool
	stateCache    map[string]watchSlot
	gh            *github.Client
}

// ---------- entry point ----------

func runPRWatch(args []string) error {
	fs := newFlagSet("pr-watch")
	repoFlag := fs.String("repo", "", "Local repository/worktree root used for Git (required)")
	discover := fs.Bool("discover-repos", false, "Include badge data for all live local Herdr repositories")
	githubRepoFlag := fs.String("github-repo", "", "GitHub repository as OWNER/NAME (inferred from git remote when omitted)")
	tokenFlag := fs.String("token", "", "GitHub token (default: GITHUB_TOKEN or gh authentication)")
	stateFlag := fs.String("state", filepath.Join(executableDir(), "pr_watch_state.json"), "Watcher state JSON path")
	statusFlag := fs.String("status", filepath.Join(executableDir(), "status.json"), "PR/CI status JSON path")
	pidFlag := fs.String("pid", filepath.Join(executableDir(), "pr_watch.pid"), "Watcher PID file")
	var accountFlags stringList
	fs.Var(&accountFlags, "github-account", "GitHub login per OWNER or OWNER/REPO: KEY=LOGIN (repeatable)")
	var skip stringList
	fs.Var(&skip, "skip", "Agent address to skip (repeatable)")
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
	statePath, err := absolutePath(*stateFlag)
	if err != nil {
		return err
	}
	statusPath, err := absolutePath(*statusFlag)
	if err != nil {
		return err
	}

	pidPath, err := absolutePath(*pidFlag)
	if err != nil {
		return err
	}
	for _, path := range []string{statePath, statusPath, pidPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
	}

	// The UI can race when several agent-list instances try to start the
	// daemon at once. A PID file is only a hint and is not sufficient for
	// singleton startup: each process can pass the health check before any of
	// them has written its PID. Hold an advisory lock for the lifetime of the
	// watcher so duplicate daemons cannot all poll and deliver the same event.
	lockPath := statePath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			fmt.Fprintln(os.Stderr, "pr-watch: another watcher is already running")
			return nil
		}
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()

	token, err := resolveToken(*tokenFlag)
	if err != nil {
		return err
	}
	gh := newGitHubClient(token)

	owner, repo, err := repositoryIdentity(root, *githubRepoFlag)
	if err != nil {
		return err
	}

	defaultGH := gh
	gh, err = repositoryClient(context.Background(), gh, accounts, owner, repo)
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

	statusWriter := newAsyncWriter(statusPath)
	stateWriter := newAsyncWriter(statePath)
	w := &watcher{
		root: root, owner: owner, repo: repo, discoverRepos: *discover, accounts: accounts, defaultGH: defaultGH,
		statePath: statePath, statusWriter: statusWriter, stateWriter: stateWriter,
		skip: ignored, gh: gh,
	}

	// Write PID file so `space-status trigger` can signal us.
	if err := atomicWrite(pidPath, []byte(fmt.Sprint(os.Getpid()))); err != nil {
		statusWriter.Stop()
		stateWriter.Stop()
		return err
	}

	// SIGUSR1 triggers an immediate poll cycle.
	poke := make(chan os.Signal, 1)
	signal.Notify(poke, syscall.SIGUSR1)

	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer statusWriter.Stop()
	defer stateWriter.Stop()
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
		case <-poke:
			timer.Stop()
			fmt.Println("pr-watch: triggered by SIGUSR1")
		case <-timer.C:
		}
	}
}

// ---------- poll loop body ----------

func (w *watcher) once(ctx context.Context) error {
	var state map[string]watchSlot
	defer func() {
		if state != nil {
			w.stateCache = state
			_ = w.saveState(state)
		}
	}()

	// Keep the sidebar's PR/CI cache fresh from the same long-running process.
	// Previously pr-watch only delivered notifications, so status.json could
	// retain an old branch (for example master) after an agent switched to its
	// actual PR branch.
	if err := refreshOnce(ctx, w.root, w.statusWriter, w.defaultGH, w.owner, w.repo, w.discoverRepos, w.accounts); err != nil {
		fmt.Fprintf(os.Stderr, "pr-watch: status refresh: %v\n", err)
	}

	// Keep the state in memory between polls. The async writer intentionally
	// flushes in the background; reloading from disk here could observe the
	// previous snapshot and deliver the same notification twice.
	if w.stateCache == nil {
		loaded, err := w.loadState()
		if err != nil {
			return err
		}
		w.stateCache = loaded
	}
	state = w.stateCache
	agents, err := w.liveAgents(ctx)
	if err != nil {
		return err
	}

	// Collect unique branches from agents.
	var branches []string
	branchSet := make(map[string]bool)
	agentBranch := make(map[string]string) // address → branch
	for _, agent := range agents {
		name := branch(agent.CWD)
		agentBranch[agent.Address] = name
		if name != "" && name != "HEAD" && !branchSet[name] {
			branchSet[name] = true
			branches = append(branches, name)
		}
	}
	prs := w.openPRs(ctx, branches)

	groups := groupAgentsByPR(agents, agentBranch, prs)
	seen := make(map[string]bool)
	snapshots := make(map[int]*prSnapshot, len(groups))
	for _, group := range groups {
		for _, agent := range group.agents {
			seen[agent.Address] = true
		}

		current := snapshots[group.number]
		if current == nil {
			current, err = w.snapshot(ctx, group.number)
			if err != nil {
				return err
			}
			snapshots[group.number] = current
		}

		preserveKnownMergeable(group.agents, state, current)
		lines, hasBaseline := groupUpdateLines(group.agents, state, current)
		if !hasBaseline {
			for _, agent := range group.agents {
				state[agent.Address] = watchSlot{PR: fmt.Sprint(group.number), Snapshot: current}
				fmt.Printf("pr-watch: baseline %s PR #%d\n", agent.Where, group.number)
			}
			continue
		}

		if len(lines) > 0 {
			message := makeMessage(current, lines)
			address, delivered := deliverToFirstAvailable(group.agents,
				func(address string) bool {
					isBusy := w.busy(ctx, address)
					if isBusy {
						fmt.Printf("pr-watch: held %s: agent is busy\n", address)
					}
					return isBusy
				},
				func(address string) bool { return w.send(ctx, address, message) },
			)
			for _, agent := range group.agents {
				if delivered {
					state[agent.Address] = watchSlot{PR: fmt.Sprint(group.number), Snapshot: current}
				} else {
					old := state[agent.Address]
					state[agent.Address] = watchSlot{PR: fmt.Sprint(group.number), Snapshot: old.Snapshot, Pending: append([]string(nil), lines...)}
				}
			}
			if delivered {
				fmt.Printf("pr-watch: sent %s PR #%d (%d lines)\n", address, group.number, len(lines))
			}
		} else {
			for _, agent := range group.agents {
				state[agent.Address] = watchSlot{PR: fmt.Sprint(group.number), Snapshot: current}
			}
		}
	}

	for address := range state {
		if !seen[address] && !w.skip[address] {
			delete(state, address)
		}
	}
	return nil
}

type prAgentGroup struct {
	number int
	agents []watchAgent
}

func groupAgentsByPR(agents []watchAgent, agentBranch map[string]string, prs map[string]*github.PullRequest) []prAgentGroup {
	groups := make([]prAgentGroup, 0)
	byNumber := make(map[int]int)
	for _, agent := range agents {
		name := agentBranch[agent.Address]
		pr, matched := prs[name]
		if !matched || name == "" || name == "HEAD" || pr == nil {
			continue
		}
		number := pr.GetNumber()
		index, exists := byNumber[number]
		if !exists {
			index = len(groups)
			byNumber[number] = index
			groups = append(groups, prAgentGroup{number: number})
		}
		groups[index].agents = append(groups[index].agents, agent)
	}
	return groups
}

func preserveKnownMergeable(agents []watchAgent, state map[string]watchSlot, current *prSnapshot) {
	if current.Fingerprint.Mergeable != "" && current.Fingerprint.Mergeable != "unknown" {
		return
	}
	for _, agent := range agents {
		previous := state[agent.Address].Snapshot
		if previous == nil || previous.Number != current.Number || previous.Fingerprint.Head != current.Fingerprint.Head {
			continue
		}
		mergeable := previous.Fingerprint.Mergeable
		if mergeable != "" && mergeable != "unknown" {
			current.Fingerprint.Mergeable = mergeable
			return
		}
	}
}

// groupUpdateLines compares each terminal's previous state with the shared PR
// snapshot. Existing differences are combined once; pending messages are used
// only when the current snapshot introduces no new differences.
func groupUpdateLines(agents []watchAgent, state map[string]watchSlot, current *prSnapshot) ([]string, bool) {
	hasBaseline := false
	lines := make([]string, 0)
	pending := make([]string, 0)
	lineSet := make(map[string]bool)
	pendingSet := make(map[string]bool)
	for _, agent := range agents {
		slot, exists := state[agent.Address]
		if !exists || slot.Snapshot == nil {
			continue
		}
		hasBaseline = true
		for _, line := range describe(slot.Snapshot, current) {
			if !lineSet[line] {
				lineSet[line] = true
				lines = append(lines, line)
			}
		}
		for _, line := range slot.Pending {
			if !pendingSet[line] {
				pendingSet[line] = true
				pending = append(pending, line)
			}
		}
	}
	if len(lines) == 0 {
		lines = pending
	}
	return lines, hasBaseline
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
	os.MkdirAll(filepath.Dir(w.statePath), 0o755)
	w.stateWriter.Write(append(data, '\n'))
	return nil
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
			Status  string `json:"status"`
		} `json:"terminals"`
	}
	if err := decodeJSON(stdout, &response); err != nil {
		return nil, err
	}

	allowed := w.repoPaths(ctx)

	var agents []watchAgent
	for _, t := range response.Terminals {
		if t.State != "live" || t.Address == "" || t.CWD == "" || w.skip[t.Address] {
			continue
		}
		// Use the foreground process cwd (where the agent is actually working)
		// rather than the shell's cwd which may have drifted.
		cwd := w.foregroundCWD(ctx, t.Address)
		if cwd == "" {
			cwd = t.CWD
		}
		if !isUnderAny(cwd, allowed) {
			continue
		}
		where := t.Where
		if where == "" {
			where = t.Address
		}
		agents = append(agents, watchAgent{Address: t.Address, CWD: cwd, Where: where})
	}
	return agents, nil
}

// foregroundCWD returns the cwd of the terminal's foreground process, if any.
func (w *watcher) foregroundCWD(ctx context.Context, address string) string {
	args := []string{"hc", "get", "--json", address}
	stdout, _, err := command(ctx, "", 15*time.Second, args...)
	if err != nil {
		return ""
	}
	var resp struct {
		Foreground *struct {
			CWD string `json:"cwd"`
		} `json:"foreground"`
	}
	if decodeJSON(stdout, &resp) != nil || resp.Foreground == nil {
		return ""
	}
	return resp.Foreground.CWD
}

// repoPaths returns the repo root and all its worktree paths.
func (w *watcher) repoPaths(ctx context.Context) []string {
	paths, err := worktrees(w.root)
	if err != nil {
		return []string{w.root}
	}
	return paths
}

// isUnderAny reports whether dir is equal to or a subdirectory of any prefix.
func isUnderAny(dir string, prefixes []string) bool {
	for _, p := range prefixes {
		if dir == p || strings.HasPrefix(dir, p+"/") {
			return true
		}
	}
	return false
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

func (w *watcher) send(ctx context.Context, address, message string) bool {
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

// deliverToFirstAvailable attempts delivery to at most one terminal: the
// first one reported as non-busy. If every terminal is busy, it sends nothing.
func deliverToFirstAvailable(agents []watchAgent, busy func(string) bool, send func(string) bool) (string, bool) {
	for _, agent := range agents {
		if busy(agent.Address) {
			continue
		}
		return agent.Address, send(agent.Address)
	}
	return "", false
}

// ---------- GitHub data (SDK) ----------

func (w *watcher) openPRs(ctx context.Context, branches []string) map[string]*github.PullRequest {
	result := make(map[string]*github.PullRequest, len(branches))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, b := range branches {
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			pr, _ := lookupPR(ctx, w.gh, w.owner, w.repo, b)
			if pr != nil {
				mu.Lock()
				result[b] = pr
				mu.Unlock()
			}
		}(b)
	}
	wg.Wait()
	return result
}

func (w *watcher) snapshot(ctx context.Context, number int) (*prSnapshot, error) {
	pr, _, err := w.gh.PullRequests.Get(ctx, w.owner, w.repo, number)
	if err != nil {
		return nil, fmt.Errorf("get PR #%d: %w", number, err)
	}

	var (
		wg            sync.WaitGroup
		issueComments []*github.IssueComment
		prReviews     []*github.PullRequestReview
		prComments    []*github.PullRequestComment
		ci            ciResult
	)

	// Fetch comments, reviews, inline comments, and CI in parallel.
	wg.Add(4)
	go func() {
		defer wg.Done()
		issueComments = paginate(func(opts *github.ListOptions) ([]*github.IssueComment, *github.Response, error) {
			return w.gh.Issues.ListComments(ctx, w.owner, w.repo, number, &github.IssueListCommentsOptions{ListOptions: *opts})
		})
	}()
	go func() {
		defer wg.Done()
		prReviews = paginate(func(opts *github.ListOptions) ([]*github.PullRequestReview, *github.Response, error) {
			return w.gh.PullRequests.ListReviews(ctx, w.owner, w.repo, number, opts)
		})
	}()
	go func() {
		defer wg.Done()
		prComments = paginate(func(opts *github.ListOptions) ([]*github.PullRequestComment, *github.Response, error) {
			return w.gh.PullRequests.ListComments(ctx, w.owner, w.repo, number, &github.PullRequestListCommentsOptions{ListOptions: *opts})
		})
	}()
	go func() {
		defer wg.Done()
		ci, _ = fetchCI(ctx, w.gh, w.owner, w.repo, pr.GetHead().GetSHA())
	}()
	wg.Wait()

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

	reviews := make([]noteRecord, 0, len(prReviews))
	for _, r := range prReviews {
		body := r.GetBody()
		state := r.GetState()
		// Skip "COMMENTED" reviews with empty bodies — they are just
		// containers for inline comments which are already captured
		// separately in the Inline list.
		if strings.EqualFold(state, "COMMENTED") && strings.TrimSpace(body) == "" {
			continue
		}
		// Skip agent-authored reviews so the watcher does not echo its
		// own actions back to the agent.
		if isAgentComment(body) {
			continue
		}
		reviews = append(reviews, noteRecord{
			ID: fmt.Sprint(r.GetID()), Author: loginOrUnknown(r.GetUser()),
			State: state, At: formatTimestamp(r.SubmittedAt),
			Body: clip(body),
		})
	}

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

// ciEqual compares two ciDetail values, treating nil and empty Failed
// slices as equivalent so that a JSON round-trip (which can turn
// []string{} into nil) does not produce phantom diffs.
func ciEqual(a, b ciDetail) bool {
	if len(a.Failed) != len(b.Failed) {
		return false
	}
	for i := range a.Failed {
		if a.Failed[i] != b.Failed[i] {
			return false
		}
	}
	return a.Pending == b.Pending && a.Passed == b.Passed
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

	if !ciEqual(old.Checks, next.Checks) {
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
