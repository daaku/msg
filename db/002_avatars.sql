-- users.avatar names the current profile picture ('' for none). It is derived
-- from the image bytes, so a picture's URL changes exactly when the picture
-- does.
ALTER TABLE users ADD COLUMN avatar TEXT NOT NULL DEFAULT '';

CREATE TABLE avatars (
  user_id INTEGER PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
  image BLOB NOT NULL
);
