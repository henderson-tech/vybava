# tsgate - the typecheck gate on TypeScript 7

TypeScript 7 is the native (Go) compiler: FixIt's api spec program checks in
8 s where TypeScript 6 took 63 s, with identical diagnostics. It ships no
classic compiler API, though, and every tool that imports `typescript`
(ts-node, jest config loaders, typescript-eslint, declaration builders, repo
scripts) still needs one. So a repo keeps `typescript` on 6 or 5.9 and adds
TS 7 beside it; `tsgate` runs the typecheck on the TS 7 one.

```sh
vybava install tsgate
tsgate tsconfig.json                          # the gate a typecheck script calls
tsgate apps/api/tsconfig.spec.json -- --extendedDiagnostics
tsgate -b tsconfig.json                       # build mode (the tsconfig must need no derivation)
tsgate plan tsconfig.json                     # compiler found, config read, why
tsgate parity tsconfig.json                   # classic compiler vs TS 7, exit 1 on a difference
tsgate                                        # every program in vybava.config.ts tsgate.programs
```

## Adopting it in a repo

1. Add the alias beside `typescript`, exact version:
   `"typescript7": "npm:typescript@7.0.2"` in the devDependencies of the
   package whose typecheck moves (or the root). The TS 7 release post's
   `"@typescript/native"` alias works too; so does `typescript` itself once
   nothing in the repo needs the classic API.
2. `"typecheck": "tsgate tsconfig.json"`, and keep the old command as
   `"typecheck:tsc6": "tsc -p tsconfig.json --noEmit"`: the comparison when an
   editor and the gate disagree, and the one-line rollback.
3. `.gitignore`: `.*.ts7.json` (only written for a chain that needs deriving).
4. CI installs the applet from a release through `ci/install.sh`
   (`--install tsgate`) before the typecheck step; the Devbox and each Mac
   need a Výbava that carries it.
5. The PR carries `tsgate parity` for every program it moves (after injecting
   a few errors, so identical output is not merely zero on both sides) and the
   wall times it prints.

Optional config, `vybava.config.ts`:

```ts
tsgate: {
  programs: ['apps/api/tsconfig.app.json', 'apps/api/tsconfig.spec.json'],
  compiler: 'typescript7', // the dependency name; default tries typescript7, @typescript/native, typescript
},
```

## What it does

- **Finds TS 7 by version, never by bin.** The `typescript@7` package's one
  bin is also named `tsc`, so beside a classic `typescript` the
  `node_modules/.bin/tsc` link is whichever the linker wrote last, and
  `bunx tsc` or `node node_modules/typescript7/bin/tsc` fail under some
  linkers. tsgate walks up node_modules to the first listed dependency whose
  package is version 7, then execs the native binary its shim would:
  `@typescript/typescript-<platform>-<arch>/lib/tsc`, beside the package (bun's
  isolated or global store) or in any node_modules above it. No node or bun is
  involved, and it runs in the caller's directory, so paths print as a bare
  `tsc` there prints them.
- **Derives a config only when TS 7 refuses the chain.** It reads the
  tsconfig and everything it `extends` (relative paths and packages, including
  `exports` subpaths like `expo/tsconfig.base`), merged the way `extends`
  merges, and flattens it into `.<name>.ts7.json` beside the leaf, rewritten on
  every run so it cannot drift, when the chain sets:
  - `baseUrl`: dropped for the `"*"` paths mapping TS documents as its
    equivalent; every `paths` target is re-anchored;
  - `moduleResolution` `node`/`node10`: `module preserve` + `moduleResolution
    bundler` + `resolvePackageJsonExports: false` (node10 read no `exports`,
    so deep imports into exports-mapped packages keep resolving). Only these
    chains are forced; a `bundler` or `nodenext` chain keeps its own;
  - `outDir` without `rootDir`: TS 7 then takes `rootDir` as the config's
    directory and refuses every file outside it (TS6059), which a monorepo
    program reaches through `paths`; `rootDir` becomes the checkout root.

  A derived config also drops `incremental`, `tsBuildInfoFile` (TS 7 must
  never overwrite the TS 6 build info) and `ignoreDeprecations`. Every path is
  re-anchored to the derived file; `${configDir}` means the leaf's directory.
  A chain TS 7 reads as it is runs unchanged.
- **Leaves every other removed option to TS 7.** `target: es5`,
  `esModuleInterop: false`, `module: amd` and the like have no equivalent to
  rewrite to; TS 7 names them (TS5102/TS5108) and the tsconfig has to move.
- **`parity`** runs the classic compiler (`typescript` while it is 6 or 5.9,
  else `@typescript/typescript6`, launched with node or bun) on the real
  tsconfig and TS 7 on what tsgate runs, both with `--pretty false`, and
  compares diagnostics by file, position and code (wording may differ between
  versions). It prints each side's wall time and diagnostic count.

## Known differences

- A node10 chain checked under `module preserve` does not flag `import.meta`
  or top-level `await` (TS1343/TS1378) the way TS 6 on commonjs does. `parity`
  shows it when a program has one.
- 5.9 to 7 also brings TS 6's default changes: `types` defaults to `[]`, so a
  program leaning on implicit `@types` or `bun:test` globals names them
  (`"types": ["bun"]`). `parity` against 5.9 shows these as TS 7-only errors.
- The IDE still reads the tsconfig with the classic compiler, so an editor and
  the gate can disagree on the differences above.
