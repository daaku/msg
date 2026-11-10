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
