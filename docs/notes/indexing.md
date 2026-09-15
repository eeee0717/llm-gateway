# 用量记录的索引

## 问题

`usage_records` 每次结算写一行，只涨不减，是这套系统里唯一会长大的表。管理员要看的是「某个 Key 最近 7 天的用量记录，按时间倒序取 20 条」。

迁移 `00001` 里给它建了一条联合索引 `(api_key_id, created_at DESC)`，但当时只是按常规做法建的，没有数据佐证：这两列该不该一起建、`DESC` 写进定义换来了什么、`model` 和 `estimated` 要不要也各来一条，全凭说法。这一页把它跑出来。

## 方案

`scripts/index-experiment.sh` 是可复现的那份实验：往一张和 `usage_records` 同构的 `usage_records_bench` 里灌 100 万行（`generate_series`），分散在 50 个 Key 上，时间摊在最近 30 天里，然后在四种索引下对同一条查询各跑一次 `EXPLAIN (ANALYZE, BUFFERS)`。

```sql
SELECT * FROM usage_records_bench
WHERE api_key_id = 7 AND created_at >= now() - interval '7 days'
ORDER BY created_at DESC
LIMIT 20;
```

灌进去的表 99 MB，每个 Key 两万行，落在最近 7 天里的四千六百多行。行是按时间递增插入的，和真实的用量记录一样属于追加写，物理顺序天然跟着时间走；`api_key_id` 轮流取值，所以同一个 Key 的行均匀散在整张表里——这也和真实情况一致，多个调用方的请求是交错着结算的。

每种配置跑两次，取第二次（缓存热的那次）：

| 索引 | 计划 | Buffers | 执行时间 | 索引大小 |
|---|---|---|---|---|
| 无 | Parallel Seq Scan + top-N heapsort | 12735 | 16.09 ms | — |
| `(api_key_id)` | Bitmap Index Scan + Bitmap Heap Scan + top-N heapsort | 12684 | 8.96 ms | 6.8 MB |
| `(api_key_id, created_at DESC)` | Index Scan | **19** | **0.048 ms** | 30 MB |
| `(api_key_id, created_at)` | Index Scan Backward | 19 | 0.039 ms | 30 MB |

**无索引**时要把 100 万行全扫一遍（三个并行进程各扔掉 331778 行），再做一次 top-N 排序。

**只有 `api_key_id`** 时，索引本身很快就定位到那两万行（`Bitmap Index Scan ... rows=20000`），但接下来是 `Bitmap Heap Scan`，`Heap Blocks: exact=12658`——**整张表的堆块几乎一块不落地都摸了一遍**。因为这个 Key 的两万行均匀散在全表，一万两千多个块里差不多每块都有它一行。时间上的 12735 → 12684 个 buffer 说明了一切：读的量根本没减，省下的只是判断条件的那点 CPU。而且时间范围要在堆上过滤（`Rows Removed by Filter: 15334`），排序还得照做。

**联合索引**把 `created_at` 放进索引的第二列之后，`WHERE` 的两个条件一起进了 `Index Cond`，索引里这个 Key 的条目本来就按时间排好，扫到第 20 条就停：**19 个 buffer，0.048 毫秒，比只有单列快 186 倍**。`Sort` 节点消失了。

### `DESC` 写进定义省掉了什么

**对这条查询，什么也没省。** 上表最后一行是故意做的对照：把索引定义成默认的升序 `(api_key_id, created_at)`，计划变成 `Index Scan Backward`，时间一样（0.039 对 0.048 毫秒，就是噪声）。B 树是双向链起来的，倒着扫和正着扫一样便宜，所以单列方向的 `ORDER BY ... DESC` 用升序索引就能满足。

**消失的 `Sort` 节点是 `created_at` 进索引换来的，不是 `DESC` 换来的。** 这一点很容易记反。

`DESC` 要在 `ORDER BY` 里同时出现两个方向时才起作用，因为反向扫给出的是"所有列都反过来"。实验第 5 步查三个 Key、按 `ORDER BY api_key_id, created_at DESC` 排：

| 索引 | 计划 | Buffers | 执行时间 |
|---|---|---|---|
| `(api_key_id, created_at DESC)` | Index Scan | 16 | 0.041 ms |
| `(api_key_id, created_at)` | Incremental Sort + Index Scan | 2985 | 4.31 ms |

升序索引这时只能保证 `api_key_id` 有序（`Presorted Key: api_key_id`），组内还得自己排，于是多出一个 `Incremental Sort`，而且得把这三个 Key 在 7 天内的 4667 行全取出来才排得动，两边差出的那 100 倍就是这么来的。

这个项目现在没有这样的查询——**所以 `DESC` 这四个字母今天是白写的**。留着它是因为它不要钱（索引大小一样、单 Key 查询一样快），而将来真要按多个 Key 分组取最近几条时，不用重建一遍 30 MB 的索引。

### `model`、`estimated` 这类低基数字段

`estimated` 只有两种取值，`model` 在这套配置里只有三种。单独给它们建索引没有意义，实验第 6 步跑的是 `estimated` 上的单列索引：

- `WHERE estimated`（占 10%，十万行）：索引**用上了**，但 `Heap Blocks: exact=12659`——又是整张表的堆块。15.40 ms。
- `WHERE NOT estimated`（占 90%）：规划器**直接不用**这条索引，走 Parallel Seq Scan。23.13 ms。

索引能省下来的是"不必碰的堆块"，而低基数列筛出来的行散落在每一个块里，省不掉任何一块；选中的行再多一些，走索引反而比顺序扫更贵（随机读加回表）。这类字段的正确位置是**跟在选择性高的列后面**，或者做部分索引（`WHERE estimated`），但这两种都得先有真实的查询才谈得上。

而索引的代价是实打实的。实验第 7 步同样插 20 万行：

| 目标表 | 耗时 |
|---|---|
| 没有索引 | 143.7 ms |
| 带 `(api_key_id, created_at DESC)` | 208.5 ms |

**多 45%**，摊到每行约 0.3 微秒。低基数索引付的就是这一份，换回来的是上面那两条计划。

顺带两条：联合索引的左前缀就是 `(api_key_id)`，所以**不需要再单独建一条 `api_key_id` 索引**，只按 Key 查（比如对账时的 `count(*)`）照样走它；`request_id` 是主键，结算的 `ON CONFLICT DO NOTHING` 走的是主键自带的唯一索引，和这条联合索引不相干。

### 实验跑在什么环境上

PostgreSQL 18.6（`postgres:18-alpine`），跑在 OrbStack 的容器里，宿主是 Apple M5，Docker 虚拟机分到 10 个 CPU、11.7 GB 内存。`shared_buffers = 160 MB`，`work_mem = 4 MB`，`effective_cache_size = 5 GB`，`random_page_cost = 4`，`max_parallel_workers_per_gather = 2`，都是镜像的默认值。

```sh
docker compose up -d
scripts/index-experiment.sh            # 约 8 秒，跑完把 bench 表删掉
KEEP_TABLE=1 scripts/index-experiment.sh   # 留着表自己接着查
```

## 取舍

- **建联合索引，不是只建 `api_key_id`。** 索引从 6.8 MB 涨到 30 MB（表本身 99 MB），换来 8.96 ms → 0.048 ms。用量记录只涨不减，而单列索引的问题会跟着表一起长大：它扫出来的行数和这个 Key 的历史记录总数成正比，联合索引只取要的那 20 行。
- **除了主键，只建这一条索引。** 每多一条，结算就多一次 B 树维护，而结算在写路径上——压测时 PostgreSQL 的 CPU 已经占到 95%，是整套系统的瓶颈（见 [压测说明](loadtest.md)）。上面那 45% 是这条索引的价签，加第二条就再付一次。
- **实验用的是同构的另一张表，不是 `usage_records` 本身。** 不带外键（外键只影响写，不影响这里测的读）、不带主键（主键是另一条索引，会掺进 `Buffers` 读数里），也就不用动真表上的索引，本地库和测试数据不受影响。
- **数字是内存热的，读的时候要记住这一点。** 99 MB 的表整个装得进 160 MB 的 `shared_buffers`，所以 `Buffers` 那一行几乎全是 `hit`，顺序扫的 16 毫秒是"内存里扫 100 万行"的价钱。真实数据大到内存装不下时，这个差距只会更大：那 12735 个 buffer 里的绝大多数会变成磁盘读，而联合索引那一侧仍然只有 19 个。
- **这条索引今天还没有查询在用。** 网关只往 `usage_records` 写，管理接口目前只暴露了余额，读记录的只有测试和人工对账。索引是为"管理员查用量"准备的——所以这一页要回答的是"它将来值不值这 30 MB 和 45%"，而不是"它现在省了多少"。

## 延伸问题

- 表一直涨怎么办？按月分区或者定期归档。这条查询天然带 `api_key_id` 和时间范围，分区裁剪对它正好合适，索引也跟着分区变小。
- 要不要做覆盖索引（`INCLUDE (cost_micro, prompt_tokens, ...)`）省掉回表？这次的计划里回表只有十几个 buffer，省不出什么；而 `SELECT *` 要覆盖就得把整行塞进索引，索引会涨到和表一个量级。
- 换成按游标翻页（`WHERE created_at < $上一页最后一条` ）会不会更好？会，而且用的还是这条索引：`OFFSET` 翻到第 100 页要先扫掉前面 2000 行，游标翻页每页都只读 20 行。等真有分页接口时再说。
- 为什么写入只贵 45%？因为 `created_at` 取的是 `now()`、插入是追加的，新条目总落在索引最右边那一页上，B 树几乎不分裂。如果索引的第一列是随机值（比如把 `request_id` 建成索引），插入会散在整棵树上，代价要高得多。
