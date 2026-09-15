# 数据库用 MySQL 8.4，不用 PostgreSQL

数据库改成 MySQL 8.4 LTS：驱动换成 `gorm.io/driver/mysql`，goose 的方言换成 `mysql`，compose 和 CI 起的都是 `mysql:8.4`，PostgreSQL 和 pgx 一并移除。GORM、goose 这一层没变，换的是它下面那一层。

换引擎不是把连接串改一改就完事，有三处写法是 PostgreSQL 专有的，还有一处顺序是被 InnoDB 的锁逼着改的：

**没有 `RETURNING`。** 管理接口的充值、改额度、禁用原来都是一条 `UPDATE ... RETURNING *`，现在放进一个事务里先 `UPDATE` 再 `SELECT`——`UPDATE` 把那一行锁到提交为止，紧跟着的 `SELECT` 读到的就是刚改完的值。顺带一个陷阱：这个 Key 存不存在改看 `SELECT` 的结果，不能看 `UPDATE` 改到了几行，因为"把值改成和原来一样"（重复禁用、额度设成同一个数）在 MySQL 默认语义下是 0 行。

**没有 `ON CONFLICT DO NOTHING`。** 结算的幂等键是用量记录的主键，原来靠 `ON CONFLICT DO NOTHING` 加 `RowsAffected` 判断"这个请求是不是已经结算过"，现在改成普通 `INSERT` 撞主键，靠 `gorm.ErrDuplicatedKey`（GORM 的 `TranslateError` 把 1062 翻译过来）认出来。撞主键在 MySQL 里只是这一句失败，事务还能接着用，所以让整个事务回滚、把刚退的钱一起撤销就行。PostgreSQL 那边一旦报错整个事务就废了，得靠 SAVEPOINT，这也是它当初用 `ON CONFLICT` 的原因。

**结算里先退余额、再写用量记录。** 反过来写会死锁：`usage_records` 上有指向 `api_keys` 的外键，InnoDB 校验外键时在父行上加共享锁，紧接着的 `UPDATE` 又要把同一行升级成排他锁，同一个 Key 的并发结算于是各拿着共享锁等对方放手。M2 的 1000 并发实测有 2% 撞上 `Error 1213`，换成"先取排他锁"之后连跑五轮零死锁。PostgreSQL 碰不到这一条：它的外键加的是 `FOR KEY SHARE`，只和改动键列的语句冲突，而这里改的是余额。

**连接串有三个开关是必须的**，由 `config.NormalizeDSN` 在开连接前补上，而不是写在文档里靠人记得：`parseTime`（否则 `DATETIME` 到 Go 这边是一串字节，扫不进 `time.Time`）；`loc=UTC` 与 `time_zone='+00:00'`（`DATETIME` 自己不带时区，两端都锁死才不会随实例所在时区变）；`clientFoundRows`（让 `RowsAffected` 数的是"WHERE 匹配到几行"而不是"改动了几行"——预扣靠它判断余额够不够，而免费模型预扣 0 微元时那条 `UPDATE` 什么也没改动，按默认语义会被当成余额不够）。

**时间列用 `DATETIME(6)` 而不是 `TIMESTAMP`。** `TIMESTAMP` 自带时区语义，更接近 PostgreSQL 的 `TIMESTAMPTZ`，但它只存得到 2038 年，而用量记录是一直往后写的。

代价记在这里：**PostgreSQL 上测出来的数字一律作废**，索引那页（[indexing](../notes/indexing.md)）已经在 MySQL 上重跑，鉴权缓存和压测两页里的旧数字在文中标了引擎。另外 MySQL 的 DDL 不在事务里，一条迁移写了多句、中间失败的话前几句已经生效，改不回去；PostgreSQL 的迁移是全有全无的。
