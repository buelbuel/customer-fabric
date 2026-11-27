-- +goose Up
ALTER TABLE attribute_definitions ADD COLUMN max_items integer CHECK (max_items BETWEEN 1 AND 100);

-- +goose Down
ALTER TABLE attribute_definitions DROP COLUMN max_items;
