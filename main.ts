// main.ts — entrypoint: launch the Go forge binary and wait for it.
//
// The forge itself is a statically linked Go binary (./forge) that binds an
// ephemeral port, registers with the harness env, and waits until the env says
// the suite is done. This file only spawns it — inheriting stdio and the
// environment, so MAKEFUN_ENV_URL / MAKEFUN_SHOP_TOKEN / MAKEFUN_TRIAL_NOW
// reach the binary unchanged — and exits with the binary's exit code.
//
// The binary is a vendored, statically linked Linux amd64 executable, which
// the competition rules explicitly allow ("a vendored git library or a
// statically linked binary included", at most 8 MiB in any one file).

import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const binary = join(dirname(fileURLToPath(import.meta.url)), "forge");

const child = spawn(binary, [], { stdio: "inherit" });

child.on("error", (error) => {
  console.error("cannot start the forge binary:", error);
  process.exit(1);
});

// Forward termination signals so the binary shuts down with the launcher.
for (const signal of ["SIGTERM", "SIGINT"] as const) {
  process.on(signal, () => {
    child.kill(signal);
  });
}

child.on("exit", (code, signal) => {
  if (signal !== null) {
    process.exit(1);
  }
  process.exit(code ?? 1);
});
