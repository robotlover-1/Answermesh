package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ai-chat-service/pkg/log"
)

// ErrRecordsClosed 表示记录写入队列已关闭。
var ErrRecordsClosed = errors.New("chat_records writer is closed")

const (
	recordWorkers   = 4
	recordQueueSize = 2048
	recordInsertTO  = 5 * time.Second
)

type IChatRecordsData interface {
	Add(ctx context.Context, record *ChatRecord) error
	GetById(ctx context.Context, id int64) (*ChatRecord, error)
	Close(ctx context.Context) error
}

type ChatRecord struct {
	ID              int64    `json:"id"`
	UserMsg         string   `json:"user_msg"`
	UserMsgTokens   int      `json:"user_msg_tokens"`
	UserMsgKeywords []string `json:"user_msg_keywords"`
	AIMsg           string   `json:"ai_msg"`
	AIMsgTokens     int      `json:"ai_msg_tokens"`
	ReqTokens       int      `json:"req_tokens"`
	CreateAt        int64    `json:"create_at"`
}

// chatRecordsData 用有界队列 + 固定 worker 异步入库：
//   - 不再为每个请求启动无界 goroutine；
//   - Add 只入队，队列满时按调用方 ctx 做有界背压/失败；
//   - 入库失败统一记录日志，不影响请求返回；
//   - Close 排空队列（有界等待）。
type chatRecordsData struct {
	db        *sql.DB
	queue     chan *ChatRecord
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
}

func NewChatRecordsData(db *sql.DB) IChatRecordsData {
	d := &chatRecordsData{
		db:    db,
		queue: make(chan *ChatRecord, recordQueueSize),
	}
	d.wg.Add(recordWorkers)
	for i := 0; i < recordWorkers; i++ {
		go d.worker()
	}
	return d
}

func (d *chatRecordsData) worker() {
	defer d.wg.Done()
	for cr := range d.queue {
		ctx, cancel := context.WithTimeout(context.Background(), recordInsertTO)
		if _, err := d.insert(ctx, cr); err != nil {
			// 不打印用户问题/答案/关键词，避免敏感内容进入日志。
			log.ErrorF("chat_records insert failed: %v", err)
		}
		cancel()
	}
}

func (d *chatRecordsData) insert(ctx context.Context, cr *ChatRecord) (int64, error) {
	const q = "insert into chat_records(user_msg,user_msg_tokens,user_msg_keywords,ai_msg,ai_msg_tokens,req_tokens,create_at)values(?,?,?,?,?,?,?)"
	res, err := d.db.ExecContext(ctx, q, cr.UserMsg, cr.UserMsgTokens,
		strings.Join(cr.UserMsgKeywords, ","), cr.AIMsg, cr.AIMsgTokens,
		cr.ReqTokens, cr.CreateAt)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last_insert_id: %w", err)
	}
	return id, nil
}

// Add 仅入队；调用方用带超时的 ctx 限制背压时间。
func (d *chatRecordsData) Add(ctx context.Context, cr *ChatRecord) error {
	if cr == nil {
		return errors.New("nil chat record")
	}
	if d.closed.Load() {
		return ErrRecordsClosed
	}
	select {
	case d.queue <- cr:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *chatRecordsData) GetById(ctx context.Context, id int64) (*ChatRecord, error) {
	const q = "select id,user_msg,user_msg_tokens,user_msg_keywords,ai_msg,ai_msg_tokens,req_tokens,create_at from chat_records where id = ?"
	cr := &ChatRecord{}
	var keywords string
	if err := d.db.QueryRowContext(ctx, q, id).Scan(
		&cr.ID, &cr.UserMsg, &cr.UserMsgTokens, &keywords, &cr.AIMsg,
		&cr.AIMsgTokens, &cr.ReqTokens, &cr.CreateAt); err != nil {
		return nil, err
	}
	cr.UserMsgKeywords = strings.Split(keywords, ",")
	return cr, nil
}

// Close 停止接收新记录，等待已入队记录写完（受 ctx 限制）。
func (d *chatRecordsData) Close(ctx context.Context) error {
	d.closeOnce.Do(func() {
		d.closed.Store(true)
		close(d.queue)
	})
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
