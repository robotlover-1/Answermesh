package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ai-chat-service/pkg/config"

	_ "github.com/go-sql-driver/mysql"
)

var db *sql.DB

// InitMysql 打开连接池并做一次带超时的 Ping；失败返回错误（不再 panic），
// 由调用方决定退出方式。DSN 通过配置/env 注入，不在此处硬编码。
func InitMysql(cnf *config.Config) error {
	if cnf == nil || cnf.Mysql.DSN == "" {
		return fmt.Errorf("mysql dsn is empty (set AI_CHAT_MYSQL_DSN)")
	}
	d, err := sql.Open("mysql", cnf.Mysql.DSN)
	if err != nil {
		return fmt.Errorf("mysql open: %w", err)
	}
	if cnf.Mysql.MaxOpenConn > 0 {
		d.SetMaxOpenConns(cnf.Mysql.MaxOpenConn)
	}
	if cnf.Mysql.MaxIdleConn > 0 {
		d.SetMaxIdleConns(cnf.Mysql.MaxIdleConn)
	}
	if cnf.Mysql.MaxLifeTime > 0 {
		d.SetConnMaxLifetime(time.Duration(cnf.Mysql.MaxLifeTime) * time.Second)
	}
	d.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.PingContext(ctx); err != nil {
		_ = d.Close()
		return fmt.Errorf("mysql ping: %w", err)
	}
	db = d
	return nil
}

func GetDB() *sql.DB {
	return db
}

// Close 关闭连接池；可重复调用。
func Close() error {
	if db == nil {
		return nil
	}
	err := db.Close()
	db = nil
	return err
}
