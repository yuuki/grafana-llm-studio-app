/**
 * Shared constants and pure verification/packaging helpers for multi-architecture
 * Go backend builds, Grafana plugin signing, release archive creation, and checksum verification.
 */
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

export const RELEASE_TARGETS = Object.freeze([
  { goos: 'linux', goarch: 'amd64' },
  { goos: 'linux', goarch: 'arm64' },
  { goos: 'darwin', goarch: 'amd64' },
  { goos: 'darwin', goarch: 'arm64' },
  { goos: 'windows', goarch: 'amd64' },
]);

export const SEMVER_RE =
  /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$/;

export function normalizeVersion(rawVersion) {
  if (typeof rawVersion !== 'string' || rawVersion.trim() === '') {
    throw new Error('Version must be a non-empty string.');
  }
  const normalized = rawVersion.trim().replace(/^v/, '');
  if (!SEMVER_RE.test(normalized)) {
    throw new Error(`Invalid SemVer version: ${JSON.stringify(rawVersion)}`);
  }
  return normalized;
}

export function validateVersionConsistency({ packageVersion, pluginJsonVersion, expectedVersion, gitTag }) {
  const pkgVer = normalizeVersion(packageVersion);
  const pluginVer = normalizeVersion(pluginJsonVersion);

  if (pkgVer !== pluginVer) {
    throw new Error(
      `Version mismatch between package.json (${pkgVer}) and src/plugin.json (${pluginVer}). Keep both synchronized before packaging.`
    );
  }

  const targetVer = expectedVersion ? normalizeVersion(expectedVersion) : pkgVer;
  if (expectedVersion && targetVer !== pkgVer) {
    throw new Error(
      `Requested release version (${targetVer}) does not match package.json / src/plugin.json (${pkgVer}).`
    );
  }

  if (gitTag) {
    const trimmedTag = gitTag.trim();
    if (!trimmedTag.startsWith('v')) {
      throw new Error(`Release tag must start with "v" (received ${JSON.stringify(gitTag)}).`);
    }
    const tagVer = normalizeVersion(trimmedTag);
    if (tagVer !== targetVer) {
      throw new Error(
        `Git tag ${trimmedTag} (${tagVer}) does not match package.json / src/plugin.json version (${targetVer}).`
      );
    }
  }

  return targetVer;
}

export function getBinaryFilename(executable, goos, goarch) {
  const ext = goos === 'windows' ? '.exe' : '';
  return `${executable}_${goos}_${goarch}${ext}`;
}

export function parseTargets(rawTargets) {
  if (!rawTargets || rawTargets.trim() === '' || rawTargets.trim() === 'all') {
    return RELEASE_TARGETS.map((t) => ({ ...t }));
  }

  const allowed = new Map(RELEASE_TARGETS.map((t) => [`${t.goos}/${t.goarch}`, t]));
  const seen = new Set();
  const parsed = [];

  for (const token of rawTargets.split(',')) {
    const key = token.trim();
    if (!key) {
      continue;
    }
    if (!allowed.has(key)) {
      throw new Error(
        `Unsupported target ${JSON.stringify(key)}. Allowed targets: ${Array.from(allowed.keys()).join(', ')}`
      );
    }
    if (!seen.has(key)) {
      seen.add(key);
      parsed.push({ ...allowed.get(key) });
    }
  }

  if (parsed.length === 0) {
    throw new Error('At least one target architecture must be specified.');
  }
  return parsed;
}

export function sha256File(filePath) {
  const hash = crypto.createHash('sha256');
  hash.update(fs.readFileSync(filePath));
  return hash.digest('hex');
}

export function sha1File(filePath) {
  const hash = crypto.createHash('sha1');
  hash.update(fs.readFileSync(filePath));
  return hash.digest('hex');
}

export function formatChecksums(entries) {
  return (
    entries
      .slice()
      .sort((a, b) => a.filename.localeCompare(b.filename))
      .map(({ digest, filename }) => `${digest}  ${filename}`)
      .join('\n') + '\n'
  );
}

export function parseChecksums(content) {
  const entries = [];
  for (const rawLine of content.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line) {
      continue;
    }
    const match = line.match(/^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})\s+\*?(.+)$/);
    if (!match) {
      throw new Error(`Invalid checksum line: ${JSON.stringify(rawLine)}`);
    }
    entries.push({ digest: match[1].toLowerCase(), filename: match[2].trim() });
  }
  return entries;
}

/**
 * Inspects the executable header bytes to confirm a built Go backend binary
 * genuinely matches its declared GOOS/GOARCH target.
 */
export function verifyBinaryArchitecture(filePath, goos, goarch) {
  const fd = fs.openSync(filePath, 'r');
  try {
    const header = Buffer.alloc(4096);
    const bytesRead = fs.readSync(fd, header, 0, header.length, 0);
    if (bytesRead < 64) {
      throw new Error(`Binary ${path.basename(filePath)} is unexpectedly small (${bytesRead} bytes).`);
    }

    if (goos === 'linux') {
      // ELF magic: 0x7f 'E' 'L' 'F', class=2 (64-bit), data=1 (little-endian)
      if (
        header[0] !== 0x7f ||
        header[1] !== 0x45 ||
        header[2] !== 0x4c ||
        header[3] !== 0x46 ||
        header[4] !== 2 ||
        header[5] !== 1
      ) {
        throw new Error(`Binary ${path.basename(filePath)} is not a 64-bit little-endian ELF executable.`);
      }
      const machine = header.readUInt16LE(18);
      const expectedMachine = goarch === 'amd64' ? 0x3e : goarch === 'arm64' ? 0xb7 : null;
      if (expectedMachine === null || machine !== expectedMachine) {
        throw new Error(
          `ELF machine mismatch for ${path.basename(filePath)}: got 0x${machine.toString(16)}, expected 0x${expectedMachine?.toString(16)} (${goarch}).`
        );
      }
      return;
    }

    if (goos === 'darwin') {
      // 64-bit little-endian Mach-O magic: 0xfeedfacf -> bytes CF FA ED FE
      const magic = header.readUInt32LE(0);
      if (magic !== 0xfeedfacf) {
        throw new Error(
          `Binary ${path.basename(filePath)} is not a 64-bit Mach-O executable (magic=0x${magic.toString(16)}).`
        );
      }
      const cpuType = header.readUInt32LE(4);
      const expectedCpu = goarch === 'amd64' ? 0x01000007 : goarch === 'arm64' ? 0x0100000c : null;
      if (expectedCpu === null || cpuType !== expectedCpu) {
        throw new Error(
          `Mach-O CPU type mismatch for ${path.basename(filePath)}: got 0x${cpuType.toString(16)}, expected 0x${expectedCpu?.toString(16)} (${goarch}).`
        );
      }
      return;
    }

    if (goos === 'windows') {
      // MZ DOS header + PE\0\0 at e_lfanew
      if (header[0] !== 0x4d || header[1] !== 0x5a) {
        throw new Error(`Binary ${path.basename(filePath)} is missing Windows MZ header.`);
      }
      const peOffset = header.readUInt32LE(0x3c);
      if (peOffset + 6 > bytesRead) {
        throw new Error(`Binary ${path.basename(filePath)} has out-of-range PE header offset (${peOffset}).`);
      }
      if (
        header[peOffset] !== 0x50 ||
        header[peOffset + 1] !== 0x45 ||
        header[peOffset + 2] !== 0x00 ||
        header[peOffset + 3] !== 0x00
      ) {
        throw new Error(`Binary ${path.basename(filePath)} is missing PE\\0\\0 signature.`);
      }
      const machine = header.readUInt16LE(peOffset + 4);
      const expectedMachine = goarch === 'amd64' ? 0x8664 : null;
      if (expectedMachine === null || machine !== expectedMachine) {
        throw new Error(
          `PE machine mismatch for ${path.basename(filePath)}: got 0x${machine.toString(16)}, expected 0x${expectedMachine?.toString(16)} (${goarch}).`
        );
      }
      return;
    }

    throw new Error(`Unsupported GOOS for header verification: ${goos}`);
  } finally {
    fs.closeSync(fd);
  }
}

/**
 * Parses and validates a Grafana plugin MANIFEST.txt generated by @grafana/sign-plugin.
 * Checks PGP clear-signed framing, JSON payload fields, POSIX path separators, and
 * verifies every listed file's SHA-256 digest against the extracted plugin directory.
 */
export function verifySignedManifest(pluginDir, { expectedPluginId, expectedVersion, expectedSignatureType } = {}) {
  const manifestPath = path.join(pluginDir, 'MANIFEST.txt');
  if (!fs.existsSync(manifestPath)) {
    throw new Error(`Missing MANIFEST.txt in ${pluginDir}`);
  }

  const raw = fs.readFileSync(manifestPath, 'utf8');
  if (!raw.includes('-----BEGIN PGP SIGNED MESSAGE-----')) {
    throw new Error('MANIFEST.txt is missing "-----BEGIN PGP SIGNED MESSAGE-----" header.');
  }
  if (!raw.includes('-----BEGIN PGP SIGNATURE-----') || !raw.includes('-----END PGP SIGNATURE-----')) {
    throw new Error('MANIFEST.txt is missing PGP signature block.');
  }

  const bodyMatch = raw.match(
    /-----BEGIN PGP SIGNED MESSAGE-----\r?\n[\s\S]*?\r?\n\r?\n([\s\S]*?)\r?\n-----BEGIN PGP SIGNATURE-----/
  );
  if (!bodyMatch) {
    throw new Error('Could not extract signed JSON payload from MANIFEST.txt.');
  }

  let manifest;
  try {
    manifest = JSON.parse(bodyMatch[1]);
  } catch (err) {
    throw new Error(`MANIFEST.txt payload is not valid JSON: ${err.message}`);
  }

  if (expectedPluginId && manifest.plugin !== expectedPluginId) {
    throw new Error(
      `MANIFEST.txt plugin mismatch: expected ${expectedPluginId}, got ${JSON.stringify(manifest.plugin)}`
    );
  }
  if (expectedVersion && manifest.version !== expectedVersion) {
    throw new Error(
      `MANIFEST.txt version mismatch: expected ${expectedVersion}, got ${JSON.stringify(manifest.version)}`
    );
  }
  if (expectedSignatureType && manifest.signatureType !== expectedSignatureType) {
    throw new Error(
      `MANIFEST.txt signatureType mismatch: expected ${expectedSignatureType}, got ${JSON.stringify(manifest.signatureType)}`
    );
  }

  if (!manifest.files || typeof manifest.files !== 'object' || Object.keys(manifest.files).length === 0) {
    throw new Error('MANIFEST.txt contains no signed file checksums.');
  }

  for (const [relPath, expectedDigest] of Object.entries(manifest.files)) {
    if (relPath.includes('\\')) {
      throw new Error(
        `MANIFEST.txt contains Windows backslash path "${relPath}", which causes Grafana "Modified signature" errors.`
      );
    }
    const resolved = path.resolve(pluginDir, relPath);
    if (!resolved.startsWith(path.resolve(pluginDir) + path.sep)) {
      throw new Error(`MANIFEST.txt path escapes plugin directory: ${relPath}`);
    }
    if (!fs.existsSync(resolved)) {
      throw new Error(`MANIFEST.txt references missing file: ${relPath}`);
    }
    const actualDigest = sha256File(resolved);
    if (actualDigest !== String(expectedDigest).toLowerCase()) {
      throw new Error(
        `MANIFEST.txt SHA-256 mismatch for ${relPath}: expected ${expectedDigest}, got ${actualDigest} (would trigger Grafana "Modified signature" error).`
      );
    }
  }

  // Ensure no unmanifested files (besides MANIFEST.txt itself) exist inside pluginDir
  const actualFiles = collectRelativeFiles(pluginDir).filter((f) => f !== 'MANIFEST.txt');
  const manifestSet = new Set(Object.keys(manifest.files));
  const extraFiles = actualFiles.filter((f) => !manifestSet.has(f));
  if (extraFiles.length > 0) {
    throw new Error(
      `Plugin directory contains files not covered by MANIFEST.txt (would trigger Grafana "Modified signature" error): ${extraFiles.join(', ')}`
    );
  }

  return manifest;
}

export function collectRelativeFiles(rootDir, currentDir = rootDir) {
  const results = [];
  for (const entry of fs.readdirSync(currentDir, { withFileTypes: true })) {
    const full = path.join(currentDir, entry.name);
    if (entry.isDirectory()) {
      results.push(...collectRelativeFiles(rootDir, full));
    } else if (entry.isFile()) {
      results.push(path.relative(rootDir, full).split(path.sep).join('/'));
    }
  }
  return results.sort();
}

export function parseRootUrls(raw) {
  if (!raw || typeof raw !== 'string') {
    return [];
  }
  const urls = raw
    .split(',')
    .map((u) => u.trim())
    .filter(Boolean);
  for (const u of urls) {
    if (!/^https?:\/\/.+/.test(u)) {
      throw new Error(`Invalid private signing rootUrl ${JSON.stringify(u)} (must start with http:// or https://).`);
    }
  }
  return urls;
}
