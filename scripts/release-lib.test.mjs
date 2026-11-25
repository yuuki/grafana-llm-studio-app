import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import {
  RELEASE_TARGETS,
  formatChecksums,
  getBinaryFilename,
  normalizeVersion,
  parseChecksums,
  parseRootUrls,
  parseTargets,
  validateVersionConsistency,
  verifyBinaryArchitecture,
  verifySignedManifest,
} from './release-lib.mjs';

test('normalizeVersion accepts valid SemVer and strips leading v', () => {
  assert.equal(normalizeVersion('0.1.0'), '0.1.0');
  assert.equal(normalizeVersion('v1.2.3'), '1.2.3');
  assert.equal(normalizeVersion('v1.2.3-rc.1'), '1.2.3-rc.1');
  assert.throws(() => normalizeVersion(''), /non-empty/);
  assert.throws(() => normalizeVersion('latest'), /Invalid SemVer/);
  assert.throws(() => normalizeVersion('1.2'), /Invalid SemVer/);
});

test('validateVersionConsistency enforces parity across package.json, plugin.json, and git tag', () => {
  assert.equal(
    validateVersionConsistency({
      packageVersion: '0.2.0',
      pluginJsonVersion: '0.2.0',
      gitTag: 'v0.2.0',
    }),
    '0.2.0'
  );

  assert.throws(
    () =>
      validateVersionConsistency({
        packageVersion: '0.2.0',
        pluginJsonVersion: '0.1.0',
      }),
    /Version mismatch/
  );

  assert.throws(
    () =>
      validateVersionConsistency({
        packageVersion: '0.2.0',
        pluginJsonVersion: '0.2.0',
        gitTag: 'v0.3.0',
      }),
    /does not match/
  );

  assert.throws(
    () =>
      validateVersionConsistency({
        packageVersion: '0.2.0',
        pluginJsonVersion: '0.2.0',
        gitTag: '0.2.0',
      }),
    /must start with "v"/
  );
});

test('parseTargets and getBinaryFilename follow Grafana backend naming conventions', () => {
  assert.equal(RELEASE_TARGETS.length, 5);
  assert.deepEqual(parseTargets('all'), RELEASE_TARGETS);
  assert.deepEqual(parseTargets('linux/amd64,windows/amd64,linux/amd64'), [
    { goos: 'linux', goarch: 'amd64' },
    { goos: 'windows', goarch: 'amd64' },
  ]);
  assert.throws(() => parseTargets('freebsd/amd64'), /Unsupported target/);

  assert.equal(
    getBinaryFilename('gpx_yuuki_llmstudio_app', 'linux', 'amd64'),
    'gpx_yuuki_llmstudio_app_linux_amd64'
  );
  assert.equal(
    getBinaryFilename('gpx_yuuki_llmstudio_app', 'darwin', 'arm64'),
    'gpx_yuuki_llmstudio_app_darwin_arm64'
  );
  assert.equal(
    getBinaryFilename('gpx_yuuki_llmstudio_app', 'windows', 'amd64'),
    'gpx_yuuki_llmstudio_app_windows_amd64.exe'
  );
});

test('formatChecksums and parseChecksums round-trip sorted SHA-256 entries', () => {
  const digestA = 'a'.repeat(64);
  const digestB = 'b'.repeat(64);
  const text = formatChecksums([
    { filename: 'z-file.zip', digest: digestB },
    { filename: 'a-file.tar.gz', digest: digestA },
  ]);
  assert.equal(text, `${digestA}  a-file.tar.gz\n${digestB}  z-file.zip\n`);
  assert.deepEqual(parseChecksums(text), [
    { filename: 'a-file.tar.gz', digest: digestA },
    { filename: 'z-file.zip', digest: digestB },
  ]);
  assert.throws(() => parseChecksums('not-a-valid-line'), /Invalid checksum line/);
});

test('parseRootUrls validates comma-separated private signing URLs', () => {
  assert.deepEqual(parseRootUrls(''), []);
  assert.deepEqual(
    parseRootUrls('https://grafana.internal, http://localhost:3000/grafana'),
    ['https://grafana.internal', 'http://localhost:3000/grafana']
  );
  assert.throws(() => parseRootUrls('grafana.internal'), /must start with http/);
});

test('verifyBinaryArchitecture validates ELF, Mach-O, and PE headers and rejects mismatches', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'release-lib-bin-'));
  try {
    // ELF x86_64
    const elfAmd64 = Buffer.alloc(128);
    elfAmd64[0] = 0x7f;
    elfAmd64[1] = 0x45;
    elfAmd64[2] = 0x4c;
    elfAmd64[3] = 0x46;
    elfAmd64[4] = 2;
    elfAmd64[5] = 1;
    elfAmd64.writeUInt16LE(0x3e, 18);
    const elfPath = path.join(tmpDir, 'bin_linux_amd64');
    fs.writeFileSync(elfPath, elfAmd64);

    verifyBinaryArchitecture(elfPath, 'linux', 'amd64');
    assert.throws(() => verifyBinaryArchitecture(elfPath, 'linux', 'arm64'), /ELF machine mismatch/);

    // Mach-O arm64
    const machoArm64 = Buffer.alloc(128);
    machoArm64.writeUInt32LE(0xfeedfacf, 0);
    machoArm64.writeUInt32LE(0x0100000c, 4);
    const machoPath = path.join(tmpDir, 'bin_darwin_arm64');
    fs.writeFileSync(machoPath, machoArm64);

    verifyBinaryArchitecture(machoPath, 'darwin', 'arm64');
    assert.throws(() => verifyBinaryArchitecture(machoPath, 'darwin', 'amd64'), /Mach-O CPU type mismatch/);

    // Windows PE amd64
    const peAmd64 = Buffer.alloc(256);
    peAmd64[0] = 0x4d;
    peAmd64[1] = 0x5a;
    peAmd64.writeUInt32LE(0x80, 0x3c);
    peAmd64[0x80] = 0x50;
    peAmd64[0x81] = 0x45;
    peAmd64[0x82] = 0x00;
    peAmd64[0x83] = 0x00;
    peAmd64.writeUInt16LE(0x8664, 0x84);
    const pePath = path.join(tmpDir, 'bin_windows_amd64.exe');
    fs.writeFileSync(pePath, peAmd64);

    verifyBinaryArchitecture(pePath, 'windows', 'amd64');
    assert.throws(() => verifyBinaryArchitecture(pePath, 'linux', 'amd64'), /not a 64-bit little-endian ELF/);
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('verifySignedManifest validates PGP framing, hashes, and detects tampering or extra files', () => {
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'release-lib-manifest-'));
  try {
    fs.mkdirSync(path.join(tmpDir, 'img'), { recursive: true });
    fs.writeFileSync(path.join(tmpDir, 'plugin.json'), '{"id":"yuuki-llmstudio-app"}');
    fs.writeFileSync(path.join(tmpDir, 'img/logo.svg'), '<svg/>');

    const hashOf = (p) => crypto.createHash('sha256').update(fs.readFileSync(p)).digest('hex');
    const buildManifestText = (payload) =>
      [
        '-----BEGIN PGP SIGNED MESSAGE-----',
        'Hash: SHA512',
        '',
        JSON.stringify(payload, null, 2),
        '-----BEGIN PGP SIGNATURE-----',
        'dummy-signature-block',
        '-----END PGP SIGNATURE-----',
        '',
      ].join('\n');

    const validPayload = {
      manifestVersion: '2.0.0',
      signatureType: 'community',
      plugin: 'yuuki-llmstudio-app',
      version: '0.1.0',
      files: {
        'plugin.json': hashOf(path.join(tmpDir, 'plugin.json')),
        'img/logo.svg': hashOf(path.join(tmpDir, 'img/logo.svg')),
      },
    };
    fs.writeFileSync(path.join(tmpDir, 'MANIFEST.txt'), buildManifestText(validPayload));

    const parsed = verifySignedManifest(tmpDir, {
      expectedPluginId: 'yuuki-llmstudio-app',
      expectedVersion: '0.1.0',
      expectedSignatureType: 'community',
    });
    assert.equal(parsed.signatureType, 'community');

    // Rejects unmanifested extra file
    fs.writeFileSync(path.join(tmpDir, 'extra.txt'), 'unexpected');
    assert.throws(
      () => verifySignedManifest(tmpDir, { expectedPluginId: 'yuuki-llmstudio-app' }),
      /not covered by MANIFEST.txt/
    );
    fs.rmSync(path.join(tmpDir, 'extra.txt'));

    // Rejects post-signing file modification
    fs.writeFileSync(path.join(tmpDir, 'plugin.json'), '{"id":"yuuki-llmstudio-app","tampered":true}');
    assert.throws(
      () => verifySignedManifest(tmpDir, { expectedPluginId: 'yuuki-llmstudio-app' }),
      /SHA-256 mismatch/
    );

    // Rejects Windows backslash path separators in MANIFEST.txt
    const badPathPayload = {
      ...validPayload,
      files: {
        'plugin.json': hashOf(path.join(tmpDir, 'plugin.json')),
        'img\\logo.svg': hashOf(path.join(tmpDir, 'img/logo.svg')),
      },
    };
    fs.writeFileSync(path.join(tmpDir, 'MANIFEST.txt'), buildManifestText(badPathPayload));
    assert.throws(
      () => verifySignedManifest(tmpDir, { expectedPluginId: 'yuuki-llmstudio-app' }),
      /Windows backslash/
    );
  } finally {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});
