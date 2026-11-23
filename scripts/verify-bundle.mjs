#!/usr/bin/env node
/**
 * Verifies the built frontend AMD bundle (dist/module.js) for dual compatibility
 * with Grafana OSS 12.4.15 (React 18) and Grafana OSS 13.3.1 (React 19):
 *  - Ensures required shared runtime externals (including react/jsx-runtime) are declared in AMD define([...]).
 *  - Ensures non-shared modules (e.g., @grafana/schema, @grafana/i18n, react-router-dom, react-dom/client)
 *    are NOT externalized.
 *  - Ensures zero bundled React SECRET_INTERNALS appear in dist/module.js.
 */
import fs from 'node:fs';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');
const bundlePath = path.join(root, 'dist', 'module.js');
const pluginJsonPath = path.join(root, 'dist', 'plugin.json');

if (!fs.existsSync(bundlePath)) {
  throw new Error(`Missing built bundle at ${bundlePath}. Run "npm run build" first.`);
}
if (!fs.existsSync(pluginJsonPath)) {
  throw new Error(`Missing built plugin manifest at ${pluginJsonPath}. Run "npm run build" first.`);
}

const bundle = fs.readFileSync(bundlePath, 'utf8');
if (bundle.length === 0) {
  throw new Error(`Bundle ${bundlePath} is empty.`);
}

const pluginJson = JSON.parse(fs.readFileSync(pluginJsonPath, 'utf8'));
if (pluginJson.id !== 'yuuki-llmstudio-app') {
  throw new Error(`Unexpected dist/plugin.json id: ${JSON.stringify(pluginJson.id)}`);
}

// Webpack AMD output starts with `define(["dep1","dep2",...], ...)` (possibly preceded by banner comments).
const defineMatch = bundle.match(/define\(\s*\[([^\]]*)\]/);
if (!defineMatch) {
  throw new Error('Could not find AMD define([...]) dependency array in dist/module.js.');
}

const declaredExternals = new Set(
  Array.from(defineMatch[1].matchAll(/["']([^"']+)["']/g), (m) => m[1])
);

const requiredExternals = [
  'react',
  'react-dom',
  'react/jsx-runtime',
  '@grafana/data',
  '@grafana/runtime',
  '@grafana/ui',
  '@emotion/css',
  'rxjs',
];

const missingExternals = requiredExternals.filter((dep) => !declaredExternals.has(dep));
if (missingExternals.length > 0) {
  throw new Error(
    `dist/module.js is missing required AMD externals: ${missingExternals.join(', ')}\n` +
      `Declared externals: ${Array.from(declaredExternals).sort().join(', ')}`
  );
}

const forbiddenExternals = [
  '@grafana/schema',
  '@grafana/i18n',
  'react-router-dom',
  'react-dom/client',
  'react-dom/server',
];

const leakedExternals = forbiddenExternals.filter((dep) => declaredExternals.has(dep));
if (leakedExternals.length > 0) {
  throw new Error(
    `dist/module.js externalized modules absent from Grafana 12.4.15/13.3.1 sharedDependenciesMap: ${leakedExternals.join(', ')}`
  );
}

const forbiddenSymbols = [
  'SECRET_INTERNALS',
  '__CLIENT_INTERNALS_DO_NOT_USE_OR_WARN_USERS_THEY_CANNOT_UPGRADE',
];

const jsFiles = fs
  .readdirSync(path.join(root, 'dist'))
  .filter((name) => name.endsWith('.js'))
  .sort();

for (const file of jsFiles) {
  const content = file === 'module.js' ? bundle : fs.readFileSync(path.join(root, 'dist', file), 'utf8');
  for (const sym of forbiddenSymbols) {
    if (content.includes(sym)) {
      throw new Error(
        `dist/${file} contains forbidden bundled React internal symbol "${sym}". ` +
          'Ensure react, react-dom, and react/jsx-runtime are externalized.'
      );
    }
  }
}

console.log(
  `Bundle verification OK: dist/module.js externalizes [${Array.from(declaredExternals).sort().join(', ')}] and ${jsFiles.length} dist/*.js file(s) contain zero SECRET_INTERNALS.`
);
