// web/src/githistory.js
// Git history sidebar: multi-repo workspace support, branch creation, commit
// list, and per-commit diff viewing. Selecting a commit shows its files and
// full patch in the main-area commit overlay; file rows open the working-tree
// file for context.
import { $, S, api, apiPost, esc } from './state.js';
import { layoutPref, parseDiff, unifiedTable, splitTable, diffHunkHeader } from './diff.js';
import { showToast } from './ui.js';
import { openFile } from './tabs.js';

const GH = {
  repos: [],
  repo: '',
  branches: [],
  current: '',
  commits: [],
  skip: 0,
  limit: 100,
  hasMore: false,
  selected: null,
  detail: null,
  diffMode: null,
  loading: false,
};

function wsPath(repoSel, repoRel) {
  if (!repoSel) return repoRel;
  if (!repoRel) return repoSel;
  return repoSel + '/' + repoRel;
}

function shortDate(iso) {
  try {
    const d = new Date(iso);
    if (isNaN(d)) return iso || '';
    const now = Date.now();
    const diff = now - d.getTime();
    if (diff < 60 * 1000) return 'just now';
    if (diff < 60 * 60 * 1000) return Math.floor(diff / 60000) + 'm ago';
    if (diff < 24 * 60 * 60 * 1000) return Math.floor(diff / 3600000) + 'h ago';
    if (diff < 30 * 24 * 60 * 60 * 1000) return Math.floor(diff / 86400000) + 'd ago';
    return d.toLocaleDateString();
  } catch { return iso || ''; }
}

export function gitHistoryState() { return GH; }

export async function loadGitRepos() {
  const sel = $('#hist-repo');
  try {
    let repos = (S.meta && S.meta.gitRepos) || null;
    if (!repos) {
      const j = await api('/api/git/repos');
      repos = j.repos || [];
    }
    GH.repos = repos || [];
  } catch {
    GH.repos = [];
  }
  if (sel) {
    sel.innerHTML = GH.repos.map(r => {
      const label = r.path === '' ? (r.name + ' · /') : (r.name + ' · ' + esc(r.path));
      return '<option value="' + esc(r.path) + '">' + label + '</option>';
    }).join('') || '<option value="">No git repositories</option>';
    if (GH.repos.length > 0) {
      if (!GH.repos.some(r => r.path === GH.repo)) GH.repo = GH.repos[0].path;
      sel.value = GH.repo;
    }
    sel.disabled = GH.repos.length <= 1;
  }
  const empty = $('#hist-empty');
  if (empty) empty.hidden = GH.repos.length > 0;
  if (GH.repos.length > 0) {
    await Promise.all([loadGitBranches(), loadGitCommits(false)]);
  } else {
    GH.branches = []; GH.commits = []; GH.selected = null; GH.detail = null;
    renderGitCommits();
    hideCommitView();
  }
}

async function loadGitBranches() {
  const el = $('#hist-branch');
  try {
    const j = await api('/api/git/branches', { repo: GH.repo });
    GH.branches = j.branches || [];
    GH.current = j.current || '';
  } catch {
    GH.branches = []; GH.current = '';
  }
  if (el) el.textContent = GH.current ? ('⎇ ' + GH.current) : '—';
}

async function loadGitCommits(append = false) {
  const list = $('#hist-commits');
  if (!GH.repo && GH.repos.length && GH.repos[0].path !== undefined && !append) {
    // repo already defaulted in loadGitRepos
  }
  if (GH.loading) return;
  GH.loading = true;
  if (list && !append) list.innerHTML = '<div class="hint">Loading commits…</div>';
  try {
    const skip = append ? GH.commits.length : 0;
    const j = await api('/api/git/log', { repo: GH.repo, limit: GH.limit, skip });
    const commits = j.commits || [];
    if (append) GH.commits = GH.commits.concat(commits);
    else { GH.commits = commits; GH.selected = null; GH.detail = null; hideCommitView(); }
    GH.hasMore = !!j.hasMore;
    GH.skip = skip;
  } catch (e) {
    if (!append) GH.commits = [];
    if (list && !append) list.innerHTML = '<div class="hint">Failed to load commits: ' + esc(e.message) + '</div>';
    GH.loading = false;
    return;
  }
  GH.loading = false;
  renderGitCommits();
}

function renderGitCommits() {
  const list = $('#hist-commits');
  if (!list) return;
  if (!GH.commits.length) {
    list.innerHTML = '<div class="hint">No commits yet.</div>';
    const more = $('#hist-more');
    if (more) more.hidden = true;
    return;
  }
  list.innerHTML = GH.commits.map(c =>
    '<div class="hcommit' + (GH.selected === c.sha ? ' sel' : '') + '" data-sha="' + esc(c.sha) + '" title="' + esc(c.subject) + '">' +
    '<span class="hsha">' + esc(c.short) + '</span>' +
    '<span class="hsub">' + esc(c.subject) + '</span>' +
    '<span class="hmeta">' + esc(c.author) + ' · ' + esc(shortDate(c.date)) + '</span>' +
    '</div>').join('');
  const more = $('#hist-more');
  if (more) more.hidden = !GH.hasMore;
}

function splitCommitDiffByFile(diffText) {
  if (!diffText) return [];
  const lines = diffText.split('\n');
  const sections = [];
  let cur = null;
  for (const line of lines) {
    if (line.startsWith('diff --git ')) {
      if (cur) sections.push(cur);
      cur = { header: line, lines: [line], file: '' };
      const m = /^diff --git a\/(.*) b\/(.*)$/.exec(line);
      if (m) cur.file = m[2] || m[1];
      continue;
    }
    if (cur) cur.lines.push(line);
  }
  if (cur) sections.push(cur);
  // Fallback: no diff --git headers (e.g. truncated); treat whole as one section.
  if (!sections.length && diffText.trim()) return [{ header: '', lines, file: '', text: diffText }];
  for (const s of sections) {
    s.text = s.lines.join('\n');
    if (!s.file) {
      for (const l of s.lines) {
        if (l.startsWith('+++ b/')) { s.file = l.slice(6); break; }
        if (l.startsWith('+++ ')) { s.file = l.slice(4).replace(/^b\//, ''); break; }
      }
    }
  }
  return sections;
}

async function selectGitCommit(sha) {
  GH.selected = sha;
  openCommitSections.clear();
  renderGitCommits();
  const detail = $('#hist-detail');
  if (detail) detail.innerHTML = '<div class="hint">Loading diff…</div>';
  showCommitViewLoading(sha);
  try {
    const j = await api('/api/git/show', { repo: GH.repo, sha });
    GH.detail = j;
  } catch (e) {
    GH.detail = null;
    if (detail) detail.innerHTML = '<div class="hint">Failed to load diff: ' + esc(e.message) + '</div>';
    const cc = $('#commitcontent');
    if (cc) cc.innerHTML = '<div class="diff-empty">Failed to load diff: ' + esc(e.message) + '</div>';
    return;
  }
  try {
    renderCommitDetail();
  } catch (e) {
    console.error('renderCommitDetail failed', e);
    if (detail) detail.innerHTML = '<div class="hint">Failed to load diff: ' + esc(e.message) + '</div>';
  }
  try {
    renderCommitView();
  } catch (e) {
    console.error('renderCommitView failed', e);
    const cc = $('#commitcontent');
    if (cc) cc.innerHTML = '<div class="diff-empty">Failed to load diff: ' + esc(e.message) + '</div>';
  }
}

function renderCommitDetail() {
  const detail = $('#hist-detail');
  if (!detail) return;
  const d = GH.detail;
  if (!d || d.sha !== GH.selected) return;
  const c = GH.commits.find(x => x.sha === d.sha);
  const files = d.files || [];
  detail.innerHTML =
    '<div class="hdetail-head">' +
    '<span class="hsha">' + esc((c && c.short) || d.sha.slice(0, 7)) + '</span>' +
    '<span class="hsub">' + esc((c && c.subject) || '') + '</span>' +
    (c ? '<span class="hmeta">' + esc(c.author) + ' · ' + esc(shortDate(c.date)) + '</span>' : '') +
    '</div>' +
    '<div class="hdetail-files">' + (files.length ? files.map(f => {
      const fp = f.path ?? f.Path ?? '';
      const st = f.status ?? f.Status ?? '';
      return '<div class="hfile" data-path="' + esc(fp) + '" title="Open ' + esc(fp) + '">' +
      '<span class="hstatus hst-' + esc(st) + '">' + esc(st) + '</span>' +
      '<span class="hfname">' + esc(fp) + '</span>' +
      '</div>'; }).join('') : '<div class="hint">No files changed.</div>') + '</div>';
}

function commitDiffMode() {
  return GH.diffMode || layoutPref() || 'split';
}

// File sections expanded in the commit view, by section index. Reset on
// every new commit selection; a split/unified switch re-renders with the
// same set open.
let openCommitSections = new Set();

function renderCommitSectionBody(body, s, mode) {
  body.replaceChildren();
  const hunks = parseDiff(s.text);
  if (!hunks.length) {
    const p = document.createElement('div');
    p.className = 'diff-empty';
    p.textContent = 'No textual changes in this file.';
    body.append(p);
    return;
  }
  for (const h of hunks) {
    body.append(diffHunkHeader(h));
    body.append(mode === 'unified' ? unifiedTable(h) : splitTable(h));
  }
}

function renderCommitView() {
  const view = $('#commitview');
  const cc = $('#commitcontent');
  if (!view || !cc) return;
  const d = GH.detail;
  if (!d || d.sha !== GH.selected) return;
  const c = GH.commits.find(x => x.sha === d.sha);
  const mode = commitDiffMode();
  cc.replaceChildren();
  const head = document.createElement('div');
  head.className = 'commit-head';
  head.innerHTML =
    '<button id="commit-close" class="mini" title="Close commit view (Esc)">✕</button>' +
    '<span class="hsha">' + esc((c && c.short) || d.sha.slice(0, 7)) + '</span>' +
    '<span class="commit-sub">' + esc((c && c.subject) || d.sha) + '</span>' +
    '<span class="grow"></span>' +
    '<span class="commit-toggle" role="group" aria-label="Diff layout">' +
    '<button data-cm="split" class="' + (mode === 'split' ? 'on' : '') + '">Split</button>' +
    '<button data-cm="unified" class="' + (mode === 'unified' ? 'on' : '') + '">Unified</button>' +
    '</span>';
  cc.append(head);
  const meta = document.createElement('div');
  meta.className = 'commit-meta';
  meta.textContent = (c ? (c.author + ' · ' + shortDate(c.date) + ' · ') : '') + d.sha;
  cc.append(meta);
  const sections = splitCommitDiffByFile(d.diff || '');
  if (!sections.length) {
    const p = document.createElement('div');
    p.className = 'diff-empty';
    p.textContent = 'No textual changes in this commit.';
    cc.append(p);
  }
  // Collapsed by default: only file headers render. A file's hunks parse
  // and render on its first expand, never eagerly for the whole commit.
  sections.forEach((s, i) => {
    const fhead = document.createElement('div');
    const open = openCommitSections.has(i);
    fhead.className = 'commit-file-head' + (open ? ' open' : '');
    fhead.dataset.file = s.file || '';
    fhead.innerHTML = '<span class="commit-chev">›</span>' +
      '<span class="commit-file-name">' + esc(s.file || '(unknown)') + '</span>' +
      '<button class="mini commit-open" data-path="' + esc(s.file || '') + '" title="Open working-tree file">Open file</button>';
    cc.append(fhead);
    const body = document.createElement('div');
    body.className = 'commit-file-body';
    body.hidden = !open;
    if (open) renderCommitSectionBody(body, s, mode);
    cc.append(body);
    fhead.addEventListener('click', () => {
      const nowOpen = body.hidden;
      body.hidden = !nowOpen;
      fhead.classList.toggle('open', nowOpen);
      if (nowOpen) {
        openCommitSections.add(i);
        renderCommitSectionBody(body, s, commitDiffMode());
      } else {
        openCommitSections.delete(i);
      }
    });
  });
  view.hidden = false;
  $('#commit-close')?.addEventListener('click', hideCommitView);
  head.querySelectorAll('[data-cm]').forEach(b => b.addEventListener('click', () => {
    GH.diffMode = b.dataset.cm;
    renderCommitView();
  }));
  cc.querySelectorAll('.commit-open').forEach(b => b.addEventListener('click', e => {
    e.stopPropagation();
    const p = b.dataset.path;
    if (p) openFile(wsPath(GH.repo, p));
  }));
}

function showCommitViewLoading(sha) {
  const view = $('#commitview');
  const cc = $('#commitcontent');
  if (!view || !cc) return;
  cc.innerHTML = '<div class="commit-head"><span class="hsha">' + esc(sha.slice(0, 7)) + '</span><span class="commit-sub">Loading…</span></div><div class="hint">Loading diff…</div>';
  view.hidden = false;
}

export function hideCommitView() {
  const view = $('#commitview');
  if (view) view.hidden = true;
}

export function commitViewOpen() {
  const view = $('#commitview');
  return !!(view && !view.hidden);
}

async function createGitBranch() {
  const input = $('#hist-new-name');
  const checkoutEl = $('#hist-new-checkout');
  const name = (input?.value || '').trim();
  if (!name) { showToast('!', 'Enter a branch name'); return; }
  const checkout = checkoutEl ? checkoutEl.checked !== false : true;
  try {
    const j = await apiPost('/api/git/branch', { repo: GH.repo, name, checkout: checkout ? '1' : '0' });
    GH.current = j.current || name;
    await loadGitBranches();
    hideNewBranchForm();
    if (input) input.value = '';
    showToast('✓', 'Branch ' + name + (j.checkedOut ? ' created & checked out' : ' created'));
  } catch (e) {
    showToast('!', e.message || 'Failed to create branch');
  }
}

function showNewBranchForm() {
  const f = $('#hist-new-form');
  if (f) f.hidden = false;
  const input = $('#hist-new-name');
  if (input) { input.focus(); input.select(); }
  const btn = $('#hist-new-branch');
  if (btn) btn.hidden = true;
}

function hideNewBranchForm() {
  const f = $('#hist-new-form');
  if (f) f.hidden = true;
  const btn = $('#hist-new-branch');
  if (btn) btn.hidden = false;
}

export function initGitHistory() {
  $('#hist-repo')?.addEventListener('change', async e => {
    GH.repo = e.target.value || '';
    GH.commits = []; GH.selected = null; GH.detail = null;
    await Promise.all([loadGitBranches(), loadGitCommits(false)]);
  });
  $('#hist-new-branch')?.addEventListener('click', showNewBranchForm);
  $('#hist-new-cancel')?.addEventListener('click', hideNewBranchForm);
  $('#hist-new-create')?.addEventListener('click', createGitBranch);
  $('#hist-new-name')?.addEventListener('keydown', e => {
    if (e.key === 'Enter') { e.preventDefault(); createGitBranch(); }
    else if (e.key === 'Escape') { e.preventDefault(); hideNewBranchForm(); }
  });
  $('#hist-commits')?.addEventListener('click', e => {
    const row = e.target.closest('.hcommit');
    if (row && row.dataset.sha) selectGitCommit(row.dataset.sha);
  });
  $('#hist-more')?.addEventListener('click', () => loadGitCommits(true));
  $('#hist-detail')?.addEventListener('click', e => {
    const f = e.target.closest('.hfile');
    if (f && f.dataset.path) openFile(wsPath(GH.repo, f.dataset.path));
  });
  $('#tabs')?.addEventListener('click', () => {
    // Returning to a file tab dismisses the commit overlay.
    if (commitViewOpen()) hideCommitView();
  });
  addEventListener('keydown', e => {
    if (e.key === 'Escape' && commitViewOpen()) {
      const nameInput = $('#hist-new-name');
      if (nameInput && document.activeElement === nameInput) return;
      const ae = document.activeElement;
      if (ae && (ae.tagName === 'INPUT' || ae.tagName === 'TEXTAREA')) return;
      hideCommitView();
    }
  });
}
