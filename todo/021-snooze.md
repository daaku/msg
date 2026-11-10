# 021 Snoozing a message

Let a user snooze any message they can see: they pick a time, and at that
time they get a push notification reminding them of the message. It is
entirely personal. Other users never see that a message was snoozed and are
not affected. Code goes in `push.go`, rendering in `chat.go`. No JavaScript.

## Rules

- One pending snooze per user per message (`snoozes` primary key). Choosing
  a new time replaces the old one.
- Any member can snooze any message in the chat, including their own.
- When the time comes the reminder is pushed to all of that user's devices
  and the snooze row is deleted. It fires once.
- If the message is deleted first, the snooze disappears with it (cascade).
  If the user is no longer a member when it fires, it is dropped silently.
- A reminder is sent even if the user is looking at the chat.

## Choosing a time

In the message menu from task 010, a "Remind me" section with one form and a
button per choice (`name="when"`):

| Value | Label | Due |
| --- | --- | --- |
| `1h` | In 1 hour | now + 1 hour |
| `3h` | In 3 hours | now + 3 hours |
| `tomorrow` | Tomorrow morning | 09:00 on the next calendar day |
| `week` | Next week | 09:00 seven calendar days from today |

"09:00" and "calendar day" are in the viewer's time zone from the `tz`
cookie (UTC when missing), the same zone used to render times. Compute with
`time.Date` in that location so daylight saving changes are handled.

The form posts to `POST /c/{id}/m/{mid}/snooze` with `data-morph`.

When the viewer has a pending snooze on a message:

- The message shows `<span id="m123-snooze" class="msg-snooze">` with a clock
  icon and the due time ("Tomorrow 09:00" style: time only when it is today,
  otherwise date and time), visible only to that viewer.
- The menu section shows the due time and a "Cancel reminder" button posting
  to `POST /c/{id}/m/{mid}/unsnooze` instead of hiding the choices; the
  choices stay so the time can be changed.

If the viewer has no push subscription at all, show one line in the section:
"Enable notifications in Settings to get reminders." with a link to
`/settings#notifications`. Find this out with one query per page render, not
one per message.

Load the viewer's snoozes for the rendered messages in one query.

## Routes

- `POST /c/{id}/m/{mid}/snooze` (member; the message must be in the chat):
  `when` must be one of the four values, else 400. Upsert the row with the
  computed `due_at`. Ping only the current user. Redirect to `/c/{id}`.
- `POST /c/{id}/m/{mid}/unsnooze` (member): delete the row. Ping only the
  current user. Redirect.

## Firing

A goroutine started with `a.bg.Go`: at startup and then every 30 seconds
(and stopping on `a.ctx.Done()`), take the snoozes with `due_at <= now`, at
most 100 per round, oldest first. For each:

1. Delete the row first, with `DELETE ... WHERE message_id = ? AND user_id =
   ? AND due_at = ? ` and check a row was deleted. This makes a reminder fire
   at most once even if two rounds overlap, and means a snooze that was just
   changed to a later time is left alone.
2. Check the user is still a member of the message's chat; if not, stop.
3. Send to each of the user's subscriptions through the same sending
   function as message pushes (same concurrency limit, same handling of
   permanent errors, same `pushes_sent` counting). Payload:

   ```json
   {"title": "Reminder · <chat title for this user>",
    "body": "<author username>: <snippet>",
    "tag": "snooze-<message id>",
    "data": {"url": "/c/<chat id>?m=<message id>"}}
   ```

   The snippet follows the same rules as message pushes.

Reminders that came due while the server was down fire at the next start.

## Jumping to a message

`GET /c/{id}?m=<mid>`: if the message exists in the chat, redirect to
`/c/{id}?n=<N>#m<mid>` where N is the larger of 100 and the number of
messages in the chat with an id greater than or equal to `mid`, so the
message is inside the rendered window. If it does not exist (deleted),
redirect to `/c/{id}`. Style `article.msg:target` with a brief highlight.

Queries: `SnoozeUpsert`, `SnoozeDelete`, `SnoozeTake` (the guarded delete,
`:execrows`), `SnoozesDue`, `SnoozesForUserMessages` (slice),
`MessagesCountFrom`, `PushSubCountByUser`.

## Tests

- Due times, with a fixed clock and `tz=Asia/Dubai`, for all four choices,
  including a "now" that is just before midnight local time, and a zone with
  a daylight saving change the next day (`America/New_York` on the day
  before the change).
- Snooze creates one row; snoozing again replaces the time; unsnooze deletes
  it; an unknown `when` is 400; a message from another chat is 404; a non
  member is 404.
- Privacy: the snooze marker renders for the user who set it and not for any
  other member of the chat.
- Firing: advancing the clock past `due_at` and running one round sends one
  push per device of that user only, deletes the row, and adds to
  `pushes_sent`; running the round again sends nothing.
- Not yet due: nothing is sent.
- The user was removed from the chat before it fired: nothing is sent and
  the row is gone.
- The message was deleted: the row is gone and nothing is sent.
- A snooze moved to a later time between `SnoozesDue` and the guarded delete
  is not fired (call the two steps separately in the test).
- Payload: title has the chat title as that user sees it (the other person's
  username for a direct message), body has the author and snippet, URL has
  `?m=`.
- `?m=`: redirects with an `n` large enough for a message older than the
  latest 100, with the fragment; a deleted message redirects to the chat.
- With no push subscription the hint renders; with one it does not.

## Done when

- In a browser with notifications enabled: snooze a message for an hour,
  move the clock (or temporarily shorten the choice while testing by hand,
  and put it back), receive the reminder, click it and land on the message.
  Say if this was not checked.
- Tests above pass and `./.hooks/pre-push` passes.
