package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage(2)
	}
	var err error
	switch os.Args[1] {
	case "refresh":
		err = runRefresh(os.Args[2:])
	case "pr-watch":
		err = runPRWatch(os.Args[2:])
	case "trigger":
		err = runTrigger(os.Args[2:])
	case "help", "-h", "--help":
		usage(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "space-status: %v\n", err)
		os.Exit(1)
	}
}

func usage(exitCode int) {
	fmt.Fprintln(os.Stderr, `Usage:
  space-status refresh --repo PATH [--github-repo OWNER/NAME] [--output PATH] [--token TOKEN] [--discover-repos] [--github-account KEY=LOGIN ...] [--once]
  space-status pr-watch --repo PATH [--github-repo OWNER/NAME] [--token TOKEN] [--state PATH] [--status PATH] [--pid PATH] [--skip ADDRESS ...] [--discover-repos] [--github-account KEY=LOGIN ...]
  space-status trigger [--pid PATH]

Commands:
  refresh   Write PR and CI status for the repository and its worktrees.
  pr-watch  Refresh PR/CI status and forward new updates to matching live terminals.
  trigger   Signal the running pr-watch daemon to refresh immediately.

Authentication:
  Use gh authentication, GITHUB_TOKEN, or --token.
  --github-account OWNER=LOGIN or OWNER/REPO=LOGIN selects a saved gh login
  for that repository, overriding inherited token variables for those requests.

Paths:
  Runtime files default to the helper executable directory; flags override them.`)
	os.Exit(exitCode)
}

func executableDir() string {
	path, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(path)
}

func runTrigger(args []string) error {
	fs := newFlagSet("trigger")
	pidFlag := fs.String("pid", filepath.Join(executableDir(), "pr_watch.pid"), "Watcher PID file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	pidPath, err := absolutePath(*pidFlag)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("no running pr-watch daemon (cannot read %s)", pidPath)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return fmt.Errorf("invalid PID in %s", pidPath)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("process %d not found", pid)
	}
	if err := proc.Signal(syscall.SIGUSR1); err != nil {
		return fmt.Errorf("cannot signal PID %d: %w", pid, err)
	}
	fmt.Printf("Triggered pr-watch (PID %d)\n", pid)
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func repositoryPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("--repo is required")
	}
	root, err := absolutePath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New("--repo must be an existing local directory")
	}
	return root, nil
}
