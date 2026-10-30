// Package migrate 提供最小的版本化迁移执行器。
//
// 设计约束：
//   - 迁移只由单个 CLI/Job 执行，应用副本启动时不自动建表；
//   - schema_migrations 记录版本与 up SQL 的 SHA-256，同一版本内容被修改会报错；
//   - MySQL DDL 会隐式提交，事务不能回滚已执行的 DDL。迁移器会对
//     duplicate column/key（1060/1061）与 drop 不存在对象（1091）做状态校验后
//     安全重试；多条 ALTER 动作请拆成多条语句，避免一条失败连带跳过后续动作。
package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Migration 是一个版本的正向/回滚 SQL。
type Migration struct {
	Version  int64
	Name     string
	UpSQL    string
	DownSQL  string
	Checksum string
}

var fileNameRe = regexp.MustCompile(`^(\d+)_(.+)\.up\.sql$`)

// Load 从 fsys 读取 *.up.sql（同目录 *.down.sql 可选），按版本升序返回。
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	byVersion := map[int64]*Migration{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migrate: bad version in %q", e.Name())
		}
		if _, dup := byVersion[version]; dup {
			return nil, fmt.Errorf("migrate: duplicate migration version %d", version)
		}
		up, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(up)
		mig := &Migration{
			Version:  version,
			Name:     m[2],
			UpSQL:    string(up),
			Checksum: hex.EncodeToString(sum[:]),
		}
		downName := fmt.Sprintf("%s_%s.down.sql", m[1], m[2])
		if b, err := fs.ReadFile(fsys, downName); err == nil {
			mig.DownSQL = string(b)
		}
		byVersion[version] = mig
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Applied 是 schema_migrations 中的一行。
type Applied struct {
	Version  int64
	Name     string
	Checksum string
}

const createTableSQL = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version BIGINT NOT NULL,
  name VARCHAR(191) NOT NULL,
  checksum CHAR(64) NOT NULL,
  applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

// queryer 由 *sql.DB / *sql.Conn 实现；迁移锁要求所有操作固定在同一连接。
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const lockName = "answermesh_schema_migrations"

// acquireLock 用 MySQL GET_LOCK 串行化迁移 Job，避免多个副本/重试并发执行 DDL。
func acquireLock(ctx context.Context, conn *sql.Conn) error {
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 30)", lockName).Scan(&got); err != nil {
		return fmt.Errorf("migrate: acquire lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return fmt.Errorf("migrate: acquire lock timed out (another migration job is running)")
	}
	return nil
}

func releaseLock(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", lockName)
}

// Ensure 创建 schema_migrations 表。
func Ensure(ctx context.Context, q queryer) error {
	_, err := q.ExecContext(ctx, createTableSQL)
	return err
}

// AppliedVersions 返回已应用版本。
func AppliedVersions(ctx context.Context, q queryer) (map[int64]Applied, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Applied{}
	for rows.Next() {
		var a Applied
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum); err != nil {
			return nil, err
		}
		out[a.Version] = a
	}
	return out, rows.Err()
}

var (
	alterTableRe = regexp.MustCompile("(?is)^\\s*ALTER\\s+TABLE\\s+([`\\w.]+)")
	addKeyRe     = regexp.MustCompile("(?is)ADD\\s+(?:UNIQUE\\s+)?(?:KEY|INDEX)\\s+`?([A-Za-z0-9_]+)")
	dropKeyRe    = regexp.MustCompile("(?is)DROP\\s+(?:KEY|INDEX)\\s+`?([A-Za-z0-9_]+)")
	addColumnRe  = regexp.MustCompile("(?is)ADD\\s+COLUMN\\s+`?([A-Za-z0-9_]+)")
)

// ddlAlreadyApplied 处理 MySQL DDL 隐式提交后的重试：DDL 已生效但版本行写入失败时，
// 下一次重跑会收到 duplicate column/key 或 “cannot drop, doesn't exist”。
// 只有在该对象确实处于目标状态（存在/不存在）时才视为可重试成功。
func ddlAlreadyApplied(ctx context.Context, q queryer, stmt string, execErr error) bool {
	var me *mysql.MySQLError
	if !errors.As(execErr, &me) {
		return false
	}
	m := alterTableRe.FindStringSubmatch(stmt)
	if m == nil {
		return false
	}
	table := strings.Trim(strings.TrimSpace(m[1]), "`")
	if i := strings.LastIndex(table, "."); i >= 0 {
		table = strings.Trim(table[i+1:], "`")
	}
	indexExists := func(name string) bool {
		var n int
		err := q.QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.statistics
WHERE table_schema=DATABASE() AND table_name=? AND index_name=?`, table, name).Scan(&n)
		return err == nil && n > 0
	}
	columnExists := func(name string) bool {
		var n int
		err := q.QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.columns
WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, table, name).Scan(&n)
		return err == nil && n > 0
	}
	switch me.Number {
	case 1061: // ER_DUP_KEYNAME
		if k := addKeyRe.FindStringSubmatch(stmt); k != nil {
			return indexExists(k[1])
		}
	case 1060: // ER_DUP_FIELDNAME
		if c := addColumnRe.FindStringSubmatch(stmt); c != nil {
			return columnExists(c[1])
		}
	case 1091: // ER_CANT_DROP_FIELD_OR_KEY
		if k := dropKeyRe.FindStringSubmatch(stmt); k != nil {
			return !indexExists(k[1])
		}
	}
	return false
}

// Apply 依次执行未应用的迁移；返回本次新应用的版本。
// 整个执行过程持有 GET_LOCK，多个 Job 并发启动时只有一个能真正执行。
func Apply(ctx context.Context, db *sql.DB, migs []Migration) ([]int64, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := acquireLock(ctx, conn); err != nil {
		return nil, err
	}
	defer releaseLock(conn)
	return apply(ctx, conn, migs)
}

func apply(ctx context.Context, q queryer, migs []Migration) ([]int64, error) {
	if err := Ensure(ctx, q); err != nil {
		return nil, fmt.Errorf("migrate: ensure schema_migrations: %w", err)
	}
	applied, err := AppliedVersions(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("migrate: read applied versions: %w", err)
	}
	var done []int64
	for _, m := range migs {
		if prev, ok := applied[m.Version]; ok {
			if prev.Checksum != m.Checksum {
				return done, fmt.Errorf("migrate: version %d (%s) changed after being applied (checksum mismatch)", m.Version, m.Name)
			}
			continue
		}
		stmts := SplitStatements(m.UpSQL)
		if len(stmts) == 0 {
			return done, fmt.Errorf("migrate: version %d (%s) has no statements", m.Version, m.Name)
		}
		for i, stmt := range stmts {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				if ddlAlreadyApplied(ctx, q, stmt, err) {
					continue
				}
				return done, fmt.Errorf("migrate: version %d (%s) statement %d: %w", m.Version, m.Name, i+1, err)
			}
		}
		// DDL 已隐式提交；版本记录最后写，失败时重跑依赖 DDL 的 IF NOT EXISTS。
		if _, err := q.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, checksum) VALUES(?,?,?)",
			m.Version, m.Name, m.Checksum); err != nil {
			return done, fmt.Errorf("migrate: record version %d: %w", m.Version, err)
		}
		done = append(done, m.Version)
	}
	return done, nil
}

// RollbackLast 回滚最近 n 个已应用版本（按版本降序执行 down SQL）。
func RollbackLast(ctx context.Context, db *sql.DB, migs []Migration, n int) ([]int64, error) {
	if n <= 0 {
		return nil, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := acquireLock(ctx, conn); err != nil {
		return nil, err
	}
	defer releaseLock(conn)
	return rollbackLast(ctx, conn, migs, n)
}

func rollbackLast(ctx context.Context, q queryer, migs []Migration, n int) ([]int64, error) {
	if err := Ensure(ctx, q); err != nil {
		return nil, err
	}
	applied, err := AppliedVersions(ctx, q)
	if err != nil {
		return nil, err
	}
	byVersion := map[int64]Migration{}
	for _, m := range migs {
		byVersion[m.Version] = m
	}
	versions := make([]int64, 0, len(applied))
	for v := range applied {
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })
	var done []int64
	for _, v := range versions {
		if len(done) >= n {
			break
		}
		m, ok := byVersion[v]
		if !ok {
			return done, fmt.Errorf("migrate: applied version %d has no local migration", v)
		}
		if strings.TrimSpace(m.DownSQL) == "" {
			return done, fmt.Errorf("migrate: version %d (%s) has no down SQL", v, m.Name)
		}
		for i, stmt := range SplitStatements(m.DownSQL) {
			if _, err := q.ExecContext(ctx, stmt); err != nil {
				if ddlAlreadyApplied(ctx, q, stmt, err) {
					continue
				}
				return done, fmt.Errorf("migrate: rollback %d statement %d: %w", v, i+1, err)
			}
		}
		if _, err := q.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version=?", v); err != nil {
			return done, err
		}
		done = append(done, v)
	}
	return done, nil
}

// SplitStatements 按顶层分号切分 SQL，忽略引号/反引号与注释中的分号。
// 迁移文件只包含 DDL，不包含存储过程，因此不需要 DELIMITER 支持。
func SplitStatements(text string) []string {
	var out []string
	var b strings.Builder
	var quote byte // 0, '\'' , '"', '`'
	i := 0
	for i < len(text) {
		c := text[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == '\\' && i+1 < len(text) {
				b.WriteByte(text[i+1])
				i += 2
				continue
			}
			if c == quote {
				// SQL 中双写引号表示转义。
				if i+1 < len(text) && text[i+1] == quote {
					b.WriteByte(text[i+1])
					i += 2
					continue
				}
				quote = 0
			}
			i++
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
			b.WriteByte(c)
			i++
		case '-':
			if i+1 < len(text) && text[i+1] == '-' {
				for i < len(text) && text[i] != '\n' {
					i++
				}
				continue
			}
			b.WriteByte(c)
			i++
		case '#':
			for i < len(text) && text[i] != '\n' {
				i++
			}
		case '/':
			if i+1 < len(text) && text[i+1] == '*' {
				i += 2
				for i+1 < len(text) && !(text[i] == '*' && text[i+1] == '/') {
					i++
				}
				if i+1 < len(text) {
					i += 2
				}
				continue
			}
			b.WriteByte(c)
			i++
		case ';':
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, s)
			}
			b.Reset()
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	if s := strings.TrimSpace(b.String()); s != "" {
		out = append(out, s)
	}
	return out
}
