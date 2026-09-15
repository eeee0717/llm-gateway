-- +goose Up

-- api_keys 是调用方的凭证和余额。只存 Key 的 SHA-256，见 docs/adr/0003。
CREATE TABLE api_keys (
    id            BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name          TEXT        NOT NULL,                            -- 备注，方便管理员认出这个 Key 是给谁的
    key_hash      CHAR(64)    NOT NULL UNIQUE,                     -- Key 的 SHA-256，十六进制定长 64；鉴权按它等值查询
    balance_micro BIGINT      NOT NULL DEFAULT 0,                  -- 余额，单位微元（1 微元 = 0.000001 元），可以为负，见 docs/adr/0001
    disabled      BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- usage_records 是每次结算留下的记录，每个请求至多一条。
--
-- 时间列用 DATETIME 而不是 TIMESTAMP：TIMESTAMP 只能存到 2038 年，而这张表是一直往后写的。
-- DATETIME 自己不带时区，所以连接串里锁定 time_zone='+00:00'，写进去、读出来的都是 UTC，
-- 见 internal/config 的 NormalizeDSN。
CREATE TABLE usage_records (
    request_id        VARCHAR(64)  NOT NULL PRIMARY KEY,           -- 网关生成的请求 ID，同时是结算的幂等键
    api_key_id        BIGINT       NOT NULL,
    model             VARCHAR(255) NOT NULL,
    prompt_tokens     INT          NOT NULL,
    completion_tokens INT          NOT NULL,
    cost_micro        BIGINT       NOT NULL,
    estimated         BOOLEAN      NOT NULL,                       -- 用量来源：true 为网关估算，false 为上游报告
    created_at        DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    -- 按 Key 查用量记录，最近的排在前面。选这两列的实测依据见 docs/notes/indexing.md。
    -- 外键要求 api_key_id 上有索引，这条索引的左前缀正好是它，InnoDB 不会再多建一条。
    INDEX usage_records_api_key_id_created_at_idx (api_key_id, created_at DESC),
    CONSTRAINT usage_records_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES api_keys (id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- +goose Down

DROP TABLE usage_records;
DROP TABLE api_keys;
