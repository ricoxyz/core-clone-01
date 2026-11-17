import { store } from '../storage/forge-store.ts';

export interface McpTool {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description?: string }>;
    required?: string[];
  };
}

export const MCP_TOOLS: McpTool[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get repository details',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue description' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'State: open, closed, or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'number', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

export function normalizeToolName(name: string): string {
  const lower = name.toLowerCase().replace(/^github[-_]/, '');
  if (lower === 'create_repository' || lower === 'repo_create' || lower === 'repos_create' || lower === 'repository_create') return 'create_repo';
  if (lower === 'get_repository' || lower === 'repo_get' || lower === 'repos_get' || lower === 'repository_get') return 'get_repo';
  if (lower === 'issue_create' || lower === 'issues_create') return 'create_issue';
  if (lower === 'issue_list' || lower === 'issues_list') return 'list_issues';
  if (lower === 'pull_request_create' || lower === 'pulls_create' || lower === 'create_pr' || lower === 'pr_create') return 'create_pull_request';
  if (lower === 'pull_request_merge' || lower === 'pulls_merge' || lower === 'merge_pr' || lower === 'pr_merge') return 'merge_pull_request';
  return lower;
}

export function handleMcpRpc(req: any, viewerLogin: string): any {
  const id = req.id ?? null;
  const method = req.method;

  if (method === 'initialize') {
    return {
      jsonrpc: '2.0',
      id,
      result: {
        protocolVersion: '2025-11-25',
        capabilities: { tools: {} },
        serverInfo: { name: 'github-forge', version: '1.0.0' },
      },
    };
  }

  if (method === 'server/discover') {
    return {
      jsonrpc: '2.0',
      id,
      result: {
        serverInfo: { name: 'github-forge', version: '1.0.0' },
        capabilities: { tools: {} },
        supportedVersions: ['2026-07-28', '2025-11-25'],
      },
    };
  }

  if (method === 'tools/list') {
    return {
      jsonrpc: '2.0',
      id,
      result: {
        tools: MCP_TOOLS,
      },
    };
  }

  if (method === 'tools/call') {
    const params = req.params || {};
    const rawName = params.name || '';
    const norm = normalizeToolName(rawName);
    const args = params.arguments || {};

    const toolDef = MCP_TOOLS.find((t) => t.name === norm);
    if (!toolDef) {
      return {
        jsonrpc: '2.0',
        id,
        error: { code: -32601, message: `Unknown tool ${rawName}` },
      };
    }

    // Check required arguments
    for (const reqField of toolDef.inputSchema.required || []) {
      if (args[reqField] === undefined || args[reqField] === null) {
        return {
          jsonrpc: '2.0',
          id,
          result: {
            isError: true,
            content: [{ type: 'text', text: `Missing required argument: ${reqField}` }],
          },
        };
      }
    }

    try {
      let resultData: any = null;

      if (norm === 'create_repo') {
        const repo = store.createRepo(
          viewerLogin,
          args.name,
          args.description || '',
          !!args.private,
        );
        resultData = {
          id: repo.id,
          name: repo.name,
          full_name: `${repo.owner}/${repo.name}`,
          owner: { login: repo.owner },
          private: repo.private,
          default_branch: repo.defaultBranch,
        };
      } else if (norm === 'get_repo') {
        const repo = store.getRepo(args.owner, args.repo);
        if (!repo) {
          return {
            jsonrpc: '2.0',
            id,
            result: {
              isError: true,
              content: [{ type: 'text', text: 'Repository not found' }],
            },
          };
        }
        resultData = {
          id: repo.id,
          name: repo.name,
          full_name: `${repo.owner}/${repo.name}`,
          owner: { login: repo.owner },
          private: repo.private,
          default_branch: repo.defaultBranch,
        };
      } else if (norm === 'create_issue') {
        const repo = store.getRepo(args.owner, args.repo);
        if (!repo) {
          return {
            jsonrpc: '2.0',
            id,
            result: { isError: true, content: [{ type: 'text', text: 'Repository not found' }] },
          };
        }
        const issue = store.createIssue(repo, args.title, args.body || '');
        resultData = {
          id: issue.id,
          number: issue.number,
          title: issue.title,
          body: issue.body,
          state: issue.state,
        };
      } else if (norm === 'list_issues') {
        const repo = store.getRepo(args.owner, args.repo);
        if (!repo) {
          return {
            jsonrpc: '2.0',
            id,
            result: { isError: true, content: [{ type: 'text', text: 'Repository not found' }] },
          };
        }
        const issues = store.listIssues(repo, args.state || 'all');
        resultData = issues.map((i) => ({
          id: i.id,
          number: i.number,
          title: i.title,
          body: i.body,
          state: i.state,
        }));
      } else if (norm === 'create_pull_request') {
        const repo = store.getRepo(args.owner, args.repo);
        if (!repo) {
          return {
            jsonrpc: '2.0',
            id,
            result: { isError: true, content: [{ type: 'text', text: 'Repository not found' }] },
          };
        }
        const pr = store.createPull(repo, args.title, args.body || '', args.head, args.base);
        resultData = {
          id: pr.id,
          number: pr.number,
          title: pr.title,
          head: pr.head,
          base: pr.base,
          state: pr.state,
        };
      } else if (norm === 'merge_pull_request') {
        const repo = store.getRepo(args.owner, args.repo);
        if (!repo) {
          return {
            jsonrpc: '2.0',
            id,
            result: { isError: true, content: [{ type: 'text', text: 'Repository not found' }] },
          };
        }
        const res = store.mergePull(repo, args.pull_number);
        resultData = res;
      }

      return {
        jsonrpc: '2.0',
        id,
        result: {
          content: [{ type: 'text', text: JSON.stringify(resultData) }],
        },
      };
    } catch (err: any) {
      return {
        jsonrpc: '2.0',
        id,
        result: {
          isError: true,
          content: [{ type: 'text', text: err.message || 'Execution error' }],
        },
      };
    }
  }

  return {
    jsonrpc: '2.0',
    id,
    error: { code: -32601, message: `Method ${method} not found` },
  };
}
