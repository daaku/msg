-- chats.icon names a group's icon ('' for none), derived from the image
-- bytes like users.avatar.
ALTER TABLE chats ADD COLUMN icon TEXT NOT NULL DEFAULT '';

CREATE TABLE chat_icons (
  chat_id INTEGER PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
  image BLOB NOT NULL
);
