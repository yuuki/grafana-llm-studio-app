#!/usr/bin/env node
/**
 * Packages yuuki-llmstudio-app into distributable multi-architecture Grafana plugin
 * archives (.zip and .tar.gz), optional signed MANIFEST.txt via @grafana/sign-plugin,
 * Grafana catalog SHA-1 checksum (.zip.sha1), SHA256SUMS, and an optional offline
 * air-gapped bundle (-airgap.tar.gz).
 *
 * Usage:
 *   node scripts/package-release.mjs
 *   node scripts/package-release.mjs --airgap
 *   node scripts/package-release.mjs --tag v0.1.0 --sign auto --airgap
 *   node scripts/package-release.mjs --sign private --root-urls https://grafana.internal
 */
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  RELEASE_TARGETS,
  collectRelativeFiles,
  formatChecksums,
  getBinaryFilename,
  parseRootUrls,
  sha1File,
  sha256File,
  validateVersionConsistency,
  verifyBinaryArchitecture,
  verifySignedManifest,
} from './release-lib.mjs';

const root = path.resolve(import.meta.dirname, '..');

function parseArgs(argv) {
  const opts = {
    version: null,
    tag: null,
    distDir: path.join(root, 'dist'),
    outDir: path.join(root, 'release'),
    signMode: 'off',
    rootUrls: process.env.GRAFANA_PLUGIN_ROOT_URLS || '',
    requireSigned: false,
    airgap: false,
  };

  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === '--version') {
      opts.version = argv[++i];
    } else if (arg === '--tag') {
      opts.tag = argv[++i];
    } else if (arg === '--dist-dir') {
      opts.distDir = path.resolve(root, argv[++i]);
    } else if (arg === '--out-dir') {
      opts.outDir = path.resolve(root, argv[++i]);
    } else if (arg === '--sign') {
      opts.signMode = argv[++i];
    } else if (arg === '--root-urls') {
      opts.rootUrls = argv[++i];
    } else if (arg === '--require-signed') {
      opts.requireSigned = true;
    } else if (arg === '--airgap') {
      opts.airgap = true;
    } else if (arg === '--help' || arg === '-h') {
      console.log(
        'Usage: node scripts/package-release.mjs [--version X.Y.Z] [--tag vX.Y.Z] [--dist-dir dist] [--out-dir release] [--sign auto|off|community|private|preserve] [--root-urls url1,url2] [--require-signed] [--airgap]'
      );
      process.exit(0);
    } else {
      throw new Error(`Unknown argument: ${arg}`);
    }
  }

  const validSignModes = new Set(['auto', 'off', 'community', 'private', 'preserve']);
  if (!validSignModes.has(opts.signMode)) {
    throw new Error(
      `Invalid --sign mode ${JSON.stringify(opts.signMode)}. Expected one of: ${Array.from(validSignModes).join(', ')}`
    );
  }
  return opts;
}

function getGitCommitSha() {
  if (process.env.GITHUB_SHA) return process.env.GITHUB_SHA;
  try {
    return execFileSync('git', ['rev-parse', 'HEAD'], { cwd: root, encoding: 'utf8' }).trim();
  } catch {
    return 'unknown';
  }
}

function ensureDistComplete(distDir, pluginId, executable, version) {
  const requiredFiles = ['plugin.json', 'module.js', 'img/logo.svg', 'README.md', 'LICENSE', 'CHANGELOG.md'];
  for (const rel of requiredFiles) {
    const full = path.join(distDir, rel);
    if (!fs.existsSync(full)) {
      throw new Error(
        `Missing required file in ${path.relative(root, distDir) || distDir}: ${rel}. Run "npm run build" first.`
      );
    }
  }

  const distPluginJson = JSON.parse(fs.readFileSync(path.join(distDir, 'plugin.json'), 'utf8'));
  if (distPluginJson.id !== pluginId) {
    throw new Error(`dist/plugin.json id mismatch: expected ${pluginId}, got ${distPluginJson.id}`);
  }
  if (distPluginJson.info?.version !== version) {
    throw new Error(
      `dist/plugin.json info.version (${distPluginJson.info?.version}) does not match target version (${version}). Rebuild frontend after updating src/plugin.json.`
    );
  }

  for (const { goos, goarch } of RELEASE_TARGETS) {
    const binName = getBinaryFilename(executable, goos, goarch);
    const binPath = path.join(distDir, binName);
    if (!fs.existsSync(binPath)) {
      throw new Error(
        `Missing Go backend binary for ${goos}/${goarch}: ${path.relative(root, binPath)}. Run "make build-backend-all" first.`
      );
    }
    fs.chmodSync(binPath, 0o755);
    verifyBinaryArchitecture(binPath, goos, goarch);
  }
}

function runSigning(distDir, { pluginId, version, signMode, rootUrls, requireSigned }) {
  const manifestPath = path.join(distDir, 'MANIFEST.txt');
  const token = (process.env.GRAFANA_ACCESS_POLICY_TOKEN || '').trim();
  const parsedRootUrls = parseRootUrls(rootUrls);

  if (signMode === 'preserve') {
    if (!fs.existsSync(manifestPath)) {
      throw new Error('--sign preserve specified, but dist/MANIFEST.txt does not exist.');
    }
    const manifest = verifySignedManifest(distDir, { expectedPluginId: pluginId, expectedVersion: version });
    return { signed: true, signatureType: manifest.signatureType || 'unknown', manifest };
  }

  let effectiveMode = signMode;
  if (signMode === 'auto') {
    if (!token) {
      effectiveMode = 'off';
    } else if (parsedRootUrls.length > 0) {
      effectiveMode = 'private';
    } else {
      effectiveMode = 'community';
    }
  }

  if (effectiveMode === 'off') {
    if (fs.existsSync(manifestPath)) {
      fs.rmSync(manifestPath);
    }
    if (requireSigned) {
      throw new Error(
        'Signed release required (--require-signed), but signing was disabled or GRAFANA_ACCESS_POLICY_TOKEN is not set.'
      );
    }
    return { signed: false, signatureType: 'unsigned', manifest: null };
  }

  if (!token) {
    throw new Error(
      `Signing mode "${effectiveMode}" requires GRAFANA_ACCESS_POLICY_TOKEN to be set in the environment.`
    );
  }

  if (effectiveMode === 'private' && parsedRootUrls.length === 0) {
    throw new Error(
      'Private signing requires at least one root URL via --root-urls or GRAFANA_PLUGIN_ROOT_URLS.'
    );
  }

  const signBin = path.join(root, 'node_modules', '.bin', 'sign-plugin');
  if (!fs.existsSync(signBin)) {
    throw new Error(
      `Missing local @grafana/sign-plugin binary at ${signBin}. Run "npm ci" before signing.`
    );
  }

  if (fs.existsSync(manifestPath)) {
    fs.rmSync(manifestPath);
  }

  const signArgs = ['--distDir', distDir];
  if (effectiveMode === 'private') {
    signArgs.push('--rootUrls', parsedRootUrls.join(','));
  }

  console.log(
    `Signing plugin with @grafana/sign-plugin (mode=${effectiveMode}${effectiveMode === 'private' ? `, rootUrls=${parsedRootUrls.join(',')}` : ''})...`
  );
  execFileSync(signBin, signArgs, {
    cwd: root,
    stdio: 'inherit',
    env: {
      ...process.env,
      GRAFANA_ACCESS_POLICY_TOKEN: token,
    },
  });

  const manifest = verifySignedManifest(distDir, {
    expectedPluginId: pluginId,
    expectedVersion: version,
    expectedSignatureType: effectiveMode,
  });

  console.log(
    `Plugin signed successfully (signatureType=${manifest.signatureType}, files=${Object.keys(manifest.files).length}).`
  );
  return { signed: true, signatureType: manifest.signatureType || effectiveMode, manifest };
}

function buildOfflineGrafanaIni({ pluginId, signed }) {
  const unsignedBlock = signed
    ? `# This plugin archive includes a signed MANIFEST.txt.\n# allow_loading_unsigned_plugins is NOT required for production.\n`
    : `# Unsigned build: allowlist ONLY this plugin ID (remove once using a signed release).\nallow_loading_unsigned_plugins = ${pluginId}\n`;

  return `# Hardened Grafana OSS configuration snippet for air-gapped / offline environments.
# Reference: docs/airgapped-deployment.md and docs/plugin-signing-and-publishing.md

[plugins]
${unsignedBlock}public_key_retrieval_disabled = true
plugin_admin_enabled = false

[analytics]
reporting_enabled = false
check_for_updates = false
check_for_plugin_updates = false

[news]
news_feed_enabled = false

[security]
disable_gravatar = true

[snapshots]
external_enabled = false
`;
}

function buildOfflineInstallDoc({ pluginId, version, signed, signatureType }) {
  const tarName = `${pluginId}-${version}.tar.gz`;
  const zipName = `${pluginId}-${version}.zip`;
  const signatureNote = signed
    ? `This bundle contains a **signed** plugin archive (\`signatureType: ${signatureType}\`). Grafana verifies \`MANIFEST.txt\` offline using its built-in Grafana Labs public key even with \`public_key_retrieval_disabled = true\` — \`allow_loading_unsigned_plugins\` is **not** required.`
    : `This bundle contains an **unsigned** plugin archive. Configure \`allow_loading_unsigned_plugins = ${pluginId}\` in \`grafana.ini\` (never use wildcards), or sign the plugin privately before deployment — see \`docs/plugin-signing-and-publishing.md\`.`;

  return `# Offline Air-Gapped Installation Runbook (${pluginId} v${version})

${signatureNote}

## 1. Verify Bundle & Plugin Checksums

\`\`\`bash
cd plugin
sha256sum -c SHA256SUMS
\`\`\`

## 2. Extract into Grafana Plugins Directory

Using \`.tar.gz\`:

\`\`\`bash
tar -xzf plugin/${tarName} -C /var/lib/grafana/plugins
\`\`\`

Or using \`.zip\`:

\`\`\`bash
unzip -q plugin/${zipName} -d /var/lib/grafana/plugins
\`\`\`

Both commands create \`/var/lib/grafana/plugins/${pluginId}/\` containing \`plugin.json\`, \`module.js\`, and multi-architecture Go backend binaries (\`linux/amd64\`, \`linux/arm64\`, \`darwin/amd64\`, \`darwin/arm64\`, \`windows/amd64\`) with \`0755\` permissions.

## 3. Install Companion \`grafana-llm-app\` Offline

Transfer and extract the matching official signed \`grafana-llm-app\` archive separately:
- **Grafana OSS 13.3.1 (React 19):** \`grafana-llm-app v1.2.0\`
- **Grafana OSS 12.4.15 (React 18):** \`grafana-llm-app v1.0.8\`

Do **not** use \`GF_INSTALL_PLUGINS\` in air-gapped environments.

## 4. Apply Hardened Offline Grafana Config

Review \`config/grafana-offline.ini.example\` and \`docs/airgapped-deployment.md\` (including upstream Prometheus / VictoriaMetrics query guardrails and reverse-proxy timeout/SSE settings), then restart Grafana.
`;
}

function createAirgapBundle({
  stagingRoot,
  outDir,
  pluginId,
  version,
  zipPath,
  tarPath,
  signed,
  signatureType,
  commitSha,
}) {
  const bundleBaseName = `${pluginId}-${version}-airgap`;
  const bundleDir = path.join(stagingRoot, bundleBaseName);
  const pluginSubdir = path.join(bundleDir, 'plugin');
  const schemasSubdir = path.join(bundleDir, 'schemas');
  const docsSubdir = path.join(bundleDir, 'docs');
  const configSubdir = path.join(bundleDir, 'config');

  for (const dir of [pluginSubdir, schemasSubdir, docsSubdir, configSubdir]) {
    fs.mkdirSync(dir, { recursive: true });
  }

  const zipName = path.basename(zipPath);
  const tarName = path.basename(tarPath);
  fs.copyFileSync(zipPath, path.join(pluginSubdir, zipName));
  fs.copyFileSync(tarPath, path.join(pluginSubdir, tarName));

  const pluginChecksums = formatChecksums([
    { filename: zipName, digest: sha256File(zipPath) },
    { filename: tarName, digest: sha256File(tarPath) },
  ]);
  fs.writeFileSync(path.join(pluginSubdir, 'SHA256SUMS'), pluginChecksums, 'utf8');

  for (const schemaFile of ['dashboard-ir.schema.json', 'dashboard-patch.schema.json']) {
    fs.copyFileSync(path.join(root, 'schemas', schemaFile), path.join(schemasSubdir, schemaFile));
  }

  for (const docFile of [
    'airgapped-deployment.md',
    'compatibility.md',
    'plugin-signing-and-publishing.md',
  ]) {
    const srcDoc = path.join(root, 'docs', docFile);
    if (!fs.existsSync(srcDoc)) {
      throw new Error(`Missing required documentation file for air-gapped bundle: docs/${docFile}`);
    }
    fs.copyFileSync(srcDoc, path.join(docsSubdir, docFile));
  }
  for (const rootDoc of ['README.md', 'CHANGELOG.md', 'LICENSE']) {
    const srcRootDoc = path.join(root, rootDoc);
    if (!fs.existsSync(srcRootDoc)) {
      throw new Error(`Missing required root file for air-gapped bundle: ${rootDoc}`);
    }
    fs.copyFileSync(srcRootDoc, path.join(bundleDir, rootDoc));
  }

  fs.writeFileSync(
    path.join(configSubdir, 'grafana-offline.ini.example'),
    buildOfflineGrafanaIni({ pluginId, signed }),
    'utf8'
  );
  fs.writeFileSync(
    path.join(bundleDir, 'INSTALL-OFFLINE.md'),
    buildOfflineInstallDoc({ pluginId, version, signed, signatureType }),
    'utf8'
  );

  // Build machine-readable BUNDLE-MANIFEST.json covering every file in the air-gapped bundle
  const relFiles = collectRelativeFiles(bundleDir);
  const filesMap = {};
  for (const rel of relFiles) {
    filesMap[rel] = sha256File(path.join(bundleDir, rel));
  }

  const builtAt =
    process.env.SOURCE_DATE_EPOCH && /^\d+$/.test(process.env.SOURCE_DATE_EPOCH.trim())
      ? new Date(Number(process.env.SOURCE_DATE_EPOCH.trim()) * 1000).toISOString()
      : new Date().toISOString();

  const bundleManifest = {
    schemaVersion: 1,
    pluginId,
    version,
    commitSha,
    builtAt,
    signed,
    signatureType,
    targets: RELEASE_TARGETS.map((t) => `${t.goos}/${t.goarch}`),
    supportedGrafanaVersions: ['13.3.1', '12.4.15'],
    companionPlugin: {
      id: 'grafana-llm-app',
      versions: {
        'grafana-13.x': '1.2.0',
        'grafana-12.4.x': '1.0.8',
      },
    },
    files: filesMap,
  };
  fs.writeFileSync(
    path.join(bundleDir, 'BUNDLE-MANIFEST.json'),
    JSON.stringify(bundleManifest, null, 2) + '\n',
    'utf8'
  );

  const airgapTarName = `${bundleBaseName}.tar.gz`;
  const airgapTarPath = path.join(outDir, airgapTarName);
  execFileSync('tar', ['-czf', airgapTarPath, '-C', stagingRoot, bundleBaseName], {
    stdio: 'inherit',
  });

  return airgapTarPath;
}

function main() {
  const opts = parseArgs(process.argv.slice(2));
  const pkgJson = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
  const srcPluginJson = JSON.parse(fs.readFileSync(path.join(root, 'src', 'plugin.json'), 'utf8'));

  const pluginId = srcPluginJson.id;
  const executable = srcPluginJson.executable;

  const version = validateVersionConsistency({
    packageVersion: pkgJson.version,
    pluginJsonVersion: srcPluginJson.info?.version,
    expectedVersion: opts.version,
    gitTag: opts.tag,
  });

  ensureDistComplete(opts.distDir, pluginId, executable, version);

  const signingResult = runSigning(opts.distDir, {
    pluginId,
    version,
    signMode: opts.signMode,
    rootUrls: opts.rootUrls,
    requireSigned: opts.requireSigned,
  });

  fs.mkdirSync(opts.outDir, { recursive: true });

  // Clean previous artifacts for this version to avoid stale entries in SHA256SUMS
  const archiveBase = `${pluginId}-${version}`;
  const zipName = `${archiveBase}.zip`;
  const tarName = `${archiveBase}.tar.gz`;
  const sha1Name = `${zipName}.sha1`;
  const airgapName = `${archiveBase}-airgap.tar.gz`;
  const sha256SumsPath = path.join(opts.outDir, 'SHA256SUMS');

  for (const candidate of [zipName, tarName, sha1Name, airgapName, 'SHA256SUMS']) {
    const p = path.join(opts.outDir, candidate);
    if (fs.existsSync(p)) {
      fs.rmSync(p);
    }
  }

  const stagingRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'yuuki-llmstudio-pkg-'));
  try {
    const stagedPluginDir = path.join(stagingRoot, pluginId);
    fs.cpSync(opts.distDir, stagedPluginDir, { recursive: true });

    // Re-assert 0755 permissions on all backend binaries in staging
    for (const { goos, goarch } of RELEASE_TARGETS) {
      fs.chmodSync(path.join(stagedPluginDir, getBinaryFilename(executable, goos, goarch)), 0o755);
    }

    const zipPath = path.join(opts.outDir, zipName);
    const tarPath = path.join(opts.outDir, tarName);

    execFileSync('zip', ['-rq', zipPath, pluginId], { cwd: stagingRoot, stdio: 'inherit' });
    execFileSync('tar', ['-czf', tarPath, '-C', stagingRoot, pluginId], { stdio: 'inherit' });

    // Grafana plugin catalog submission accepts .zip.sha1
    const zipSha1 = sha1File(zipPath);
    fs.writeFileSync(path.join(opts.outDir, sha1Name), `${zipSha1}  ${zipName}\n`, 'utf8');

    const checksumEntries = [
      { filename: zipName, digest: sha256File(zipPath) },
      { filename: tarName, digest: sha256File(tarPath) },
    ];

    if (opts.airgap) {
      const airgapPath = createAirgapBundle({
        stagingRoot,
        outDir: opts.outDir,
        pluginId,
        version,
        zipPath,
        tarPath,
        signed: signingResult.signed,
        signatureType: signingResult.signatureType,
        commitSha: getGitCommitSha(),
      });
      checksumEntries.push({ filename: path.basename(airgapPath), digest: sha256File(airgapPath) });
    }

    fs.writeFileSync(sha256SumsPath, formatChecksums(checksumEntries), 'utf8');

    const summary = {
      pluginId,
      version,
      signed: signingResult.signed,
      signatureType: signingResult.signatureType,
      outDir: path.relative(root, opts.outDir) || '.',
      artifacts: checksumEntries.map((e) => e.filename).concat([sha1Name, 'SHA256SUMS']),
    };
    console.log('\nRelease packaging complete:');
    console.log(JSON.stringify(summary, null, 2));
  } finally {
    fs.rmSync(stagingRoot, { recursive: true, force: true });
  }
}

main();
