// migrate 是 AnswerMesh 的版本化迁移 CLI。只应由一个部署 Job 执行，
// 不要在每个应用副本启动时并发建表。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"ai-chat-service/db/migrations"
	"ai-chat-service/pkg/db/migrate"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	var (
		dsn    = flag.String("dsn", os.Getenv("AI_CHAT_MYSQL_DSN"), "MySQL DSN（默认读 AI_CHAT_MYSQL_DSN）")
		down   = flag.Int("down", 0, "回滚最近 N 个版本；0 表示执行 up")
		dryRun = flag.Bool("dry-run", false, "只打印将要执行的语句，不连接数据库")
		list   = flag.Bool("list", false, "列出内嵌迁移版本")
	)
	flag.Parse()

	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load migrations: %v\n", err)
		os.Exit(1)
	}
	if *list {
		for _, m := range migs {
			fmt.Printf("%04d %-32s checksum=%s down=%v\n", m.Version, m.Name, m.Checksum[:12], m.DownSQL != "")
		}
		return
	}
	if *dryRun {
		for _, m := range migs {
			fmt.Printf("== %04d %s ==\n", m.Version, m.Name)
			for _, s := range migrate.SplitStatements(m.UpSQL) {
				fmt.Printf("%s;\n", s)
			}
		}
		return
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "mysql dsn is empty (set AI_CHAT_MYSQL_DSN or -dsn)")
		os.Exit(1)
	}

	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open mysql: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ping mysql: %v\n", err)
		os.Exit(1)
	}

	if *down > 0 {
		done, err := migrate.RollbackLast(ctx, db, migs, *down)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rollback: %v (rolled back: %v)\n", err, done)
			os.Exit(1)
		}
		fmt.Printf("rolled back versions: %v\n", done)
		return
	}
	done, err := migrate.Apply(ctx, db, migs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v (applied: %v)\n", err, done)
		os.Exit(1)
	}
	if len(done) == 0 {
		fmt.Println("schema up to date")
		return
	}
	fmt.Printf("applied versions: %v\n", done)
}
