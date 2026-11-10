# 020 Design pass for mobile and desktop

Use fable.

Every screen exists and works. This task makes the whole application feel
like one considered product on a phone and on a desktop, without changing
behaviour. The output is almost entirely `assets/main.css`, plus class names
and small markup adjustments in the render functions.

## Constraints

- No new JavaScript. No web fonts, no icon fonts, no CSS framework, no build
  step. System font stack.
- Do not change any element `id`, any form `action`, any input `name`, or
  any `data-*` attribute; tests and the morph depend on them. Classes and
  wrapper elements may change, as long as wrappers that sit among repeated
  siblings get a stable `id`.
- No inline styles (CSP).
- Keep one stylesheet. Organise it with comment headed sections: tokens,
  reset, primitives, layout, sidebar, chat, composer, media, reactions,
  previews, forms and pages, utilities.

## What to deliver

1. **Tokens.** A small palette as custom properties (background, surface,
   raised surface, text, muted text, border, accent, accent text, danger,
   own message bubble, other message bubble, ring track), defined for light
   and for dark through `prefers-color-scheme`. Text contrast at least 4.5:1,
   and 3:1 for large text and control outlines, in both schemes. One radius
   scale, one shadow, the existing type and space scales.
2. **Chat.** Bubbles with clear own and other distinction that does not rely
   on colour alone (alignment too). Tight spacing inside a run of messages
   from one person, more between runs. Quiet timestamps. Day separators that
   read as dividers. Long words, long URLs and long file names never cause
   horizontal scrolling. Emoji only messages of up to three emoji render
   larger (decide this in Go and add a class).
3. **Composer.** The textarea grows with its content up to about six lines
   using `field-sizing: content` with a sensible fixed fallback height where
   that is unsupported. Send and attach are comfortable touch targets. The
   composer stays attached to the bottom above the keyboard.
4. **Sidebar.** Group rows with name, snippet and time aligned on a grid,
   truncated with an ellipsis, a clear current row, and a visible focus
   style.
5. **Media.** Images and video sized by their aspect ratio with rounded
   corners; the processing placeholder has the same footprint style so the
   swap is calm; file rows look like something you can click.
6. **Reactions and picker.** Chips that read at a glance; the picker sheet
   has 44px targets and a backdrop.
7. **Forms and admin pages.** Account, invites, admin, group info and new
   group share one page layout with a max width, consistent section
   headings, and tables that become stacked cards under 48rem.
8. **States.** Hover, active, `:focus-visible`, disabled, and the flash
   messages. Respect `prefers-reduced-motion`. Any transition is under
   200ms.
9. **Empty states** for no groups and an empty group.

## How to work

- Use the `frontend-design` skill if it is available to you.
- Run the app with `S3_ENDPOINT=fake DEV=1 go run .`, create a few users and
  groups, and fill a group with realistic content: short and long messages,
  a run of several from one user, links with previews, an image, a portrait
  image, a video, a file, reactions, a day boundary.
- Look at every page at 360x740, 768x1024 and 1440x900, in light and in
  dark. Take screenshots with Chromium (`/usr/bin/chromium --headless
  --screenshot`, or drive it through a browser tool) and actually look at
  them. Iterate until nothing is misaligned, cramped or clipped.
- Check keyboard use: tab order is sensible, everything interactive is
  reachable and shows focus, and the reaction picker can be opened, used and
  closed from the keyboard.

## Done when

- A screenshot of each of these exists for the final state at phone and
  desktop size, in light and dark, and you have looked at each: sign in,
  invite acceptance, home with groups, a busy group, the reaction picker
  open, group info, new group, account, invites, admin users, admin user,
  admin groups. Save them under the scratch directory you were given, not in
  the repository, and list the paths in your report.
- No horizontal scroll on any page at 320px wide.
- `go test ./...` still passes with no test changes other than class name
  assertions.
- `./.hooks/pre-push` passes.
