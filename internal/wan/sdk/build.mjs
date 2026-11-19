// Run this trusted script from the repository root, never from an acquired archive.
import { readFile, writeFile, realpath } from 'node:fs/promises';
import { builtinModules, createRequire, isBuiltin } from 'node:module';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const repository = path.resolve(here, '../../..');
const args = process.argv.slice(2);
if (args.length !== 0 && (args.length !== 2 || args[0] !== '--dependency-root')) {
  throw new Error('Usage: node internal/wan/sdk/build.mjs [--dependency-root PATH_TO_NODE_MODULES]');
}
if (await realpath(process.cwd()) !== await realpath(repository)) {
  throw new Error('Run the build from the repository root, not a dependency or extracted-archive directory.');
}
if (process.platform !== 'win32' || process.arch !== 'x64' || Number(process.versions.node.split('.')[0]) < 20) {
  throw new Error('This verified build-tool lock supports Windows x64 with Node 20+ only.');
}
if (process.env.ESBUILD_BINARY_PATH) {
  throw new Error('Do not override the pinned esbuild platform binary with ESBUILD_BINARY_PATH.');
}
const dependencies = await realpath(args.length ? path.resolve(args[1]) : path.join(here, 'node_modules'));
const lock = JSON.parse(await readFile(path.join(here, 'package-lock.json'), 'utf8'));
const manifest = JSON.parse(await readFile(path.join(here, 'package.json'), 'utf8'));
if (lock.lockfileVersion !== 3 || JSON.stringify(lock.packages[''].devDependencies) !== JSON.stringify(manifest.devDependencies) || JSON.stringify(lock.packages[''].dependencies) !== JSON.stringify(manifest.dependencies)) {
  throw new Error('package.json and package-lock.json must describe the same pinned graph.');
}
const within = (root, file) => {
  const relative = path.relative(root, file);
  return relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative));
};
// Check installed package identities as data before loading the build tool. npm ci
// verifies archive integrity; manually prepared graphs must do that independently.
for (const [location, expected] of Object.entries(lock.packages)) {
  if (!location) continue;
  const relative = location.replace(/^node_modules\//, '');
  let installed;
  try {
    installed = JSON.parse(await readFile(path.join(dependencies, relative, 'package.json'), 'utf8'));
  } catch (error) {
    if (expected.optional && error.code === 'ENOENT') continue;
    throw error;
  }
  const name = location.slice(location.lastIndexOf('node_modules/') + 'node_modules/'.length);
  if (installed.name !== name || installed.version !== expected.version) {
    throw new Error(`Dependency differs from the lock: ${location}`);
  }
}
const tool = path.join(dependencies, 'esbuild');
const requireTool = createRequire(pathToFileURL(path.join(tool, 'package.json')));
const esbuild = requireTool('./lib/main.js');
if (esbuild.version !== manifest.devDependencies.esbuild) {
  throw new Error('The build tool must match the exact pinned esbuild version.');
}
const sourceFiles = new Set([path.join(here, 'host.ts')]);
const fallbacks = new Map([
  ['bufferutil', path.join(dependencies, 'bufferutil/fallback.js')],
  ['utf-8-validate', path.join(dependencies, 'utf-8-validate/fallback.js')],
]);
const builtin = new Set([...builtinModules, ...builtinModules.map(name => `node:${name}`)]);
const actualFiles = new Map();
// Canonical dependency paths keep output bytes independent of the acquisition
// folder. Resolve code only from the selected graph and the owned entry source.
const sealedGraph = {
  name: 'sealed-wan-dependencies',
  setup(build) {
    build.onResolve({ filter: /.*/ }, async args => {
      if (args.pluginData?.wanResolving) return;
      // debug's optional console-color probe is normally absent from this graph.
      // Keep its try/catch behavior without leaving a runtime package lookup.
      if (args.path === 'supports-color' && args.namespace === 'wan-dependency' && args.importer === 'node_modules/debug/src/node.js') {
        return { path: 'supports-color', namespace: 'wan-absent-optional' };
      }
      // The pinned SDK uses this undeclared library only for Node <10.12,
      // never as a retry after native RSA failure. This helper targets Node 20.
      if (args.path === 'node-rsa' && args.namespace === 'wan-dependency' && args.importer === 'node_modules/@microsoft/dev-tunnels-ssh/algorithms/node/nodeRsa.js') {
        return { path: 'node-rsa', namespace: 'wan-unsupported-legacy' };
      }
      if (fallbacks.has(args.path)) {
        const canonical = `node_modules/${args.path}/fallback.js`;
        actualFiles.set(canonical, await realpath(fallbacks.get(args.path)));
        return { path: canonical, namespace: 'wan-dependency' };
      }
      if (builtin.has(args.path) || isBuiltin(args.path)) return { path: args.path, external: true };
      const resolveDir = args.namespace === 'wan-dependency' ? args.resolveDir : path.dirname(dependencies);
      const result = await build.resolve(args.path, {
        kind: args.kind,
        resolveDir: args.kind === 'entry-point' ? repository : resolveDir,
        pluginData: { wanResolving: true },
      });
      if (result.errors.length) return { errors: result.errors };
      if (result.external) throw new Error(`Unexpected external dependency: ${args.path}`);
      const file = await realpath(result.path);
      if (sourceFiles.has(file)) return { path: file };
      if (!within(dependencies, file)) throw new Error(`Dependency escaped the selected graph: ${args.path}`);
      if (path.extname(file) === '.node') throw new Error(`Native runtime addon is forbidden: ${args.path}`);
      const canonical = `node_modules/${path.relative(dependencies, file).split(path.sep).join('/')}`;
      const packageLocation = canonical.match(/^(.*node_modules\/(?:@[^/]+\/)?[^/]+)(?:\/|$)/)?.[1];
      if (!lock.packages[packageLocation]) throw new Error(`Dependency is not in the lock: ${canonical}`);
      actualFiles.set(canonical, file);
      return { path: canonical, namespace: 'wan-dependency' };
    });
    build.onLoad({ filter: /.*/, namespace: 'wan-absent-optional' }, () => ({
      contents: 'throw new Error("Optional console colors are not bundled.");',
      loader: 'js',
    }));
    build.onLoad({ filter: /.*/, namespace: 'wan-unsupported-legacy' }, () => ({
      contents: 'throw new Error("Legacy Node RSA key generation is unsupported; native crypto is required.");',
      loader: 'js',
    }));
    build.onLoad({ filter: /.*/, namespace: 'wan-dependency' }, async args => {
      const file = actualFiles.get(args.path);
      const extension = path.extname(file);
      if (!['.js', '.cjs', '.mjs', '.json'].includes(extension)) throw new Error(`Unexpected runtime input: ${args.path}`);
      return { contents: await readFile(file), loader: extension === '.json' ? 'json' : 'js', resolveDir: path.dirname(file) };
    });
  },
};
const notices = await readFile(path.join(here, 'THIRD_PARTY_NOTICES.txt'), 'utf8');
const result = await esbuild.build({
  absWorkingDir: repository,
  entryPoints: ['internal/wan/sdk/host.ts'],
  outfile: 'internal/wan/sdk/host.cjs',
  bundle: true,
  platform: 'node',
  format: 'cjs',
  target: 'node20',
  mainFields: ['main', 'module'],
  conditions: ['node'],
  nodePaths: [dependencies],
  plugins: [sealedGraph],
  metafile: true,
  write: false,
  sourcemap: false,
  charset: 'utf8',
  legalComments: 'inline',
  logLevel: 'warning',
  logOverride: { 'unsupported-require-call': 'error' },
  banner: { js: `// Generated by build.mjs. Do not edit.\n${notices.trimEnd().split(/\r?\n/).map(line => `// ${line}`).join('\n')}` },
});
for (const [file, output] of Object.entries(result.metafile.outputs)) {
  for (const imported of output.imports) {
    if (imported.external && !builtin.has(imported.path) && !isBuiltin(imported.path)) {
      throw new Error(`Bundle requires a non-builtin at runtime: ${imported.path} (${file})`);
    }
  }
}
for (const input of Object.keys(result.metafile.inputs)) {
  if (/\.(node|wasm)$/.test(input) || /node_modules\/node-gyp-build\//.test(input) || (/node_modules\/(bufferutil|utf-8-validate)\//.test(input) && !/\/(bufferutil|utf-8-validate)\/fallback\.js$/.test(input))) {
    throw new Error(`Native addon or loader entered the bundle: ${input}`);
  }
}
if (result.outputFiles.length !== 1) throw new Error('The runtime must be one self-contained CJS file.');
await writeFile(path.join(here, 'host.cjs'), result.outputFiles[0].contents);
await writeFile(path.join(here, 'host.meta.json'), JSON.stringify(result.metafile, null, 2) + '\n');
console.log(`Built host.cjs with esbuild ${esbuild.version}; ${Object.keys(result.metafile.inputs).length} inputs; only Node builtins external.`);
