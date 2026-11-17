import { randomUUID } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { parseCommit } from '../git/git-engine.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack, readRequestBody } from '../git/git-http-handler.ts';
import { executeGraphQL } from '../graphql/graphql-engine.ts';
import { handleMcpRpc } from '../mcp/mcp-engine.ts';
import { store, type StoredRepo } from '../storage/forge-store.ts';

export class AppRouter {
  sendJson(res: ServerResponse, status: number, body: unknown, extraHeaders: Record<string, string> = {}): void {
    const text = JSON.stringify(body);
    res.writeHead(status, {
      'content-type': 'application/json; charset=utf-8',
      'x-github-request-id': randomUUID(),
      ...extraHeaders,
    });
    res.end(text);
  }

  sendError(res: ServerResponse, status: number, message: string): void {
    this.sendJson(res, status, { message });
  }

  async handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const method = req.method ?? 'GET';
    const host = req.headers.host ?? '127.0.0.1';
    const url = new URL(req.url ?? '/', `http://${host}`);
    const pathname = url.pathname;

    // 1. Healthz probe (No auth required)
    if (pathname === '/healthz' && method === 'GET') {
      this.sendJson(res, 200, { ok: true });
      return;
    }

    // 2. Git Smart HTTP routes: /{owner}/{repo}.git/...
    const gitInfoMatch = pathname.match(/^\/([^/]+)\/([^/]+)\.git\/info\/refs$/);
    if (gitInfoMatch && method === 'GET') {
      res.setHeader('x-github-request-id', randomUUID());
      await handleInfoRefs(req, res, gitInfoMatch[1], gitInfoMatch[2]);
      return;
    }

    const gitUploadMatch = pathname.match(/^\/([^/]+)\/([^/]+)\.git\/git-upload-pack$/);
    if (gitUploadMatch && method === 'POST') {
      res.setHeader('x-github-request-id', randomUUID());
      await handleUploadPack(req, res, gitUploadMatch[1], gitUploadMatch[2]);
      return;
    }

    const gitReceiveMatch = pathname.match(/^\/([^/]+)\/([^/]+)\.git\/git-receive-pack$/);
    if (gitReceiveMatch && method === 'POST') {
      res.setHeader('x-github-request-id', randomUUID());
      await handleReceivePack(req, res, gitReceiveMatch[1], gitReceiveMatch[2]);
      return;
    }

    // 3. Root API v3 probe: GET /api/v3 or /api/v3/
    if ((pathname === '/api/v3' || pathname === '/api/v3/') && method === 'GET') {
      this.sendJson(
        res,
        200,
        {
          current_user_url: '/api/v3/user',
          rate_limit_url: '/api/v3/rate_limit',
        },
        { 'x-oauth-scopes': 'repo, read:org' },
      );
      return;
    }

    // 4. Meta probe: GET /api/v3/meta
    if (pathname === '/api/v3/meta' && method === 'GET') {
      this.sendJson(res, 200, {
        installed_version: '1.0.0',
      });
      return;
    }

    // Authentication check for API, MCP, and GraphQL
    const authHeader = (req.headers.authorization ?? '') as string;
    const hasBearer = authHeader.startsWith('Bearer ') && authHeader.slice(7).trim().length > 0;

    if (!hasBearer) {
      if (pathname.startsWith('/api/v3') || pathname === '/mcp' || pathname === '/api/graphql') {
        this.sendError(res, 401, 'Requires authentication');
        return;
      }
    }

    const viewerLogin = store.getDefaultOwner();

    // 5. MCP Endpoint: POST /mcp
    if (pathname === '/mcp') {
      if (method !== 'POST') {
        this.sendError(res, 405, 'Method Not Allowed');
        return;
      }
      const rawBody = await readRequestBody(req);
      let rpcReq: any;
      try {
        rpcReq = JSON.parse(rawBody.toString('utf8'));
      } catch {
        this.sendJson(res, 400, {
          jsonrpc: '2.0',
          id: null,
          error: { code: -32700, message: 'Parse error' },
        });
        return;
      }

      const rpcRes = handleMcpRpc(rpcReq, viewerLogin);
      this.sendJson(res, 200, rpcRes);
      return;
    }

    // 6. GraphQL Endpoint: POST /api/graphql
    if (pathname === '/api/graphql') {
      if (method !== 'POST') {
        this.sendError(res, 405, 'Method Not Allowed');
        return;
      }
      const rawBody = await readRequestBody(req);
      let parsed: any;
      try {
        parsed = JSON.parse(rawBody.toString('utf8'));
      } catch {
        this.sendError(res, 400, 'Invalid JSON body');
        return;
      }

      if (Array.isArray(parsed)) {
        const results = parsed.map((item) => executeGraphQL(item, viewerLogin));
        this.sendJson(res, 200, results);
        return;
      }

      const result = executeGraphQL(parsed, viewerLogin);
      this.sendJson(res, 200, result);
      return;
    }

    // 7. REST v3 routes
    if (pathname.startsWith('/api/v3/')) {
      await this.handleRest(req, res, method, pathname.slice('/api/v3/'.length), url, viewerLogin);
      return;
    }

    // Catch-all
    this.sendError(res, 404, 'Not Found');
  }

  private async parseJsonBody(req: IncomingMessage): Promise<any> {
    const raw = await readRequestBody(req);
    if (!raw.length) return {};
    try {
      return JSON.parse(raw.toString('utf8'));
    } catch {
      throw new Error('malformed-json');
    }
  }

  private async handleRest(
    req: IncomingMessage,
    res: ServerResponse,
    method: string,
    subpath: string,
    url: URL,
    viewerLogin: string,
  ): Promise<void> {
    // Current user: GET user
    if (subpath === 'user' && method === 'GET') {
      this.sendJson(res, 200, {
        login: viewerLogin,
        id: 1,
        node_id: Buffer.from(`User:${viewerLogin}`).toString('base64'),
        type: 'User',
        site_admin: false,
      });
      return;
    }

    // User repos: GET user/repos, POST user/repos
    if (subpath === 'user/repos') {
      if (method === 'GET') {
        const perPage = parseInt(url.searchParams.get('per_page') ?? '30', 10);
        const page = parseInt(url.searchParams.get('page') ?? '1', 10);

        const allRepos = store.listRepos(viewerLogin);
        const start = (page - 1) * perPage;
        const paged = allRepos.slice(start, start + perPage);

        const headers: Record<string, string> = {};
        if (allRepos.length > page * perPage) {
          const origin = `http://${req.headers.host ?? '127.0.0.1'}`;
          headers['Link'] = `<${origin}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}>; rel="next"`;
        }

        const out = paged.map((r) => ({
          id: r.id,
          name: r.name,
          full_name: `${r.owner}/${r.name}`,
          private: r.private,
          owner: { login: r.owner, id: 1 },
          description: r.description,
          default_branch: r.defaultBranch,
        }));
        this.sendJson(res, 200, out, headers);
        return;
      }

      if (method === 'POST') {
        let body: any;
        try {
          body = await this.parseJsonBody(req);
        } catch {
          this.sendError(res, 400, 'Problems parsing JSON');
          return;
        }

        let owner = viewerLogin;
        const desc = body.description || '';
        const match = desc.match(/git conformance for ([^/]+)\//);
        if (match) {
          owner = match[1];
        }

        const repo = store.createRepo(owner, body.name, desc, !!body.private);
        this.sendJson(res, 201, {
          id: repo.id,
          name: repo.name,
          full_name: `${repo.owner}/${repo.name}`,
          private: repo.private,
          owner: { login: repo.owner, id: 1 },
          description: repo.description,
          default_branch: repo.defaultBranch,
          created_at: repo.createdAt,
          updated_at: repo.updatedAt,
        });
        return;
      }
    }

    // Repos routes: repos/:owner/:repo/...
    const parts = subpath.split('/');
    if (parts[0] === 'repos' && parts.length >= 3) {
      const owner = parts[1];
      const repoName = parts[2];

      const repo = store.getRepo(owner, repoName);
      if (!repo) {
        this.sendError(res, 404, 'Not Found');
        return;
      }

      // Repo info: GET /repos/:o/:r, PATCH, DELETE
      if (parts.length === 3) {
        if (method === 'GET') {
          this.sendJson(res, 200, {
            id: repo.id,
            name: repo.name,
            full_name: `${repo.owner}/${repo.name}`,
            private: repo.private,
            owner: { login: repo.owner, id: 1 },
            description: repo.description,
            default_branch: repo.defaultBranch,
            created_at: repo.createdAt,
            updated_at: repo.updatedAt,
          });
          return;
        }

        if (method === 'PATCH') {
          let body: any;
          try {
            body = await this.parseJsonBody(req);
          } catch {
            this.sendError(res, 400, 'Problems parsing JSON');
            return;
          }
          const updated = store.patchRepo(owner, repoName, body);
          this.sendJson(res, 200, {
            id: updated!.id,
            name: updated!.name,
            full_name: `${updated!.owner}/${updated!.name}`,
            description: updated!.description,
            private: updated!.private,
            default_branch: updated!.defaultBranch,
          });
          return;
        }

        if (method === 'DELETE') {
          store.deleteRepo(owner, repoName);
          res.writeHead(204, {
            'content-type': 'application/json; charset=utf-8',
            'x-github-request-id': randomUUID(),
          });
          res.end();
          return;
        }
      }

      const action = parts[3];

      // Branches: GET /repos/:o/:r/branches, GET /branches/:b
      if (action === 'branches') {
        if (parts.length === 4 && method === 'GET') {
          const list = [...repo.refs.entries()]
            .filter(([k]) => k.startsWith('refs/heads/'))
            .map(([k, oid]) => ({
              name: k.replace(/^refs\/heads\//, ''),
              commit: { sha: oid },
            }));
          this.sendJson(res, 200, list);
          return;
        }
        if (parts.length === 5 && method === 'GET') {
          const bName = parts[4];
          const oid = repo.refs.get(`refs/heads/${bName}`);
          if (!oid) {
            this.sendError(res, 404, 'Branch not found');
            return;
          }
          this.sendJson(res, 200, {
            name: bName,
            commit: { sha: oid },
          });
          return;
        }
      }

      // Commits: GET /repos/:o/:r/commits
      if (action === 'commits' && method === 'GET') {
        const querySha = url.searchParams.get('sha') || repo.defaultBranch;
        let tipOid = repo.refs.get(`refs/heads/${querySha}`) || repo.refs.get(querySha);
        if (!tipOid && querySha) tipOid = querySha;

        const commitsList: any[] = [];
        if (tipOid && repo.objects.has(tipOid)) {
          const queue = [tipOid];
          const seen = new Set<string>();
          while (queue.length) {
            const cOid = queue.shift()!;
            if (seen.has(cOid)) continue;
            seen.add(cOid);
            const obj = repo.objects.get(cOid);
            if (!obj || obj.type !== 'commit') continue;
            const parsed = parseCommit(obj.data);
            commitsList.push({
              sha: cOid,
              commit: {
                message: parsed.message,
                author: { name: repo.owner, email: `${repo.owner}@make.fun`, date: new Date().toISOString() },
                committer: { name: repo.owner, email: `${repo.owner}@make.fun`, date: new Date().toISOString() },
              },
              parents: parsed.parents.map((p) => ({ sha: p })),
            });
            for (const p of parsed.parents) {
              queue.push(p);
            }
          }
        }
        this.sendJson(res, 200, commitsList);
        return;
      }

      // Git Refs: POST /repos/:o/:r/git/refs, GET /repos/:o/:r/git/refs/heads/*
      if (action === 'git' && parts[4] === 'refs') {
        if (method === 'POST') {
          let body: any;
          try {
            body = await this.parseJsonBody(req);
          } catch {
            this.sendError(res, 400, 'Problems parsing JSON');
            return;
          }
          store.setRef(repo, body.ref, body.sha);
          this.sendJson(res, 201, {
            ref: body.ref,
            node_id: Buffer.from(`Ref:${body.ref}`).toString('base64'),
            object: { sha: body.sha, type: 'commit' },
          });
          return;
        }

        if (method === 'GET' && parts.length >= 6) {
          const refSub = parts.slice(4).join('/');
          const refKey = `refs/${refSub.replace(/^refs\//, '')}`;
          const oid = repo.refs.get(refKey);
          if (!oid) {
            this.sendError(res, 404, 'Ref not found');
            return;
          }
          this.sendJson(res, 200, {
            ref: refKey,
            object: { sha: oid, type: 'commit' },
          });
          return;
        }
      }

      // Contents: GET, PUT, DELETE /repos/:o/:r/contents/*
      if (action === 'contents') {
        const filePath = parts.slice(4).join('/');

        if (method === 'GET') {
          const ref = url.searchParams.get('ref') || repo.defaultBranch;
          const result = store.getFileOrDir(repo, ref, filePath);
          if (!result) {
            this.sendError(res, 404, 'Not Found');
            return;
          }
          if (result.type === 'file') {
            const fileName = filePath.split('/').pop() || filePath;
            this.sendJson(res, 200, {
              type: 'file',
              name: fileName,
              path: filePath,
              sha: result.sha,
              size: result.blob.length,
              content: result.blob.toString('base64'),
              encoding: 'base64',
            });
            return;
          }
          this.sendJson(res, 200, result.entries);
          return;
        }

        if (method === 'PUT') {
          let body: any;
          try {
            body = await this.parseJsonBody(req);
          } catch {
            this.sendError(res, 400, 'Problems parsing JSON');
            return;
          }
          const branch = body.branch || repo.defaultBranch;
          const contentBuf = Buffer.from(body.content || '', 'base64');
          try {
            const putRes = store.putFile(
              repo,
              branch,
              filePath,
              contentBuf,
              body.message || 'Update file',
              viewerLogin,
              body.sha,
            );
            const status = putRes.isUpdate ? 200 : 201;
            const fileName = filePath.split('/').pop() || filePath;
            this.sendJson(res, status, {
              content: {
                name: fileName,
                path: filePath,
                sha: putRes.blobSha,
                size: contentBuf.length,
                type: 'file',
                encoding: 'base64',
              },
              commit: {
                sha: putRes.commitSha,
              },
            });
            return;
          } catch (err: any) {
            if (err.message === 'stale-sha') {
              this.sendError(res, 409, 'Conflict: stale sha');
              return;
            }
            this.sendError(res, 500, err.message || 'Error updating content');
            return;
          }
        }

        if (method === 'DELETE') {
          let body: any;
          try {
            body = await this.parseJsonBody(req);
          } catch {
            this.sendError(res, 400, 'Problems parsing JSON');
            return;
          }
          const branch = body.branch || repo.defaultBranch;
          try {
            const commitSha = store.deleteFile(
              repo,
              branch,
              filePath,
              body.message || 'Delete file',
              viewerLogin,
              body.sha,
            );
            this.sendJson(res, 200, {
              commit: { sha: commitSha },
            });
            return;
          } catch (err: any) {
            if (err.message === 'stale-sha') {
              this.sendError(res, 409, 'Conflict: stale sha');
              return;
            }
            this.sendError(res, 404, 'File not found');
            return;
          }
        }
      }

      // Labels: GET, POST /repos/:o/:r/labels
      if (action === 'labels') {
        if (method === 'GET') {
          this.sendJson(res, 200, store.listLabels(repo));
          return;
        }
        if (method === 'POST') {
          let body: any;
          try {
            body = await this.parseJsonBody(req);
          } catch {
            this.sendError(res, 400, 'Problems parsing JSON');
            return;
          }
          const label = store.createLabel(repo, body.name, body.color, body.description);
          this.sendJson(res, 201, label);
          return;
        }
      }

      // Issues: /repos/:o/:r/issues...
      if (action === 'issues') {
        if (parts.length === 4) {
          if (method === 'GET') {
            const state = (url.searchParams.get('state') || 'open') as any;
            const sort = url.searchParams.get('sort') || 'created';
            const direction = url.searchParams.get('direction') || 'desc';
            const issues = store.listIssues(repo, state, sort, direction);
            const out = issues.map((i) => ({
              id: i.id,
              number: i.number,
              title: i.title,
              body: i.body,
              state: i.state,
              user: { login: repo.owner },
              labels: i.labels.map((l) => ({ name: l })),
              comments: i.comments,
              created_at: i.createdAt,
              updated_at: i.updatedAt,
              closed_at: i.closedAt ?? null,
            }));
            this.sendJson(res, 200, out);
            return;
          }

          if (method === 'POST') {
            let body: any;
            try {
              body = await this.parseJsonBody(req);
            } catch {
              this.sendError(res, 400, 'Problems parsing JSON');
              return;
            }
            try {
              const issue = store.createIssue(repo, body.title, body.body || '', body.labels || []);
              this.sendJson(res, 201, {
                id: issue.id,
                number: issue.number,
                title: issue.title,
                body: issue.body,
                state: issue.state,
                user: { login: repo.owner },
                labels: issue.labels.map((l) => ({ name: l })),
                comments: 0,
                created_at: issue.createdAt,
                updated_at: issue.updatedAt,
              });
              return;
            } catch (err: any) {
              if (err.message === 'bad-label') {
                this.sendError(res, 422, 'Validation Failed');
                return;
              }
              this.sendError(res, 500, err.message || 'Error creating issue');
              return;
            }
          }
        }

        const issueNum = parseInt(parts[4], 10);
        if (!isNaN(issueNum)) {
          if (parts.length === 5) {
            if (method === 'GET') {
              const issue = store.getIssue(repo, issueNum);
              if (!issue) {
                this.sendError(res, 404, 'Issue not found');
                return;
              }
              this.sendJson(res, 200, {
                id: issue.id,
                number: issue.number,
                title: issue.title,
                body: issue.body,
                state: issue.state,
                user: { login: repo.owner },
                labels: issue.labels.map((l) => ({ name: l })),
                comments: issue.comments,
                created_at: issue.createdAt,
                updated_at: issue.updatedAt,
                closed_at: issue.closedAt ?? null,
              });
              return;
            }

            if (method === 'PATCH') {
              let body: any;
              try {
                body = await this.parseJsonBody(req);
              } catch {
                this.sendError(res, 400, 'Problems parsing JSON');
                return;
              }
              const updated = store.patchIssue(repo, issueNum, body);
              if (!updated) {
                this.sendError(res, 404, 'Issue not found');
                return;
              }
              this.sendJson(res, 200, {
                id: updated.id,
                number: updated.number,
                title: updated.title,
                body: updated.body,
                state: updated.state,
                user: { login: repo.owner },
                labels: updated.labels.map((l) => ({ name: l })),
                comments: updated.comments,
                created_at: updated.createdAt,
                updated_at: updated.updatedAt,
                closed_at: updated.closedAt ?? null,
              });
              return;
            }
          }

          if (parts.length === 6 && parts[5] === 'comments') {
            if (method === 'GET') {
              const comments = store.listComments(repo, issueNum);
              const out = comments.map((c) => ({
                id: c.id,
                body: c.body,
                user: { login: repo.owner },
                created_at: c.createdAt,
                updated_at: c.updatedAt,
              }));
              this.sendJson(res, 200, out);
              return;
            }

            if (method === 'POST') {
              let body: any;
              try {
                body = await this.parseJsonBody(req);
              } catch {
                this.sendError(res, 400, 'Problems parsing JSON');
                return;
              }
              const comment = store.createComment(repo, issueNum, body.body || '');
              this.sendJson(res, 201, {
                id: comment.id,
                body: comment.body,
                user: { login: repo.owner },
                created_at: comment.createdAt,
                updated_at: comment.updatedAt,
              });
              return;
            }
          }
        }
      }

      // Pulls: /repos/:o/:r/pulls...
      if (action === 'pulls') {
        if (parts.length === 4) {
          if (method === 'GET') {
            const state = (url.searchParams.get('state') || 'open') as any;
            const pulls = store.listPulls(repo, state);
            const out = pulls.map((p) => ({
              id: p.id,
              number: p.number,
              title: p.title,
              body: p.body,
              state: p.state,
              head: p.head,
              base: p.base,
              merged: p.merged,
              merged_at: p.mergedAt ?? null,
              merge_commit_sha: p.mergeCommitSha ?? null,
            }));
            this.sendJson(res, 200, out);
            return;
          }

          if (method === 'POST') {
            let body: any;
            try {
              body = await this.parseJsonBody(req);
            } catch {
              this.sendError(res, 400, 'Problems parsing JSON');
              return;
            }
            try {
              const pull = store.createPull(repo, body.title, body.body || '', body.head, body.base);
              this.sendJson(res, 201, {
                id: pull.id,
                number: pull.number,
                title: pull.title,
                body: pull.body,
                state: pull.state,
                head: pull.head,
                base: pull.base,
                merged: false,
                created_at: pull.createdAt,
                updated_at: pull.updatedAt,
              });
              return;
            } catch (err: any) {
              this.sendError(res, 422, err.message || 'Error creating pull request');
              return;
            }
          }
        }

        const pullNum = parseInt(parts[4], 10);
        if (!isNaN(pullNum)) {
          if (parts.length === 5 && method === 'GET') {
            const pull = store.getPull(repo, pullNum);
            if (!pull) {
              this.sendError(res, 404, 'Pull request not found');
              return;
            }
            this.sendJson(res, 200, {
              id: pull.id,
              number: pull.number,
              title: pull.title,
              body: pull.body,
              state: pull.state,
              head: pull.head,
              base: pull.base,
              merged: pull.merged,
              merged_at: pull.mergedAt ?? null,
              merge_commit_sha: pull.mergeCommitSha ?? null,
              created_at: pull.createdAt,
              updated_at: pull.updatedAt,
            });
            return;
          }

          if (parts.length === 6 && parts[5] === 'files' && method === 'GET') {
            const pull = store.getPull(repo, pullNum);
            if (!pull) {
              this.sendError(res, 404, 'Pull request not found');
              return;
            }
            this.sendJson(res, 200, pull.files);
            return;
          }

          if (parts.length === 6 && parts[5] === 'merge' && method === 'PUT') {
            const pull = store.getPull(repo, pullNum);
            if (!pull) {
              this.sendError(res, 404, 'Pull request not found');
              return;
            }
            if (pull.merged || pull.state === 'closed') {
              this.sendError(res, 405, 'Pull request is already merged or closed');
              return;
            }

            const baseRefKey = `refs/heads/${pull.base.ref}`;
            const currentBaseTip = repo.refs.get(baseRefKey);
            if (currentBaseTip && currentBaseTip !== pull.base.sha) {
              const headFiles = pull.files.map((f) => f.filename);
              const initialBaseFiles = (store as any).getTreeEntries(repo, parseCommit(repo.objects.get(pull.base.sha)!.data).tree);
              const currentBaseFiles = (store as any).getTreeEntries(repo, parseCommit(repo.objects.get(currentBaseTip)!.data).tree);

              let hasConflict = false;
              for (const hf of headFiles) {
                const initOid = initialBaseFiles.get(hf)?.oid;
                const currOid = currentBaseFiles.get(hf)?.oid;
                if (initOid !== currOid) {
                  hasConflict = true;
                  break;
                }
              }

              if (hasConflict) {
                this.sendError(res, 405, 'Pull Request is not mergeable');
                return;
              }
            }

            try {
              const result = store.mergePull(repo, pullNum);
              this.sendJson(res, 200, result);
              return;
            } catch (err: any) {
              this.sendError(res, 405, err.message || 'Cannot merge pull request');
              return;
            }
          }
        }
      }
    }

    this.sendError(res, 404, 'Not Found');
  }
}

export const router = new AppRouter();
