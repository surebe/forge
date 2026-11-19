// Entry wrapper: the grader runs `node /bot/main.ts`; the forge itself is the statically linked
// Go binary committed beside this file as ./forge. This wrapper starts it with this process's
// environment (MAKEFUN_ENV_URL, MAKEFUN_SHOP_TOKEN, MAKEFUN_TRIAL_SEED, ...) untouched, forwards
// its output, and exits with its exit code. The binary owns the lifecycle: it binds an ephemeral
// port, POSTs {"port": N} to $MAKEFUN_ENV_URL/register, polls $MAKEFUN_ENV_URL/done until the env
// says the trial is over, then exits 0.
//
// main.ts imports only Node built-ins, per the contest's "standard library and committed source"
// rule. If the binary cannot be started at all, that is reported on stderr and the exit is 1.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const binaryPath = fileURLToPath(new URL('./forge', import.meta.url));

const child = spawn(binaryPath, [], { stdio: 'inherit', env: process.env });

child.on('error', (err) => {
  process.stderr.write(`[entry] could not start the forge binary: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal !== null) {
    process.stderr.write(`[entry] the forge binary was ended by signal ${signal}\n`);
    process.exit(1);
    return;
  }
  process.exit(code ?? 1);
});
