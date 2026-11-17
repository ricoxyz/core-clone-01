import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { router } from './src/http/router.ts';

export function createApp(): Server {
  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    try {
      await router.handle(req, res);
    } catch (err) {
      console.error('[forge error]', err);
      if (!res.headersSent) {
        router.sendError(res, 500, 'Internal Server Error');
      }
    }
  });
}
