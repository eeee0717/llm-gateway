#!/usr/bin/env bash
# usage_records 上那条联合索引的实测依据。
#
# 往一张和 usage_records 同构的表里灌 100 万行，对典型查询「某个 Key 最近 7 天的用量记录，
# 按时间倒序取 20 条」在四种索引下各跑一次 EXPLAIN ANALYZE，再看方向写进索引定义有什么用、
# 低基数字段单独建索引是什么下场，以及索引和主键形态在写入上各要多少钱。
# 结论和数字见 docs/notes/indexing.md。
#
# MySQL 的 EXPLAIN 没有 BUFFERS，"这条计划到底翻了多少数据"改看 Handler_read_* 计数器：
# 它数的是存储引擎被要过多少行，和 PostgreSQL 的 Buffers 一样能把"扫全表"和"只读 20 行"分开。
# 计数器要 FLUSH STATUS 才清得掉，那需要 RELOAD 权限，所以整个实验用 root 连本地这套 compose。
#
# 用法（先 docker compose up -d 起 MySQL）：
#   scripts/index-experiment.sh [行数] [Key 数]
#   KEEP_TABLE=1 scripts/index-experiment.sh   # 跑完留下 usage_records_bench 好自己接着查
set -euo pipefail

cd "$(dirname "$0")/.."

rows=${1:-1000000}
keys=${2:-50}
keep=${KEEP_TABLE:-}

# 口令走 MYSQL_PWD，写在命令行上的话每条语句都会附带一句告警。
run() {
	docker compose exec -e MYSQL_PWD=root -T mysql mysql -uroot --default-character-set=utf8mb4 --table gateway "$@"
}

# 计划是一大段带换行的文本，用 --raw -B -N 原样打出来，不要表格框也不要转义。
plan() {
	docker compose exec -e MYSQL_PWD=root -T mysql mysql -uroot --default-character-set=utf8mb4 --raw -B -N gateway "$@"
}

# MySQL 没有 DROP INDEX IF EXISTS，先问一句 information_schema。
drop_index() {
	local table=$1 index=$2
	local columns
	columns=$(docker compose exec -e MYSQL_PWD=root -T mysql mysql -uroot --default-character-set=utf8mb4 -B -N gateway -e "
		SELECT count(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = '$table' AND index_name = '$index';")
	if [ "$columns" != "0" ]; then
		run -e "DROP INDEX $index ON $table;"
	fi
}

query="
	SELECT * FROM usage_records_bench
	WHERE api_key_id = 7 AND created_at >= NOW() - INTERVAL 7 DAY
	ORDER BY created_at DESC
	LIMIT 20"

echo "灌 ${rows} 行，分散在 ${keys} 个 Key 上，时间摊在最近 30 天里"

# 同构的另一张表，不带主键、索引和外键：外键只影响写，这里先测读。
# InnoDB 的表数据就存在主键那棵树里，所以"没有主键"这件事要说明白：这张表用的是 InnoDB
# 自己生成的隐藏行号做聚簇索引，行按插入顺序排，正好对应真实用量记录的追加写。
# MySQL 没有 generate_series，行由递归 CTE 生成，递归深度默认只有 1000，得先放开。
run <<SQL
DROP TABLE IF EXISTS usage_records_bench;
CREATE TABLE usage_records_bench (
    request_id        VARCHAR(64)  NOT NULL,
    api_key_id        BIGINT       NOT NULL,
    model             VARCHAR(255) NOT NULL,
    prompt_tokens     INT          NOT NULL,
    completion_tokens INT          NOT NULL,
    cost_micro        BIGINT       NOT NULL,
    estimated         BOOLEAN      NOT NULL,
    created_at        DATETIME(6)  NOT NULL
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

SET SESSION cte_max_recursion_depth = 100000000;
INSERT INTO usage_records_bench
WITH RECURSIVE seq (g) AS (
    SELECT 1 UNION ALL SELECT g + 1 FROM seq WHERE g < $rows
)
SELECT
    CONCAT('bench-', g),
    1 + g % $keys,
    ELT(1 + g % 3, 'deepseek-chat', 'deepseek-reasoner', 'mock-model'),
    100 + g % 900,
    50 + g % 450,
    2000 + g % 8000,
    g % 10 = 0,
    NOW(6) - INTERVAL FLOOR(($rows - g) * (30 * 86400 / $rows)) SECOND
FROM seq;

ANALYZE TABLE usage_records_bench;
SELECT ROUND(data_length / 1024 / 1024) AS table_mb
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name = 'usage_records_bench';
SQL

# explain 跑两遍同一条语句：第一遍可能要从磁盘读，第二遍缓冲池是热的，读数取第二遍。
# Handler 计数器要和查询在同一个会话里，所以 FLUSH STATUS、查询、SHOW STATUS 一次发过去。
explain() {
	local title=$1 index=${2:-}
	echo
	echo "=============================================================="
	echo "$title"
	echo "=============================================================="
	drop_index usage_records_bench bench_idx
	if [ -n "$index" ]; then
		run -e "CREATE INDEX bench_idx ON usage_records_bench $index;"
	fi
	run -e "ANALYZE TABLE usage_records_bench;" > /dev/null
	run -e "SELECT ROUND(index_length / 1024 / 1024, 1) AS index_mb FROM information_schema.tables
	        WHERE table_schema = DATABASE() AND table_name = 'usage_records_bench';"
	for attempt in 1 2; do
		echo "--- 第 $attempt 次 ---"
		plan -e "EXPLAIN ANALYZE $query;"
	done
	echo "--- 存储引擎被要了多少行 ---"
	run <<SQL
FLUSH STATUS;
SELECT count(*) FROM ($query) t;
SHOW SESSION STATUS WHERE Variable_name LIKE 'Handler_read%' AND Value > 0;
SQL
}

explain "1. 只有聚簇索引，没有二级索引"
explain "2. 只有 api_key_id 单列" "(api_key_id)"
explain "3. 联合索引 (api_key_id, created_at DESC)——迁移里建的就是这个" "(api_key_id, created_at DESC)"
explain "4. 联合索引 (api_key_id, created_at)，方向写成默认的升序" "(api_key_id, created_at)"

# 方向写进索引定义有什么用：单列排序时索引可以反着扫，所以升序索引一样不用排序。
# 要看出差别，得让 ORDER BY 里同时出现两个方向——这时反着扫给出的是"两列都反过来"，对不上。
for direction in "(api_key_id, created_at DESC)" "(api_key_id, created_at)"; do
	echo
	echo "=============================================================="
	echo "5. 多个 Key、方向混合的排序：$direction"
	echo "=============================================================="
	drop_index usage_records_bench bench_idx
	run -e "CREATE INDEX bench_idx ON usage_records_bench $direction;"
	run -e "ANALYZE TABLE usage_records_bench;" > /dev/null
	plan -e "
		EXPLAIN ANALYZE
		SELECT * FROM usage_records_bench
		WHERE api_key_id IN (7, 8, 9) AND created_at >= NOW() - INTERVAL 7 DAY
		ORDER BY api_key_id, created_at DESC
		LIMIT 20;"
done
drop_index usage_records_bench bench_idx

# 低基数字段：estimated 只有两种取值，model 只有三种。单独建索引也筛不掉多少行，
# 优化器会直接不用它，留下的只有写入时要维护这个索引的代价。
echo
echo "=============================================================="
echo "6. 低基数字段单独建索引：estimated（两种取值）"
echo "=============================================================="
run -e "CREATE INDEX bench_low ON usage_records_bench (estimated);"
run -e "ANALYZE TABLE usage_records_bench;" > /dev/null
plan -e "EXPLAIN ANALYZE SELECT count(*) FROM usage_records_bench WHERE estimated;"
plan -e "EXPLAIN ANALYZE SELECT count(*) FROM usage_records_bench WHERE NOT estimated;"
drop_index usage_records_bench bench_low

# 索引和主键形态都要在写入上付钱。四张表插同样的 20 万行：光板、带联合索引，
# 以及两种主键——真实 usage_records 那样拿随机的请求 ID 做主键，和自增主键加一个唯一键。
# InnoDB 的表就存在主键那棵树里，主键是随机值的话每次插入都落在树的不同位置。
echo
echo "=============================================================="
echo "7. 写入的代价：同样插 20 万行"
echo "=============================================================="
run <<'SQL'
DROP TABLE IF EXISTS bench_plain, bench_indexed, bench_random_pk, bench_serial_pk;
CREATE TABLE bench_plain     LIKE usage_records_bench;
CREATE TABLE bench_indexed   LIKE usage_records_bench;
CREATE TABLE bench_random_pk LIKE usage_records_bench;
CREATE TABLE bench_serial_pk LIKE usage_records_bench;
CREATE INDEX bench_idx ON bench_indexed (api_key_id, created_at DESC);
-- 真实 usage_records 的样子：请求 ID 直接做主键，而请求 ID 是随机的
ALTER TABLE bench_random_pk ADD PRIMARY KEY (request_id),
    ADD INDEX (api_key_id, created_at DESC);
-- 另一种：自增主键加 request_id 上的唯一键，幂等性照旧，但插入总落在树的最右边
ALTER TABLE bench_serial_pk ADD COLUMN id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY FIRST,
    ADD UNIQUE (request_id), ADD INDEX (api_key_id, created_at DESC);

SET @t = NOW(6);
INSERT INTO bench_plain SELECT * FROM usage_records_bench LIMIT 200000;
SELECT TIMESTAMPDIFF(MICROSECOND, @t, NOW(6)) / 1000 AS plain_ms;

SET @t = NOW(6);
INSERT INTO bench_indexed SELECT * FROM usage_records_bench LIMIT 200000;
SELECT TIMESTAMPDIFF(MICROSECOND, @t, NOW(6)) / 1000 AS composite_index_ms;

SET @t = NOW(6);
INSERT INTO bench_random_pk
SELECT SUBSTRING(MD5(request_id), 1, 26), api_key_id, model, prompt_tokens,
       completion_tokens, cost_micro, estimated, created_at
FROM usage_records_bench LIMIT 200000;
SELECT TIMESTAMPDIFF(MICROSECOND, @t, NOW(6)) / 1000 AS random_pk_ms;

SET @t = NOW(6);
INSERT INTO bench_serial_pk (request_id, api_key_id, model, prompt_tokens, completion_tokens, cost_micro, estimated, created_at)
SELECT SUBSTRING(MD5(request_id), 1, 26), api_key_id, model, prompt_tokens,
       completion_tokens, cost_micro, estimated, created_at
FROM usage_records_bench LIMIT 200000;
SELECT TIMESTAMPDIFF(MICROSECOND, @t, NOW(6)) / 1000 AS auto_increment_pk_ms;

DROP TABLE bench_plain, bench_indexed, bench_random_pk, bench_serial_pk;
SQL

if [ -n "$keep" ]; then
	echo
	echo "usage_records_bench 留着了，自己查完记得 DROP TABLE usage_records_bench;"
else
	run -e "DROP TABLE usage_records_bench;"
fi
