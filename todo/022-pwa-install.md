# 022 Installable on the iOS home screen

Make the app installable as a standalone web app on iOS (and Android and
desktop as a side effect): manifest, icons, meta tags and safe area handling.
No new JavaScript and no offline support.

## Steps

1. **Icons.** Create a simple, legible icon: a rounded speech bubble glyph in
   white on the accent colour, edge to edge background with no transparency
   and no rounded corners baked in (iOS rounds them). Draw it once as
   `assets/icon.svg` by hand, then generate PNGs with ImageMagick:

   ```
   magick -background none -density 384 svg:assets/icon.svg -resize 512x512 png:assets/icon-512.png
   magick png:assets/icon-512.png -resize 192x192 png:assets/icon-192.png
   magick png:assets/icon-512.png -resize 180x180 png:assets/apple-touch-icon.png
   magick png:assets/icon-512.png -resize 32x32 png:assets/favicon.png
   ```

   Keep the important part of the glyph inside the central 80 percent so the
   512 icon also works as a maskable icon. Commit the SVG and the PNGs. Look
   at the PNGs before committing.

2. **Manifest.** `GET /app.webmanifest` (public), content type
   `application/manifest+json`, built from config:

   ```json
   {
     "name": "<APP_NAME>", "short_name": "<APP_NAME>",
     "id": "/", "start_url": "/", "scope": "/",
     "display": "standalone",
     "background_color": "<page background>", "theme_color": "<accent or bar colour>",
     "icons": [
       {"src": "<hashed /assets/icon-192.png>", "sizes": "192x192", "type": "image/png"},
       {"src": "<hashed /assets/icon-512.png>", "sizes": "512x512", "type": "image/png"},
       {"src": "<hashed /assets/icon-512.png>", "sizes": "512x512", "type": "image/png", "purpose": "maskable"}
     ]
   }
   ```

3. **Head tags** in the page shell (`ui.go`):

   ```html
   <link rel="manifest" href="/app.webmanifest">
   <link rel="icon" type="image/png" href="<hashed favicon>">
   <link rel="apple-touch-icon" href="<hashed apple-touch-icon>">
   <meta name="mobile-web-app-capable" content="yes">
   <meta name="apple-mobile-web-app-capable" content="yes">
   <meta name="apple-mobile-web-app-title" content="<APP_NAME>">
   <meta name="apple-mobile-web-app-status-bar-style" content="default">
   <meta name="theme-color" content="..." media="(prefers-color-scheme: light)">
   <meta name="theme-color" content="..." media="(prefers-color-scheme: dark)">
   ```

4. **Safe areas.** The viewport already has `viewport-fit=cover`. Pad with
   `env(safe-area-inset-*)`: the top of every page header, the bottom of the
   composer and of scrolling pages, the left and right in landscape, and the
   upload ring and reaction sheet positions.

5. **Standalone behaviour.**
   - Confirm every page other than `/` has a visible back link (there is no
     browser chrome). Add any that are missing.
   - Links inside messages and previews already open with `target="_blank"`,
     which on iOS opens the in app browser sheet and keeps the app in place.
     Internal navigation must never use `target="_blank"`.
   - Use `100dvh` for the app height so the composer is not hidden behind
     the keyboard or the home indicator. Set `interactive-widget=
     resizes-content` in the viewport meta so Android resizes the layout when
     the keyboard opens.
   - Inputs must have a font size of at least 16px, otherwise iOS zooms the
     page on focus.

6. **Install hint.** On `/settings`, in the notifications section, a short
   paragraph: "On iPhone and iPad: tap Share, then Add to Home Screen, and
   open the app from there to enable notifications." Hide it with
   `@media (display-mode: standalone)`.

## Tests

- `/app.webmanifest` is valid JSON with `display: standalone`, `start_url`,
  and three icon entries whose `src` paths return 200 with `image/png`.
- The manifest is reachable signed out (the browser fetches it without
  cookies in some cases).
- The page shell contains the manifest link, the apple touch icon link and
  the two capability meta tags.
- The icon PNG files have the stated pixel sizes (decode with `image/png`).

## Done when

- Chromium's Application panel shows the manifest with no errors or
  warnings and offers to install.
- On a real iPhone over HTTPS: Add to Home Screen gives the right icon and
  name, the app opens without Safari's bars, content is not under the notch
  or the home indicator, the keyboard does not cover the composer, signing in
  with a passkey works inside the installed app, and notifications can be
  enabled there. If no iPhone or HTTPS origin is available, say so; this is
  the main thing the user will want to check themselves.
- `./.hooks/pre-push` passes.
