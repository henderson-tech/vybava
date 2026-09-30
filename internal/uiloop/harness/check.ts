// Manifest validation against the repo's uiLoop config, run by `vybava ui-loop
// check` through the repo's tsRunner. Prints one JSON document
// ({ screens, captures, problems }) and exits 1 when there are problems.

import * as fs from 'node:fs';

import project from '../project';
import { SCREEN_KINDS, SCREEN_STATES, type Screen, validateScreens } from './manifest';
import type { Project } from './project';
import { type LoopConfig, appOf, fromRoot, loadConfigEnv } from './run';

export function checkManifest(p: Project, config: LoopConfig): { screens: number; captures: number; problems: string[] } {
  const problems = validateScreens(p.screens);
  let captures = 0;
  const kinds: readonly string[] = SCREEN_KINDS;
  const states: readonly string[] = SCREEN_STATES;
  for (const s of p.screens as readonly Screen[]) {
    if (!config.areas.includes(s.area)) problems.push(`${s.id}: area "${s.area}" is not in uiLoop.areas`);
    if (!kinds.includes(s.kind)) problems.push(`${s.id}: unknown kind "${s.kind}"`);
    if (s.state !== undefined && !states.includes(s.state)) problems.push(`${s.id}: unknown state "${s.state}"`);
    const app = appOf(s, config.apps);
    if (!app) {
      problems.push(
        s.app
          ? `${s.id}: app "${s.app}" is not in uiLoop.apps`
          : `${s.id}: no app — the config has ${Object.keys(config.apps).length} apps, so every screen names one`,
      );
      continue;
    }
    const cfg = config.apps[app]!;
    for (const v of s.viewports ?? []) if (!config.viewports[v]) problems.push(`${s.id}: unknown viewport "${v}"`);
    for (const t of s.themes ?? []) if (t !== 'light' && t !== 'dark') problems.push(`${s.id}: unknown theme "${String(t)}"`);
    if (s.viewports && !s.viewports.length) problems.push(`${s.id}: viewports is empty`);
    if (s.themes && !s.themes.length) problems.push(`${s.id}: themes is empty`);
    for (const f of s.sourceFiles) if (!fs.existsSync(fromRoot(f))) problems.push(`${s.id}: source file ${f} does not exist`);
    if (!s.unreachable) captures += (s.viewports ?? cfg.viewports).length * (s.themes ?? cfg.themes).length;
  }
  return { screens: p.screens.length, captures, problems };
}

function main(): void {
  const result = checkManifest(project as Project, loadConfigEnv());
  process.stdout.write(`${JSON.stringify(result)}\n`);
  if (result.problems.length) process.exit(1);
}

main();
