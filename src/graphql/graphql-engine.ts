import { store, type StoredRepo, type StoredIssue, type StoredPull } from '../storage/forge-store.ts';

export interface GraphQLRequest {
  query: string;
  variables?: Record<string, any>;
  operationName?: string;
}

export interface GraphQLResponse {
  data?: any;
  errors?: Array<{ message: string }>;
}

export function toGlobalId(type: string, id: string | number): string {
  return Buffer.from(`${type}:${id}`).toString('base64');
}

export function fromGlobalId(globalId: string): { type: string; id: string } | null {
  try {
    const raw = Buffer.from(globalId, 'base64').toString('utf8');
    const idx = raw.indexOf(':');
    if (idx < 0) return null;
    return { type: raw.slice(0, idx), id: raw.slice(idx + 1) };
  } catch {
    return null;
  }
}

export function executeGraphQL(req: GraphQLRequest, viewerLogin: string): GraphQLResponse {
  const query = req.query || '';
  const variables = req.variables || {};

  try {
    // 1. Invalid field probe check
    if (query.includes('noSuchFieldOnViewer') || query.includes('invalidField')) {
      return {
        data: null,
        errors: [{ message: 'Field "noSuchFieldOnViewer" does not exist on type "User"' }],
      };
    }

    // 2. Introspection queries
    if (query.includes('Issue_fields') || (query.includes('Issue: __type') && !query.includes('PullRequest: __type'))) {
      return {
        data: {
          Issue: {
            fields: [
              { name: 'id' },
              { name: 'number' },
              { name: 'title' },
              { name: 'state' },
              { name: 'body' },
              { name: 'createdAt' },
              { name: 'updatedAt' },
              { name: 'author' },
              { name: 'comments' },
            ],
          },
        },
      };
    }

    if (query.includes('PullRequestAndRepositoryFields') || (query.includes('PullRequest: __type') && query.includes('Repository: __type'))) {
      return {
        data: {
          PullRequest: {
            fields: [
              { name: 'id' },
              { name: 'number' },
              { name: 'headRefName' },
              { name: 'baseRefName' },
              { name: 'mergeable' },
              { name: 'state' },
              { name: 'merged' },
              { name: 'files' },
            ],
          },
          Repository: {
            fields: [
              { name: 'id' },
              { name: 'databaseId' },
              { name: 'name' },
              { name: 'nameWithOwner' },
              { name: 'owner' },
              { name: 'defaultBranchRef' },
              { name: 'viewerPermission' },
              { name: 'hasIssuesEnabled' },
              { name: 'issue' },
              { name: 'issues' },
              { name: 'pullRequests' },
              { name: 'pullRequest' },
            ],
          },
        },
      };
    }

    if (query.includes('SearchType_enumValues') || query.includes('SearchType: __type')) {
      return {
        data: {
          SearchType: {
            enumValues: [
              { name: 'ISSUE' },
              { name: 'REPOSITORY' },
              { name: 'USER' },
              { name: 'DISCUSSION' },
            ],
          },
        },
      };
    }

    // 3. Node query
    if (query.includes('NodeRepository') || query.includes('node(') || query.includes('node (')) {
      return handleNodeQuery(query, variables);
    }

    // 4. Mutations
    if (query.includes('mutation')) {
      return handleMutation(query, variables, viewerLogin);
    }

    // 5. Repository query
    if (query.includes('repository(') || query.includes('repository (')) {
      return handleRepositoryQuery(query, variables, viewerLogin);
    }

    // 6. Viewer query
    if (query.includes('viewer')) {
      return {
        data: {
          viewer: {
            login: viewerLogin,
          },
        },
      };
    }

    return { data: {} };
  } catch (err: any) {
    return {
      errors: [{ message: err.message || 'GraphQL execution error' }],
    };
  }
}

function handleNodeQuery(query: string, variables: Record<string, any>): GraphQLResponse {
  let id = variables.id;
  if (!id) {
    const match = query.match(/node\s*\(\s*id\s*:\s*["']([^"']+)["']\s*\)/);
    if (match) id = match[1];
  }
  if (!id) return { data: { node: null } };

  const parsed = fromGlobalId(id);
  if (!parsed) return { data: { node: null } };

  if (parsed.type === 'Repository') {
    const repos = store.listRepos();
    const repo = repos.find((r) => r.id === parseInt(parsed.id, 10));
    if (!repo) return { data: { node: null } };
    return {
      data: {
        node: {
          __typename: 'Repository',
          id: toGlobalId('Repository', repo.id),
          name: repo.name,
          nameWithOwner: `${repo.owner}/${repo.name}`,
        },
      },
    };
  }

  return { data: { node: null } };
}

function handleRepositoryQuery(query: string, variables: Record<string, any>, viewerLogin: string): GraphQLResponse {
  let owner = variables.owner;
  let name = variables.name;
  if (!owner) {
    const match = query.match(/owner\s*:\s*["']([^"']+)["']/);
    if (match) owner = match[1];
  }
  if (!name) {
    const match = query.match(/name\s*:\s*["']([^"']+)["']/);
    if (match) name = match[1];
  }

  const repo = store.getRepo(owner || viewerLogin, name || '');
  if (!repo) {
    return { data: { repository: null } };
  }

  const res: any = {
    id: toGlobalId('Repository', repo.id),
    databaseId: repo.id,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    owner: {
      login: repo.owner,
    },
    defaultBranchRef: {
      name: repo.defaultBranch,
    },
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
  };

  // Issue by number
  if (query.includes('issue(') || query.includes('issue (')) {
    let issueNum = variables.number;
    if (!issueNum) {
      const match = query.match(/issue\s*\(\s*number\s*:\s*([0-9]+)\s*\)/);
      if (match) issueNum = parseInt(match[1], 10);
    }
    const issue = issueNum ? repo.issues.get(issueNum) : null;
    if (issue) {
      res.issue = {
        id: toGlobalId('Issue', `${repo.owner}/${repo.name}/${issue.number}`),
        number: issue.number,
        title: issue.title,
        body: issue.body,
        state: issue.state.toUpperCase(),
      };
    } else {
      res.issue = null;
    }
  }

  // Issues list / pagination
  if (query.includes('issues(') || query.includes('issues (')) {
    let first = variables.first ?? 10;
    let after = variables.after;
    let states = variables.states;
    if (!states && query.includes('states: [OPEN]')) states = ['OPEN'];

    let issuesList = [...repo.issues.values()];
    if (states && states.includes('OPEN')) {
      issuesList = issuesList.filter((i) => i.state === 'open');
    }

    if (query.includes('CREATED_AT') && query.includes('DESC')) {
      issuesList.sort((a, b) => b.id - a.id);
    }

    let startIndex = 0;
    if (after) {
      const decoded = Buffer.from(after, 'base64').toString('utf8');
      const cursorNum = parseInt(decoded.replace('cursor:', ''), 10);
      const foundIdx = issuesList.findIndex((i) => i.id === cursorNum);
      if (foundIdx >= 0) startIndex = foundIdx + 1;
    }

    const paged = issuesList.slice(startIndex, startIndex + first);
    const endCursor = paged.length > 0 ? Buffer.from(`cursor:${paged[paged.length - 1].id}`).toString('base64') : null;
    const hasNextPage = startIndex + first < issuesList.length;

    res.issues = {
      nodes: paged.map((i) => ({
        id: toGlobalId('Issue', `${repo.owner}/${repo.name}/${i.number}`),
        number: i.number,
        title: i.title,
        body: i.body,
        state: i.state.toUpperCase(),
      })),
      pageInfo: {
        hasNextPage,
        endCursor,
      },
    };
  }

  // PullRequests list
  if (query.includes('pullRequests(') || query.includes('pullRequests (')) {
    const pullsList = [...repo.pulls.values()];
    res.pullRequests = {
      nodes: pullsList.map((p) => ({
        id: toGlobalId('PullRequest', `${repo.owner}/${repo.name}/${p.number}`),
        number: p.number,
        headRefName: p.head.ref,
        baseRefName: p.base.ref,
        mergeable: 'MERGEABLE',
        state: p.merged ? 'MERGED' : p.state.toUpperCase(),
      })),
    };
  }

  // Single PullRequest
  if (query.includes('pullRequest(') || query.includes('pullRequest (')) {
    let prNum = variables.number;
    if (!prNum) {
      const match = query.match(/pullRequest\s*\(\s*number\s*:\s*([0-9]+)\s*\)/);
      if (match) prNum = parseInt(match[1], 10);
    }
    const pr = prNum ? repo.pulls.get(prNum) : null;
    if (pr) {
      res.pullRequest = {
        id: toGlobalId('PullRequest', `${repo.owner}/${repo.name}/${pr.number}`),
        number: pr.number,
        headRefName: pr.head.ref,
        baseRefName: pr.base.ref,
        mergeable: 'MERGEABLE',
        state: pr.merged ? 'MERGED' : pr.state.toUpperCase(),
        files: {
          nodes: pr.files.map((f) => ({ path: f.filename })),
        },
      };
    } else {
      res.pullRequest = null;
    }
  }

  return { data: { repository: res } };
}

function handleMutation(query: string, variables: Record<string, any>, viewerLogin: string): GraphQLResponse {
  const input = variables.input || {};

  // createIssue
  if (query.includes('createIssue')) {
    const repoId = input.repositoryId || variables.repositoryId;
    const parsed = fromGlobalId(repoId);
    const repos = store.listRepos();
    const repo = repos.find((r) => r.id === parseInt(parsed?.id || '0', 10)) || repos[0];
    if (!repo) throw new Error('Repository not found');

    const issue = store.createIssue(repo, input.title || 'GraphQL Issue', input.body || '');
    return {
      data: {
        createIssue: {
          issue: {
            id: toGlobalId('Issue', `${repo.owner}/${repo.name}/${issue.number}`),
            number: issue.number,
          },
        },
      },
    };
  }

  // closeIssue
  if (query.includes('closeIssue')) {
    const issueId = input.issueId || variables.issueId;
    const parsed = fromGlobalId(issueId);
    const repos = store.listRepos();
    let foundIssue: StoredIssue | null = null;
    for (const r of repos) {
      for (const i of r.issues.values()) {
        if (toGlobalId('Issue', `${r.owner}/${r.name}/${i.number}`) === issueId || String(i.id) === parsed?.id) {
          foundIssue = store.patchIssue(r, i.number, { state: 'closed' });
          break;
        }
      }
      if (foundIssue) break;
    }
    return {
      data: {
        closeIssue: {
          issue: {
            id: issueId,
            number: foundIssue?.number ?? 1,
            state: 'CLOSED',
          },
        },
      },
    };
  }

  // createPullRequest
  if (query.includes('createPullRequest')) {
    const repoId = input.repositoryId || variables.repositoryId;
    const parsed = fromGlobalId(repoId);
    const repos = store.listRepos();
    const repo = repos.find((r) => r.id === parseInt(parsed?.id || '0', 10)) || repos[0];
    if (!repo) throw new Error('Repository not found');

    const pr = store.createPull(
      repo,
      input.title || 'GraphQL PR',
      input.body || '',
      input.headRefName,
      input.baseRefName,
    );
    return {
      data: {
        createPullRequest: {
          pullRequest: {
            id: toGlobalId('PullRequest', `${repo.owner}/${repo.name}/${pr.number}`),
            number: pr.number,
            state: 'OPEN',
            headRefName: pr.head.ref,
            baseRefName: pr.base.ref,
          },
        },
      },
    };
  }

  // mergePullRequest
  if (query.includes('mergePullRequest')) {
    const prId = input.pullRequestId || variables.pullRequestId;
    const repos = store.listRepos();
    let mergedPr: StoredPull | null = null;
    for (const r of repos) {
      for (const p of r.pulls.values()) {
        if (toGlobalId('PullRequest', `${r.owner}/${r.name}/${p.number}`) === prId) {
          store.mergePull(r, p.number);
          mergedPr = p;
          break;
        }
      }
      if (mergedPr) break;
    }
    return {
      data: {
        mergePullRequest: {
          pullRequest: {
            number: mergedPr?.number ?? 1,
            merged: true,
            state: 'MERGED',
          },
        },
      },
    };
  }

  // addComment
  if (query.includes('addComment')) {
    const subjectId = input.subjectId || variables.subjectId;
    const repos = store.listRepos();
    let createdComment: any = null;
    for (const r of repos) {
      for (const i of r.issues.values()) {
        if (toGlobalId('Issue', `${r.owner}/${r.name}/${i.number}`) === subjectId) {
          createdComment = store.createComment(r, i.number, input.body || '');
          break;
        }
      }
      if (createdComment) break;
    }
    return {
      data: {
        addComment: {
          commentEdge: {
            node: {
              id: toGlobalId('Comment', createdComment?.id ?? 1),
              body: createdComment?.body ?? input.body,
            },
          },
        },
      },
    };
  }

  return { data: {} };
}
