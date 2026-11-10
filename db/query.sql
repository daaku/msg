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

-- name: CredentialCountByUser :one
SELECT count(*) FROM credentials WHERE user_id = ?;

-- name: CredentialDelete :execrows
DELETE FROM credentials WHERE id = @id AND user_id = @user_id;

-- name: InvitesPendingForUser :many
SELECT * FROM invites WHERE user_id = @user_id AND expires_at > @now
ORDER BY id DESC;

-- name: InviteRevokeForUser :execrows
DELETE FROM invites WHERE id = @id AND user_id = @user_id;

-- name: InvitesPendingByCreator :many
SELECT * FROM invites
WHERE user_id IS NULL AND created_by = @created_by AND expires_at > @now
ORDER BY id DESC;

-- name: InvitesPendingAll :many
SELECT sqlc.embed(invites), users.username AS creator
FROM invites JOIN users ON users.id = invites.created_by
WHERE invites.user_id IS NULL AND invites.expires_at > @now
ORDER BY invites.id DESC;

-- name: InviteDeleteExpiredByUsername :exec
DELETE FROM invites
WHERE username = @username AND user_id IS NULL AND expires_at <= @now;

-- name: InvitePendingByUsername :one
SELECT * FROM invites
WHERE username = @username AND user_id IS NULL AND expires_at > @now;

-- name: InviteRevoke :execrows
DELETE FROM invites
WHERE id = @id AND user_id IS NULL AND created_by IS NOT NULL
  AND (created_by = @created_by OR CAST(@any_creator AS BOOLEAN));

-- name: UsersAll :many
SELECT sqlc.embed(users), CAST(coalesce(inviter.username, '') AS TEXT) AS inviter,
  (SELECT count(*) FROM credentials WHERE credentials.user_id = users.id) AS passkeys,
  CAST(coalesce(user_stats.messages_sent, 0) AS INTEGER) AS messages_sent,
  CAST(coalesce(user_stats.bytes_uploaded, 0) AS INTEGER) AS bytes_uploaded,
  CAST(coalesce(user_stats.bytes_downloaded, 0) AS INTEGER) AS bytes_downloaded
FROM users
LEFT JOIN users AS inviter ON inviter.id = users.invited_by
LEFT JOIN user_stats ON user_stats.user_id = users.id
ORDER BY users.username;

-- name: UserSetPerms :exec
UPDATE users SET perm_admin = @perm_admin, perm_invite = @perm_invite, perm_group = @perm_group
WHERE id = @id;

-- name: CredentialDeleteByAdmin :execrows
DELETE FROM credentials WHERE id = @id AND user_id = @user_id AND user_id != @admin_id;

-- name: ChatCreate :one
INSERT INTO chats (name, created_by, created_at, last_message_at)
VALUES (@name, @created_by, @created_at, @last_message_at)
RETURNING *;

-- name: ChatByID :one
SELECT * FROM chats WHERE id = ?;

-- name: ChatsForUserWithLast :many
SELECT sqlc.embed(chats),
  CAST(coalesce(other_user.username, '') AS TEXT) AS other,
  CAST(coalesce(last.body, '') AS TEXT) AS last_body,
  CAST(coalesce(last_user.username, '') AS TEXT) AS last_username,
  CAST(coalesce(last.created_at, 0) AS INTEGER) AS last_created_at,
  CAST(coalesce(last_file.kind, '') AS TEXT) AS last_file_kind,
  CAST(coalesce(last_file.name, '') AS TEXT) AS last_file_name
FROM chats
JOIN members ON members.chat_id = chats.id AND members.user_id = @user_id
  AND members.archived = @archived
LEFT JOIN members other ON chats.is_dm
  AND other.chat_id = chats.id AND other.user_id != @user_id
LEFT JOIN users other_user ON other_user.id = other.user_id
LEFT JOIN messages last ON last.id = (
  SELECT max(m.id) FROM messages m WHERE m.chat_id = chats.id)
LEFT JOIN users last_user ON last_user.id = last.user_id
LEFT JOIN files last_file ON last_file.id = last.file_id
ORDER BY chats.last_message_at DESC, chats.id DESC;

-- name: ChatByDMKey :one
SELECT * FROM chats WHERE dm_key = ?;

-- name: ChatCreateDM :one
INSERT INTO chats (name, is_dm, dm_key, created_by, created_at, last_message_at)
VALUES ('', TRUE, @dm_key, @created_by, @created_at, @last_message_at)
RETURNING *;

-- name: ChatRename :exec
UPDATE chats SET name = @name WHERE id = @id;

-- name: MemberAdd :exec
INSERT INTO members (chat_id, user_id, added_by, created_at)
SELECT @chat_id, users.id, @added_by, @created_at FROM users WHERE users.id = @user_id
ON CONFLICT (chat_id, user_id) DO NOTHING;

-- name: MemberRemove :exec
DELETE FROM members WHERE chat_id = @chat_id AND user_id = @user_id;

-- name: MemberGet :one
SELECT * FROM members WHERE chat_id = @chat_id AND user_id = @user_id;

-- name: MembersOfChat :many
SELECT members.user_id, users.username FROM members
JOIN users ON users.id = members.user_id
WHERE members.chat_id = ?
ORDER BY users.username;

-- name: MemberIDs :many
SELECT user_id FROM members WHERE chat_id = ? ORDER BY user_id;

-- name: UsersNotInChat :many
SELECT id, username FROM users
WHERE id NOT IN (SELECT user_id FROM members WHERE chat_id = ?)
ORDER BY username;

-- name: UsersAllBasic :many
SELECT id, username FROM users ORDER BY username;

-- name: MessageCreate :one
INSERT INTO messages (chat_id, user_id, body, file_id, created_at)
VALUES (@chat_id, @user_id, @body, @file_id, @created_at)
RETURNING *;

-- name: MessagesLatest :many
SELECT sqlc.embed(messages), users.username
FROM messages JOIN users ON users.id = messages.user_id
WHERE messages.chat_id = @chat_id
ORDER BY messages.id DESC
LIMIT @n;

-- name: MessageCount :one
SELECT count(*) FROM messages WHERE chat_id = ?;

-- name: ChatTouch :exec
UPDATE chats SET last_message_at = @last_message_at WHERE id = @id;

-- name: MessageByID :one
SELECT * FROM messages WHERE id = ?;

-- name: MessageUpdateBody :exec
UPDATE messages SET body = @body, edited_at = @edited_at WHERE id = @id;

-- name: MessageDelete :exec
DELETE FROM messages WHERE id = ?;

-- name: MessageInChat :one
SELECT id FROM messages WHERE id = @id AND chat_id = @chat_id;

-- name: ReactionDelete :execrows
DELETE FROM reactions
WHERE message_id = @message_id AND user_id = @user_id AND emoji = @emoji;

-- name: ReactionAdd :exec
INSERT INTO reactions (message_id, user_id, emoji, created_at)
VALUES (@message_id, @user_id, @emoji, @created_at);

-- name: ReactionsForMessages :many
SELECT reactions.message_id, reactions.emoji, reactions.user_id, users.username
FROM reactions JOIN users ON users.id = reactions.user_id
WHERE reactions.message_id IN (sqlc.slice('ids'))
ORDER BY reactions.created_at, reactions.user_id;

-- name: MemberSetArchived :exec
UPDATE members SET archived = @archived
WHERE chat_id = @chat_id AND user_id = @user_id;

-- name: MembersUnarchive :exec
UPDATE members SET archived = FALSE
WHERE chat_id = @chat_id AND user_id != @author_id AND archived;

-- name: ArchivedCount :one
SELECT count(*) FROM members WHERE user_id = ? AND archived;

-- name: FileCreate :one
INSERT INTO files (chat_id, user_id, kind, status, name, s3_prefix, upload_id, size, created_at)
VALUES (@chat_id, @user_id, 'other', 'uploading', @name, @s3_prefix, @upload_id, @size, @created_at)
RETURNING *;

-- name: FileByID :one
SELECT * FROM files WHERE id = ?;

-- name: FileCompleteUpload :execrows
UPDATE files SET kind = @kind, status = @status, upload_id = ''
WHERE id = @id AND status = 'uploading';

-- name: FileDelete :exec
DELETE FROM files WHERE id = ?;

-- name: FileDeleteUploading :execrows
DELETE FROM files WHERE id = ? AND status = 'uploading';

-- name: FilesStaleUploads :many
SELECT * FROM files WHERE status = 'uploading' AND created_at < @before ORDER BY id;

-- name: FilesByIDs :many
SELECT * FROM files WHERE id IN (sqlc.slice('ids'));

-- name: FilesResetProcessing :exec
UPDATE files SET status = 'pending' WHERE status = 'processing';

-- name: FileClaim :one
UPDATE files SET status = 'processing', attempts = attempts + 1
WHERE id = (
  SELECT f.id FROM files f
  WHERE f.status = 'pending' AND f.kind IN ('image', 'video')
  ORDER BY f.id LIMIT 1
)
RETURNING *;

-- name: FileSetReady :execrows
UPDATE files SET status = 'ready', conv_type = @conv_type, conv_size = @conv_size,
  poster_type = @poster_type, poster_size = @poster_size,
  width = @width, height = @height, duration = @duration
WHERE id = @id AND status = 'processing';

-- name: FileSetStatus :execrows
UPDATE files SET status = @status WHERE id = @id AND status = 'processing';

-- name: PushSubUpsert :exec
INSERT INTO push_subscriptions (user_id, endpoint, subscription, label, created_at)
VALUES (@user_id, @endpoint, @subscription, @label, @created_at)
ON CONFLICT (endpoint) DO UPDATE SET
  user_id = excluded.user_id,
  subscription = excluded.subscription,
  label = excluded.label;

-- name: PushSubsByUser :many
SELECT * FROM push_subscriptions WHERE user_id = ? ORDER BY id;

-- name: PushSubsByUsers :many
SELECT * FROM push_subscriptions WHERE user_id IN (sqlc.slice('ids')) ORDER BY id;

-- name: PushSubDelete :execrows
DELETE FROM push_subscriptions WHERE id = @id AND user_id = @user_id;

-- name: PushSubDeleteByID :exec
DELETE FROM push_subscriptions WHERE id = ?;

-- name: SnoozeUpsert :exec
INSERT INTO snoozes (message_id, user_id, due_at, created_at)
VALUES (@message_id, @user_id, @due_at, @created_at)
ON CONFLICT (message_id, user_id) DO UPDATE SET
  due_at = excluded.due_at,
  created_at = excluded.created_at;

-- name: SnoozeDelete :exec
DELETE FROM snoozes WHERE message_id = @message_id AND user_id = @user_id;

-- name: SnoozeTake :execrows
DELETE FROM snoozes
WHERE message_id = @message_id AND user_id = @user_id AND due_at = @due_at;

-- name: SnoozesDue :many
SELECT * FROM snoozes WHERE due_at <= @now
ORDER BY due_at, message_id, user_id
LIMIT 100;

-- name: SnoozesForUserMessages :many
SELECT message_id, due_at FROM snoozes
WHERE user_id = @user_id AND message_id IN (sqlc.slice('ids'));

-- name: MessagesCountFrom :one
SELECT count(*) FROM messages WHERE chat_id = @chat_id AND id >= @id;

-- name: PushSubCountByUser :one
SELECT count(*) FROM push_subscriptions WHERE user_id = ?;

-- name: GroupsAllWithStats :many
SELECT sqlc.embed(chats), users.username AS creator,
  (SELECT count(*) FROM members WHERE members.chat_id = chats.id) AS member_count,
  CAST(coalesce(chat_stats.images_uploaded, 0) AS INTEGER) AS images_uploaded,
  CAST(coalesce(chat_stats.videos_uploaded, 0) AS INTEGER) AS videos_uploaded,
  CAST(coalesce(chat_stats.files_uploaded, 0) AS INTEGER) AS files_uploaded,
  CAST(coalesce(chat_stats.bytes_uploaded, 0) AS INTEGER) AS bytes_uploaded,
  CAST(coalesce(chat_stats.bytes_downloaded, 0) AS INTEGER) AS bytes_downloaded
FROM chats
JOIN users ON users.id = chats.created_by
LEFT JOIN chat_stats ON chat_stats.chat_id = chats.id
WHERE NOT chats.is_dm
ORDER BY chats.name, chats.id;

-- name: PreviewCreate :exec
INSERT INTO previews (message_id, url, status, created_at)
VALUES (@message_id, @url, 'pending', @created_at);

-- name: PreviewClaim :one
-- A preview is tried once: claiming it marks it failed, and success later
-- marks it ready. A crash in between leaves it failed.
UPDATE previews SET status = 'failed'
WHERE message_id = (
  SELECT p.message_id FROM previews p WHERE p.status = 'pending'
  ORDER BY p.message_id LIMIT 1
)
RETURNING *;

-- name: PreviewSetReady :execrows
UPDATE previews SET status = 'ready', title = @title, description = @description,
  site_name = @site_name, file_id = @file_id
WHERE message_id = @message_id AND url = @url AND status = 'failed';

-- name: PreviewByMessage :one
SELECT * FROM previews WHERE message_id = ?;

-- name: PreviewDelete :exec
DELETE FROM previews WHERE message_id = ?;

-- name: PreviewsForMessages :many
SELECT sqlc.embed(previews),
  CAST(coalesce(files.width, 0) AS INTEGER) AS width,
  CAST(coalesce(files.height, 0) AS INTEGER) AS height
FROM previews LEFT JOIN files ON files.id = previews.file_id
WHERE previews.status = 'ready'
  AND previews.message_id IN (sqlc.slice('message_ids'));

-- name: FileCreatePreview :one
INSERT INTO files (chat_id, user_id, kind, status, name, s3_prefix, size,
  conv_type, conv_size, width, height, created_at)
VALUES (@chat_id, @user_id, 'preview', 'ready', 'preview', @s3_prefix, 0,
  @conv_type, @conv_size, @width, @height, @created_at)
RETURNING *;
