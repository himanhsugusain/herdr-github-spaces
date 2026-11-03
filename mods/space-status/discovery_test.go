package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-github/v72/github"
)

func testRepository(t *testing.T, owner, name string) string {
	t.Helper()
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "feature"},
		{"-c", "core.hooksPath=" + t.TempDir(), "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "test"},
		{"remote", "add", "origin", "git@github.com:" + owner + "/" + name + ".git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestHerdrDirectoriesUsesLocalVisibleForeground(t *testing.T) {
	fixtures := t.TempDir()
	files := map[string]string{
		"list.json":      `{"terminals":[{"address":"local","state":"live"},{"address":"duplicate","state":"live"},{"address":"remote/t1","state":"live"},{"address":"finished","state":"finished"},{"address":"hidden","state":"live"}]}`,
		"local.json":     `{"cwd":"/home/person","labels":{"herdr.space":"space"},"foreground":{"cwd":"/repos/work/src"}}`,
		"duplicate.json": `{"cwd":"/repos/work/src","labels":{"herdr.space":"space"}}`,
		"hidden.json":    `{"cwd":"/repos/hidden","labels":{}}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(fixtures, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\ncase \"$1\" in\nls) cat \"$HERDR_TEST_FIXTURES/list.json\" ;;\nget) cat \"$HERDR_TEST_FIXTURES/$3.json\" ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(fixtures, "hc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_TEST_FIXTURES", fixtures)
	t.Setenv("PATH", fixtures+string(os.PathListSeparator)+os.Getenv("PATH"))
	directories, errs := herdrDirectories(context.Background())
	if len(errs) != 0 || !reflect.DeepEqual(directories, []string{"/repos/work/src"}) {
		t.Fatalf("directories=%v errors=%v", directories, errs)
	}
}

func TestDiscoverTargetsKeepsRepositoryOwnersSeparate(t *testing.T) {
	personal := testRepository(t, "personal", "layout")
	work := testRepository(t, "work-org", "app")
	sub := filepath.Join(work, "src")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	targets := discoverTargets(context.Background(), repositoryTarget{owner: "personal", repo: "layout", paths: []string{personal}}, []string{personal, sub, work, t.TempDir()})
	if len(targets) != 2 {
		t.Fatalf("targets=%+v", targets)
	}
	if targets[1].owner != "work-org" || targets[1].repo != "app" || !reflect.DeepEqual(targets[1].paths, []string{work}) {
		t.Fatalf("work target=%+v", targets[1])
	}
}

func TestCollectRepositoryStatusIncludesPRCIAndReviewFromDifferentOwners(t *testing.T) {
	personal := testRepository(t, "personal", "layout")
	work := testRepository(t, "work-org", "app")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/personal/layout/pulls":
			if r.URL.Query().Get("head") != "personal:feature" {
				t.Errorf("wrong personal head: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `[]`)
		case r.URL.Path == "/repos/work-org/app/pulls":
			if r.URL.Query().Get("head") != "work-org:feature" {
				t.Errorf("wrong work head: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `[{"number":42,"state":"open","head":{"sha":"abc"}}]`)
		case r.URL.Path == "/repos/work-org/app/commits/abc/check-runs":
			fmt.Fprint(w, `{"check_runs":[{"name":"unit","conclusion":"success"},{"name":"lint","conclusion":"failure"}]}`)
		case r.URL.Path == "/repos/work-org/app/commits/abc/status":
			fmt.Fprint(w, `{"statuses":[{"context":"build","state":"pending"}]}`)
		case r.URL.Path == "/repos/work-org/app/pulls/42/reviews":
			fmt.Fprint(w, `[{"user":{"login":"reviewer"},"state":"APPROVED"}]`)
		default:
			t.Errorf("unexpected API request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := github.NewClient(server.Client())
	client.BaseURL, _ = url.Parse(server.URL + "/")
	records, errs := collectRepositoryStatus(context.Background(), client, []repositoryTarget{
		{owner: "personal", repo: "layout", paths: []string{personal}},
		{owner: "work-org", repo: "app", paths: []string{work}},
	}, nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	cacheKey, _ := jsonText([]any{nil, work, "feature"}, false)
	record, ok := records[string(cacheKey)].(statusRecord)
	if !ok || record.Repository != "work-org/app" || record.Number != 42 || record.Passed != 1 || record.Failed != 1 || record.Pending != 1 || record.ReviewDecision != "APPROVED" {
		t.Fatalf("record=%+v", record)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records", len(records))
	}
}

func TestRepositoryErrorsIdentifyTheInaccessibleOwner(t *testing.T) {
	work := testRepository(t, "work-org", "app")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer server.Close()
	client := github.NewClient(server.Client())
	client.BaseURL, _ = url.Parse(server.URL + "/")
	_, errs := collectRepositoryStatus(context.Background(), client, []repositoryTarget{{owner: "work-org", repo: "app", paths: []string{work}}}, nil)
	if len(errs) != 1 || !strings.Contains(errs[0], "work-org/app") || !strings.Contains(errs[0], "404") {
		t.Fatalf("errors=%v", errs)
	}
}
