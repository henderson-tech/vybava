// Native inputs @expo/fingerprint cannot see on its own, for every consumer of
// the app's fingerprint: `bun run app:remote` (the dev-client rebuild verdict,
// scripts/dev/app-remote.ts) and Výbava perflab's native build index (the
// `perflab` section of vybava.config.ts keys builds by this fingerprint).
//
// Reanimated compiles `reanimated.staticFeatureFlags` from this package.json
// into the native build (its Podspec and Gradle script read them), while the
// default fingerprint never hashes that key: flipping a flag needs a new
// binary, and without this source the fingerprint said the old one still fit.
//
// CommonJS on purpose: @expo/fingerprint require()s this file, and a file it
// cannot load is skipped without a word (the source would silently vanish).
const { reanimated } = require('./package.json');

/** @type {import('@expo/fingerprint').Config} */
module.exports = {
  extraSources: [
    {
      type: 'contents',
      id: 'apps/client/package.json#/reanimated/staticFeatureFlags',
      contents: JSON.stringify(reanimated?.staticFeatureFlags ?? {}),
      reasons: ['reanimatedStaticFeatureFlags'],
    },
  ],
};
