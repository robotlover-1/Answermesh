// Package migrations 内嵌版本化 SQL；只允许由迁移 Job/CLI 执行，
// 不要在每个应用副本启动时同时建表。
package migrations

import "embed"

// FS 包含 0001..0004 的 up/down SQL。
//
//go:embed *.sql
var FS embed.FS
