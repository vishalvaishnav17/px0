package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initTestRepo(t *testing.T, dir string, files map[string]string, msg string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	run("config", "commit.gpgsign", "false")
	run("add", "-A")
	run("commit", "-qm", msg)
}

func TestGitDiscoverMultiRepo(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	os.MkdirAll(filepath.Join(ws, "repoA"), 0o755)
	os.MkdirAll(filepath.Join(ws, "repoB"), 0o755)
	os.MkdirAll(filepath.Join(ws, "plain"), 0o755)
	initTestRepo(t, filepath.Join(ws, "repoA"), map[string]string{"a.go": "package a\n"}, "init A")
	initTestRepo(t, filepath.Join(ws, "repoB"), map[string]string{"b.go": "package b\n"}, "init B")
	os.WriteFile(filepath.Join(ws, "plain", "x.txt"), []byte("x\n"), 0o644)

	repos := gitDiscoverRepos(ws)
	if len(repos) != 2 {
		t.Fatalf("discover = %v, want 2 repos", repos)
	}
	if repos[0].Path != "repoA" || repos[1].Path != "repoB" {
		t.Fatalf("discover paths = %v, want [repoA repoB]", repos)
	}
	if !gitAvailable(ws) {
		t.Fatal("gitAvailable false for multi-repo workspace")
	}
	// Dirty one file in repoA; aggregated status must prefix with repo dir.
	os.WriteFile(filepath.Join(ws, "repoA", "a.go"), []byte("package a2\n"), 0o644)
	st := gitStatusAgainst(ws, "HEAD")
	if st["repoA/a.go"] != "M" {
		t.Fatalf("aggregated status = %v, want repoA/a.go=M", st)
	}
	if _, ok := st["repoB/b.go"]; ok {
		t.Fatalf("repoB should be clean, got %v", st)
	}
	// Diff resolves to the containing repo.
	d := gitDiffAgainst(ws, "repoA/a.go", "HEAD")
	if !strings.Contains(d, "package a") {
		t.Fatalf("multi diff missing content:\n%s", d)
	}
	if got := gitDiffAgainst(ws, "plain/x.txt", "HEAD"); got != "" {
		t.Fatalf("non-repo file diff = %q, want empty", got)
	}
	// Index badges aggregate too.
	ix := NewIndex(ws)
	ix.Build()
	kids, _ := ix.Children("")
	byName := map[string]Node{}
	for _, k := range kids {
		byName[k.Name] = k
	}
	if !byName["repoA"].Dirty {
		t.Errorf("repoA dir node should be Dirty, got %+v", byName["repoA"])
	}
	if byName["repoB"].Dirty {
		t.Errorf("repoB dir node should be clean, got %+v", byName["repoB"])
	}
}

func TestGitLogBranchesShow(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	initTestRepo(t, dir, map[string]string{"f.go": "v1\n"}, "first")
	os.WriteFile(filepath.Join(dir, "f.go"), []byte("v2\n"), 0o644)
	cmd := exec.Command("git", "commit", "-am", "second")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}

	commits, err := gitLog(dir, 10, 0)
	if err != nil || len(commits) != 2 {
		t.Fatalf("log = %v err %v, want 2", commits, err)
	}
	if commits[0].Subject != "second" || commits[1].Subject != "first" {
		t.Fatalf("subjects = %q %q", commits[0].Subject, commits[1].Subject)
	}
	if len(commits[0].SHA) != 40 || len(commits[0].Short) < 4 {
		t.Fatalf("sha fields = %+v", commits[0])
	}

	cur, branches, err := gitListBranches(dir)
	if err != nil || len(branches) == 0 {
		t.Fatalf("branches = %v err %v", branches, err)
	}
	foundCur := false
	for _, b := range branches {
		if b == cur {
			foundCur = true
		}
	}
	if !foundCur {
		t.Fatalf("current %q not in %v", cur, branches)
	}

	files, err := gitCommitFiles(dir, commits[0].SHA)
	if err != nil || len(files) != 1 || files[0].Path != "f.go" {
		t.Fatalf("commit files = %v err %v", files, err)
	}
	full, err := gitShowFull(dir, commits[0].SHA)
	if err != nil || !strings.Contains(full, "+v2") {
		t.Fatalf("show full missing +v2: %v err %v", full, err)
	}
	single, err := gitShowFile(dir, commits[0].SHA, "f.go")
	if err != nil || !strings.Contains(single, "+v2") {
		t.Fatalf("show file missing +v2: %v err %v", single, err)
	}
	// Pagination.
	page2, err := gitLog(dir, 1, 1)
	if err != nil || len(page2) != 1 || page2[0].Subject != "first" {
		t.Fatalf("page2 = %v err %v", page2, err)
	}
}

func TestGitCreateBranch(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	initTestRepo(t, dir, map[string]string{"f.go": "v1\n"}, "init")
	before := gitCurrentBranch(dir)
	if err := gitCreateBranch(dir, "feature/x", "", true); err != nil {
		t.Fatalf("create+checkout: %v", err)
	}
	if cur := gitCurrentBranch(dir); cur != "feature/x" {
		t.Fatalf("current = %q, want feature/x (was %q)", cur, before)
	}
	if err := gitCreateBranch(dir, "no-checkout", "", false); err != nil {
		t.Fatalf("create without checkout: %v", err)
	}
	if cur := gitCurrentBranch(dir); cur != "feature/x" {
		t.Fatalf("current moved to %q after no-checkout create", cur)
	}
	if err := gitCreateBranch(dir, "bad name!", "", false); err == nil {
		t.Fatal("expected error for invalid branch name")
	}
	if err := gitCreateBranch(dir, "feature/x", "", false); err == nil {
		t.Fatal("expected error for duplicate branch")
	}
}

func TestGitHistoryAPI(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	os.MkdirAll(filepath.Join(ws, "repoA"), 0o755)
	initTestRepo(t, filepath.Join(ws, "repoA"), map[string]string{"a.go": "v1\n"}, "init A")
	os.WriteFile(filepath.Join(ws, "repoA", "a.go"), []byte("v2\n"), 0o644)
	cmd := exec.Command("git", "commit", "-am", "second A")
	cmd.Dir = filepath.Join(ws, "repoA")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}

	ix := NewIndex(ws)
	ix.Build()
	s := NewServer(ix, nil)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	getJSON := func(path string) map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s status %d", path, resp.StatusCode)
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	reposBody := getJSON("/api/git/repos")
	repos, _ := reposBody["repos"].([]any)
	if len(repos) != 1 {
		t.Fatalf("repos = %v", reposBody)
	}
	logBody := getJSON("/api/git/log?repo=repoA&limit=10")
	commits, _ := logBody["commits"].([]any)
	if len(commits) != 2 {
		t.Fatalf("log commits = %v", logBody)
	}
	sha := commits[0].(map[string]any)["sha"].(string)
	showBody := getJSON("/api/git/show?repo=repoA&sha=" + sha)
	if showBody["diff"] == "" {
		t.Fatalf("show diff empty: %v", showBody)
	}
	files, _ := showBody["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("show files = %v", showBody)
	}
	diffBody := getJSON("/api/git/commit-diff?path=repoA/a.go&sha=" + sha)
	if diffBody["available"] != true || !strings.Contains(diffBody["diff"].(string), "+v2") {
		t.Fatalf("commit-diff = %v", diffBody)
	}
	// /api/diff with sha reuses the file overlay for commit patches.
	diff2 := getJSON("/api/diff?path=repoA/a.go&sha=" + sha)
	if diff2["available"] != true {
		t.Fatalf("diff with sha = %v", diff2)
	}
	// Meta carries repos for the sidebar without an extra fetch.
	meta := getJSON("/api/meta")
	if _, ok := meta["gitRepos"]; !ok {
		t.Fatalf("meta missing gitRepos: %v", meta)
	}

	// POST /api/git/branch requires localPost: POST from the page origin.
	branchURL := ts.URL + "/api/git/branch?repo=repoA&name=feature/api&checkout=1"
	req, _ := http.NewRequest("POST", branchURL, nil)
	req.Host = strings.TrimPrefix(ts.URL, "http://")
	req.Header.Set("Origin", ts.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	if resp.StatusCode != 200 || created["ok"] != true || created["current"] != "feature/api" {
		t.Fatalf("create branch via API = %d %v", resp.StatusCode, created)
	}
}
