#!/usr/bin/env bash
# usage_records 上那条联合索引的实测依据。
#
# 往一张和 usage_records 同构的表里灌 100 万行，对典型查询「某个 Key 最近 7 天的用量记录，
# 按时间倒序取 20 条」在四种索引下各跑一次 EXPLAIN (ANALYZE, BUFFERS)，最后再看一眼
# 低基数字段单独建索引是什么下场。结论和数字见 docs/notes/indexing.md。
#
# 用法（先 docker compose up -d 起 PostgreSQL）：
#   scripts/index-experiment.sh [行数] [Key 数]
#   KEEP_TABLE=1 scripts/index-experiment.sh   # 跑完留下 usage_records_bench 好自己接着查
set -euo pipefail

cd "$(dirname "$0")/.."

rows=${1:-1000000}
keys=${2:-50}
keep=${KEEP_TABLE:-}

psql() {
	docker compose exec -T postgres psql -U gateway -d gateway -v ON_ERROR_STOP=1 -q "$@"
}

echo "灌 ${rows} 行，分散在 ${keys} 个 Key 上，时间摊在最近 30 天里"

# 同构的另一张表，不带主键、索引和外键：外键只影响写，这里测的是读。
# 行按时间递增插入，和真实的用量记录一样是追加写，物理顺序天然跟着时间走。
psql -v rows="$rows" -v keys="$keys" <<'SQL'
DROP TABLE IF EXISTS usage_records_bench;
CREATE TABLE usage_records_bench (LIKE usage_records);

INSERT INTO usage_records_bench
SELECT
    'bench-' || g,
    1 + g % :keys,
    (ARRAY['deepseek-chat', 'deepseek-reasoner', 'mock-model'])[1 + g % 3],
    100 + g % 900,
    50 + g % 450,
    2000 + g % 8000,
    g % 10 = 0,
    now() - make_interval(secs => (:rows - g) * (30 * 86400.0 / :rows))
FROM generate_series(1, :rows) AS g;

ANALYZE usage_records_bench;
SQL

psql -c "SELECT pg_size_pretty(pg_table_size('usage_records_bench')) AS 表大小"

# explain 跑两遍同一条语句：第一遍可能要从磁盘读，第二遍是缓存热的，取第二遍读数。
explain() {
	local title=$1 index=${2:-}
	echo
	echo "=============================================================="
	echo "$title"
	echo "=============================================================="
	psql -c "DROP INDEX IF EXISTS bench_idx;"
	if [ -n "$index" ]; then
		psql -c "CREATE INDEX bench_idx ON usage_records_bench $index;"
		psql -c "SELECT pg_size_pretty(pg_relation_size('bench_idx')) AS 索引大小;"
	fi
	psql -c "ANALYZE usage_records_bench;"
	for run in 1 2; do
		echo "--- 第 $run 次 ---"
		psql -c "
			EXPLAIN (ANALYZE, BUFFERS)
			SELECT * FROM usage_records_bench
			WHERE api_key_id = 7 AND created_at >= now() - interval '7 days'
			ORDER BY created_at DESC
			LIMIT 20;"
	done
}

explain "1. 无索引"
explain "2. 只有 api_key_id 单列" "(api_key_id)"
explain "3. 联合索引 (api_key_id, created_at DESC)——迁移里建的就是这个" "(api_key_id, created_at DESC)"
explain "4. 联合索引 (api_key_id, created_at)，方向写成默认的升序" "(api_key_id, created_at)"

# 方向写进索引定义有什么用：单列排序时 btree 可以反向扫，所以升序索引一样不用排序。
# 要看出差别，得让 ORDER BY 里同时出现两个方向——这时反向扫给出的是"两列都反过来"，对不上。
for direction in "(api_key_id, created_at DESC)" "(api_key_id, created_at)"; do
	echo
	echo "=============================================================="
	echo "5. 多个 Key、方向混合的排序：$direction"
	echo "=============================================================="
	psql -c "DROP INDEX IF EXISTS bench_idx;"
	psql -c "CREATE INDEX bench_idx ON usage_records_bench $direction;"
	psql -c "ANALYZE usage_records_bench;"
	psql -c "
		EXPLAIN (ANALYZE, BUFFERS)
		SELECT * FROM usage_records_bench
		WHERE api_key_id IN (7, 8, 9) AND created_at >= now() - interval '7 days'
		ORDER BY api_key_id, created_at DESC
		LIMIT 20;"
done
psql -c "DROP INDEX IF EXISTS bench_idx;"

# 低基数字段：estimated 只有两种取值，model 只有三种。单独建索引也筛不掉多少行，
# 规划器会直接不用它，留下的只有写入时要维护这个索引的代价。
echo
echo "=============================================================="
echo "6. 低基数字段单独建索引：estimated（两种取值）"
echo "=============================================================="
psql -c "DROP INDEX IF EXISTS bench_idx;"
psql -c "CREATE INDEX bench_low ON usage_records_bench (estimated);"
psql -c "ANALYZE usage_records_bench;"
psql -c "
	EXPLAIN (ANALYZE, BUFFERS)
	SELECT count(*) FROM usage_records_bench WHERE estimated;"
psql -c "
	EXPLAIN (ANALYZE, BUFFERS)
	SELECT count(*) FROM usage_records_bench WHERE NOT estimated;"
psql -c "DROP INDEX IF EXISTS bench_low;"

# 索引不是白拿的：同样的插入，带着这条联合索引要多花多少时间。
# 批量插入和真实的一行一行写不一样，这里只取个量级。
echo
echo "=============================================================="
echo "7. 写入的代价：同样插 20 万行，带索引和不带索引"
echo "=============================================================="
# 这一段要看 psql 自己报的耗时，所以不走上面那个 -q 的封装。
docker compose exec -T postgres psql -U gateway -d gateway -v ON_ERROR_STOP=1 <<'SQL'
\timing on
DROP TABLE IF EXISTS bench_write_plain, bench_write_indexed;
CREATE TABLE bench_write_plain (LIKE usage_records);
CREATE TABLE bench_write_indexed (LIKE usage_records);
CREATE INDEX ON bench_write_indexed (api_key_id, created_at DESC);

INSERT INTO bench_write_plain
SELECT 'w-' || g, 1 + g % 50, 'deepseek-chat', 100, 50, 2000, false, now()
FROM generate_series(1, 200000) AS g;

INSERT INTO bench_write_indexed
SELECT 'w-' || g, 1 + g % 50, 'deepseek-chat', 100, 50, 2000, false, now()
FROM generate_series(1, 200000) AS g;

DROP TABLE bench_write_plain, bench_write_indexed;
SQL

if [ -n "$keep" ]; then
	echo
	echo "usage_records_bench 留着了，自己查完记得 DROP TABLE usage_records_bench;"
else
	psql -c "DROP TABLE usage_records_bench;"
fi
