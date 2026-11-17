// The forge's HTTP server: one in-memory store, four surfaces (see src/dispatch.ts).
//
// Two transports serve the same dispatch:
//   - node:http (default) — the server the entry has always run on;
//   - raw node:net HTTP/1.1 (prototype #2, behind the FORGE_RAW_HTTP flag) — see src/rawhttp.ts.
//
// Both return the same minimal server surface (ForgeServer): listen on a port,
// address, close, closeAllConnections.

import { createServer } from 'node:http';
import { Store } from './src/store.ts';
import { dispatch } from './src/dispatch.ts';
import { createRawApp, type ForgeServerLike } from './src/rawhttp.ts';

/** The minimal server surface main.ts and the profiling launcher use. */
export type ForgeServer = ForgeServerLike;

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): ForgeServer {
  // Prototype #2 (raw node:net HTTP/1.1) behind a flag; node:http stays the fallback.
  // The flag is presence-based (any non-empty value other than 0/false enables it):
  // grade-local can only pass entry variables through --dotenv secrets, whose values
  // must be >= 8 chars, so the grading value is an arbitrary token, never '1'.
  const rawFlag = process.env.FORGE_RAW_HTTP;
  if (rawFlag !== undefined && rawFlag !== '' && rawFlag.toLowerCase() !== '0' && rawFlag.toLowerCase() !== 'false') {
    return createRawApp();
  }

  const store = new Store();
  const server = createServer((req, res) => {
    void dispatch(store, req, res);
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
