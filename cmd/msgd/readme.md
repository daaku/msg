# msgd

A desktop companion for msg. It keeps the server's event stream open and
shows each new message from someone else in a window of its own, in the top
right corner of the screen, that stays until it is opened or dismissed.
There is no push service in the path, and nothing goes to the system's
notification daemon, so nothing is lost in its noise.

- One card per chat, newest on top: the group's name and icon, or the sender
  and their picture for a direct message, and the newest few messages, with
  attachments and lone links shown as "sent a photo", "sent a link"
- New messages in a chat add to its card
- Clicking a card opens the chat in the browser at the first new message
- "Dismiss all" or Escape (once the window has been clicked) clears them all
- Writing in a chat from anywhere clears its card
- The window never takes the keyboard on its own

It is a separate Go module, built on [MyGo](https://github.com/egoist/mygo),
and not part of msg's build, tests or CI. Linux only for now: X11, or a
Wayland compositor with the layer shell (Sway, Hyprland, KDE Plasma...) and
`gtk-layer-shell` installed; elsewhere it is an ordinary always on top
window.

## How it works

It opens msg's event stream with `?messages=1`, which sends each new message
in your chats as JSON, with the message's id as the event id. msgd saves the
last id it got, and a reconnect sends it back as `Last-Event-ID`, so the
server first sends what came meanwhile: after a restart, a network blip or a
laptop asleep for the night. The first run starts from now.

It signs in with a token from msg's settings page, which is a session
cookie of its own. The server renews it every day and msgd keeps the new
one. The token is bound to the passkey of the browser that made it:
deleting that passkey signs msgd out.

## Use

```sh
makepkg -si                                  # in this directory, on Arch
msgd login https://msg.example.com           # paste the token from Settings, Desktop app
systemctl --user enable --now msgd
```

The state, with the token, is in `~/.config/msgd/state.json`, readable only
by you. Logs go to the journal: `journalctl --user -u msgd`.

## Development

```sh
go test ./...
go run .
```

The JSON msgd reads is pinned by the same line in its tests and in msg's
`TestEventsMessages`: change both or neither.
