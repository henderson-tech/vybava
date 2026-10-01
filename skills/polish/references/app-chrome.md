# App chrome matrix - the `full` pass for the `app` target

Every screen the feature touches × every `app` lane the repo declares × light
and dark × the repo's default locale, judged against the rows below. The
lanes come from `polish-kit lanes`; the shots from `polish-kit shoot <lane>`
(simulators and Android devices) or a hand shot / the repo's recorder for a
physical iPhone; the side-by-side and edge crops from `polish-kit sheet`.

Two tiers exist on every platform and every row is a disagreement between
them:

| Tier | Where | What the OS draws for you |
|---|---|---|
| native glass | iOS 26 and newer | Liquid Glass discs, header and tab-bar material, scroll-edge fade |
| fallback | iOS 18-25, iOS 26 with Reduce Transparency, Android | nothing: a bare glyph, a see-through bar, a flat surface |

The repo's design skill names the owner primitive for each row (header icon
control, header pill, glass surface, floating body, sheet manifest, state
view). A finding is filed against the owner, never the screen.

## 1. Glass and chrome that only exists on the native tier

| Row | Check on the fallback lanes | Correct |
|---|---|---|
| Header icon controls (back, close, menu, single action) | zoom-crop every header slot | a solid disc with a hairline; never a lone glyph. Native tier stays bare, the OS draws the disc |
| Glass surfaces (chips, badges, glass buttons, floating discs) | over a photo and over a plain surface, light and dark | a wash plus a hairline is visible on both; the Android solid branch keeps its hairline after a remount |
| Native tab bar | scroll a long list to the end | the bar keeps a background; never see-through over content |
| Header pills (label, glyph and label, avatar and name) | every screen with a pill | one geometry app-wide (one height, one label size); solid plus hairline on the fallback tier |
| Anything taller than the header bar (identity card, avatar over name) | the chat and profile headers | an inline pill fallback below the native tier; native and Android may keep the tall card only if the bar grows with it |
| Translucent header wanted (chat, media) | the native lane | dimmed and soft, never solid, controls float as glass; the fallback lane gets a solid bar, never a see-through one |
| Status bar over media | photo headers, galleries, the swipe deck | readable in both themes; the media header owns the status bar style |

Cheap proxy: Reduce Transparency on a native-tier simulator takes the fallback
code path for glass surfaces. It is a proxy for §1 only and never counts as
the fallback lane's evidence.

## 2. Spacing and clearance

| Row | Check | Correct |
|---|---|---|
| Top clearance under the header | the first content row on every screen, iOS vs Android | iOS headers overlay content, Android headers are solid: any `insets.top + header` clearance is gated on the platform's overlay flag, never added unconditionally |
| Bottom clearance over the tab bar and home indicator | the last row of every list, the deck card, floating orbs, composers | the in-tab inset already includes the bar plus the indicator, so nothing adds it again; Android is checked in gesture AND 3-button navigation; the last row never renders under the Material bar |
| Keyboard | every screen with an input, keyboard open | primary action visible, last rows reachable, a gap between the keyboard and the bar |
| Largest text size | every header with a pill or a title budget | pills do not fold into an overflow menu that does nothing; titles truncate, controls stay |
| Dead gaps and magic numbers | any gap that is not a token or an inset | a plain title without a subtitle drops its subtitle slot; a sheet's height maths uses the live screen height, never a hard-coded one |
| Empty states | search-like screens with a filter row | high under the filter row, never centred (the keyboard covers centred content) |

## 3. Backgrounds and surfaces

| Row | Check | Correct |
|---|---|---|
| One fill per surface family | hub cards, detail cards, sheets, both themes | every card in a family paints the same fill from the owner function; no card a shade off in dark mode; no grey under a card the design shows white |
| Floating headers | showcase and profile headers | float over content; never a solid white bar the design shows floating |
| Sheet style | every sheet the feature opens | a manifest row, so the rounded floating style, the handle and the exit are the family's; no hand-written presentation |
| Borders and shadows | pills, chips, cards | hairline, not a vibrant border; no drop shadow the design language has retired |
| Corners on square displays | home-button iPhones, an Android device that reports no corner radius | one square-display rule feeds every floating body; corners agree across the feature's bodies on the same display |
| Rims and clips | the top rim of a sheet in dark mode, the bottom edge of a compact sheet during a close | the rim is one unbroken line in every pose; nothing shows through under a floating body's edge |

Zoom-crop the edges and corners of any transformed element near a mask, clip
or overflow ancestor before calling it verified; `polish-kit sheet` writes the
edge sheet for exactly this.

## 4. Stacked navigation

| Row | Check | Correct |
|---|---|---|
| Gate sheets (sign-in, add card, consent) | open the gate from every entry point, complete it, swipe back once | the sheet closes onto the screen under it; one swipe or system back lands on the origin; no second copy of the screen on the stack |
| Accept, confirm and open-detail actions | trigger them with the destination already open under the modal | the modal closes onto the destination; never a second copy |
| Persona or tree switches | switch while a sheet, modal or picker is open; Android back afterwards | the crossing closes the sheet first; back never reveals the other tree |
| Cold start and deep links | kill the app, open a deep link; cold start with a persona remembered | the right tree, the right persona, the right screen; a stack's first screen shows a back or close |
| Stacked sheets (report a problem over a chat modal) | system back, swipe | one layer at a time; never further than the layer under |

Where the repo exposes a route-state probe (a dev endpoint or a test hook),
assert the stack after each row instead of trusting the picture.

## 5. Behaviour under interaction

Recordings, not stills: pan a map, flick a sheet between poses, scroll inside
an expanded sheet, tap every control with feedback.

| Row | Correct |
|---|---|
| Map pan and zoom | loaded items stay until replaced; no pop-in and pop-out of pins, cards or the carousel |
| Scroll inside a sheet | the expanded content scrolls; the sheet and the inner list do not fight for the gesture |
| Drag between poses | a slow drag scrubs 1:1, a flick crossfades; no snap, no content leaking under the body |
| Tap feedback | ripple or highlight fits the control's shape; no wonky ripple on a rounded element |
| Steppers and counters | the step matches the screen on every transition |
| Endless animations | rest when nothing watches them; nothing spins on a background screen |

## 6. Platform hygiene the device passes keep catching

| Row | Correct |
|---|---|
| Platform copy | no Face ID or Touch ID wording on Android; the Android biometric name on Android |
| Icons | every symbol the app uses has a mapping on the platform; a grey square is a finding |
| Locale | no source-language string on a default-locale screen; the same datum is formatted one way on one screen |
| Titles | a screen title is never repeated as its first section header |
| Typography bans | the repo's banned characters (long dashes) do not appear in shipped copy |

## 7. How a chrome finding closes

A chrome finding is a pattern, not a spot. It closes when all four hold:

1. **Every sibling is fixed** in the same PR: grep for the shape (the bare
   header button, the unconditional clearance, the hand-written sheet
   presentation) and fix every hit, not the one on the board.
2. **A guard or a manifest row keeps it closed**: a CI check that refuses the
   shape, or the owner primitive and a registry row that makes the shape
   unreachable.
3. **The lane that found it is re-shot** after a full dev-client reload, in
   the theme and navigation mode that showed it.
4. **The evidence names the device**: simulator, physical phone and OS
   version. Simulator evidence is not handset proof for a lane declared as a
   device.
