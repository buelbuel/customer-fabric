-- +goose Up
-- The matching reader (#41, slice 4b of #123): identity-match candidates read only the values an access
-- policy grants to matching, like every other use of governed attributes.
ALTER TABLE attribute_access_policies DROP CONSTRAINT attribute_access_policies_readers_check,
  ADD CONSTRAINT attribute_access_policies_readers_check CHECK (readers <@ ARRAY['operator','segmentation','activation','matching']::text[]);

-- +goose Down
-- Fails while any policy revision names the matching reader. Use only in a disposable database.
ALTER TABLE attribute_access_policies DROP CONSTRAINT attribute_access_policies_readers_check,
  ADD CONSTRAINT attribute_access_policies_readers_check CHECK (readers <@ ARRAY['operator','segmentation','activation']::text[]);
