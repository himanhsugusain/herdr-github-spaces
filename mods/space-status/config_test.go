package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryMustBeExplicit(t *testing.T) {
	for name, run := range map[string]func([]string) error{"refresh": runRefresh, "pr-watch": runPRWatch} {
		t.Run(name, func(t *testing.T) {
			if err := run(nil); err == nil || !strings.Contains(err.Error(), "--repo is required") {
				t.Fatalf("got %v, want required-repository error before authentication or runtime writes", err)
			}
		})
	}
}

func TestRepositoryIdentityOverride(t *testing.T) {
	owner, repo, err := repositoryIdentity(t.TempDir(), "example-owner/example-repo")
	if err != nil || owner != "example-owner" || repo != "example-repo" {
		t.Fatalf("got %q/%q, %v", owner, repo, err)
	}
	for _, invalid := range []string{"/repo", "owner/", "owner/repo/extra", "missing-separator"} {
		if _, _, err := repositoryIdentity("", invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestTriggerRejectsUnsafePIDFromCustomFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom watcher.pid")
	for _, value := range []string{"-1", "0", "1", "42 trailing-text", ""} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runTrigger([]string{"--pid", path}); err == nil || !strings.Contains(err.Error(), "invalid PID") {
			t.Fatalf("PID %q: got %v", value, err)
		}
	}
}

func TestAuthenticationPrecedenceAndCLIFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nprintf 'test-cli-value\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GITHUB_TOKEN", "test-env-value")
	if token, err := resolveToken("test-flag-value"); err != nil || token != "test-flag-value" {
		t.Fatalf("flag: %q, %v", token, err)
	}
	if token, err := resolveToken(""); err != nil || token != "test-env-value" {
		t.Fatalf("env: %q, %v", token, err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if token, err := resolveToken(""); err != nil || token != "test-cli-value" {
		t.Fatalf("CLI: %q, %v", token, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nprintf 'private-output'\nprintf 'private-error' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if token, err := resolveToken(""); token != "" || err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatalf("failure exposed authentication output: %q, %v", token, err)
	}
}

func TestRepositoryAccountsPreferExactRepository(t *testing.T) {
	accounts, err := parseGitHubAccounts([]string{"work-org=work-login", "Work-Org/special=other-login", "personal/layout=personal-login"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ owner, repo, want string }{
		{"work-org", "app", "work-login"}, {"WORK-ORG", "SPECIAL", "other-login"}, {"personal", "layout", "personal-login"}, {"unknown", "app", ""},
	} {
		if got := repositoryAccount(accounts, tc.owner, tc.repo); got != tc.want {
			t.Fatalf("%s/%s: got %q want %q", tc.owner, tc.repo, got, tc.want)
		}
	}
	for _, invalid := range []string{"owner", "owner=", "owner/repo/extra=login", "owner=login with spaces", "owner=login\n"} {
		if _, err := parseGitHubAccounts([]string{invalid}); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestAccountTokenIgnoresInheritedTokensWithoutChangingParent(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
if [ -n "${GH_TOKEN-}${GITHUB_TOKEN-}${GH_ENTERPRISE_TOKEN-}${GITHUB_ENTERPRISE_TOKEN-}" ]; then
  printf 'inherited credential was not cleared' >&2
  exit 1
fi
if [ "$1" != auth ] || [ "$2" != token ] || [ "$3" != --hostname ] || [ "$4" != github.com ] || [ "$5" != --user ]; then
  exit 2
fi
printf 'test-token-for-%s\n' "$6"
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(name, "inherited-personal-token")
	}
	for _, login := range []string{"personal-login", "work-login"} {
		token, err := accountToken(context.Background(), login)
		if err != nil || token != "test-token-for-"+login {
			t.Fatalf("%s: token=%q error=%v", login, token, err)
		}
	}
	if os.Getenv("GITHUB_TOKEN") != "inherited-personal-token" {
		t.Fatal("parent environment changed")
	}
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nprintf 'secret-stdout'\nprintf 'secret-stderr' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	token, err := accountToken(context.Background(), "work-login")
	if token != "" || err == nil || strings.Contains(err.Error(), "secret-") {
		t.Fatalf("unsafe error: token=%q err=%v", token, err)
	}
}
