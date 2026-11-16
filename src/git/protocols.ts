import { createHash } from 'node:crypto';
import { storage } from '../storage/git.ts';

interface PktLine {
  data: string;
}

function encodePktLine(data: string): Buffer {
  const len = data.length + 4;
  const hexLen = len.toString(16).padStart(4, '0');
  return Buffer.from(hexLen + data);
}

function encodePktLineRaw(data: Uint8Array): Buffer {
  const len = data.length + 4;
  const hexLen = len.toString(16).padStart(4, '0');
  return Buffer.concat([Buffer.from(hexLen), data]);
}

function encodeFlushPkt(): Buffer {
  return Buffer.from('0000');
}

function decodePktLines(data: Uint8Array): string[] {
  const lines: string[] = [];
  let offset = 0;
  
  while (offset < data.length) {
    if (offset + 4 > data.length) break;
    const lenHex = Buffer.from(data.subarray(offset, offset + 4)).toString();
    const len = parseInt(lenHex, 16);
    
    if (len === 0) {
      offset += 4;
      break;
    }
    
    if (offset + len > data.length) break;
    
    const line = data.subarray(offset + 4, offset + len).toString();
    lines.push(line);
    offset += len;
  }
  
  return lines;
}

export class UploadPackV2 {
  private repo: string;
  
  constructor(repo: string) {
    this.repo = repo;
  }

  async handleRequest(req: Request): Promise<Response> {
    const url = new URL(req.url);
    const service = url.searchParams.get('service');
    
    if (req.method === 'GET') {
      if (service === 'git-upload-pack') {
        return this.advertiseV2();
      }
      if (service === 'git-upload-pack' && url.searchParams.get('protocol') === 'version=2') {
        return this.advertiseV2();
      }
    }
    
    if (req.method === 'POST') {
      if (service === 'git-upload-pack') {
        const body = await req.arrayBuffer();
        return this.processUploadPack(new Uint8Array(body));
      }
    }
    
    return new Response('Not Found', { status: 404 });
  }

  private async advertiseV2(): Promise<Response> {
    const refs = await this.getAllRefs();
    
    let output = '';
    output += 'version 2\n';
    
    // Advertise capabilities
    output += 'agent=makefun-git/1.0\n';
    output += 'ls-refs\n';
    output += 'fetch\n';
    output += 'server-option\n';
    output += '\n';
    
    // Advertise refs
    for (const [ref, oid] of Object.entries(refs)) {
      output += `${ref} ${oid}\n`;
    }
    
    return new Response(output, {
      headers: {
        'Content-Type': 'application/x-git-upload-pack-advertisement',
        'Cache-Control': 'no-cache'
      }
    });
  }

  private async getAllRefs(): Promise<Record<string, string>> {
    // This would integrate with the storage layer
    return {};
  }

  private async processUploadPack(body: Uint8Array): Promise<Response> {
    const lines = decodePktLines(body);
    
    // Parse the request
    let command = '';
    const args: string[] = [];
    
    for (const line of lines) {
      if (line.startsWith('command=')) {
        command = line.slice(8);
      } else if (line.startsWith('argument=')) {
        args.push(line.slice(9));
      }
    }
    
    if (command === 'ls-refs') {
      return this.handleLsRefs(args);
    }
    
    if (command === 'fetch') {
      return this.handleFetch(args);
    }
    
    return new Response('Not Implemented', { status: 501 });
  }

  private async handleLsRefs(args: string[]): Promise<Response> {
    // Implementation for ls-refs
    const refs = await this.getAllRefs();
    
    let output = '';
    for (const [ref, oid] of Object.entries(refs)) {
      output += `${oid} ${ref}\n`;
    }
    output += '\n';
    
    return new Response(output, {
      headers: { 'Content-Type': 'application/x-git-upload-pack-result' }
    });
  }

  private async handleFetch(args: string[]): Promise<Response> {
    // This is complex - would need to implement pack file generation
    return new Response('Not Implemented', { status: 501 });
  }
}

export class ReceivePack {
  private repo: string;
  
  constructor(repo: string) {
    this.repo = repo;
  }

  async handleRequest(req: Request): Promise<Response> {
    const url = new URL(req.url);
    const service = url.searchParams.get('service');
    
    if (req.method === 'GET' && service === 'git-receive-pack') {
      return this.advertise();
    }
    
    if (req.method === 'POST' && service === 'git-receive-pack') {
      const body = await req.arrayBuffer();
      return this.processReceivePack(new Uint8Array(body));
    }
    
    return new Response('Not Found', { status: 404 });
  }

  private async advertise(): Promise<Response> {
    const refs = await this.getAllRefs();
    
    let output = '# service=git-receive-pack\n';
    output += '0000';
    
    for (const [ref, oid] of Object.entries(refs)) {
      output += `${oid} ${ref}\n`;
    }
    
    return new Response(output, {
      headers: {
        'Content-Type': 'application/x-git-receive-pack-advertisement',
        'Cache-Control': 'no-cache'
      }
    });
  }

  private async getAllRefs(): Promise<Record<string, string>> {
    return {};
  }

  private async processReceivePack(body: Uint8Array): Promise<Response> {
    // Handle receive-pack v0
    return new Response('Not Implemented', { status: 501 });
  }
}