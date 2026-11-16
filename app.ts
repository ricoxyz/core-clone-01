import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { router } from './src/http/router.ts';
import { storage } from './src/storage/git.ts';

// Initialize storage
await storage.init();

// Register all routes
await import('./src/http/router.ts');

export function createApp(): Server {
  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    try {
      const handled = await router.handle(req, res);
      if (!handled) {
        router.sendError(res, 404, 'Not Found');
      }
    } catch (err) {
      console.error('Request error:', err);
      router.sendError(res, 500, 'Internal Server Error');
    }
  });
}

// Only start server if this is the main module (not imported)
if (import.meta.url === `file://${process.argv[1]}`) {
  const app = createApp();
  let port = 0;
  
  await new Promise<void>((resolve, reject) => {
    const server = app;
    server.once('error', reject);
    server.listen(0, '0.0.0.0', () => {
      const address = server.address();
      if (address === null || typeof address === 'string') {
        throw new Error('the server has no port');
      }
      port = address.port;
      
      if (port === 8080) throw new Error('the OS picked 8080, which the env owns on one layout: restart the trial');
      
      console.log(`[entry] listening on port ${port}`);
      resolve();
    });
  });
  
  // Register with env
  const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
  if (ENV_URL) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(5000)
      });
      if (res.ok) {
        console.log('[entry] registered');
      }
    } catch (err) {
      console.error('[entry] registration failed:', err);
    }
  }
  
  // Poll for done
  if (ENV_URL) {
    const poll = async () => {
      try {
        const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(2000) });
        if (res.ok) {
          const work = await res.json() as { done?: boolean };
          if (work?.done === true) {
            process.exit(0);
          }
        }
      } catch {}
      setTimeout(poll, 100);
    };
    poll();
  }
}