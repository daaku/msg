# 023 Stats pages

The counters have been collected since task 002. This task shows them, and
audits that every counter is wired. Code goes in `stats.go`.

## Who sees what

| Stats | Visible to | Where |
| --- | --- | --- |
| A user's | that user | a "Stats" section (`id="stats"`) on `/settings` |
| A user's | admins | `/admin` table columns and `/admin/u/{id}` |
| A group's | its members and admins | a "Stats" section on `/c/{id}/info` |
| A direct message's | its two participants only | the same section on its info page |
| All groups | admins | `GET /admin/groups` |

Nobody else can see them. A member cannot see another user's stats.

## Display

User stats, in this order, with these labels:

1. Messages sent
2. Images uploaded
3. Videos uploaded
4. Other files uploaded
5. Uploaded (bytes, through `fmtBytes`)
6. Downloaded (bytes)
7. Notifications sent (pushes sent to this user's devices)
8. Notifications opened
9. Invites sent

Group stats: images, videos, other files, uploaded bytes, downloaded bytes.

Render them as a definition list (`<dl class="stats">`) with one reusable
function for each of the two shapes. Under the downloaded figure add a small
note: "Estimated from media links opened".

`/admin`: add columns for messages sent, uploaded and downloaded to the user
table, and a link to `/admin/groups`. Load with one query that left joins
`user_stats`.

`GET /admin/groups` (`accAdmin`): every group (never direct messages) with member count, created
date, creator and its five stats, each linking to `/c/{id}/info`. One query.
Back link to `/admin`.

## Audit

Write one test, `TestStatsWiring`, that drives the real handlers and asserts
each counter moves by exactly the expected amount and no other counter moves:

| Action | User counters | Group counters |
| --- | --- | --- |
| Send a text message | `messages_sent` +1 | none |
| Upload a PNG of S bytes | `messages_sent` +1, `images_uploaded` +1, `bytes_uploaded` +S | `images_uploaded` +1, `bytes_uploaded` +S |
| Upload an MP4 | same with `videos_uploaded` | same with `videos_uploaded` |
| Upload random bytes | same with `files_uploaded` | same with `files_uploaded` |
| `GET /f/{id}/o` | `bytes_downloaded` + size | `bytes_downloaded` + size |
| `GET /f/{id}/c` after conversion | `bytes_downloaded` + `conv_size` | same |
| Push delivered to a member | that member's `pushes_sent` +1 | none |
| `POST /push/opened` | `pushes_opened` +1 | none |
| Create an invite | `invites_sent` +1 | none |
| Failed upload, rejected invite, 404 on `/f/` | nothing | nothing |
| Reaction, add passkey link, recovery link | nothing | nothing |

Fix anything the test finds, in the file that owns the behaviour.

## Tests

- `TestStatsWiring` as above.
- Visibility: `/settings` shows the viewer's own numbers; `/admin/u/{id}` is
  admin only; group info shows stats to a member and to an admin, and is 404
  for others; `/admin/groups` is admin only.
- Direct messages: stats show to both participants, the info page is 404
  for an admin, and they are not listed on `/admin/groups`.
- A user or group with no stats row renders zeros.
- `fmtBytes` table: 0, 999, 1000, 1536, 1048576, 5 GiB.

## Done when

- Tests above pass and `./.hooks/pre-push` passes.
