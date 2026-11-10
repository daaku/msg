-- name: ConfigGet :one
SELECT value FROM config WHERE key = ?;

-- name: ConfigSetDefault :exec
INSERT INTO config (key, value) VALUES (?, ?)
ON CONFLICT (key) DO NOTHING;

-- name: UserCount :one
SELECT count(*) FROM users;

-- name: UserStatsAdd :exec
INSERT INTO user_stats (
  user_id, messages_sent, images_uploaded, videos_uploaded, files_uploaded,
  bytes_uploaded, bytes_downloaded, pushes_sent, pushes_opened, invites_sent
) VALUES (
  @user_id, @messages_sent, @images_uploaded, @videos_uploaded, @files_uploaded,
  @bytes_uploaded, @bytes_downloaded, @pushes_sent, @pushes_opened, @invites_sent
) ON CONFLICT (user_id) DO UPDATE SET
  messages_sent = messages_sent + excluded.messages_sent,
  images_uploaded = images_uploaded + excluded.images_uploaded,
  videos_uploaded = videos_uploaded + excluded.videos_uploaded,
  files_uploaded = files_uploaded + excluded.files_uploaded,
  bytes_uploaded = bytes_uploaded + excluded.bytes_uploaded,
  bytes_downloaded = bytes_downloaded + excluded.bytes_downloaded,
  pushes_sent = pushes_sent + excluded.pushes_sent,
  pushes_opened = pushes_opened + excluded.pushes_opened,
  invites_sent = invites_sent + excluded.invites_sent;

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

-- name: UserStatsGet :one
SELECT * FROM user_stats WHERE user_id = ?;

-- name: ChatStatsGet :one
SELECT * FROM chat_stats WHERE chat_id = ?;

-- name: UserBySession :one
SELECT users.* FROM users
JOIN credentials ON credentials.user_id = users.id
WHERE users.id = @user_id AND credentials.id = @credential_id;

-- name: UserByHandle :one
SELECT * FROM users WHERE handle = ?;

-- name: UserByID :one
SELECT * FROM users WHERE id = ?;

-- name: UserByUsername :one
SELECT * FROM users WHERE username = ?;

-- name: UserCreate :one
INSERT INTO users (username, handle, perm_admin, invited_by, created_at)
VALUES (@username, @handle, @perm_admin, @invited_by, @created_at)
RETURNING *;

-- name: CredentialsByUser :many
SELECT * FROM credentials WHERE user_id = ? ORDER BY id;

-- name: CredentialByCredentialID :one
SELECT * FROM credentials WHERE credential_id = ?;

-- name: CredentialCreate :one
INSERT INTO credentials (user_id, credential_id, credential, label, created_at, last_used_at)
VALUES (@user_id, @credential_id, @credential, @label, @created_at, @last_used_at)
RETURNING *;

-- name: CredentialUpdate :exec
UPDATE credentials SET credential = @credential, last_used_at = @last_used_at
WHERE id = @id;

-- name: InviteByToken :one
SELECT * FROM invites WHERE token = @token AND expires_at > @now;

-- name: InviteCreate :one
INSERT INTO invites (token, username, handle, user_id, perm_admin, created_by, created_at, expires_at)
VALUES (@token, @username, @handle, @user_id, @perm_admin, @created_by, @created_at, @expires_at)
RETURNING *;

-- name: InviteUse :one
DELETE FROM invites WHERE token = @token AND expires_at > @now
RETURNING *;

-- name: InviteDeleteBootstrap :exec
DELETE FROM invites WHERE created_by IS NULL AND user_id IS NULL;
