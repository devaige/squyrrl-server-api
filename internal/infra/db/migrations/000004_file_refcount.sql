-- +goose Up
-- 触发器自动维护 files.ref_count：element 插入 +1，删除 -1
-- 软删除碎片时由应用层 DELETE FROM elements WHERE snippet_id 触发减一

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION elements_after_insert() RETURNS TRIGGER AS $$
BEGIN
    UPDATE files SET ref_count = ref_count + 1 WHERE id = NEW.file_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION elements_after_delete() RETURNS TRIGGER AS $$
BEGIN
    UPDATE files SET ref_count = ref_count - 1 WHERE id = OLD.file_id;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_elements_after_insert
    AFTER INSERT ON elements
    FOR EACH ROW EXECUTE FUNCTION elements_after_insert();

CREATE TRIGGER trg_elements_after_delete
    AFTER DELETE ON elements
    FOR EACH ROW EXECUTE FUNCTION elements_after_delete();


-- +goose Down
DROP TRIGGER IF EXISTS trg_elements_after_delete ON elements;
DROP TRIGGER IF EXISTS trg_elements_after_insert ON elements;
DROP FUNCTION IF EXISTS elements_after_delete();
DROP FUNCTION IF EXISTS elements_after_insert();
