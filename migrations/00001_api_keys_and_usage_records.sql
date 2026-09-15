-- +goose Up

-- api_keys 是调用方的凭证和余额。只存 Key 的 SHA-256，见 docs/adr/0003。
CREATE TABLE api_keys (
    id            BIGSERIAL   PRIMARY KEY,
    name          TEXT        NOT NULL,                -- 备注，方便管理员认出这个 Key 是给谁的
    key_hash      TEXT        NOT NULL UNIQUE,         -- Key 的 SHA-256，十六进制；鉴权按它等值查询
    balance_micro BIGINT      NOT NULL DEFAULT 0,      -- 余额，单位微元（1 微元 = 0.000001 元），可以为负，见 docs/adr/0001
    disabled      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- usage_records 是每次结算留下的记录，每个请求至多一条。
CREATE TABLE usage_records (
    request_id        TEXT        PRIMARY KEY,         -- 网关生成的请求 ID，同时是结算的幂等键
    api_key_id        BIGINT      NOT NULL REFERENCES api_keys (id),
    model             TEXT        NOT NULL,
    prompt_tokens     INTEGER     NOT NULL,
    completion_tokens INTEGER     NOT NULL,
    cost_micro        BIGINT      NOT NULL,
    estimated         BOOLEAN     NOT NULL,            -- 用量来源：true 为网关估算，false 为上游报告
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 按 Key 查用量记录，最近的排在前面。选这两列的实测依据见 docs/notes/indexing.md。
CREATE INDEX usage_records_api_key_id_created_at_idx ON usage_records (api_key_id, created_at DESC);

-- +goose Down

DROP TABLE usage_records;
DROP TABLE api_keys;
