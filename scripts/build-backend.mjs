#!/usr/bin/env node
/**
 * Cross-compiles the Go backend binary for yuuki-llmstudio-app across supported
 * Grafana plugin architectures with embedded grafana-plugin-sdk-go build metadata
 * and 0755 executable permissions.
 *
 * Usage:
 *   node scripts/build-backend.mjs                          # linux/amd64 (fast local default)
 *   node scripts/build-backend.mjs --all                    # all 5 release targets
 *   node scripts/build-backend.mjs --targets linux/arm64,darwin/arm64
 *   node scripts/build-backend.mjs --all --version 0.1.0 --out-dir dist
 */
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import {
  getBinaryFilename,
  normalizeVersion,
  parseTargets,
  verifyBinaryArchitecture,
} from './release-lib.mjs';

const root = path.resolve(import.meta.dirname, '..');

function parseArgs(argv) {
  const opts = {
    all: false,
    targets: null,
    outDir: path.join(root, 'dist'),
    version: null,
    pkg: './pkg',
    vendor: false,
  };

  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === '--all') {
      opts.all = true;
    } else if (arg === '--targets') {
      opts.targets = argv[++i];
    } else if (arg === '--out-dir') {
      opts.outDir = path.resolve(root, argv[++i]);
    } else if (arg === '--version') {
      opts.version = argv[++i];
    } else if (arg === '--pkg') {
      opts.pkg = argv[++i];
    } else if (arg === '--vendor') {
      opts.vendor = true;
    } else if (arg === '--help' || arg === '-h') {
      console.log(
        'Usage: node scripts/build-backend.mjs [--all | --targets os/arch,...] [--out-dir dist] [--version X.Y.Z] [--vendor]'
      );
      process.exit(0);
    } else {
      throw new Error(`Unknown argument: ${arg}`);
    }
  }
  return opts;
}

function getBuildTimestampMs() {
  if (process.env.SOURCE_DATE_EPOCH && /^\d+$/.test(process.env.SOURCE_DATE_EPOCH.trim())) {
    return Number(process.env.SOURCE_DATE_EPOCH.trim()) * 1000;
  }
  return Date.now();
}

function main() {
  const opts = parseArgs(process.argv.slice(2));
  const pkgJson = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
  const pluginJson = JSON.parse(fs.readFileSync(path.join(root, 'src', 'plugin.json'), 'utf8'));

  const pluginId = pluginJson.id;
  const executable = pluginJson.executable;
  if (!pluginId || !executable) {
    throw new Error('src/plugin.json must define both "id" and "executable".');
  }

  const version = normalizeVersion(opts.version || pkgJson.version);
  const targets = opts.all
    ? parseTargets('all')
    : opts.targets
      ? parseTargets(opts.targets)
      : parseTargets('linux/amd64');

  fs.mkdirSync(opts.outDir, { recursive: true });

  const buildInfo = JSON.stringify({
    time: getBuildTimestampMs(),
    pluginID: pluginId,
    version,
  });

  const ldflags = [
    '-w',
    '-s',
    `-X 'github.com/grafana/grafana-plugin-sdk-go/build.buildInfoJSON=${buildInfo}'`,
  ].join(' ');

  for (const { goos, goarch } of targets) {
    const filename = getBinaryFilename(executable, goos, goarch);
    const outPath = path.join(opts.outDir, filename);

    const goArgs = ['build', '-trimpath', '-tags', 'arrow_json_stdlib'];
    if (opts.vendor) {
      goArgs.push('-mod=vendor');
    }
    goArgs.push('-ldflags', ldflags, '-o', outPath, opts.pkg);

    console.log(`Building ${goos}/${goarch} -> ${path.relative(root, outPath)} (version=${version})`);
    execFileSync('go', goArgs, {
      cwd: root,
      stdio: 'inherit',
      env: {
        ...process.env,
        CGO_ENABLED: '0',
        GOOS: goos,
        GOARCH: goarch,
      },
    });

    fs.chmodSync(outPath, 0o755);
    verifyBinaryArchitecture(outPath, goos, goarch);

    const sizeMiB = (fs.statSync(outPath).size / (1024 * 1024)).toFixed(2);
    console.log(`  OK ${filename} (${sizeMiB} MiB, mode 0755)`);
  }

  console.log(`Built ${targets.length} Go backend binary target(s) in ${path.relative(root, opts.outDir) || '.'}.`);
}

main();
