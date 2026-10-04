// Runs the integration suite in a real VS Code (downloaded by
// @vscode/test-electron into .vscode-test/) against a freshly built sy and
// a scripted Claude CLI (fakeClaude.ts): no quota is used, and nothing
// touches your own VS Code profile or sy state.
//
//   npm run test:integration
//
// Environment:
//   SY_EXE          use this sy instead of building one (go build ./cmd/sy)
//   VSCODE_VERSION  VS Code to test with: "stable" (default), "insiders" or e.g. "1.90.0"
//   SY_VSCODE_TEST_DIR  scratch folder (default: %TEMP%/sy-vscode-test)
//   SY_VSCODE_TEST_KEEP=1  keep this run's folder even when the tests pass
//
// Everything a run creates lives in <scratch>/run-<time>: the VS Code user
// data and extensions folders, sy's APPDATA/LOCALAPPDATA (config, task
// state, worktree pool), the fake CLI and the test repository.

import { execFileSync } from 'child_process';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';
import { runTests } from '@vscode/test-electron';
import { NOTES, NOTES_LINES } from './fakeClaude.consts';

const win = process.platform === 'win32';

function git(cwd: string, ...args: string[]): void {
  execFileSync('git', args, { cwd, stdio: 'pipe' });
}

/** sy processes started from exe that are still running. */
export function syProcesses(exe: string): number[] {
  if (!win) {
    try {
      return execFileSync('pgrep', ['-f', exe], { encoding: 'utf8' }).split('\n').filter(Boolean).map(Number);
    } catch {
      return [];
    }
  }
  const ps = `Get-CimInstance Win32_Process -Filter "Name='${path.basename(exe)}'" | Where-Object { $_.ExecutablePath -eq '${exe.replace(/'/g, "''")}' } | ForEach-Object { $_.ProcessId }`;
  const out = execFileSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', ps], { encoding: 'utf8' });
  return out.split(/\r?\n/).filter((l) => l.trim()).map(Number);
}

async function main(): Promise<void> {
  // Read before the VSCODE_* variables are dropped below.
  const version = process.env.VSCODE_VERSION || 'stable';
  const extRoot = path.resolve(__dirname, '..', '..', '..');
  const repoRoot = path.resolve(extRoot, '..', '..');
  const base = path.resolve(process.env.SY_VSCODE_TEST_DIR || path.join(os.tmpdir(), 'sy-vscode-test'));
  const run = path.join(base, 'run-' + new Date().toISOString().replace(/[:.]/g, '-'));
  fs.mkdirSync(run, { recursive: true });
  console.log('integration run folder:', run);

  // sy
  let sy = process.env.SY_EXE ? path.resolve(process.env.SY_EXE) : '';
  if (!sy) {
    sy = path.join(base, win ? 'sy.exe' : 'sy');
    console.log('building', sy);
    execFileSync('go', ['build', '-o', sy, './cmd/sy'], { cwd: repoRoot, stdio: 'inherit' });
  }
  if (!fs.existsSync(sy)) {
    throw new Error('no sy at ' + sy);
  }

  // sy's user folders, config and the fake claude CLI
  const roaming = path.join(run, 'appdata', 'Roaming');
  const local = path.join(run, 'appdata', 'Local');
  const xdg = path.join(run, 'appdata', 'config');
  const state = path.join(run, 'fake-agent');
  for (const d of [roaming, local, xdg, state]) {
    fs.mkdirSync(d, { recursive: true });
  }
  const fake = path.join(__dirname, 'fakeClaude.js');
  const shim = path.join(run, 'bin', win ? 'claude.cmd' : 'claude');
  fs.mkdirSync(path.dirname(shim), { recursive: true });
  if (win) {
    fs.writeFileSync(shim, `@"${process.execPath}" "${fake}" "${state}" %*\r\n`);
  } else {
    fs.writeFileSync(shim, `#!/bin/sh\nexec '${process.execPath}' '${fake}' '${state}' "$@"\n`, { mode: 0o755 });
  }
  const cfgDir = path.join(win ? roaming : xdg, 'switchyard');
  fs.mkdirSync(cfgDir, { recursive: true });
  fs.writeFileSync(path.join(cfgDir, 'switchyard.yaml'), [
    'providers:',
    '  claude:',
    `    command: ${JSON.stringify(shim)}`,
    '  codex:',
    '    disabled: true',
    'orchestrator:',
    '  approve_plan: true',
    '  review_changes: true',
    '  max_cpu_percent: 0        # the test machine may be busy: never wait for it',
    '  min_free_memory_mb: 0',
    '  min_free_disk_gb: 0',
    'notify:',
    '  enabled: false',
    '',
  ].join('\n'));

  // The workspace: a git repo. Windows keeps Git for Windows' default
  // (core.autocrlf=true: CRLF in the working tree, LF in git's diffs).
  // A folder name with a space and a non-ASCII letter, like many real ones.
  const ws = path.join(run, 'workspace', 'my project ä');
  fs.mkdirSync(ws, { recursive: true });
  git(ws, 'init', '-q', '-b', 'main');
  git(ws, 'config', 'user.name', 'sy vscode test');
  git(ws, 'config', 'user.email', 'vscode-test@localhost');
  git(ws, 'config', 'commit.gpgsign', 'false');
  git(ws, 'config', 'core.autocrlf', win ? 'true' : 'false');
  const eol = win ? '\r\n' : '\n';
  const notes = Array.from({ length: NOTES_LINES }, (_, i) => `line ${i + 1}`);
  fs.writeFileSync(path.join(ws, NOTES), notes.join(eol) + eol);
  fs.writeFileSync(path.join(ws, 'README.md'), `# test project${eol}`);
  git(ws, 'add', '-A');
  git(ws, 'commit', '-q', '-m', 'base');

  // VS Code: its own user data and extensions folders; the folder trusted.
  const userData = path.join(run, 'vscode-user');
  const extDir = path.join(run, 'vscode-extensions');
  fs.mkdirSync(path.join(userData, 'User'), { recursive: true });
  fs.mkdirSync(extDir, { recursive: true });
  fs.writeFileSync(path.join(userData, 'User', 'settings.json'), JSON.stringify({
    'security.workspace.trust.enabled': false,
    'switchyard.path': sy,
    'telemetry.telemetryLevel': 'off',
    'update.mode': 'none',
    'extensions.autoUpdate': false,
    'extensions.autoCheckUpdates': false,
    'workbench.startupEditor': 'none',
    'git.enabled': false,
    'git.autoRepositoryDetection': false,
    'diffEditor.codeLens': true,
    'chat.disableAIFeatures': true,
  }, null, 2));

  const env: Record<string, string> = {
    SY_TEST_SY: sy,
    SY_TEST_REPO: ws,
    SY_TEST_STATE: state,
  };
  if (win) {
    env.APPDATA = roaming;
    env.LOCALAPPDATA = local;
  } else {
    env.XDG_CONFIG_HOME = xdg;
    env.XDG_CACHE_HOME = path.join(run, 'appdata', 'cache');
  }

  // Run from a VS Code terminal (or an agent inside one), the environment
  // has ELECTRON_RUN_AS_NODE (Code.exe would start as plain node) and
  // VSCODE_* handles of that window: the test instance must not see them.
  for (const k of Object.keys(process.env)) {
    if (/^(ELECTRON_|VSCODE_)/i.test(k)) {
      delete process.env[k];
    }
  }

  let failed: unknown;
  try {
    await runTests({
      version,
      cachePath: path.join(extRoot, '.vscode-test'),
      extensionDevelopmentPath: extRoot,
      extensionTestsPath: path.join(__dirname, 'index.js'),
      launchArgs: [ws, '--user-data-dir', userData, '--extensions-dir', extDir, '--disable-extensions', '--disable-gpu', '--new-window'],
      extensionTestsEnv: env,
    });
  } catch (e) {
    failed = e;
  }

  // No sy (or agent) may outlive the window that started it. The last test
  // left one running with a working agent.
  const alive = (pid: number) => {
    try {
      process.kill(pid, 0);
      return true;
    } catch {
      return false;
    }
  };
  const shutdown = path.join(state, 'shutdown.json');
  if (fs.existsSync(shutdown)) {
    const { sy: syPid, agent } = JSON.parse(fs.readFileSync(shutdown, 'utf8')) as { sy: number; agent: number };
    const end = Date.now() + 20_000;
    while ((alive(syPid) || alive(agent)) && Date.now() < end) {
      await new Promise((r) => setTimeout(r, 250));
    }
    if (alive(syPid) || alive(agent)) {
      console.error(`FAIL: after VS Code closed, still running: ${alive(syPid) ? 'sy ' + syPid : ''} ${alive(agent) ? 'agent ' + agent : ''}`);
      try {
        process.kill(agent);
      } catch {
        // gone
      }
      failed ??= new Error('sy or its agent outlived VS Code');
    } else {
      console.log('closing VS Code stopped sy and its working agent');
    }
  }
  const left = syProcesses(sy);
  if (left.length) {
    console.error(`FAIL: sy processes left over after VS Code closed: ${left.join(', ')} (killing them)`);
    for (const pid of left) {
      try {
        process.kill(pid);
      } catch {
        // gone meanwhile
      }
    }
    failed ??= new Error('leftover sy processes');
  } else {
    console.log('no sy process left over');
  }

  if (failed) {
    console.error('integration tests failed; the run folder is kept:', run);
    throw failed;
  }
  if (process.env.SY_VSCODE_TEST_KEEP !== '1') {
    fs.rmSync(run, { recursive: true, force: true, maxRetries: 5, retryDelay: 500 });
  }
}

main().catch((e) => {
  console.error(e instanceof Error ? e.message : e);
  process.exit(1);
});
