# 002 Database, migrations and sqlc

Add SQLite, the complete schema, sqlc code generation, the transaction helper,
stored secrets and the stats helpers. The whole schema is created here in one
migration so that later tasks only add queries.

## Steps

1. Dependencies: `modernc.org/sqlite`, `github.com/daaku/sqlmig`, and the tool
   `go get -tool github.com/sqlc-dev/sqlc/cmd/sqlc` (bento pins v1.30.0, which
   is known to accept the schema below).
2. Create `db/001_init.sql` with exactly the schema at the bottom of this
   file. It has been validated against sqlc already. Do not rename anything.
3. Create `sqlc.yaml`:

   ```yaml
   version: '2'
   sql:
     - engine: sqlite
       queries: db/query.sql
       schema: db
       gen:
         go:
           package: main
           out: .
           output_db_file_name: sqlc_db.go
           output_models_file_name: sqlc_models.go
           output_files_suffix: ''
   ```

   Note `schema: db` reads every `.sql` in `db/`, so `query.sql` must not
   contain DDL, and the sqlmig glob must be `db/[0-9]*.sql` so that it does not
   run `query.sql`.
4. Add `//go:generate go tool sqlc generate` to `main.go`.
5. `db/query.sql`, initial queries: `ConfigGet`, `ConfigSetDefault` (insert,
   on conflict do nothing), `UserCount`, `UserStatsAdd`, `ChatStatsAdd`,
   `UserStatsGet`, `ChatStatsGet`. The two `Add` queries are upserts that add
   deltas, in this form:

   ```sql
   -- name: ChatStatsAdd :exec
   INSERT INTO chat_stats (
     chat_id, images_uploaded, videos_uploaded, files_uploaded,
     bytes_uploaded, bytes_downloaded
   ) VALUES (
     @chat_id, @images_uploaded, @videos_uploaded, @files_uploaded,
     @bytes_uploaded, @bytes_downloaded
   ) ON CONFLICT (chat_id) DO UPDATE SET
     images_uploaded = images_uploaded + excluded.images_uploaded,
     videos_uploaded = videos_uploaded + excluded.videos_uploaded,
     files_uploaded = files_uploaded + excluded.files_uploaded,
     bytes_uploaded = bytes_uploaded + excluded.bytes_uploaded,
     bytes_downloaded = bytes_downloaded + excluded.bytes_downloaded;
   ```

   `UserStatsAdd` is the same shape over all nine `user_stats` counters.
   The `Get` queries must return zeros, not `sql.ErrNoRows`, when no row
   exists; handle that in the Go helper.
6. In `main.go`:
   - Open the database at `<STATE_DIRECTORY>/msg.db` with bento's pragmas plus
     `_txlock=immediate`. Run migrations on start.
   - Add `db *sql.DB` and `q *Queries` to `app`.
   - `a.tx(ctx, func(q *Queries) error) error`: begin, run, commit, roll back
     on error or panic.
   - `a.secret(purpose string) []byte`: on first start generate 32 random
     bytes and store them under config key `secret` with `ConfigSetDefault`,
     then read back (so two racing starts agree). Derive per purpose keys with
     `hkdf.Expand(sha256.New, master, purpose, 32)` and cache them. Replace
     the development constant from task 001.
   - Close the database on shutdown after `a.bg.Wait()`.
7. `stats.go`: `a.statUser(ctx, q *Queries, userID int64, d UserStatsAddParams)`
   and `a.statChat(ctx, q, chatID, d ChatStatsAddParams)`, thin wrappers that
   set the id and call the query, plus `a.userStats` and `a.chatStats`
   returning zero values when there is no row. Passing `q` lets callers use
   them inside `a.tx`.
8. `newTestApp` now opens a real database in the temp directory and runs the
   migrations.
9. Tests: migrations apply to an empty database and applying twice is a no
   op; `a.secret("x")` is stable across two `app` instances on the same file
   and differs between purposes; `a.tx` rolls back on error; the stats upserts
   accumulate (add twice, read once); foreign keys are enforced (inserting a
   `members` row for a missing chat fails).

## Done when

- `go generate ./...` produces no diff on a second run.
- `./.hooks/pre-push` passes.

## Schema (`db/001_init.sql`)

```sql
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
  created_by INTEGER NOT NULL REFERENCES users (id),
  created_at INTEGER NOT NULL,
  last_message_at INTEGER NOT NULL
);

CREATE TABLE members (
  chat_id INTEGER NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
  user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  added_by INTEGER NOT NULL REFERENCES users (id),
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
  created_at INTEGER NOT NULL
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
```

Column notes:

- `users.handle`: 16 random bytes, the WebAuthn user handle.
- `credentials.credential_id`: the raw WebAuthn credential id.
  `credentials.credential`: JSON of `webauthn.Credential`.
- `invites.user_id`: `NULL` for a new user invite, set for an "add passkey"
  link. `invites.created_by`: `NULL` only for the bootstrap admin invite.
  `invites.handle`: the handle the new user will get (or already has).
- `files.size`: bytes of the original. `conv_*` and `poster_*` describe the
  converted object and the video poster. `duration` is seconds.
- `messages.file_id`: the attachment, `NULL` for a text message.
