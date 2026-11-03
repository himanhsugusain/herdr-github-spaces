package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/go-github/v72/github"
)

type repositoryTarget struct {
	owner string
	repo  string
	paths []string
}

func herdrDirectories(ctx context.Context) ([]string, []string) {
	stdout, _, err := command(ctx, "", commandTimeout, "hc", "ls", "--json")
	if err != nil {
		return nil, []string{"Cannot discover local Herdr repositories: hc ls failed"}
	}
	var list struct {
		Terminals []struct {
			Address string `json:"address"`
			State   string `json:"state"`
		} `json:"terminals"`
	}
	if err := decodeJSON(stdout, &list); err != nil {
		return nil, []string{"Cannot discover local Herdr repositories: invalid hc ls response"}
	}
	var directories, errs []string
	seen := make(map[string]bool)
	for _, terminal := range list.Terminals {
		if terminal.State != "live" || terminal.Address == "" || strings.Contains(terminal.Address, "/") {
			continue
		}
		stdout, _, err := command(ctx, "", commandTimeout, "hc", "get", "--json", terminal.Address)
		var detail struct {
			CWD        string            `json:"cwd"`
			Labels     map[string]string `json:"labels"`
			Foreground *struct {
				CWD string `json:"cwd"`
			} `json:"foreground"`
		}
		if err != nil || decodeJSON(stdout, &detail) != nil {
			errs = append(errs, fmt.Sprintf("Cannot inspect Herdr terminal %s", terminal.Address))
			continue
		}
		if detail.Labels["herdr.space"] == "" {
			continue
		}
		cwd := detail.CWD
		if detail.Foreground != nil && detail.Foreground.CWD != "" {
			cwd = detail.Foreground.CWD
		}
		if filepath.IsAbs(cwd) && !seen[cwd] {
			seen[cwd] = true
			directories = append(directories, cwd)
		}
	}
	return directories, errs
}

func discoverTargets(ctx context.Context, primary repositoryTarget, directories []string) []repositoryTarget {
	targets := []repositoryTarget{primary}
	byRepository := map[string]int{primary.owner + "/" + primary.repo: 0}
	seen := make(map[string]bool)
	for _, path := range primary.paths {
		seen[path] = true
	}
	for _, cwd := range directories {
		stdout, _, err := command(ctx, cwd, commandTimeout, "git", "rev-parse", "--show-toplevel")
		root := strings.TrimSpace(string(stdout))
		if err != nil || !filepath.IsAbs(root) || seen[root] {
			continue
		}
		seen[root] = true
		owner, repo, err := repoFromRemote(root)
		if err != nil {
			continue
		}
		identity := owner + "/" + repo
		index, exists := byRepository[identity]
		if !exists {
			index = len(targets)
			byRepository[identity] = index
			targets = append(targets, repositoryTarget{owner: owner, repo: repo})
		}
		targets[index].paths = append(targets[index].paths, root)
	}
	return targets
}

func collectRepositoryStatus(ctx context.Context, gh *github.Client, targets []repositoryTarget, accounts map[string]string) (map[string]any, []string) {
	prs := make(map[string]any)
	var errs []string
	for _, target := range targets {
		client, err := repositoryClient(ctx, gh, accounts, target.owner, target.repo)
		if err != nil {
			errs = append(errs, target.owner+"/"+target.repo+": "+err.Error())
			continue
		}
		records, failures := pullRequests(ctx, client, target.owner, target.repo, target.paths)
		for key, value := range records {
			prs[key] = value
		}
		for _, failure := range failures {
			errs = append(errs, target.owner+"/"+target.repo+": "+failure)
		}
	}
	return prs, errs
}
