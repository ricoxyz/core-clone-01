import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { storage } from '../storage/git.ts';
import { UploadPackV2, ReceivePack } from '../git/protocols.ts';

interface Route {
  method: string;
  path: RegExp;
  handler: (req: IncomingMessage, res: ServerResponse, params: Record<string, string>) => Promise<void>;
}

export class Router {
  private routes: Route[] = [];

  get(path: string, handler: Route['handler']): void {
    this.addRoute('GET', path, handler);
  }

  post(path: string, handler: Route['handler']): void {
    this.addRoute('POST', path, handler);
  }

  patch(path: string, handler: Route['handler']): void {
    this.addRoute('PATCH', path, handler);
  }

  put(path: string, handler: Route['handler']): void {
    this.addRoute('PUT', path, handler);
  }

  delete(path: string, handler: Route['handler']): void {
    this.addRoute('DELETE', path, handler);
  }

  private addRoute(method: string, path: string, handler: Route['handler']): void {
    // Convert path pattern to regex
    const regexPath = path
      .replace(/\{([^}]+)\}/g, '(?<$1>[^/]+)')
      .replace(/\//g, '\\/');
    this.routes.push({
      method,
      path: new RegExp(`^${regexPath}$`),
      handler
    });
  }

  async handle(req: IncomingMessage, res: ServerResponse): Promise<boolean> {
    const method = req.method ?? 'GET';
    const url = new URL(req.url ?? '/', `http://${req.headers.host}`);
    const path = url.pathname;

    for (const route of this.routes) {
      if (route.method !== method) continue;
      const match = path.match(route.path);
      if (!match) continue;

      const params: Record<string, string> = {};
      for (const [key, value] of Object.entries(match.groups ?? {})) {
        params[key] = value;
      }

      try {
        await route.handler(req, res, params);
        return true;
      } catch (err) {
        console.error('Route handler error:', err);
        this.sendError(res, 500, 'Internal Server Error');
        return true;
      }
    }

    return false;
  }

  sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
    const text = JSON.stringify(body);
    res.writeHead(status, {
      'content-type': 'application/json; charset=utf-8',
      'x-github-request-id': crypto.randomUUID(),
      ...headers
    });
    res.end(text);
  }

  sendError(res: ServerResponse, status: number, message: string): void {
    this.sendJson(res, status, { message });
  }

  // Catch-all route for 404
  all(path: string, handler: Route['handler']): void {
    const methods = ['GET', 'POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS'];
    for (const method of methods) {
      this.addRoute(method, path, handler);
    }
  }
}

export const router = new Router();

// Git Smart HTTP routes
router.get('/:owner/:repo.git/info/refs', async (req, res, params) => {
  const { owner, repo } = params;
  const service = new URL(req.url ?? '', `http://${req.headers.host}`).searchParams.get('service');
  
  if (service === 'git-upload-pack') {
    // Advertise upload-pack v2
    const refs = await getRefs(params.owner, params.repo);
    let output = 'version 2\n';
    output += 'agent=makefun-git/1.0\n';
    output += 'ls-refs\n';
    output += 'fetch\n';
    output += 'server-option\n';
    output += '\n';
    
    for (const [ref, oid] of Object.entries(await getAllRefs(params.owner, params.repo))) {
      output += `${ref} ${oid}\n`;
    }
    
    res.writeHead(200, {
      'Content-Type': 'application/x-git-upload-pack-advertisement',
      'Cache-Control': 'no-cache'
    });
    res.end(output);
    return;
  }
  
  if (service === 'git-receive-pack') {
    const refs = await getRefs(params.owner, params.repo);
    let output = '# service=git-receive-pack\n0000';
    for (const [ref, oid] of Object.entries(refs)) {
      output += `${oid} ${ref}\n`;
    }
    
    res.writeHead(200, {
      'Content-Type': 'application/x-git-receive-pack-advertisement',
      'Cache-Control': 'no-cache'
    });
    res.end(output);
    return;
  }
  
  router.sendError(res, 404, 'Not Found');
});

async function getRefs(owner: string, repo: string): Promise<Record<string, string>> {
  // This would read from storage
  return {};
}

async function getAllRefs(owner: string, repo: string): Promise<Record<string, string>> {
  // This would read from storage
  return {
    'refs/heads/main': '0'.repeat(40)
  };
}

export async function registerRestRoutes(): Promise<void> {
  // Repository endpoints
  router.get('/api/v3', async (req, res, _params) => {
    router.sendJson(res, 200, {
      current_user_url: '/api/v3/user',
      rate_limit_url: '/api/v3/rate_limit'
    }, { 'x-oauth-scopes': 'repo, read:org' });
  });

  router.get('/api/v3/user/repos', async (req, res, _params) => {
    // List repositories
    router.sendJson(res, 200, []);
  });

  router.post('/api/v3/user/repos', async (req, res, _params) => {
    // Create repository
    router.sendJson(res, 201, { message: 'Repository created' });
  });

  router.get('/api/v3/repos/:owner/:repo', async (req, res, params) => {
    router.sendJson(res, 200, { 
      name: params.repo,
      owner: { login: params.owner },
      full_name: `${params.owner}/${params.repo}`
    });
  });

  router.post('/api/v3/repos/:owner/:repo/contents/:path', async (req, res, _params) => {
    router.sendJson(res, 201, { message: 'Content created' });
  });

  router.delete('/api/v3/repos/:owner/:repo/contents/*', async (req, res, _params) => {
    router.sendJson(res, 200, { message: 'Content deleted' });
  });

  router.post('/api/v3/repos/:owner/:repo/issues', async (req, res, _params) => {
    router.sendJson(res, 201, { message: 'Issue created' });
  });

  router.post('/api/v3/repos/:owner/:repo/pulls', async (req, res, _params) => {
    router.sendJson(res, 201, { message: 'Pull request created' });
  });

  router.put('/api/v3/repos/:owner/:repo/pulls/:number/merge', async (req, res, _params) => {
    router.sendJson(res, 200, { sha: 'abc123', merged: true, message: 'Pull request merged' });
  });

  // GraphQL endpoint
  router.post('/api/graphql', async (req, res, _params) => {
    const body = await readBody(req);
    const { query, variables } = JSON.parse(body);
    
    // Basic GraphQL implementation
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ data: { message: 'GraphQL endpoint' } }));
  });

  // MCP endpoint
  router.post('/mcp', async (req, res, _params) => {
    const body = await readBody(req);
    const { method, params } = JSON.parse(body);
    
    let result: any = { jsonrpc: '2.0', id: 1 };
    
    if (method === 'initialize') {
      result.result = {
        protocolVersion: '2025-11-25',
        capabilities: { tools: {} },
        serverInfo: { name: 'github-forge', version: '1.0.0' }
      };
    } else if (method === 'tools/list') {
      result.result = {
        tools: [
          { name: 'create_repo', description: 'Create a new repository', inputSchema: { type: 'object', properties: { name: { type: 'string' } } } },
          { name: 'get_repo', description: 'Get repository info', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' } } } },
          { name: 'create_issue', description: 'Create an issue', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, body: { type: 'string' } } } },
          { name: 'list_issues', description: 'List issues', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' } } } },
          { name: 'create_pull_request', description: 'Create a pull request', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, title: { type: 'string' }, head: { type: 'string' }, base: { type: 'string' } } } },
          { name: 'merge_pull_request', description: 'Merge a pull request', inputSchema: { type: 'object', properties: { owner: { type: 'string' }, repo: { type: 'string' }, pull_number: { type: 'number' } } } }
        ]
      };
    } else if (method === 'tools/call') {
      const { name, arguments: args } = params;
      result.result = { content: [{ type: 'text', text: `Called ${name} with ${JSON.stringify(args)}` }] };
    } else {
      result.error = { code: -32601, message: 'Method not found' };
    }
    
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify(result));
  });

  // Git smart HTTP
  router.get('/:owner/:repo.git/info/refs', async (req, res, params) => {
    const service = new URL(req.url ?? '', `http://${req.headers.host}`).searchParams.get('service');
    
    if (service === 'git-upload-pack') {
      const refs = { 'refs/heads/main': '0'.repeat(40) };
      let output = 'version 2\nagent=makefun-git/1.0\nls-refs\nfetch\nserver-option\n\n';
      for (const [ref, oid] of Object.entries({ 'refs/heads/main': '0'.repeat(40) })) {
        output += `${ref} ${oid}\n`;
      }
      res.writeHead(200, {
        'Content-Type': 'application/x-git-upload-pack-advertisement',
        'Cache-Control': 'no-cache'
      });
      res.end(output);
      return;
    }
    
    if (service === 'git-receive-pack') {
      let output = '# service=git-receive-pack\n0000';
      for (const [ref, oid] of Object.entries({ 'refs/heads/main': '0'.repeat(40) })) {
        output += `${oid} ${ref}\n`;
      }
      res.writeHead(200, {
        'Content-Type': 'application/x-git-receive-pack-advertisement',
        'Cache-Control': 'no-cache'
      });
      res.end(output);
      return;
    }
    
    router.sendError(res, 404, 'Not Found');
  });

  // Catch-all 404
  router.all('*', async (req, res, _params) => {
    router.sendError(res, 404, 'Not Found');
  });
}

async function readBody(req: IncomingMessage): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }
  return Buffer.concat(chunks).toString();
}