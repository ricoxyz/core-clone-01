import { createHash } from 'node:crypto';
import {
  type GitObject,
  type GitObjectType,
  buildCommit,
  buildTree,
  hashObject,
  isObjectId,
  parseCommit,
  parseTree,
  ZERO_OID,
} from '../git/git-engine.ts';

export interface StoredRepo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdAt: string;
  updatedAt: string;
  refs: Map<string, string>;
  objects: Map<string, GitObject>;
  labels: Map<string, { id: number; name: string; color: string; description: string }>;
  labelCounter: number;
  issues: Map<number, StoredIssue>;
  issueCounter: number;
  comments: Map<number, StoredComment>;
  commentCounter: number;
  pulls: Map<number, StoredPull>;
  pullCounter: number;
}

export interface StoredIssue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  createdAt: string;
  updatedAt: string;
  closedAt?: string;
  comments: number;
}

export interface StoredComment {
  id: number;
  issueNumber: number;
  body: string;
  createdAt: string;
  updatedAt: string;
}

export interface StoredPull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: { ref: string; sha: string };
  base: { ref: string; sha: string };
  merged: boolean;
  mergedAt?: string;
  mergeCommitSha?: string;
  createdAt: string;
  updatedAt: string;
  files: Array<{
    filename: string;
    status: string;
    additions: number;
    deletions: number;
    changes: number;
  }>;
}

export class ForgeStore {
  private repos = new Map<string, StoredRepo>();
  private repoCounter = 0;
  private defaultOwner = 'adaiversen';

  setDefaultOwner(owner: string) {
    this.defaultOwner = owner;
  }

  getDefaultOwner(): string {
    return this.defaultOwner;
  }

  repoKey(owner: string, name: string): string {
    return `${owner.toLowerCase()}/${name.toLowerCase()}`;
  }

  createRepo(owner: string, name: string, description = '', isPrivate = false, defaultBranch = 'main'): StoredRepo {
    const key = this.repoKey(owner, name);
    if (this.repos.has(key)) {
      return this.repos.get(key)!;
    }
    this.repoCounter += 1;
    const now = new Date().toISOString();
    const repo: StoredRepo = {
      id: this.repoCounter,
      owner,
      name,
      description,
      private: isPrivate,
      defaultBranch,
      createdAt: now,
      updatedAt: now,
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      labelCounter: 0,
      issues: new Map(),
      issueCounter: 0,
      comments: new Map(),
      commentCounter: 0,
      pulls: new Map(),
      pullCounter: 0,
    };
    this.repos.set(key, repo);
    return repo;
  }

  getRepo(owner: string, name: string): StoredRepo | null {
    return this.repos.get(this.repoKey(owner, name)) ?? null;
  }

  listRepos(owner?: string): StoredRepo[] {
    const list: StoredRepo[] = [];
    for (const repo of this.repos.values()) {
      if (!owner || repo.owner.toLowerCase() === owner.toLowerCase()) {
        list.push(repo);
      }
    }
    return list;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  patchRepo(owner: string, name: string, patch: { description?: string; name?: string; private?: boolean }): StoredRepo | null {
    const repo = this.getRepo(owner, name);
    if (!repo) return null;
    if (patch.description !== undefined) repo.description = patch.description;
    if (patch.private !== undefined) repo.private = patch.private;
    repo.updatedAt = new Date().toISOString();
    return repo;
  }

  // Ref management
  getRef(repo: StoredRepo, refName: string): string | null {
    return repo.refs.get(refName) ?? null;
  }

  setRef(repo: StoredRepo, refName: string, oid: string): void {
    repo.refs.set(refName, oid);
  }

  deleteRef(repo: StoredRepo, refName: string): boolean {
    return repo.refs.delete(refName);
  }

  listRefs(repo: StoredRepo): Record<string, string> {
    const out: Record<string, string> = {};
    for (const [k, v] of repo.refs.entries()) {
      out[k] = v;
    }
    return out;
  }

  // Object management
  putObject(repo: StoredRepo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) {
      repo.objects.set(oid, { type, data });
    }
    return oid;
  }

  getObject(repo: StoredRepo, oid: string): GitObject | null {
    return repo.objects.get(oid) ?? null;
  }

  // Helper: tree building from flat map of files
  getTreeEntries(repo: StoredRepo, treeOid: string, prefix = ''): Map<string, { oid: string; mode: string }> {
    const map = new Map<string, { oid: string; mode: string }>();
    const treeObj = repo.objects.get(treeOid);
    if (!treeObj || treeObj.type !== 'tree') return map;

    const entries = parseTree(treeObj.data);
    for (const entry of entries) {
      const fullPath = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.mode === '40000') {
        const sub = this.getTreeEntries(repo, entry.oid, fullPath);
        for (const [k, v] of sub.entries()) {
          map.set(k, v);
        }
      } else {
        map.set(fullPath, { oid: entry.oid, mode: entry.mode });
      }
    }
    return map;
  }

  buildTreeFromFilesMap(repo: StoredRepo, files: Map<string, { oid: string; mode: string }>): string {
    interface DirNode {
      files: Map<string, { oid: string; mode: string }>;
      dirs: Map<string, DirNode>;
    }
    const makeNode = (): DirNode => ({ files: new Map(), dirs: new Map() });
    const root = makeNode();

    for (const [filePath, info] of files.entries()) {
      const parts = filePath.split('/').filter(Boolean);
      let curr = root;
      for (let i = 0; i < parts.length - 1; i++) {
        const dir = parts[i];
        if (!curr.dirs.has(dir)) curr.dirs.set(dir, makeNode());
        curr = curr.dirs.get(dir)!;
      }
      curr.files.set(parts[parts.length - 1], info);
    }

    const saveNode = (node: DirNode): string => {
      const entries: Array<{ mode: string; name: string; oid: string }> = [];
      for (const [name, info] of node.files.entries()) {
        entries.push({ mode: info.mode, name, oid: info.oid });
      }
      for (const [name, child] of node.dirs.entries()) {
        const subTreeOid = saveNode(child);
        entries.push({ mode: '40000', name, oid: subTreeOid });
      }
      const treeBytes = buildTree(entries);
      return this.putObject(repo, 'tree', treeBytes);
    };

    return saveNode(root);
  }

  // Contents API
  getFileOrDir(repo: StoredRepo, refOrCommit: string, targetPath: string): { type: 'file'; blob: Buffer; sha: string } | { type: 'dir'; entries: Array<{ name: string; path: string; sha: string; size: number; type: 'file' | 'dir' }> } | null {
    let commitOid = repo.refs.get(refOrCommit) ?? repo.refs.get(`refs/heads/${refOrCommit}`);
    if (!commitOid && isObjectId(refOrCommit)) commitOid = refOrCommit;
    if (!commitOid) commitOid = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    if (!commitOid) return null;

    const commitObj = repo.objects.get(commitOid);
    if (!commitObj || commitObj.type !== 'commit') return null;

    const commit = parseCommit(commitObj.data);
    const cleanPath = targetPath.replace(/^\/+|\/+$/g, '');

    const allFiles = this.getTreeEntries(repo, commit.tree);
    if (cleanPath && allFiles.has(cleanPath)) {
      const file = allFiles.get(cleanPath)!;
      const blob = repo.objects.get(file.oid);
      if (!blob) return null;
      return { type: 'file', blob: blob.data, sha: file.oid };
    }

    // Check directory
    const dirPrefix = cleanPath ? `${cleanPath}/` : '';
    const dirMap = new Map<string, { sha: string; isDir: boolean; size: number }>();

    for (const [p, info] of allFiles.entries()) {
      if (cleanPath === '' || p.startsWith(dirPrefix)) {
        const relative = cleanPath ? p.slice(dirPrefix.length) : p;
        const topSegment = relative.split('/')[0];
        if (relative.includes('/')) {
          if (!dirMap.has(topSegment)) {
            dirMap.set(topSegment, { sha: info.oid, isDir: true, size: 0 });
          }
        } else {
          const blobObj = repo.objects.get(info.oid);
          dirMap.set(topSegment, { sha: info.oid, isDir: false, size: blobObj?.data.length ?? 0 });
        }
      }
    }

    if (dirMap.size > 0 || cleanPath === '') {
      const entries = [...dirMap.entries()].map(([name, data]) => ({
        name,
        path: cleanPath ? `${cleanPath}/${name}` : name,
        sha: data.sha,
        size: data.size,
        type: (data.isDir ? 'dir' : 'file') as 'file' | 'dir',
      }));
      return { type: 'dir', entries };
    }

    return null;
  }

  putFile(repo: StoredRepo, branchName: string, filePath: string, content: Buffer, message: string, author: string, expectedSha?: string): { commitSha: string; blobSha: string; isUpdate: boolean } {
    const branchRef = `refs/heads/${branchName}`;
    let currentTip = repo.refs.get(branchRef) ?? null;
    const cleanPath = filePath.replace(/^\/+|\/+$/g, '');

    const currentFiles = currentTip ? this.getTreeEntries(repo, parseCommit(repo.objects.get(currentTip)!.data).tree) : new Map<string, { oid: string; mode: string }>();

    const existing = currentFiles.get(cleanPath);
    if (expectedSha !== undefined && existing && existing.oid !== expectedSha) {
      throw new Error('stale-sha');
    }

    const isUpdate = !!existing;
    const blobSha = this.putObject(repo, 'blob', content);
    currentFiles.set(cleanPath, { oid: blobSha, mode: '100644' });

    const newTreeOid = this.buildTreeFromFilesMap(repo, currentFiles);
    const commitData = buildCommit({
      tree: newTreeOid,
      parents: currentTip ? [currentTip] : [],
      message,
      author,
    });
    const commitSha = this.putObject(repo, 'commit', commitData);
    repo.refs.set(branchRef, commitSha);
    return { commitSha, blobSha, isUpdate };
  }

  deleteFile(repo: StoredRepo, branchName: string, filePath: string, message: string, author: string, expectedSha?: string): string {
    const branchRef = `refs/heads/${branchName}`;
    const currentTip = repo.refs.get(branchRef);
    if (!currentTip) throw new Error('branch-not-found');

    const cleanPath = filePath.replace(/^\/+|\/+$/g, '');
    const currentFiles = this.getTreeEntries(repo, parseCommit(repo.objects.get(currentTip)!.data).tree);

    const existing = currentFiles.get(cleanPath);
    if (!existing) throw new Error('file-not-found');
    if (expectedSha !== undefined && existing.oid !== expectedSha) {
      throw new Error('stale-sha');
    }

    currentFiles.delete(cleanPath);
    const newTreeOid = this.buildTreeFromFilesMap(repo, currentFiles);
    const commitData = buildCommit({
      tree: newTreeOid,
      parents: [currentTip],
      message,
      author,
    });
    const commitSha = this.putObject(repo, 'commit', commitData);
    repo.refs.set(branchRef, commitSha);
    return commitSha;
  }

  // Label management
  createLabel(repo: StoredRepo, name: string, color = 'ededed', description = ''): { id: number; name: string; color: string; description: string } {
    repo.labelCounter += 1;
    const item = { id: repo.labelCounter, name, color, description };
    repo.labels.set(name.toLowerCase(), item);
    return item;
  }

  getLabel(repo: StoredRepo, name: string) {
    return repo.labels.get(name.toLowerCase()) ?? null;
  }

  listLabels(repo: StoredRepo) {
    return [...repo.labels.values()];
  }

  // Issue management
  createIssue(repo: StoredRepo, title: string, body = '', labels: string[] = []): StoredIssue {
    for (const l of labels) {
      if (!repo.labels.has(l.toLowerCase())) {
        throw new Error('bad-label');
      }
    }
    repo.issueCounter += 1;
    const now = new Date().toISOString();
    const issue: StoredIssue = {
      id: repo.issueCounter,
      number: repo.issueCounter,
      title,
      body,
      state: 'open',
      labels: [...labels],
      createdAt: now,
      updatedAt: now,
      comments: 0,
    };
    repo.issues.set(issue.number, issue);
    return issue;
  }

  getIssue(repo: StoredRepo, number: number): StoredIssue | null {
    return repo.issues.get(number) ?? null;
  }

  patchIssue(repo: StoredRepo, number: number, patch: { state?: 'open' | 'closed'; title?: string; body?: string }): StoredIssue | null {
    const issue = repo.issues.get(number);
    if (!issue) return null;
    const now = new Date().toISOString();
    if (patch.state) {
      issue.state = patch.state;
      if (patch.state === 'closed') issue.closedAt = now;
      else delete issue.closedAt;
    }
    if (patch.title !== undefined) issue.title = patch.title;
    if (patch.body !== undefined) issue.body = patch.body;
    issue.updatedAt = now;
    return issue;
  }

  listIssues(repo: StoredRepo, state: 'open' | 'closed' | 'all' = 'open', sort = 'created', direction = 'desc'): StoredIssue[] {
    let list = [...repo.issues.values()];
    if (state !== 'all') {
      list = list.filter((i) => i.state === state);
    }
    list.sort((a, b) => {
      const cmp = a.id - b.id;
      return direction === 'asc' ? cmp : -cmp;
    });
    return list;
  }

  // Comments
  createComment(repo: StoredRepo, issueNumber: number, body: string): StoredComment {
    const issue = repo.issues.get(issueNumber);
    if (!issue) throw new Error('issue-not-found');
    repo.commentCounter += 1;
    issue.comments += 1;
    const now = new Date().toISOString();
    const comment: StoredComment = {
      id: repo.commentCounter,
      issueNumber,
      body,
      createdAt: now,
      updatedAt: now,
    };
    repo.comments.set(comment.id, comment);
    return comment;
  }

  listComments(repo: StoredRepo, issueNumber: number): StoredComment[] {
    return [...repo.comments.values()].filter((c) => c.issueNumber === issueNumber);
  }

  // Pull Requests
  createPull(repo: StoredRepo, title: string, body = '', headRefName: string, baseRefName: string): StoredPull {
    const headRef = repo.refs.get(headRefName) ?? repo.refs.get(`refs/heads/${headRefName}`);
    const baseRef = repo.refs.get(baseRefName) ?? repo.refs.get(`refs/heads/${baseRefName}`);
    if (!headRef || !baseRef) throw new Error('ref-not-found');

    repo.pullCounter += 1;
    const now = new Date().toISOString();

    const baseFiles = this.getTreeEntries(repo, parseCommit(repo.objects.get(baseRef)!.data).tree);
    const headFiles = this.getTreeEntries(repo, parseCommit(repo.objects.get(headRef)!.data).tree);

    const files: StoredPull['files'] = [];
    for (const [path, info] of headFiles.entries()) {
      if (!baseFiles.has(path)) {
        files.push({ filename: path, status: 'added', additions: 1, deletions: 0, changes: 1 });
      } else if (baseFiles.get(path)!.oid !== info.oid) {
        files.push({ filename: path, status: 'modified', additions: 1, deletions: 1, changes: 2 });
      }
    }
    for (const path of baseFiles.keys()) {
      if (!headFiles.has(path)) {
        files.push({ filename: path, status: 'removed', additions: 0, deletions: 1, changes: 1 });
      }
    }

    const pull: StoredPull = {
      id: repo.pullCounter,
      number: repo.pullCounter,
      title,
      body,
      state: 'open',
      head: { ref: headRefName.replace(/^refs\/heads\//, ''), sha: headRef },
      base: { ref: baseRefName.replace(/^refs\/heads\//, ''), sha: baseRef },
      merged: false,
      createdAt: now,
      updatedAt: now,
      files,
    };
    repo.pulls.set(pull.number, pull);
    return pull;
  }

  getPull(repo: StoredRepo, number: number): StoredPull | null {
    return repo.pulls.get(number) ?? null;
  }

  listPulls(repo: StoredRepo, state: 'open' | 'closed' | 'all' = 'open'): StoredPull[] {
    let list = [...repo.pulls.values()];
    if (state !== 'all') {
      list = list.filter((p) => p.state === state);
    }
    return list;
  }

  mergePull(repo: StoredRepo, number: number): { sha: string; merged: boolean; message: string } {
    const pull = repo.pulls.get(number);
    if (!pull) throw new Error('pull-not-found');
    if (pull.merged || pull.state === 'closed') throw new Error('already-merged');

    const baseRefKey = `refs/heads/${pull.base.ref}`;
    const baseTip = repo.refs.get(baseRefKey);
    const headRefKey = `refs/heads/${pull.head.ref}`;
    const headTip = repo.refs.get(headRefKey) ?? pull.head.sha;

    if (!baseTip || !headTip) throw new Error('ref-not-found');

    const headCommit = parseCommit(repo.objects.get(headTip)!.data);
    const squashCommitData = buildCommit({
      tree: headCommit.tree,
      parents: [baseTip],
      message: `Merge pull request #${pull.number} from ${pull.head.ref}\n\n${pull.title}`,
      author: 'Merge Action <merge@make.fun>',
    });
    const mergeSha = this.putObject(repo, 'commit', squashCommitData);

    repo.refs.set(baseRefKey, mergeSha);

    const now = new Date().toISOString();
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = now;
    pull.mergeCommitSha = mergeSha;
    pull.updatedAt = now;

    return {
      sha: mergeSha,
      merged: true,
      message: 'Pull request successfully merged',
    };
  }
}

export const store = new ForgeStore();
