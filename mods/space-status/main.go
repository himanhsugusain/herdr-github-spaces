package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
  space-status refresh [--repo PATH] [--output PATH] [--token TOKEN] [--once]
  space-status pr-watch [--repo PATH] [--github-repo OWNER/NAME] [--token TOKEN] [--state PATH] [--skip ADDRESS ...]

Commands:
  refresh   Write PR and CI status for the repository and its worktrees.
  pr-watch  Poll open PRs and forward new updates to matching live Pi agents.

Authentication:
  Set GITHUB_TOKEN or pass --token on each subcommand.`)
	os.Exit(exitCode)
}

func executableDir() string {
	path, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(path)
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
