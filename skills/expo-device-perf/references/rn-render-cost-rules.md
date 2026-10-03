# React Native render-cost rules

Evidence cites FixIt (henderson-tech/FixIt): squash-merged PRs, short SHAs of commits inside PR #1770 or on the vt-4229 branch, vitrinka fixit/4740 board cards and Eve review comment ids. Numbers are release perf builds on a Samsung S20 (Android 13, 120 Hz) or iPhone Air (iOS 26, 120 Hz) unless named.

## Android drawing

### 1. Android has no partial redraw
Any change repaints the whole window: budget every always-on animation (a pulsing dot, shimmer, Skia canvas) as a full-window frame every vsync.
Check: `perflab analyze <pftrace> --sql damage`.
Evidence: RenderThread `Drawing 0.00 0.00 1080.00 2016.00` on 733 of 733 frames of a hub fling trace.

### 2. Long scroll content: virtualize lists, layer static stacks
Long uniform lists go to FlashList; a static card stack in a ScrollView gets one Android hardware layer per static block (rules 3-4).
Check: `probe drag`, `probe fling`; `hazards` lists ScrollView route files.
Evidence: PR #1770 (4c2099245b), worker hub in 6 layered blocks: slow drags 1189 -> ~1590 frames per 20 s, RenderThread draw 7.7 -> 3.7 ms, RT p50/p90/p99 8.3/11.8/13.6 -> 4.9/8.8/11.1 ms; flings draw 3.4 -> 2.6 ms, two-vsync gaps 44-49 -> 35-45. iOS unchanged (Android-only props).

### 3. Hardware layers need `collapsable={false}` under Fabric
A layer toggling at runtime keeps `collapsable={false}` constant on Android and toggles only `renderToHardwareTextureAndroid`.
Check: `hazards` flags the texture prop without `collapsable`.
Evidence: PR #1770. ModeStack (d1ac9d0b4a) toggles only the texture (rule 6). PR #1770's HubBlock flips `collapsable={!layer}`; that re-mount cost is unmeasured.

### 4. Never layer content that animates every frame
Keep ambient animations between blocks, never inside one. A block whose content can animate (a shimmer, a live item) drops its layer while it does, through a layer hold:

```tsx
const LayerHoldContext = createContext<(() => () => void) | null>(null);

function useLayerHolds() {
  const [holds, setHolds] = useState(0);
  // Stable: holders' effects depend on it.
  const acquire = useCallback(() => {
    setHolds((h) => h + 1);
    return () => setHolds((h) => h - 1);
  }, []);
  return { held: holds > 0, acquire };
}

// In the animating descendant, e.g. a skeleton: useLayerHold(!reduceMotion).
function useLayerHold(active: boolean) {
  const acquire = useContext(LayerHoldContext);
  // Layout effect: unlayered before the first frame paints.
  useLayoutEffect(() => (active && acquire ? acquire() : undefined), [active, acquire]);
}

function LayeredBlock({ layered = true, style, children }: LayeredBlockProps) {
  const holds = useLayerHolds();
  const layer = Platform.OS === 'android' && layered && !holds.held;
  return (
    <LayerHoldContext value={holds.acquire}>
      <View style={style} collapsable={Platform.OS !== 'android'} renderToHardwareTextureAndroid={layer}>
        {children}
      </View>
    </LayerHoldContext>
  );
}
```

Evidence: PR #1770 (2790f17bda -> c6de07c21f): one hold in the skeleton fixed every retry slot and replaced a per-site flag; Eve 4163608433 and 4163734054 flagged shimmers inside layered blocks; a contract test keeps the pulse pill outside every block.

### 5. No `removeClippedSubviews` on scroll content
Evidence: hub culling gave +27% slow-drag frames, but fling two-vsync gaps 44-49 -> 55-68 and main p99 5.5-7.1 -> 8.2-9.7 ms; layers plus culling drew 1499 drag frames vs 1535-1592 for layers alone (card 51570).

### 6. Raise a moving body as one texture, only while it moves
Turn the texture on when a whole-body translate starts and off in the animation's finish callback (`withTiming(1, cfg, (finished) => { if (finished) scheduleOnRN(setRising, false); })`). A body hidden mid-move drops it. A fading veil stays outside the texture.
Evidence: d1ac9d0b4a, calendar mode switch: RenderThread draw 6.1 -> 3.2 ms per frame; median / worst-decile fps 110/102 -> 114/108.

### 7. Reanimated: synchronous UI props on Android; split layout props
Set `"reanimated": {"staticFeatureFlags": {"ANDROID_SYNCHRONOUSLY_UPDATE_UI_PROPS": true}}` in the app's `package.json` (iOS twin `IOS_SYNCHRONOUSLY_UPDATE_UI_PROPS`). It is native: a new native key and build; OTA cannot carry it. Layout props (`width`, `left`/`right`): animate them in a separate style from transforms, skip the layout spring when unchanged, or grow with a transform on a masking container.
Evidence: 0b0cb65f61: one translate + one opacity cost 4-7 ms of main-thread animation phase per frame before; mode switches 92/77 -> 110/102, janky 26.6% -> 13.1% (perflab analyze; the earlier TypeScript reader placed stale DisplayPresentTime frames and read 91/77, 26.9%). A `width` animated beside `translateX` froze the indicator behind the tap's commit, then it jumped.

### 8. Rule out composer-handled layers before blaming media
A backdrop the hardware composer composes costs no GPU frame time: check `gpu_composition` share in `analyze` first.
Evidence: video vs still backdrop A/B: no Perfetto difference (card 51571).

## Loops at rest

### 9. No endless loop on a screen at rest
No `withRepeat(..., -1)`, always-on `useFrameCallback` or Skia clock. An ambient cue runs a bounded number of times and re-arms on the next touch:

```ts
const PULSE_MS = 2000;
const PULSES_PER_RUN = 3;

useEffect(() => {
  if (!ambientOpen || reduceMotion) return; // gate: focused, app active, touched within 60 s
  let restsAt = 0;
  const playRun = () => {
    restsAt = Date.now() + PULSES_PER_RUN * PULSE_MS;
    scale.set(withRepeat(
      withSequence(withTiming(peak, { duration: PULSE_MS / 2 }), withTiming(1, { duration: PULSE_MS / 2 })),
      PULSES_PER_RUN,
    ));
  };
  playRun();
  // Every touch start, capture phase, no React state.
  const stopListening = onAmbientInteraction(() => { if (Date.now() >= restsAt) playRun(); });
  return () => { stopListening(); cancelAnimation(scale); };
}, [ambientOpen, reduceMotion]);
```

Reduce Motion shows the static state. A test pins the repeat count (an infinite `-1` fails), no restart mid-run, a restart after rest, and unsubscribe on unmount and gate close.
Check: `probe rest` after one touch.
Evidence: PulseDot, owner hub, 20 s at rest: 2394 frames (max 2400), RenderThread busy 11.5 s, main 8.0 s -> 582 frames, 2.5 s, 4.8 s. A loop under another screen kept Choreographer running: 0.5-1 ms main per vsync at rest.

### 10. Gate every loop on real visibility
Gate on the ambient state AND the visible condition (`useHalo(animating && !quiet && shown)`). `hazards` lists loop sites; review visibility by hand.
Evidence: 44b7986b09: a map pin's halo looped in every journey state without a drawn pin.

### 11. No Skia `usePathValue` (react-native-skia 2.6 under Reanimated 4.5)
Use `useDerivedValue(() => Skia.Path.RRect(...))`, or `Skia.PathBuilder.Make()` ... `b.detach()` inside `useDerivedValue`. Ban `/\busePathValue\s*\(/` in a source-contract test.
Check: `probe rest`; main-thread eglSwaps above 0 at rest.
Evidence: PR #1770 (44b7986b09): a settled journey, 20 s at rest: 2388 frames, RT 12.4 s, main 9.7 s, 4786 main-thread GL swaps -> 0 frames, 0 swaps, main 3.7 s; pixel diff unchanged.

### 12. A derived value tracks only the `.value` reads in its own worklet
Read every `.value` directly in the worklet passed to `useDerivedValue` / `useAnimatedStyle`; pass values into helpers as arguments.
Evidence: SheetSurface's `bandBody()` read `frame.value.progress` and `seam.value` itself and never updated; fixed as `bandBody(frame.value.progress, seam.value)`.

## Skia

### 13. One canvas per row, keyed on its drawing; numbers to native setters
Draw a row (or more) per canvas (each is its own CAMetalLayer + IOSurface), with overlay Pressables carrying testIDs and accessibility labels. Key a `SkiaPictureView` on everything its picture is drawn from (`key={JSON.stringify([months, width, gap, digitSize, ink])}`): react-native-skia 2.6.2-2.14 drops a picture set from JS while the previous draw runs. Never pass booleans to `SkFont.setSubpixel` / `setEmbolden` (they throw on device; mocked tests pass); Skia and native-module calls get a device smoke run.
Evidence: a year view 24 -> 8 canvases (64953e2a25; its worst step had been 65 ms JS + 17 ms main). Stale rows on 2 of 2 cold launches -> correct in 9 of 9 shots (54df75d89f). The boolean setter threw "Value is true, expected a number" (de56e3c367).

## React work per render

### 14. Gate React Compiler bails with the compiler Metro loads
Resolve the compiler as Metro does (app -> `expo` -> `babel-preset-expo` -> `babel-plugin-react-compiler`), never the app's own devDependency. Run it with Metro's options: `target: '19'`, `panicThreshold: 'none'`, `environment.enableCustomTypeDefinitionForReanimated: true`, `customOptOutDirectives: ['widget', 'use no memo', 'use no forget']`, plus a logger collecting events. Fail on any CompileError / PipelineError and on any module-level component or `use*` function never compiled; list `'use no memo'` functions without failing.

| Bail | Fix |
|---|---|
| hook called off an object (`source.useX()`) | a `'use no memo'` bridge hook (`usePersonaOf(source, input)`) |
| ref read during render | state (`const [origins] = useState(() => new Map())`, `const [gate] = useState(() => createGate(ms))`); a module-level id counter instead of `seq.current++` |
| `sv.value = x` in a returned handler | `sv.set(x)` |
| `??=`; try/finally without catch | rewrite |
| a `use*` function calling no hook | never compiled and no event: make it a hook or rename it |
| a latest-state ref | carry the data in the reducer action so callbacks close only over `dispatch`; a no-op reducer case returns `state` |

Evidence: the calendar screen, month grid, day timeline and two press hooks shipped uncompiled; claims based on the app's rc.3 devDependency (never loaded; Metro loads 1.0.0) were wrong. FixIt gate: `scripts/ci/check-react-compiler.ts`.

### 15. Stable data identity
`useQueries` gets a module-level `combine`. Build derived sets inside `combine`, map DTOs through a `WeakMap<Dto, View>`, precompute per-item placement fields, and keep one cache entry per scope and period, never per visible window.
Evidence: `formatToParts` placement cost ~43 us per item per render on Hermes; one entry per scope and month stopped 2-4 refetches and re-renders per month crossed (de56e3c367).

### 16. Cache Intl formatters app-wide
A bounded cache keyed by locale + sorted defined options; a throwing construction caches nothing. Same for `Intl.RelativeTimeFormat` and `Intl.NumberFormat`. `hazards` flags `new Intl.` in a component body.
Evidence: on Hermes `new Intl.DateTimeFormat` costs 88-107 us vs ~1.5 us per cached `.format()`; a month render made 100-140 constructions, 10-15 ms.

### 17. A mounted hidden screen makes zero commits
A hidden screen's tree subscribes to no router store (`usePathname`): latch focus with `useFocusEffect` + `useState`, and pass TanStack `subscribed: isFocused` to its reads.
Check: park on another tab 60 s, push and pop 20 screens; the hidden screen's JS-thread frames in Time Profiler are hidden renders.
Evidence: one hook calling `usePathname` re-rendered the hidden calendar on every navigation anywhere; its observers refetched 3-25 windows per invalidation sweep.

### 18. No ratchets
A list extending on `onEndReached` grows to its cap on sparse data. Extend only after a real user drag, once per edge, after the edge window delivered; reset on leave; size persisted-cache `maxEntries` to the reach.
Check: slope over N identical cycles.
Evidence: 25 live month queries on a sparse account; opening the list 12 -> 4 GETs.

### 19. No per-cell Reanimated
No shared value + animated style per cell. Use a plain pressed tint or opacity, `const Box = entering ? Animated.View : View`, and one highlight shared value per grid keyed by the pressed id. CSS transitions are not free either.
Evidence: 90-125 mappers and ~300 shareables per month mount, ~500 UI jobs on unmount, then a batched settle re-render of ~100 components 1-1.5 s later.

### 20. Keys never defeat recycling or remount a list
Key recycled rows and cells by index or recycle slot. Never `key={mode}`, `key={term}` or `key={start}` on a list: keep it mounted and swap its data. Exception: a native drawing view keyed on its drawing (rule 13).
Evidence: block keys carrying a resize id remounted ~130 native views and 31 mappers per month crossed; a search-term key blanked the stage 0.3-0.6 s and lost the scroll position.

### 21. Stable gesture objects
A recreated RNGH gesture costs a native `updateGestureHandler` per detector plus worklet re-serialization every render; compiled components memoize them.
Evidence: 1-3 -> 0 updates on a tap that changes no gesture config.

### 22. Reanimated 4: create shared values at their start value
A JS write to a shared value paints the old value on mount: create it with `makeMutable(start)` (FixIt ledger #t308).

## Mount and animation choreography

### 23. Mount nothing during an animation
Keep a level or mode body mounted once drawn, hidden off stage (`opacity: 0`, `pointerEvents: 'none'`, `accessibilityElementsHidden`, `importantForAccessibility: 'no-hide-descendants'`). A switch between drawn bodies mounts and unmounts nothing; a different target mounts afresh. State that matters only to the visible body updates only while it is the view. Neighbour pages lay out at once; their content mounts 800 ms after landing or at the first finger (gesture `onBegin`).
Evidence: the hitch was the incoming body's mount chunk (15-30 ms main: ~10 ms mounting + 5-10 ms text drawing) plus the outgoing unmount (32 ms). Kept levels: worker 1.98 -> 0.48 ms/s, customer 2.71 -> 1.60 (Air). A swipe 0.45 s after the tap still turned a full page.

### 24. iOS: fixed sibling order; no opacity over glass
Render layers in a fixed depth order; a sibling reorder costs a mount. Never animate opacity on an ancestor of a glass / `UIVisualEffectView`.
Evidence: b58a91780d: into-month 45.8 ms -> ~0; customer 1.60 -> 0.85 ms/s.

### 25. End springs when they are visually home
Per call, `{...spring, energyThreshold: 2e-5}` on chrome springs; never change the global spring tokens. Start a transition spring from a layout effect after its layers attach.
Evidence: a 6e-9 energy tail ended a move at ~0.79 s vs ~0.42 s visible, keeping both levels mounted; layout-animation commits after a zoom ~650 -> ~200 ms.

### 26. Dev client only: bound React's performance entries
React 19.2's dev renderer logs a `performance.measure` per re-rendered component into an uncapped native buffer (RN 0.86): in `__DEV__` call `performance.clearMeasures()` every 10 s. Source-confirmed, never measured on device.

## Landing

### 27. The fix playbook
Every fix lands with:
- the `perflab compare` table: before and after, same scenario, device and native key;
- a source-contract test in the nearest perf contract suite that reads the source text: the exact `collapsable` / `renderToHardwareTextureAndroid` pair, the block count, loops outside layered blocks, no `removeClippedSubviews`, no `usePathValue`;
- a scenario row for the worst offenders.

Fix a pattern bug in the one place that owns it and sweep its siblings.
Evidence: PR #1770 `scroll-performance-contracts.test.ts`, `chassis.performance.test.ts`.

