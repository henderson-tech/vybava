// Renders the app map (uiLoop.appMap) from the screen manifest: per area,
// every screen depth-first under the screen it opens from, variants folded
// into their base. `vybava ui-loop map` writes it; `vybava ui-loop check`
// runs it with --check, which exits 1 when the committed map is stale.

import * as fs from 'node:fs';
import * as path from 'node:path';

import project from '../project';
import { SCREEN_KINDS, type Screen, type Theme } from './manifest';
import type { Project } from './project';
import { type LoopConfig, appOf, fromRoot, loadConfigEnv } from './run';

const code = (s: string): string => `\`${s}\``;

export function renderAppMap(p: Project, config: LoopConfig): string {
  const screens = p.screens;
  const base = screens.filter((s) => !s.variantOf);
  const variantsOf = (id: string): Screen[] => screens.filter((s) => s.variantOf === id);
  const areaOf = new Map(base.map((s) => [s.id, s.area]));
  const multiApp = Object.keys(config.apps).length > 1;
  const appCfg = (s: Screen) => {
    const app = appOf(s, config.apps);
    return app ? { app, cfg: config.apps[app]! } : null;
  };
  const viewportsOf = (s: Screen): readonly string[] => s.viewports ?? appCfg(s)?.cfg.viewports ?? [];
  const themesOf = (s: Screen): readonly Theme[] => s.themes ?? appCfg(s)?.cfg.themes ?? [];
  const captures = (s: Screen): number => (s.unreachable ? 0 : viewportsOf(s).length * themesOf(s).length);

  const line = (s: Screen, note: string): string => {
    const what = s.kind === 'state' ? `state:${s.state ?? '?'}` : s.kind;
    const a = appCfg(s);
    const variants = variantsOf(s.id);
    const extras = [
      multiApp && a ? `app ${a.app}` : '',
      s.as !== undefined && s.as !== p.defaultAs ? `as ${s.as}` : '',
      s.viewports && a && s.viewports.join() !== a.cfg.viewports.join() ? `@${s.viewports.join(',')}` : '',
      s.themes && a && s.themes.join() !== a.cfg.themes.join() ? `${s.themes.join('+')} only` : '',
      variants.length ? `+ ${variants.map((v) => code(v.id)).join(', ')}` : '',
      s.unreachable ? '**unreachable**' : '',
      s.destructive ? '**destructive**' : '',
      note,
    ].filter(Boolean);
    return [code(s.id), what, code(s.route), s.title, s.sourceFiles.map(code).join(', ') || 'no source files', ...extras].join(' · ');
  };

  const areas = [...config.areas, ...[...new Set(base.map((s) => s.area))].filter((a) => !config.areas.includes(a)).sort()];
  const appDefaults = Object.entries(config.apps)
    .map(([name, cfg]) => `${multiApp ? `${name}: ` : ''}${cfg.viewports.join(', ')} × ${cfg.themes.join(', ')}`)
    .join(' · ');
  const out: string[] = [
    '# App map',
    '',
    'Every screen: route × tab × panel/overlay × state, depth-first under the screen it opens from.',
    `Generated from the screen manifest (${code(`${config.dir}/project.ts`)}) by ${code('vybava ui-loop map')} — never edit it by hand; ${code('vybava ui-loop check')} fails when it is stale.`,
    `A line reads ${code('id')} · kind · ${code('route')} · title · source files · extras (app, identity, non-default viewports or themes, re-shot variants). Defaults: ${appDefaults}.`,
    '',
    '| Area | Screens | States | Captures | Unreachable |',
    '|---|--:|--:|--:|--:|',
  ];
  let totals = [0, 0, 0, 0];
  for (const area of areas) {
    const inArea = base.filter((s) => s.area === area);
    const row = [
      inArea.filter((s) => s.kind !== 'state').length,
      inArea.filter((s) => s.kind === 'state').length,
      screens.filter((s) => s.area === area).reduce((n, s) => n + captures(s), 0),
      inArea.filter((s) => s.unreachable).length,
    ];
    totals = totals.map((t, i) => t + row[i]!);
    out.push(`| ${area} | ${row.join(' | ')} |`);
  }
  out.push(`| **all** | ${totals.map((t) => `**${t}**`).join(' | ')} |`, '');
  out.push(`By kind: ${SCREEN_KINDS.map((k) => `${k} ${base.filter((s) => s.kind === k).length}`).join(' · ')}.`, '');

  for (const area of areas) {
    const inArea = base.filter((s) => s.area === area);
    if (!inArea.length) continue;
    const children = (id: string): Screen[] => inArea.filter((s) => s.parentId === id);
    const roots = inArea.filter((s) => !s.parentId || areaOf.get(s.parentId) !== area);
    out.push(`## ${area}`, '', `${inArea.filter((s) => s.kind !== 'state').length} screens · ${inArea.filter((s) => s.kind === 'state').length} states`, '');
    const walk = (s: Screen, depth: number): void => {
      const note = depth === 0 && s.parentId ? `opens from ${code(s.parentId)} (${areaOf.get(s.parentId) ?? '?'})` : '';
      out.push(`${'  '.repeat(depth)}- ${line(s, note)}`);
      for (const c of children(s.id)) walk(c, depth + 1);
    };
    for (const r of roots) walk(r, 0);
    const questions = p.notes?.areas?.[area] ?? [];
    if (questions.length) out.push('', 'Open questions:', '', ...questions.map((q) => `- ${q}`));
    out.push('');
  }

  const unreachable = base.filter((s) => s.unreachable);
  out.push('## Unreachable on the dev stack', '');
  out.push(...(unreachable.length ? unreachable.map((s) => `- ${code(s.id)}: ${s.unreachable ?? ''}`) : ['None — every mapped screen has a recipe.']), '');
  const issues = screens.filter((s) => s.knownIssues?.length);
  out.push('## Known issues', '');
  out.push(...(issues.length ? issues.flatMap((s) => (s.knownIssues ?? []).map((i) => `- ${code(s.id)}: ${i}`)) : ['None recorded.']), '');
  if (p.notes?.crossCutting?.length) out.push('## Cross-cutting', '', ...p.notes.crossCutting.map((n) => `- ${n}`), '');
  if (p.notes?.decisions?.length) out.push('## Decisions', '', ...p.notes.decisions.map((n) => `- ${n}`), '');
  return out.join('\n');
}

function main(): void {
  const config = loadConfigEnv();
  const file = fromRoot(config.appMap);
  const next = renderAppMap(project as Project, config);
  if (process.argv.includes('--check')) {
    const current = fs.existsSync(file) ? fs.readFileSync(file, 'utf-8') : '';
    if (current !== next) {
      console.error(`app-map: ${config.appMap} is stale — run \`vybava ui-loop map\` and commit it`);
      process.exit(1);
    }
    console.log(`app-map: ${config.appMap} is up to date`);
    return;
  }
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, next, 'utf-8');
  console.log(`app-map: wrote ${config.appMap} (${project.screens.length} entries)`);
}

main();
