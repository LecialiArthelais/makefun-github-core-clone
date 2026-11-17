// The forge's HTTP server: one in-memory store, four surfaces.
//   GET  /healthz                       -> 200 {"ok":true} (the env's boot check, twice)
//   /{owner}/{repo}.git/*               -> git smart HTTP (src/git.ts)
//   /api/v3/*                           -> REST v3 (src/rest.ts)
//   POST /api/graphql                   -> GraphQL (src/graphql.ts)
//   POST /mcp                           -> MCP (src/mcp.ts)
//   anything else                       -> JSON 404
//
// A handler that throws is answered with a JSON 500 and the server stays up (the suite's
// repo-survives-errors check), never a crash and never a hung request.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './src/store.ts';
import { sendJson, sendNotFound, pathOf } from './src/util.ts';
import { handleRest } from './src/rest.ts';
import { handleGraphQL } from './src/graphql.ts';
import { handleMcp } from './src/mcp.ts';
import { handleGit } from './src/git.ts';

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  const store = new Store();
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    void (async () => {
      try {
        const path = pathOf(req);
        const method = req.method ?? 'GET';
        const segments = path.split('/').filter((s) => s !== '');

        // The env's boot check: 200 {"ok":true}, no auth.
        if (method === 'GET' && path === '/healthz') {
          sendJson(res, 200, { ok: true });
          return;
        }

        // GraphQL.
        if (path === '/api/graphql') {
          if (await handleGraphQL(store, req, res)) return;
          sendNotFound(res, 'Not Found');
          return;
        }

        // MCP.
        if (path === '/mcp') {
          if (await handleMcp(store, req, res)) return;
          sendNotFound(res, 'Not Found');
          return;
        }

        // REST v3 (the /api/v3 root is segments.length === 0 after the prefix).
        if (segments[0] === 'api' && segments[1] === 'v3') {
          if (await handleRest(store, req, res, segments.slice(2))) return;
          sendNotFound(res, 'Not Found');
          return;
        }

        // Git smart HTTP: /{owner}/{repo}.git/...
        if (segments.length >= 2 && segments[1] !== undefined && segments[1].endsWith('.git')) {
          if (await handleGit(store, req, res, segments)) return;
          sendNotFound(res, 'Not Found');
          return;
        }

        sendNotFound(res, 'Not Found');
      } catch (err) {
        // Never crash the server and never hang the request: a JSON 500.
        if (!res.headersSent) {
          try {
            sendJson(res, 500, { message: err instanceof Error ? err.message : 'Internal Server Error' });
          } catch {
            // The socket is gone; nothing more to do.
          }
        } else {
          try { res.end(); } catch { /* already closing */ }
        }
      }
    })();
  });
  // S0: longer keep-alive (30 s, was Node's 5 s default) so the 50 concurrent clone
  // sockets are not torn down and rebuilt between rounds; explicit TCP_NODELAY so small
  // responses are not held for Nagle coalescing.
  server.keepAliveTimeout = 30000;
  server.on('connection', (socket) => {
    socket.setNoDelay(true);
  });
  return server;
}
