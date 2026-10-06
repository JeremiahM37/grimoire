import esbuild from 'esbuild'
import { builtinModules } from 'node:module'

const production = process.argv[2] === 'production'

const ctx = await esbuild.context({
  entryPoints: ['src/main.ts'],
  bundle: true,
  // Obsidian provides these at runtime; bundling a second CodeMirror would
  // break the editor extension.
  external: ['obsidian', 'electron', '@codemirror/*', '@lezer/*', ...builtinModules],
  format: 'cjs',
  target: 'es2020',
  logLevel: 'info',
  sourcemap: production ? false : 'inline',
  treeShaking: true,
  outfile: 'main.js',
})

if (production) {
  await ctx.rebuild()
  await ctx.dispose()
} else {
  await ctx.watch()
}
