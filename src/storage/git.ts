import { createHash } from 'node:crypto';
import { readFile, writeFile, mkdir, rm, readdir, stat } from 'node:fs/promises';
import { join, resolve } from 'node:path';

export interface GitObject {
  type: 'commit' | 'tree' | 'blob' | 'tag';
  content: Uint8Array;
  size: number;
  oid: string;
}

export interface RefUpdate {
  ref: string;
  oldOid: string | null;
  newOid: string | null;
}

export interface PushResult {
  ref: string;
  ok: boolean;
  error?: string;
}

export class GitStorage {
  private basePath: string;
  private objectsPath: string;
  private refsPath: string;
  private reposPath: string;

  constructor(basePath: string = './data') {
    this.basePath = resolve(basePath);
    this.objectsPath = join(this.basePath, 'objects');
    this.refsPath = join(this.basePath, 'refs');
    this.reposPath = join(this.basePath, 'repos');
  }

  async init(): Promise<void> {
    await mkdir(this.objectsPath, { recursive: true });
    await mkdir(this.refsPath, { recursive: true });
    await mkdir(this.reposPath, { recursive: true });
  }

  private objectPath(oid: string): string {
    const dir = oid.slice(0, 2);
    const file = oid.slice(2);
    return join(this.objectsPath, dir, file);
  }

  private refFilePath(repo: string, ref: string): string {
    return join(this.refsPath, repo, ref);
  }

  async hasObject(oid: string): Promise<boolean> {
    try {
      await stat(this.objectPath(oid));
      return true;
    } catch {
      return false;
    }
  }

  async getObject(oid: string): Promise<GitObject | null> {
    try {
      const data = await readFile(this.objectPath(oid));
      const typeEnd = data.indexOf(0x20);
      const sizeEnd = data.indexOf(0x00, typeEnd + 1);
      const type = data.subarray(0, typeEnd).toString();
      const size = parseInt(data.subarray(typeEnd + 1, sizeEnd).toString(), 10);
      const content = data.subarray(sizeEnd + 1);
      return {
        type: type as GitObject['type'],
        content,
        size,
        oid
      };
    } catch {
      return null;
    }
  }

  async writeObject(obj: Omit<GitObject, 'oid'>): Promise<string> {
    const header = `${obj.type} ${obj.content.length}\0`;
    const data = Buffer.concat([Buffer.from(header), obj.content]);
    const oid = createHash('sha1').update(data).digest('hex');
    
    const path = this.objectPath(oid);
    await mkdir(join(this.objectsPath, oid.slice(0, 2)), { recursive: true });
    await writeFile(path, data);
    
    return oid;
  }

  async readRef(repo: string, ref: string): Promise<string | null> {
    try {
      const content = await readFile(this.refFilePath(repo, ref), 'utf8');
      return content.trim();
    } catch {
      return null;
    }
  }

  async writeRef(repo: string, ref: string, oid: string): Promise<void> {
    const path = this.refFilePath(repo, ref);
    await mkdir(resolve(path, '..'), { recursive: true });
    await writeFile(path, oid + '\n');
  }

  async updateRefs(repo: string, updates: RefUpdate[]): Promise<PushResult[]> {
    const results: PushResult[] = [];
    
    for (const update of updates) {
      const refPath = this.refFilePath(repo, update.ref);
      const currentOid = await this.readRef(repo, update.ref);
      
      if (update.oldOid !== null && currentOid !== update.oldOid) {
        results.push({ ref: update.ref, ok: false, error: `non-fast-forward: expected ${update.oldOid}, got ${currentOid}` });
        continue;
      }
      
      if (update.newOid === null) {
        // Delete ref
        try {
          await rm(this.refFilePath(repo, update.ref));
          results.push({ ref: update.ref, ok: true });
        } catch {
          results.push({ ref: update.ref, ok: false, error: 'ref not found' });
        }
      } else {
        // Update ref
        await this.writeRef(repo, update.ref, update.newOid);
        results.push({ ref: update.ref, ok: true });
      }
    }
    
    return results;
  }

  async listRefs(repo: string): Promise<Record<string, string>> {
    const refs: Record<string, string> = {};
    const repoRefsPath = join(this.refsPath, repo);
    
    try {
      const entries = await readdir(repoRefsPath, { recursive: true });
      for (const entry of entries) {
        const fullPath = join(repoRefsPath, entry);
        const fileStat = await stat(fullPath);
        if (fileStat.isFile()) {
          const content = await readFile(fullPath, 'utf8');
          refs[entry.replace(/\\/g, '/')] = content.trim();
        }
      }
    } catch {
      // No refs
    }
    
    return refs;
  }

  async createRepo(owner: string, name: string): Promise<void> {
    const repoPath = join(this.reposPath, owner, name);
    await mkdir(join(repoPath, 'objects'), { recursive: true });
    await mkdir(join(repoPath, 'refs', 'heads'), { recursive: true });
    await mkdir(join(repoPath, 'refs', 'tags'), { recursive: true });
    await mkdir(join(repoPath, 'info'), { recursive: true });
    
    // Initialize HEAD
    await writeFile(join(repoPath, 'HEAD'), 'ref: refs/heads/main\n');
    await writeFile(join(repoPath, 'config'), '[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n');
    
    // Initialize empty refs
    await this.writeRef(`${owner}/${name}`, 'heads/main', '0'.repeat(40));
  }

  async repoExists(owner: string, name: string): Promise<boolean> {
    try {
      await stat(join(this.reposPath, owner, name));
      return true;
    } catch {
      return false;
    }
  }
}

export const storage = new GitStorage();