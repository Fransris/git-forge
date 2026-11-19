// The entrypoint: Node 22 supervises the vendored static forge binary. The binary binds an ephemeral
// port, registers it with the env and polls until the trial is over; this file only starts it, passes
// the environment through, forwards termination, and exits with the binary's own code.

import { spawn } from 'node:child_process';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const binary = join(here, 'forge');

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
  process.stderr.write(`[entry] cannot start the forge binary: ${err.message}\n`);
  process.exit(1);
});
