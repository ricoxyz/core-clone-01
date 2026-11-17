import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  type GitObject,
  collectReachable,
  collectShallowReachable,
  decodePktLines,
  encodePktLine,
  encodePktLines,
  hashObject,
  isObjectId,
  parseCommit,
  parseTag,
  parseTree,
  PktLineDecoder,
  pktLineDelim,
  pktLineFlush,
  readPack,
  writePack,
  ZERO_OID,
} from './git-engine.ts';
import { store, type StoredRepo } from '../storage/forge-store.ts';

export async function readRequestBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }
  return Buffer.concat(chunks);
}

export async function handleInfoRefs(
  req: IncomingMessage,
  res: ServerResponse,
  owner: string,
  repoName: string,
): Promise<void> {
  const repo = store.getRepo(owner, repoName.replace(/\.git$/, ''));
  if (!repo) {
    res.writeHead(404, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ message: 'Repository not found' }));
    return;
  }

  const url = new URL(req.url ?? '/', `http://${req.headers.host}`);
  const service = url.searchParams.get('service');
  const gitProtocol = req.headers['git-protocol'] as string | undefined;

  if (service === 'git-upload-pack') {
    res.writeHead(200, {
      'content-type': 'application/x-git-upload-pack-advertisement',
      'cache-control': 'no-cache',
    });

    if (gitProtocol && gitProtocol.includes('version=2')) {
      const body = encodePktLines([
        'version 2\n',
        'agent=makefun-git/1.0\n',
        'ls-refs=peel symrefs\n',
        'fetch=shallow\n',
        'server-option\n',
        'flush',
      ]);
      res.end(body);
      return;
    }

    const parts: Buffer[] = [
      encodePktLine('# service=git-upload-pack\n'),
      pktLineFlush(),
    ];

    const caps = 'multi_ack side-band-64k ofs-delta shallow agent=makefun-git/1.0';
    const allRefs = [...repo.refs.entries()].filter(([k]) => k.startsWith('refs/'));

    if (allRefs.length === 0) {
      parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    } else {
      const headTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? allRefs[0][1];
      const headSymref = `symref=HEAD:refs/heads/${repo.defaultBranch}`;
      parts.push(encodePktLine(`${headTip} HEAD\0${headSymref} ${caps}\n`));
      for (const [name, oid] of allRefs) {
        parts.push(encodePktLine(`${oid} ${name}\n`));
      }
    }
    parts.push(pktLineFlush());
    res.end(Buffer.concat(parts));
    return;
  }

  if (service === 'git-receive-pack') {
    res.writeHead(200, {
      'content-type': 'application/x-git-receive-pack-advertisement',
      'cache-control': 'no-cache',
    });

    const parts: Buffer[] = [
      encodePktLine('# service=git-receive-pack\n'),
      pktLineFlush(),
    ];

    const caps = 'report-status report-status-v2 delete-refs side-band-64k ofs-delta agent=makefun-git/1.0';
    const allRefs = [...repo.refs.entries()].filter(([k]) => k.startsWith('refs/'));

    if (allRefs.length === 0) {
      parts.push(encodePktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    } else {
      let first = true;
      for (const [name, oid] of allRefs) {
        if (first) {
          parts.push(encodePktLine(`${oid} ${name}\0${caps}\n`));
          first = false;
        } else {
          parts.push(encodePktLine(`${oid} ${name}\n`));
        }
      }
    }
    parts.push(pktLineFlush());
    res.end(Buffer.concat(parts));
    return;
  }

  res.writeHead(400, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ message: 'Unknown service' }));
}

export async function handleUploadPack(
  req: IncomingMessage,
  res: ServerResponse,
  owner: string,
  repoName: string,
): Promise<void> {
  const repo = store.getRepo(owner, repoName.replace(/\.git$/, ''));
  if (!repo) {
    res.writeHead(404, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ message: 'Repository not found' }));
    return;
  }

  const rawBody = await readRequestBody(req);
  const decoder = new PktLineDecoder(rawBody);
  decoder.finish();

  const packets: any[] = [];
  try {
    for (let p = decoder.next(); p !== null; p = decoder.next()) {
      packets.push(p);
    }
  } catch {
    res.writeHead(400, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ message: 'Malformed pkt-line' }));
    return;
  }

  let command = '';
  const args: string[] = [];
  for (const pkt of packets) {
    if (pkt.kind === 'data') {
      const line = pkt.payload.toString('utf8').replace(/\n$/, '');
      if (line.startsWith('command=')) command = line.slice(8);
      else args.push(line);
    }
  }

  if (!command && args.some((a) => a.startsWith('want '))) {
    command = 'fetch';
  }

  res.writeHead(200, {
    'content-type': 'application/x-git-upload-pack-result',
    'cache-control': 'no-cache',
  });

  if (command === 'ls-refs') {
    const peel = args.includes('peel');
    const symrefs = args.includes('symrefs');
    const prefixes = args.filter((a) => a.startsWith('ref-prefix ')).map((a) => a.slice(11));

    const matchesPrefix = (name: string): boolean => {
      if (prefixes.length === 0) return true;
      return prefixes.some((p) => name.startsWith(p));
    };

    const allRefs = [...repo.refs.entries()].filter(([k]) => k.startsWith('refs/'));
    const parts: Buffer[] = [];

    if (allRefs.length === 0) {
      if (symrefs && matchesPrefix('HEAD')) {
        parts.push(encodePktLine(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
      }
      parts.push(pktLineFlush());
      res.end(Buffer.concat(parts));
      return;
    }

    if (matchesPrefix('HEAD')) {
      const headTip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      if (headTip) {
        let headLine = `${headTip} HEAD`;
        if (symrefs) headLine += ` symref-target:refs/heads/${repo.defaultBranch}`;
        parts.push(encodePktLine(`${headLine}\n`));
      }
    }

    for (const [name, oid] of allRefs) {
      if (!matchesPrefix(name)) continue;

      let line = `${oid} ${name}`;
      if (peel && name.startsWith('refs/tags/')) {
        const tagObj = repo.objects.get(oid);
        if (tagObj && tagObj.type === 'tag') {
          const parsed = parseTag(tagObj.data);
          line += ` peeled:${parsed.object}`;
        }
      }
      parts.push(encodePktLine(`${line}\n`));
    }
    parts.push(pktLineFlush());
    res.end(Buffer.concat(parts));
    return;
  }

  if (command === 'fetch') {
    const wants: string[] = [];
    const haves: string[] = [];
    const clientShallows: string[] = [];
    let depth: number | null = null;
    let isV0 = !rawBody.toString('utf8').includes('command=fetch');

    for (const a of args) {
      if (a.startsWith('want ')) {
        const tokens = a.slice(5).split(' ');
        if (isObjectId(tokens[0])) wants.push(tokens[0]);
      } else if (a.startsWith('have ')) {
        const tokens = a.slice(5).split(' ');
        if (isObjectId(tokens[0])) haves.push(tokens[0]);
      } else if (a.startsWith('shallow ')) {
        const tokens = a.slice(8).split(' ');
        if (isObjectId(tokens[0])) clientShallows.push(tokens[0]);
      } else if (a.startsWith('deepen ')) {
        depth = parseInt(a.slice(7), 10);
      }
    }

    let objectsToSend: GitObject[] = [];
    let newShallowList: string[] = [];
    let unshallowList: string[] = [];

    // Helper: collect complete reachability from haves (commits, trees, blobs)
    // stopping commit parent traversal at client shallow boundaries
    const collectHavesReachable = (): Set<string> => {
      const seen = new Set<string>();
      const queue = [...haves];
      const stopAt = new Set<string>(clientShallows);

      while (queue.length) {
        const oid = queue.pop()!;
        if (seen.has(oid)) continue;
        const obj = repo.objects.get(oid);
        if (!obj) continue;
        seen.add(oid);

        if (obj.type === 'commit') {
          const parsed = parseCommit(obj.data);
          queue.push(parsed.tree);
          if (!stopAt.has(oid)) {
            for (const p of parsed.parents) queue.push(p);
          }
        } else if (obj.type === 'tree') {
          for (const entry of parseTree(obj.data)) {
            if (entry.mode !== '160000') queue.push(entry.oid);
          }
        } else if (obj.type === 'tag') {
          queue.push(parseTag(obj.data).object);
        }
      }
      return seen;
    };

    if (depth !== null && depth > 0) {
      const shallowResult = collectShallowReachable(wants, (id) => repo.objects.get(id), depth);
      newShallowList = shallowResult.shallow;

      for (const s of clientShallows) {
        if (!newShallowList.includes(s) && shallowResult.objects.has(s)) {
          unshallowList.push(s);
        }
      }

      const havesReachable = collectHavesReachable();
      for (const oid of shallowResult.objects) {
        if (!havesReachable.has(oid)) {
          const obj = repo.objects.get(oid);
          if (obj) objectsToSend.push(obj);
        }
      }
    } else {
      const havesReachable = collectHavesReachable();
      const wantsReachable = collectReachable(wants, (id) => repo.objects.get(id));
      for (const oid of wantsReachable) {
        if (!havesReachable.has(oid)) {
          const obj = repo.objects.get(oid);
          if (obj) objectsToSend.push(obj);
        }
      }
    }

    const packBytes = writePack(objectsToSend);
    const parts: Buffer[] = [];

    if (!isV0) {
      parts.push(encodePktLine('acknowledgments\n'));
      parts.push(encodePktLine('ready\n'));
      parts.push(pktLineDelim());

      if (newShallowList.length > 0 || unshallowList.length > 0) {
        parts.push(encodePktLine('shallow-info\n'));
        for (const s of newShallowList) {
          parts.push(encodePktLine(`shallow ${s}\n`));
        }
        for (const u of unshallowList) {
          parts.push(encodePktLine(`unshallow ${u}\n`));
        }
        parts.push(pktLineDelim());
      }

      parts.push(encodePktLine('packfile\n'));

      const CHUNK_SIZE = 65515;
      for (let offset = 0; offset < packBytes.length; offset += CHUNK_SIZE) {
        const chunk = packBytes.subarray(offset, offset + CHUNK_SIZE);
        const payload = Buffer.concat([Buffer.from([1]), chunk]);
        parts.push(encodePktLine(payload));
      }
      parts.push(pktLineFlush());
    } else {
      if (newShallowList.length > 0 || unshallowList.length > 0) {
        for (const s of newShallowList) {
          parts.push(encodePktLine(`shallow ${s}\n`));
        }
        for (const u of unshallowList) {
          parts.push(encodePktLine(`unshallow ${u}\n`));
        }
        parts.push(pktLineFlush());
      }
      parts.push(encodePktLine('NAK\n'));

      const CHUNK_SIZE = 65515;
      for (let offset = 0; offset < packBytes.length; offset += CHUNK_SIZE) {
        const chunk = packBytes.subarray(offset, offset + CHUNK_SIZE);
        const payload = Buffer.concat([Buffer.from([1]), chunk]);
        parts.push(encodePktLine(payload));
      }
      parts.push(pktLineFlush());
    }

    res.end(Buffer.concat(parts));
    return;
  }

  res.end(pktLineFlush());
}

export async function handleReceivePack(
  req: IncomingMessage,
  res: ServerResponse,
  owner: string,
  repoName: string,
): Promise<void> {
  const repo = store.getRepo(owner, repoName.replace(/\.git$/, ''));
  if (!repo) {
    res.writeHead(404, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ message: 'Repository not found' }));
    return;
  }

  const rawBody = await readRequestBody(req);
  const decoder = new PktLineDecoder(rawBody);

  interface PushCmd {
    oldOid: string;
    newOid: string;
    ref: string;
  }

  const commands: PushCmd[] = [];
  let clientCaps: string[] = [];

  for (let p = decoder.next(); p !== null; p = decoder.next()) {
    if (p.kind === 'flush') break;
    if (p.kind === 'data') {
      const line = p.payload.toString('utf8').replace(/\n$/, '');
      if (commands.length === 0 && line.includes('\0')) {
        const [cmdPart, capsPart] = line.split('\0');
        clientCaps = capsPart.split(' ');
        const [oldOid, newOid, ref] = cmdPart.split(' ');
        commands.push({ oldOid, newOid, ref });
      } else {
        const [oldOid, newOid, ref] = line.split(' ');
        commands.push({ oldOid, newOid, ref });
      }
    }
  }

  const packData = decoder.remainder();
  let unpackOk = true;
  let unpackError = '';

  if (packData.length > 0) {
    try {
      const readResult = readPack(packData, {
        resolveBase: (oid) => repo.objects.get(oid),
      });
      for (const [oid, obj] of readResult.objects.entries()) {
        repo.objects.set(oid, obj);
      }
    } catch (err: any) {
      unpackOk = false;
      unpackError = err.message || 'pack-corrupt';
    }
  }

  const refReports: Array<{ ref: string; ok: boolean; reason?: string }> = [];

  if (unpackOk) {
    for (const cmd of commands) {
      const currentOid = repo.refs.get(cmd.ref);

      if (cmd.newOid === ZERO_OID) {
        if (currentOid && cmd.oldOid !== ZERO_OID && currentOid !== cmd.oldOid) {
          refReports.push({ ref: cmd.ref, ok: false, reason: 'stale-info' });
        } else {
          repo.refs.delete(cmd.ref);
          refReports.push({ ref: cmd.ref, ok: true });
        }
      } else if (cmd.oldOid === ZERO_OID) {
        if (currentOid && currentOid !== ZERO_OID) {
          refReports.push({ ref: cmd.ref, ok: false, reason: 'already exists' });
        } else {
          repo.refs.set(cmd.ref, cmd.newOid);
          refReports.push({ ref: cmd.ref, ok: true });
        }
      } else {
        if (currentOid && currentOid !== cmd.oldOid) {
          refReports.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
        } else {
          const ancestors = collectReachable([cmd.newOid], (id) => repo.objects.get(id), { allowMissing: true });
          if (!ancestors.has(cmd.oldOid)) {
            refReports.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
          } else {
            repo.refs.set(cmd.ref, cmd.newOid);
            refReports.push({ ref: cmd.ref, ok: true });
          }
        }
      }
    }
  }

  const sideband = clientCaps.includes('side-band-64k');
  const reportLines: Array<string | 'flush'> = [];

  if (unpackOk) {
    reportLines.push('unpack ok\n');
  } else {
    reportLines.push(`unpack ${unpackError}\n`);
  }

  for (const r of refReports) {
    if (r.ok) {
      reportLines.push(`ok ${r.ref}\n`);
    } else {
      reportLines.push(`ng ${r.ref} ${r.reason || 'failed'}\n`);
    }
  }
  reportLines.push('flush');

  const reportPktBytes = encodePktLines(reportLines);

  res.writeHead(200, {
    'content-type': 'application/x-git-receive-pack-result',
    'cache-control': 'no-cache',
  });

  if (sideband) {
    const payload = Buffer.concat([Buffer.from([1]), reportPktBytes]);
    const responseBody = Buffer.concat([encodePktLine(payload), pktLineFlush()]);
    res.end(responseBody);
  } else {
    res.end(reportPktBytes);
  }
}
