package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/google/go-github/v72/github"
)

func TestLookupPRBranchExclusions(t *testing.T) {
	for _, tc := range []struct {
		branch string
		states []string
	}{
		{branch: "master"},
		{branch: "main"},
		{branch: "feature", states: []string{"open", "closed"}},
		{branch: "feature/main", states: []string{"open", "closed"}},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			var states []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/owner/repo/pulls" || r.URL.Query().Get("head") != "owner:"+tc.branch {
					t.Errorf("unexpected request: %s", r.URL)
				}
				state := r.URL.Query().Get("state")
				states = append(states, state)
				w.Header().Set("Content-Type", "application/json")
				if state == "open" {
					fmt.Fprint(w, `[]`)
				} else {
					fmt.Fprint(w, `[{"number":42,"state":"closed"}]`)
				}
			}))
			defer server.Close()
			client := github.NewClient(server.Client())
			client.BaseURL, _ = url.Parse(server.URL + "/")
			pr, err := lookupPR(context.Background(), client, "owner", "repo", tc.branch)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(states, tc.states) {
				t.Fatalf("requested states = %v, want %v", states, tc.states)
			}
			if len(tc.states) == 0 {
				if pr != nil {
					t.Fatalf("excluded branch returned PR #%d", pr.GetNumber())
				}
			} else if pr.GetNumber() != 42 {
				t.Fatalf("PR number = %d, want 42", pr.GetNumber())
			}
		})
	}
}

func TestLookupPRPrefersOpenPR(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprintf("draft=%t", draft), func(t *testing.T) {
			var states []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query := r.URL.Query()
				if r.URL.Path != "/repos/owner/repo/pulls" || query.Get("head") != "owner:feature" || query.Get("sort") != "updated" || query.Get("per_page") != "1" {
					t.Errorf("unexpected request: %s", r.URL)
				}
				state := query.Get("state")
				states = append(states, state)
				w.Header().Set("Content-Type", "application/json")
				if state == "open" {
					fmt.Fprintf(w, `[{"number":43,"state":"open","draft":%t}]`, draft)
				} else {
					fmt.Fprint(w, `[{"number":42,"state":"closed"}]`)
				}
			}))
			defer server.Close()
			client := github.NewClient(server.Client())
			client.BaseURL, _ = url.Parse(server.URL + "/")
			pr, err := lookupPR(context.Background(), client, "owner", "repo", "feature")
			if err != nil {
				t.Fatal(err)
			}
			if pr.GetNumber() != 43 || pr.GetDraft() != draft {
				t.Fatalf("got PR #%d (draft=%t), want #43 (draft=%t)", pr.GetNumber(), pr.GetDraft(), draft)
			}
			if !reflect.DeepEqual(states, []string{"open"}) {
				t.Fatalf("requested states = %v, want [open]", states)
			}
		})
	}
}
