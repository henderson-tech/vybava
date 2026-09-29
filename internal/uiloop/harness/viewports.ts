// The capture viewports. Every shot is DPR 2. A viewport with insets emulates
// an iPhone's safe area through CDP, so `env(safe-area-inset-*)` resolves
// in-page; `mobile` means a coarse pointer (touch, mobile UA, touch-target
// lint). A repo adds or overrides ids in `uiLoop.viewports`; `vybava ui-loop
// run` resolves the table into run.json, which is what the capture reads.
//
// BUILTIN_VIEWPORTS is mirrored by the Go side (internal/uiloop/viewports.go)
// and a Go test keeps the two tables equal: change both or neither.

export interface Insets {
  top: number;
  right: number;
  bottom: number;
  left: number;
}

export interface Viewport {
  width: number;
  height: number;
  /** Coarse pointer: isMobile + hasTouch + a phone/tablet user agent, and the touch-target lint. */
  mobile?: boolean;
  insets?: Insets;
}

export const DPR = 2;

export const BUILTIN_VIEWPORTS = {
  phone: { width: 390, height: 844, mobile: true, insets: { top: 59, right: 0, bottom: 34, left: 0 } },
  'phone-320': { width: 320, height: 568, mobile: true, insets: { top: 20, right: 0, bottom: 0, left: 0 } },
  'phone-360': { width: 360, height: 780, mobile: true },
  'phone-landscape': { width: 844, height: 390, mobile: true, insets: { top: 0, right: 47, bottom: 21, left: 47 } },
  tablet: { width: 820, height: 1180, mobile: true },
  laptop: { width: 1024, height: 768 },
  desktop: { width: 1440, height: 810 },
} as const satisfies Record<string, Viewport>;

export type BuiltinViewportId = keyof typeof BUILTIN_VIEWPORTS;

export const NO_INSETS: Insets = { top: 0, right: 0, bottom: 0, left: 0 };

export function insetsOf(viewport: Viewport): Insets {
  return viewport.insets ?? NO_INSETS;
}

export function hasInsets(viewport: Viewport): boolean {
  const i = insetsOf(viewport);
  return i.top > 0 || i.right > 0 || i.bottom > 0 || i.left > 0;
}

/** `390x844@2` — the vitrinka --viewport spelling. */
export function viewportLabel(viewport: Viewport): string {
  return `${viewport.width}x${viewport.height}@${DPR}`;
}
