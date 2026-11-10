CREATE TABLE config (
  key TEXT PRIMARY KEY,
  value BLOB NOT NULL
);

CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  username TEXT NOT NULL UNIQUE,
  handle BLOB NOT NULL UNIQUE,
  perm_admin BOOLEAN NOT NULL DEFAULT FALSE,
  perm_invite BOOLEAN NOT NULL DEFAULT FALSE,
  perm_group BOOLEAN NOT NULL DEFAULT FALSE,
  invited_by INTEGER REFERENCES users (id),
  created_at INTEGER NOT NULL
);

CREATE TABLE credentials (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  credential_id BLOB NOT NULL UNIQUE,
  credential BLOB NOT NULL,
  label TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL
);

CREATE INDEX credentials_user_id ON credentials (user_id);

CREATE TABLE invites (
  id INTEGER PRIMARY KEY,
  token TEXT NOT NULL UNIQUE,
  username TEXT NOT NULL,
  handle BLOB NOT NULL,
  user_id INTEGER REFERENCES users (id) ON DELETE CASCADE,
  perm_admin BOOLEAN NOT NULL DEFAULT FALSE,
  created_by INTEGER REFERENCES users (id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX invites_new_username ON invites (username) WHERE user_id IS NULL;

CREATE TABLE chats (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  is_dm BOOLEAN NOT NULL DEFAULT FALSE,
  dm_key TEXT UNIQUE,
  created_by INTEGER NOT NULL REFERENCES users (id),
  created_at INTEGER NOT NULL,
  last_message_at INTEGER NOT NULL
);

CREATE TABLE members (
  chat_id INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  added_by INTEGER NOT NULL REFERENCES users (id),
  archived BOOLEAN NOT NULL DEFAULT FALSE,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (chat_id, user_id)
);

CREATE INDEX members_user_id ON members (user_id);

CREATE TABLE files (
  id INTEGER PRIMARY KEY,
  chat_id INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users (id),
  kind TEXT NOT NULL DEFAULT 'other',
  status TEXT NOT NULL,
  name TEXT NOT NULL,
  s3_prefix TEXT NOT NULL UNIQUE,
  upload_id TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL,
  conv_type TEXT NOT NULL DEFAULT '',
  conv_size INTEGER NOT NULL DEFAULT 0,
  poster_type TEXT NOT NULL DEFAULT '',
  poster_size INTEGER NOT NULL DEFAULT 0,
  width INTEGER NOT NULL DEFAULT 0,
  height INTEGER NOT NULL DEFAULT 0,
  duration INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE INDEX files_status ON files (status, id);

CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  chat_id INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users (id),
  body TEXT NOT NULL DEFAULT '',
  file_id INTEGER REFERENCES files (id),
  created_at INTEGER NOT NULL,
  edited_at INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX messages_chat_id ON messages (chat_id, id);

CREATE TABLE previews (
  message_id INTEGER PRIMARY KEY REFERENCES messages (id) ON DELETE CASCADE,
  url TEXT NOT NULL,
  status TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  site_name TEXT NOT NULL DEFAULT '',
  file_id INTEGER REFERENCES files (id),
  created_at INTEGER NOT NULL
);

CREATE INDEX previews_status ON previews (status, message_id);

CREATE TABLE reactions (
  message_id INTEGER NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users (id),
  emoji TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (message_id, user_id, emoji)
);

CREATE TABLE snoozes (
  message_id INTEGER NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  due_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (message_id, user_id)
);

CREATE INDEX snoozes_due_at ON snoozes (due_at);

CREATE TABLE push_subscriptions (
  id INTEGER PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  endpoint TEXT NOT NULL UNIQUE,
  subscription BLOB NOT NULL,
  label TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE INDEX push_subscriptions_user_id ON push_subscriptions (user_id);

CREATE TABLE user_stats (
  user_id INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
  messages_sent INTEGER NOT NULL DEFAULT 0,
  images_uploaded INTEGER NOT NULL DEFAULT 0,
  videos_uploaded INTEGER NOT NULL DEFAULT 0,
  files_uploaded INTEGER NOT NULL DEFAULT 0,
  bytes_uploaded INTEGER NOT NULL DEFAULT 0,
  bytes_downloaded INTEGER NOT NULL DEFAULT 0,
  pushes_sent INTEGER NOT NULL DEFAULT 0,
  pushes_opened INTEGER NOT NULL DEFAULT 0,
  invites_sent INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE chat_stats (
  chat_id INTEGER PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
  images_uploaded INTEGER NOT NULL DEFAULT 0,
  videos_uploaded INTEGER NOT NULL DEFAULT 0,
  files_uploaded INTEGER NOT NULL DEFAULT 0,
  bytes_uploaded INTEGER NOT NULL DEFAULT 0,
  bytes_downloaded INTEGER NOT NULL DEFAULT 0
);
