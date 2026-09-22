package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// GitRepoInfo describes one repository served under the workspace root.
// Path is slash-separated relative to the served root; "" means the root
// itself is the repository.
type GitRepoInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// GitCommit is one row of `git log`.
type GitCommit struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Email   string `json:"email"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// GitCommitFile is one path touched by a commit.
type GitCommitFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

var (
	validSHA    = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)
	validRef    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/._-]*$`)
	validBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/._-]*$`)
)

// maxGitRepos bounds discovery (and every per-repo status/diff fan-out built
// on it). Workspaces with per-ticket worktrees can hold well over a hundred
// checkouts, so this must clear that comfortably while staying bounded.
const maxGitRepos = 300

// gitIsRepoDir reports whether abs contains a .git dir or file (plain repo,
// worktree, or submodule) without shelling out.
func gitIsRepoDir(abs string) bool {
	fi, err := os.Stat(filepath.Join(abs, ".git"))
	if err != nil {
		return false
	}
	return fi.IsDir() || fi.Mode().IsRegular()
}

// gitHasNestedRepos is the fast path for gitAvailable when the served root
// itself is not a repo: true when the recursive walk finds any repository
// nested at any depth below root.
func gitHasNestedRepos(root string) bool {
	if gitDisabled {
		return false
	}
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	return len(gitDiscoverRepos(root)) > 0
}

// gitDiscoverRepos lists repositories under root. When root itself is a repo
// it returns just that one (Path ""). Otherwise it walks the directory tree
// to any depth recording every directory that holds a .git entry, so a
// folder holding several checkouts at any nesting (workspace/group/.../repo)
// is served as a multi-repo workspace. Directories already identified as
// repositories are not descended into; symlinks, VCS internals and
// directories with more than 200 entries are skipped, and at most
// maxGitRepos repositories are returned.
func gitDiscoverRepos(root string) []GitRepoInfo {
	if gitDisabled {
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	if info := gitProbe(root); info.ok {
		branch := gitCurrentBranch(root)
		return []GitRepoInfo{{Name: filepath.Base(root), Path: "", Branch: branch}}
	}
	if _, err := os.ReadDir(root); err != nil {
		return nil
	}
	var out []GitRepoInfo
	seen := map[string]bool{}
	add := func(rel string) {
		rel = filepath.ToSlash(rel)
		if rel == "." {
			rel = ""
		}
		if seen[rel] {
			return
		}
		seen[rel] = true
		abs := root
		if rel != "" {
			abs = filepath.Join(root, filepath.FromSlash(rel))
		}
		if !gitIsRepoDir(abs) {
			return
		}
		// Verify with git so a stray .git file/dir that is not a repo is skipped.
		if out2, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output(); err != nil {
			return
		} else {
			_ = out2
		}
		name := filepath.Base(abs)
		if rel == "" {
			name = filepath.Base(root)
		}
		out = append(out, GitRepoInfo{Name: name, Path: rel, Branch: gitCurrentBranch(abs)})
		if len(out) >= maxGitRepos {
			return
		}
	}
	// Breadth-first walk with an explicit queue; "" is root itself. Shallow
	// checkouts are recorded before deeper ones, so if the cap ever binds it
	// drops the deepest worktrees first, never the primary checkouts. A
	// directory that holds a repo is recorded as a leaf and never descended
	// into, so the walk stops at repo boundaries instead of sweeping whole
	// checkouts.
	queue := []string{""}
	visited := 0
	for len(queue) > 0 && len(out) < maxGitRepos {
		rel := queue[0]
		queue = queue[1:]
		abs := root
		if rel != "" {
			abs = filepath.Join(root, filepath.FromSlash(rel))
		}
		ents, err := os.ReadDir(abs)
		if err != nil {
			continue
		}
		if rel != "" && len(ents) > 200 {
			continue
		}
		visited++
		if visited > 5000 {
			break
		}
		for _, e := range ents {
			if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				continue
			}
			name := e.Name()
			if vcsDirs[name] {
				continue
			}
			child := name
			if rel != "" {
				child = rel + "/" + name
			}
			if gitIsRepoDir(filepath.Join(root, filepath.FromSlash(child))) {
				add(child)
				continue
			}
			queue = append(queue, child)
		}
	}
	// Worktree pass: a `git worktree add` checkout nested inside another
	// repository is invisible to the walk above (repos are leaves), so ask
	// every discovered repo for its registered worktrees. Entries outside
	// the served root cannot be browsed and are skipped; the rest go through
	// add(), which deduplicates against the walk results, verifies with git
	// (stale and bare entries fall out there: neither holds a .git entry),
	// and enforces the cap.
	seed := append([]GitRepoInfo(nil), out...)
	for _, r := range seed {
		if len(out) >= maxGitRepos {
			break
		}
		repoAbs := root
		if r.Path != "" {
			repoAbs = filepath.Join(root, filepath.FromSlash(r.Path))
		}
		for _, wt := range gitWorktreePaths(repoAbs) {
			rel, err := filepath.Rel(root, wt)
			if err != nil || rel == "." || rel == ".." ||
				strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
				filepath.IsAbs(rel) {
				continue
			}
			add(filepath.ToSlash(rel))
			if len(out) >= maxGitRepos {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// gitWorktreePaths returns the absolute paths of every worktree registered
// for the repository at repoAbs, main checkout first. Empty when git is
// missing or the directory is not a repository.
func gitWorktreePaths(repoAbs string) []string {
	out, err := exec.Command("git", "-C", repoAbs, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, block := range strings.Split(string(out), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				if p = strings.TrimSpace(p); p != "" {
					paths = append(paths, p)
				}
				break // one path per block; the rest is HEAD/branch state
			}
		}
	}
	return paths
}

// gitRepoAbs validates a client-supplied repo selector ("" for the root repo,
// otherwise a slash-separated path under root) and returns its absolute path.
func gitRepoAbs(root, repoRel string) (string, bool) {
	repoRel = strings.Trim(strings.TrimSpace(repoRel), "/")
	if repoRel == "" || repoRel == "." {
		if gitProbe(root).ok {
			return root, true
		}
		// Workspace root itself is not a repo: no default repo.
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(repoRel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", false
	}
	abs := filepath.Join(root, clean)
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", false
	}
	if !gitIsRepoDir(abs) {
		return "", false
	}
	if _, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output(); err != nil {
		return "", false
	}
	return abs, true
}

// gitResolveRepo finds which served repo contains workspace-relative rel
// (slash-separated). Returns the repo's selector, its absolute path, and the
// path inside that repo. When root itself is a repo the selector is "".
func gitResolveRepo(root, rel string) (repoRel, repoAbs, relInside string) {
	rel = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(rel)), "/")
	if info := gitProbe(root); info.ok {
		return "", root, rel
	}
	repos := gitDiscoverRepos(root)
	best := ""
	bestAbs := ""
	for _, r := range repos {
		if r.Path == "" {
			continue
		}
		if rel == r.Path || strings.HasPrefix(rel, r.Path+"/") {
			if len(r.Path) > len(best) {
				best = r.Path
				bestAbs, _ = gitRepoAbs(root, r.Path)
			}
		}
	}
	if bestAbs == "" {
		return "", "", ""
	}
	inside := strings.TrimPrefix(strings.TrimPrefix(rel[len(best):], "/"), "./")
	return best, bestAbs, inside
}

// gitStatusMulti aggregates `git status` across every nested repo when the
// served root itself is not a repository. Keys are workspace-relative paths
// (repo prefix + path inside the repo) so the index tree can badge them.
func gitStatusMulti(root, base string) map[string]string {
	repos := gitDiscoverRepos(root)
	if len(repos) == 0 {
		return nil
	}
	status := map[string]string{}
	for _, r := range repos {
		abs, ok := gitRepoAbs(root, r.Path)
		if !ok {
			continue
		}
		single := gitStatusSingle(abs, base)
		if len(single) == 0 {
			continue
		}
		prefix := ""
		if r.Path != "" {
			prefix = r.Path + "/"
		}
		for p, code := range single {
			status[prefix+p] = code
		}
	}
	if len(status) == 0 {
		return nil
	}
	return status
}

// gitStatusSingle is the single-repository status scan used both directly and
// as the per-repo unit of gitStatusMulti. repoAbs must be a repository root.
func gitStatusSingle(repoAbs, base string) map[string]string {
	out, err := exec.Command("git", "-C", repoAbs, "status", "--porcelain=v2", "-z", "-uall").Output()
	if err != nil {
		return nil
	}
	status := map[string]string{}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		switch f[0] {
		case '?':
			status[f[2:]] = "U"
		case '1':
			if p := strings.SplitN(f, " ", 9); len(p) == 9 {
				status[p[8]] = mapXY(p[1])
			}
		case '2':
			if p := strings.SplitN(f, " ", 10); len(p) == 10 {
				status[p[9]] = mapXY(p[1])
			}
			i++
		case 'u':
			if p := strings.SplitN(f, " ", 11); len(p) == 11 {
				status[p[10]] = "!"
			}
		}
	}
	if base != "" && base != "HEAD" {
		if diffOut, err := exec.Command("git", "-C", repoAbs, "diff", "--name-status", "-z", base).Output(); err == nil {
			parts := strings.Split(string(diffOut), "\x00")
			for i := 0; i < len(parts); i++ {
				stStr := parts[i]
				if stStr == "" {
					continue
				}
				code := stStr[0]
				if code == 'R' || code == 'C' {
					i += 2
					if i < len(parts) {
						if _, exists := status[parts[i]]; !exists {
							status[parts[i]] = string(code)
						}
					}
				} else {
					i++
					if i < len(parts) {
						if _, exists := status[parts[i]]; !exists {
							status[parts[i]] = string(code)
						}
					}
				}
			}
		}
	}
	if len(status) == 0 {
		return nil
	}
	return status
}

// gitCurrentBranch returns the current branch name, "HEAD" when detached, or
// "" when it cannot be determined.
func gitCurrentBranch(repoAbs string) string {
	if gitDisabled || repoAbs == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", repoAbs, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitListBranches returns the current branch plus all local branch names.
func gitListBranches(repoAbs string) (string, []string, error) {
	cur := gitCurrentBranch(repoAbs)
	out, err := exec.Command("git", "-C", repoAbs, "branch", "--format=%(refname:short)").Output()
	if err != nil {
		return cur, nil, err
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		b := strings.TrimSpace(line)
		if b != "" {
			branches = append(branches, b)
		}
	}
	sort.Strings(branches)
	return cur, branches, nil
}

// gitCreateBranch creates (and optionally checks out) a new branch.
func gitCreateBranch(repoAbs, name, startPoint string, checkout bool) error {
	name = strings.TrimSpace(name)
	if !validBranch.MatchString(name) {
		return &gitUserError{"invalid branch name"}
	}
	if out, err := exec.Command("git", "-C", repoAbs, "check-ref-format", "--branch", name).CombinedOutput(); err != nil {
		_ = out
		return &gitUserError{"invalid branch name"}
	}
	if startPoint == "" {
		startPoint = "HEAD"
	} else {
		startPoint = strings.TrimSpace(startPoint)
		if !validSHA.MatchString(startPoint) && !validRef.MatchString(startPoint) && startPoint != "HEAD" {
			return &gitUserError{"invalid start point"}
		}
	}
	var cmd *exec.Cmd
	if checkout {
		if startPoint == "HEAD" {
			cmd = exec.Command("git", "-C", repoAbs, "checkout", "-b", name)
		} else {
			cmd = exec.Command("git", "-C", repoAbs, "checkout", "-b", name, startPoint)
		}
	} else {
		if startPoint == "HEAD" {
			cmd = exec.Command("git", "-C", repoAbs, "branch", name)
		} else {
			cmd = exec.Command("git", "-C", repoAbs, "branch", name, startPoint)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		// Surface git's own message (e.g. "branch already exists") as-is.
		return &gitUserError{msg}
	}
	// A checkout moves HEAD, so drop the cached probe for this repo root.
	gitMu.Lock()
	for k := range gitCache {
		if k == repoAbs {
			delete(gitCache, k)
		}
	}
	gitMu.Unlock()
	return nil
}

type gitUserError struct{ msg string }

func (e *gitUserError) Error() string { return e.msg }

// gitLog lists recent commits newest-first.
func gitLog(repoAbs string, limit, skip int) ([]GitCommit, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if skip < 0 {
		skip = 0
	}
	args := []string{"-C", repoAbs, "log",
		"--format=%H%x1f%h%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1e",
		"--skip=" + strconv.Itoa(skip), "-n" + strconv.Itoa(limit),
	}
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		// Empty repo (no commits yet) reports an error; treat as empty log.
		if msg := strings.ToLower(err.Error()); strings.Contains(msg, "exit status") {
			return []GitCommit{}, nil
		}
		return nil, err
	}
	var commits []GitCommit
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.Trim(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		parts := strings.SplitN(rec, "\x1f", 7)
		if len(parts) < 6 {
			continue
		}
		body := ""
		if len(parts) == 7 {
			body = strings.TrimSpace(parts[6])
		}
		commits = append(commits, GitCommit{
			SHA:     strings.TrimSpace(parts[0]),
			Short:   strings.TrimSpace(parts[1]),
			Author:  strings.TrimSpace(parts[2]),
			Email:   strings.TrimSpace(parts[3]),
			Date:    strings.TrimSpace(parts[4]),
			Subject: strings.TrimSpace(parts[5]),
			Body:    body,
		})
	}
	if commits == nil {
		commits = []GitCommit{}
	}
	return commits, nil
}

// gitCommitFiles lists paths touched by sha (works for root commits via --root).
func gitCommitFiles(repoAbs, sha string) ([]GitCommitFile, error) {
	if !validSHA.MatchString(sha) {
		return nil, &gitUserError{"invalid commit"}
	}
	out, err := exec.Command("git", "-C", repoAbs, "diff-tree", "--no-commit-id", "--name-status", "-r", "--root", "-z", sha).Output()
	if err != nil {
		return nil, err
	}
	var files []GitCommitFile
	parts := strings.Split(string(out), "\x00")
	for i := 0; i < len(parts); i++ {
		st := parts[i]
		if st == "" {
			continue
		}
		code := st[0]
		if code == 'R' || code == 'C' {
			i += 2
			if i < len(parts) && parts[i] != "" {
				files = append(files, GitCommitFile{Path: parts[i], Status: string(code)})
			}
		} else {
			i++
			if i < len(parts) && parts[i] != "" {
				files = append(files, GitCommitFile{Path: parts[i], Status: string(code)})
			}
		}
	}
	if files == nil {
		files = []GitCommitFile{}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// gitShowFull returns the full `git show` output (message + patch) for sha.
func gitShowFull(repoAbs, sha string) (string, error) {
	if !validSHA.MatchString(sha) {
		return "", &gitUserError{"invalid commit"}
	}
	out, err := exec.Command("git", "-C", repoAbs, "show", "--no-color", sha).Output()
	if err != nil {
		return "", err
	}
	const maxBytes = 512 << 10
	if len(out) > maxBytes {
		out = append(out[:maxBytes], []byte("\n... [truncated]")...)
	}
	return string(out), nil
}

// gitShowFile returns the diff of one file inside sha (no commit message).
func gitShowFile(repoAbs, sha, relInside string) (string, error) {
	if !validSHA.MatchString(sha) {
		return "", &gitUserError{"invalid commit"}
	}
	relInside = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(relInside)), "/")
	if relInside == "" || relInside == "." {
		return "", &gitUserError{"bad path"}
	}
	clean := filepath.Clean(filepath.FromSlash(relInside))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", &gitUserError{"bad path"}
	}
	out, err := exec.Command("git", "-C", repoAbs, "show", "--no-color", "--format=", sha, "--", clean).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
