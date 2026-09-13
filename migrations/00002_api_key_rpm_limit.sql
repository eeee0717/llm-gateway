-- +goose Up

-- 每个 Key 自己的限流额度，见 docs/adr/0004。0 表示跟着配置里的默认值走。
ALTER TABLE api_keys ADD COLUMN rpm_limit INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE api_keys DROP COLUMN rpm_limit;
