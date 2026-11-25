#!/usr/bin/env node
/**
 * End-to-end verification of packaged release artifacts in release/:
 *  - Verifies SHA256SUMS covers every .zip and .tar.gz artifact and all digests match.
 *  - Verifies <plugin>-<version>.zip.sha1 matches the .zip archive.
 *  - Extracts both .zip and .tar.gz and verifies:
 *      * Top-level directory is strictly yuuki-llmstudio-app/
 *      * Required plugin files & plugin.json metadata (id, executable, version, links)
 *      * All 5 Go backend binaries exist, have executable bits (0755), and match
 *        their declared ELF / Mach-O / PE CPU architecture headers
 *      * module.js declares required AMD externals and zero SECRET_INTERNALS
 *      * MANIFEST.txt PGP framing & per-file SHA-256 digests (when present or --require-signed)
 *  - When an air-gapped bundle (-airgap.tar.gz) is present (or --airgap is passed),
 *    verifies BUNDLE-MANIFEST.json hashes, embedded plugin archives, and zero dev-only file leakage.
 *
 * Usage:
 *   node scripts/verify-release.mjs
 *   node scripts/verify-release.mjs --out-dir release --version 0.1.0 --airgap
 *   node scripts/verify-release.mjs --require-signed
 */
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  RELEASE_TARGETS,
  collectRelativeFiles,
  getBinaryFilename,
  normalizeVersion,
  parseChecksums,
  sha1File,
  sha256File,
  verifyBinaryArchitecture,
  verifySignedManifest,
} from './release-lib.mjs';

const root = path.resolve(import.meta.dirname, '..');

function parseArgs(argv) {
  const opts = {
    outDir: path.join(root, 'release'),
    version: null,
    requireSigned: false,
    expectAirgap: false,
  };

  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === '--out-dir') {
      opts.outDir = path.resolve(root, argv[++i]);
    } else if (arg === '--version') {
      opts.version = argv[++i];
    } else if (arg === '--require-signed') {
      opts.requireSigned = true;
    } else if (arg === '--airgap') {
      opts.expectAirgap = true;
    } else if (arg === '--help' || arg === '-h') {
      console.log(
        'Usage: node scripts/verify-release.mjs [--out-dir release] [--version X.Y.Z] [--require-signed] [--airgap]'
      );
      process.exit(0);
    } else {
      throw new Error(`Unknown argument: ${arg}`);
    }
  }
  return opts;
}

function verifyExtractedPluginDir(extractRoot, { pluginId, executable, version, requireSigned, label }) {
  const topEntries = fs.readdirSync(extractRoot);
  if (topEntries.length !== 1 || topEntries[0] !== pluginId) {
    throw new Error(
      `[${label}] Archive top-level entries must be strictly ["${pluginId}"], got ${JSON.stringify(topEntries)}`
    );
  }

  const pluginDir = path.join(extractRoot, pluginId);
  const requiredFiles = ['plugin.json', 'module.js', 'img/logo.svg', 'README.md', 'LICENSE', 'CHANGELOG.md'];
  for (const rel of requiredFiles) {
    const full = path.join(pluginDir, rel);
    if (!fs.existsSync(full)) {
      throw new Error(`[${label}] Missing required plugin file: ${rel}`);
    }
  }

  const pluginJson = JSON.parse(fs.readFileSync(path.join(pluginDir, 'plugin.json'), 'utf8'));
  if (pluginJson.id !== pluginId) {
    throw new Error(`[${label}] plugin.json id mismatch: ${pluginJson.id}`);
  }
  if (pluginJson.executable !== executable) {
    throw new Error(`[${label}] plugin.json executable mismatch: ${pluginJson.executable}`);
  }
  if (pluginJson.info?.version !== version) {
    throw new Error(`[${label}] plugin.json version mismatch: expected ${version}, got ${pluginJson.info?.version}`);
  }
  if (!Array.isArray(pluginJson.info?.links) || pluginJson.info.links.length === 0) {
    throw new Error(`[${label}] plugin.json info.links must be populated for catalog submission.`);
  }

  // Verify frontend AMD bundle externals and zero React SECRET_INTERNALS
  const bundle = fs.readFileSync(path.join(pluginDir, 'module.js'), 'utf8');
  const defineMatch = bundle.match(/define\(\s*\[([^\]]*)\]/);
  if (!defineMatch) {
    throw new Error(`[${label}] Could not find AMD define([...]) array in module.js`);
  }
  const declared = new Set(Array.from(defineMatch[1].matchAll(/["']([^"']+)["']/g), (m) => m[1]));
  for (const dep of [
    'react',
    'react-dom',
    'react/jsx-runtime',
    'i18next',
    '@grafana/data',
    '@grafana/runtime',
    '@grafana/ui',
    '@emotion/css',
    'rxjs',
  ]) {
    if (!declared.has(dep)) {
      throw new Error(`[${label}] module.js missing required AMD external: ${dep}`);
    }
  }
  for (const forbidden of [
    '@grafana/schema',
    '@grafana/i18n',
    'react-router-dom',
    'react-dom/client',
    'react-dom/server',
  ]) {
    if (declared.has(forbidden)) {
      throw new Error(`[${label}] module.js externalized forbidden non-shared module: ${forbidden}`);
    }
  }
  for (const sym of ['SECRET_INTERNALS', '__CLIENT_INTERNALS_DO_NOT_USE_OR_WARN_USERS_THEY_CANNOT_UPGRADE']) {
    if (bundle.includes(sym)) {
      throw new Error(`[${label}] module.js contains forbidden React internal symbol: ${sym}`);
    }
  }

  // Verify all 5 Go backend binaries: existence, 0755 executable permissions, and binary headers
  for (const { goos, goarch } of RELEASE_TARGETS) {
    const binName = getBinaryFilename(executable, goos, goarch);
    const binPath = path.join(pluginDir, binName);
    if (!fs.existsSync(binPath)) {
      throw new Error(`[${label}] Missing backend binary: ${binName}`);
    }
    const mode = fs.statSync(binPath).mode & 0o777;
    if ((mode & 0o111) === 0) {
      throw new Error(
        `[${label}] Backend binary ${binName} is not executable (mode 0${mode.toString(8)}, expected 0755).`
      );
    }
    verifyBinaryArchitecture(binPath, goos, goarch);
  }

  // Verify signature manifest if present or required
  const manifestPath = path.join(pluginDir, 'MANIFEST.txt');
  const hasManifest = fs.existsSync(manifestPath);
  if (requireSigned && !hasManifest) {
    throw new Error(`[${label}] --require-signed was specified, but MANIFEST.txt is missing from archive.`);
  }
  if (hasManifest) {
    const manifest = verifySignedManifest(pluginDir, {
      expectedPluginId: pluginId,
      expectedVersion: version,
    });
    return { signed: true, signatureType: manifest.signatureType };
  }
  return { signed: false, signatureType: 'unsigned' };
}

function verifyAirgapBundle(airgapPath, { pluginId, version, zipPath, tarPath }) {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'yuuki-llmstudio-airgap-verify-'));
  try {
    execFileSync('tar', ['-xzf', airgapPath, '-C', tmpDir], { stdio: 'inherit' });
    const expectedRoot = `${pluginId}-${version}-airgap`;
    const topEntries = fs.readdirSync(tmpDir);
    if (topEntries.length !== 1 || topEntries[0] !== expectedRoot) {
      throw new Error(
        `[airgap] Top-level directory must be ["${expectedRoot}"], got ${JSON.stringify(topEntries)}`
      );
    }

    const bundleDir = path.join(tmpDir, expectedRoot);
    const manifestPath = path.join(bundleDir, 'BUNDLE-MANIFEST.json');
    if (!fs.existsSync(manifestPath)) {
      throw new Error('[airgap] Missing BUNDLE-MANIFEST.json');
    }
    const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
    if (manifest.pluginId !== pluginId || manifest.version !== version) {
      throw new Error(
        `[airgap] BUNDLE-MANIFEST.json metadata mismatch: ${manifest.pluginId}@${manifest.version}`
      );
    }

    // Verify every file listed in BUNDLE-MANIFEST.json matches its SHA-256
    for (const [rel, expectedHash] of Object.entries(manifest.files || {})) {
      const full = path.join(bundleDir, rel);
      if (!fs.existsSync(full)) {
        throw new Error(`[airgap] Manifest file missing: ${rel}`);
      }
      const actual = sha256File(full);
      if (actual !== expectedHash) {
        throw new Error(`[airgap] SHA-256 mismatch for ${rel}: expected ${expectedHash}, got ${actual}`);
      }
    }

    // Ensure embedded plugin archives match top-level release archives byte-for-byte
    const embeddedZip = path.join(bundleDir, 'plugin', path.basename(zipPath));
    const embeddedTar = path.join(bundleDir, 'plugin', path.basename(tarPath));
    if (sha256File(embeddedZip) !== sha256File(zipPath)) {
      throw new Error('[airgap] Embedded plugin .zip does not match top-level release .zip');
    }
    if (sha256File(embeddedTar) !== sha256File(tarPath)) {
      throw new Error('[airgap] Embedded plugin .tar.gz does not match top-level release .tar.gz');
    }

    // Ensure plugin/SHA256SUMS validates
    const pluginSums = parseChecksums(fs.readFileSync(path.join(bundleDir, 'plugin', 'SHA256SUMS'), 'utf8'));
    for (const { filename, digest } of pluginSums) {
      const actual = sha256File(path.join(bundleDir, 'plugin', filename));
      if (actual !== digest) {
        throw new Error(`[airgap] plugin/SHA256SUMS mismatch for ${filename}`);
      }
    }

    // Ensure zero development-only files leaked into the air-gapped bundle
    const allFiles = collectRelativeFiles(bundleDir);
    const forbiddenFragments = ['llm-studio-dev.yaml', 'exporter.py', '.env', 'node_modules/'];
    for (const file of allFiles) {
      for (const bad of forbiddenFragments) {
        if (file.includes(bad)) {
          throw new Error(`[airgap] Forbidden development-only file leaked into air-gapped bundle: ${file}`);
        }
      }
    }

    console.log(
      `  OK air-gapped bundle ${path.basename(airgapPath)} (${allFiles.length} files verified, zero dev leaks)`
    );
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
}

function main() {
  const opts = parseArgs(process.argv.slice(2));
  const pkgJson = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
  const srcPluginJson = JSON.parse(fs.readFileSync(path.join(root, 'src', 'plugin.json'), 'utf8'));

  const pluginId = srcPluginJson.id;
  const executable = srcPluginJson.executable;
  const version = normalizeVersion(opts.version || pkgJson.version);

  const archiveBase = `${pluginId}-${version}`;
  const zipName = `${archiveBase}.zip`;
  const tarName = `${archiveBase}.tar.gz`;
  const sha1Name = `${zipName}.sha1`;
  const airgapName = `${archiveBase}-airgap.tar.gz`;

  const zipPath = path.join(opts.outDir, zipName);
  const tarPath = path.join(opts.outDir, tarName);
  const sha1Path = path.join(opts.outDir, sha1Name);
  const sha256SumsPath = path.join(opts.outDir, 'SHA256SUMS');
  const airgapPath = path.join(opts.outDir, airgapName);

  for (const required of [zipPath, tarPath, sha1Path, sha256SumsPath]) {
    if (!fs.existsSync(required)) {
      throw new Error(`Missing expected release artifact: ${path.relative(root, required)}`);
    }
  }

  // 1. Verify SHA256SUMS completeness and integrity
  const sha256Entries = parseChecksums(fs.readFileSync(sha256SumsPath, 'utf8'));
  const listedSet = new Set(sha256Entries.map((e) => e.filename));
  const archiveFiles = fs
    .readdirSync(opts.outDir)
    .filter((f) => f.endsWith('.zip') || f.endsWith('.tar.gz'))
    .sort();

  for (const archive of archiveFiles) {
    if (!listedSet.has(archive)) {
      throw new Error(`Archive ${archive} is missing from SHA256SUMS.`);
    }
  }
  for (const { filename, digest } of sha256Entries) {
    const filePath = path.join(opts.outDir, filename);
    if (!fs.existsSync(filePath)) {
      throw new Error(`SHA256SUMS lists missing file: ${filename}`);
    }
    const actual = sha256File(filePath);
    if (actual !== digest) {
      throw new Error(`SHA256SUMS mismatch for ${filename}: expected ${digest}, got ${actual}`);
    }
  }
  console.log(`  OK SHA256SUMS (${sha256Entries.length} archive(s) verified)`);

  // 2. Verify .zip.sha1
  const sha1Entries = parseChecksums(fs.readFileSync(sha1Path, 'utf8'));
  if (sha1Entries.length !== 1 || sha1Entries[0].filename !== zipName) {
    throw new Error(`Invalid ${sha1Name} contents.`);
  }
  const actualSha1 = sha1File(zipPath);
  if (actualSha1 !== sha1Entries[0].digest) {
    throw new Error(`${sha1Name} mismatch: expected ${sha1Entries[0].digest}, got ${actualSha1}`);
  }
  console.log(`  OK ${sha1Name} (${actualSha1})`);

  // 3. Extract and verify .zip
  const zipTmp = fs.mkdtempSync(path.join(os.tmpdir(), 'yuuki-llmstudio-zip-verify-'));
  let zipSignStatus;
  try {
    execFileSync('unzip', ['-q', zipPath, '-d', zipTmp], { stdio: 'inherit' });
    zipSignStatus = verifyExtractedPluginDir(zipTmp, {
      pluginId,
      executable,
      version,
      requireSigned: opts.requireSigned,
      label: zipName,
    });
    console.log(
      `  OK ${zipName} (5/5 architectures verified, signature=${zipSignStatus.signatureType})`
    );
  } finally {
    fs.rmSync(zipTmp, { recursive: true, force: true });
  }

  // 4. Extract and verify .tar.gz
  const tarTmp = fs.mkdtempSync(path.join(os.tmpdir(), 'yuuki-llmstudio-tar-verify-'));
  let tarSignStatus;
  try {
    execFileSync('tar', ['-xzf', tarPath, '-C', tarTmp], { stdio: 'inherit' });
    tarSignStatus = verifyExtractedPluginDir(tarTmp, {
      pluginId,
      executable,
      version,
      requireSigned: opts.requireSigned,
      label: tarName,
    });
    console.log(
      `  OK ${tarName} (5/5 architectures verified, signature=${tarSignStatus.signatureType})`
    );
  } finally {
    fs.rmSync(tarTmp, { recursive: true, force: true });
  }

  if (zipSignStatus.signatureType !== tarSignStatus.signatureType) {
    throw new Error(
      `Signature status mismatch between .zip (${zipSignStatus.signatureType}) and .tar.gz (${tarSignStatus.signatureType}).`
    );
  }

  // 5. Verify optional air-gapped bundle if present or requested
  if (opts.expectAirgap && !fs.existsSync(airgapPath)) {
    throw new Error(`--airgap specified, but missing ${path.relative(root, airgapPath)}`);
  }
  if (fs.existsSync(airgapPath)) {
    verifyAirgapBundle(airgapPath, { pluginId, version, zipPath, tarPath });
  }

  console.log(`Release verification PASSED for ${pluginId} v${version}.`);
}

main();
