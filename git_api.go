package main

import (
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) handleGitRepos(w http.ResponseWriter, r *http.Request) {
	repos := gitDiscoverRepos(s.ix.Root())
	if repos == nil {
		repos = []GitRepoInfo{}
	}
	rootIsRepo := gitProbe(s.ix.Root()).ok
	writeJSON(w, map[string]any{"repos": repos, "rootIsRepo": rootIsRepo})
}

func (s *Server) gitRepoFromRequest(r *http.Request) (string, string, bool) {
	root := s.ix.Root()
	q := r.URL.Query()
	repoParam := q.Get("repo")
	// Default: when root is a repo, "" selects it. In a multi-repo workspace
	// with no explicit repo, pick the first repo so the history view has content.
	if repoParam == "" && gitProbe(root).ok {
		return root, "", true
	}
	if repoParam == "" {
		repos := gitDiscoverRepos(root)
		if len(repos) == 0 {
			return "", "", false
		}
		abs, ok := gitRepoAbs(root, repos[0].Path)
		if !ok {
			return "", "", false
		}
		return abs, repos[0].Path, true
	}
	abs, ok := gitRepoAbs(root, repoParam)
	if !ok {
		return "", "", false
	}
	// Normalize the selector for responses.
	sel := strings.Trim(strings.TrimSpace(repoParam), "/")
	if sel == "." {
		sel = ""
	}
	return abs, sel, true
}

func (s *Server) handleGitBranches(w http.ResponseWriter, r *http.Request) {
	repoAbs, sel, ok := s.gitRepoFromRequest(r)
	if !ok {
		fail(w, 400, "unknown repo")
		return
	}
	cur, branches, err := gitListBranches(repoAbs)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if branches == nil {
		branches = []string{}
	}
	writeJSON(w, map[string]any{"repo": sel, "current": cur, "branches": branches})
}

func (s *Server) handleGitCreateBranch(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	repoAbs, sel, ok := s.gitRepoFromRequest(r)
	if !ok {
		fail(w, 400, "unknown repo")
		return
	}
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		fail(w, 400, "branch name is required")
		return
	}
	checkout := true
	if v := strings.TrimSpace(q.Get("checkout")); v == "0" || strings.EqualFold(v, "false") {
		checkout = false
	}
	from := strings.TrimSpace(q.Get("from"))
	if err := gitCreateBranch(repoAbs, name, from, checkout); err != nil {
		if _, isUser := err.(*gitUserError); isUser {
			fail(w, 400, err.Error())
			return
		}
		fail(w, 500, err.Error())
		return
	}
	if s.gitWatcher != nil {
		s.gitWatcher.Trigger()
	}
	// Refresh the index's git status so badges update without waiting for the watcher.
	s.ix.UpdateGitStatus()
	cur, branches, _ := gitListBranches(repoAbs)
	if branches == nil {
		branches = []string{}
	}
	writeJSON(w, map[string]any{"ok": true, "repo": sel, "branch": name, "checkedOut": checkout, "current": cur, "branches": branches})
}

func (s *Server) handleGitLog(w http.ResponseWriter, r *http.Request) {
	repoAbs, sel, ok := s.gitRepoFromRequest(r)
	if !ok {
		fail(w, 400, "unknown repo")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	skip, _ := strconv.Atoi(q.Get("skip"))
	if skip < 0 {
		skip = 0
	}
	commits, err := gitLog(repoAbs, limit, skip)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"repo": sel, "commits": commits, "limit": limit, "skip": skip, "hasMore": len(commits) == limit})
}

func (s *Server) handleGitShow(w http.ResponseWriter, r *http.Request) {
	repoAbs, sel, ok := s.gitRepoFromRequest(r)
	if !ok {
		fail(w, 400, "unknown repo")
		return
	}
	sha := strings.TrimSpace(r.URL.Query().Get("sha"))
	if sha == "" {
		fail(w, 400, "sha is required")
		return
	}
	files, err := gitCommitFiles(repoAbs, sha)
	if err != nil {
		if _, isUser := err.(*gitUserError); isUser {
			fail(w, 400, err.Error())
			return
		}
		fail(w, 500, err.Error())
		return
	}
	diff, err := gitShowFull(repoAbs, sha)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// Commit subject for the header without a second log call: first diff line
	// after "Date:" is not reliable, so fetch it cheaply from the log cache
	// path only when needed by the UI (it already has the subject from /log).
	writeJSON(w, map[string]any{"repo": sel, "sha": sha, "files": files, "diff": diff})
}

func (s *Server) handleGitCommitDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sha := strings.TrimSpace(q.Get("sha"))
	rawPath := strings.TrimSpace(q.Get("path"))
	if sha == "" || rawPath == "" {
		fail(w, 400, "sha and path are required")
		return
	}
	if !validSHA.MatchString(sha) {
		fail(w, 400, "invalid commit")
		return
	}
	_, rel, ok := s.safePath(rawPath)
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	// path is workspace-relative (as in the tree); resolve its repo.
	_, repoAbs, relInside := gitResolveRepo(s.ix.Root(), rel)
	if repoAbs == "" {
		fail(w, 400, "not in a git repo")
		return
	}
	if relInside == "" {
		fail(w, 400, "bad path")
		return
	}
	diff, err := gitShowFile(repoAbs, sha, relInside)
	if err != nil {
		if _, isUser := err.(*gitUserError); isUser {
			fail(w, 400, err.Error())
			return
		}
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": rel, "sha": sha, "diff": diff, "available": diff != ""})
}
