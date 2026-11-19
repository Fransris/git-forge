// The entrypoint: Node 22 supervises a vendored, statically linked Go forge. Two builds ship — one for
// x86-64 and one for arm64 — and this file starts the one the running machine can execute. The binary
// binds an ephemeral port, registers it with the env and serves until the trial ends; this file only
// starts it, passes the environment through, forwards termination, and exits with the binary's code.

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));

const candidates = process.arch === 'arm64' ? ['forge-arm64', 'forge'] : ['forge-amd64', 'forge'];
const binary = candidates.map((name) => join(here, name)).find((path) => existsSync(path));

if (binary === undefined) {
  process.stderr.write(`[entry] no forge binary for ${process.arch}\n`);
  process.exit(1);
}

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });

for (const signal of ['SIGTERM', 'SIGINT'] as const) {
  process.on(signal, () => {
    child.kill(signal);
  });
}

child.on('exit', (code, signal) => {
  if (signal !== null) process.exit(0);
  process.exit(code ?? 0);
});

child.on('error', (err) => {
  process.stderr.write(`[entry] cannot start ${binary}: ${err.message}\n`);
  process.exit(1);
});
