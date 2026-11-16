// main.ts — entrypoint: bind the forge, register it with the harness env,
// and wait until the env says the suite is done.
//
// The harness runs a small env server next to this process and tells us
// where it lives through MAKEFUN_ENV_URL. The dance is four steps:
//
//   1. listen on a kernel-chosen (ephemeral) port — never 8080, which is
//      the env's own port on the same loopback;
//   2. POST {"port": <ours>} to <env>/register so the suite can reach us;
//   3. poll GET <env>/done until it answers {"done": true};
//   4. shut the server down and leave with code 0.
//
// When MAKEFUN_ENV_URL is not set there is no env to talk to, so the
// process leaves with code 1 at once. The forge itself lives in app.ts;
// this file only runs the lifecycle. Node standard library only — no
// dependencies, no git binary, no network beyond the env.

import { createApp } from "./app.ts";
import type { Server } from "node:http";

const ENV_VAR = "MAKEFUN_ENV_URL";
const REGISTER_PATH = "/register";
const DONE_PATH = "/done";
const REGISTER_FIRST_WAIT_MS = 100;
const REGISTER_MAX_WAIT_MS = 2000;
const DONE_POLL_MS = 250;

/** Pause for `ms` milliseconds. */
function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** POST a JSON body and report the HTTP status, or null when the request fails outright. */
async function postJson(url: string, body: unknown): Promise<number | null> {
  try {
    const response = await fetch(url, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
    return response.status;
  } catch {
    return null;
  }
}

/** GET a URL and parse its JSON body, or null when the request or the parse fails. */
async function getJson(url: string): Promise<Record<string, unknown> | null> {
  try {
    const response = await fetch(url);
    if (!response.ok) return null;
    return (await response.json()) as Record<string, unknown>;
  } catch {
    return null;
  }
}

/** Bind `server` to an ephemeral port and return the port the kernel picked. */
async function bindEphemeral(server: Server): Promise<number> {
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, () => {
      server.removeListener("error", reject);
      resolve();
    });
  });
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("the server did not bind to a TCP port");
  }
  return address.port;
}

/**
 * Tell the env which port we bound. The first register wins: a 409 means
 * another process already registered, and it does not move the base URL,
 * so 409 counts as done. Anything else that is not a 2xx, or a request
 * that fails outright, is retried with exponential backoff.
 */
async function register(envUrl: string, port: number): Promise<void> {
  const target = `${envUrl}${REGISTER_PATH}`;
  let waitMs = REGISTER_FIRST_WAIT_MS;
  for (;;) {
    const status = await postJson(target, { port });
    if (status !== null && (status < 300 || status === 409)) return;
    await sleep(waitMs);
    waitMs = Math.min(waitMs * 2, REGISTER_MAX_WAIT_MS);
  }
}

/**
 * Poll the env until it reports the suite finished. Each poll is a single
 * short GET that answers at once; the request is never held open.
 */
async function waitForDone(envUrl: string): Promise<void> {
  const target = `${envUrl}${DONE_PATH}`;
  for (;;) {
    const body = await getJson(target);
    if (body !== null && body["done"] === true) return;
    await sleep(DONE_POLL_MS);
  }
}

/** Run the lifecycle. Returns the process exit code. */
async function main(): Promise<number> {
  const envUrl = process.env[ENV_VAR];
  if (envUrl === undefined || envUrl === "") {
    console.error(`cannot start: ${ENV_VAR} is not set`);
    return 1;
  }
  const server = createApp();
  const port = await bindEphemeral(server);
  await register(envUrl, port);
  await waitForDone(envUrl);
  server.close();
  server.closeAllConnections();
  return 0;
}

const exitCode = await main().catch((error: unknown) => {
  console.error("fatal error in the entrypoint:", error);
  return 1;
});
process.exit(exitCode);
