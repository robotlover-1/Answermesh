package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// RecordsObserver 由入库队列上报运行指标。reason 只允许固定枚举值，
// 实现方禁止把 SQL、用户问题、答案或主键放进标签。
type RecordsObserver interface {
	EnqueueFailure(reason string)
	InsertFailure(reason string)
	InsertDuration(seconds float64)
	QueueLength(n int)
	RecordPersisted()
	DBObserver
}

type nopRecordsObserver struct{}

func (nopRecordsObserver) EnqueueFailure(string)           {}
func (nopRecordsObserver) InsertFailure(string)            {}
func (nopRecordsObserver) InsertDuration(float64)          {}
func (nopRecordsObserver) QueueLength(int)                 {}
func (nopRecordsObserver) RecordPersisted()                {}
func (nopRecordsObserver) DBQuery(string, string, float64) {}
func (nopRecordsObserver) TxRetry(string)                  {}

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
//   - 入库失败统一记录日志与指标，不影响请求返回；
//   - Close 排空队列（有界等待）。
type chatRecordsData struct {
	db     *sql.DB
	queue  chan *ChatRecord
	wg     sync.WaitGroup
	obs    RecordsObserver
	insert func(context.Context, *ChatRecord) (int64, error)

	// 关闭协议：Close 在 mu 下置 closed 并 close(closing)，阻塞中的 Add 通过
	// <-closing 立刻返回；Close 再等在途 Add 退出后才 close(queue)，
	// 因此不存在 “检查时未关闭、发送时 channel 已被 close” 的 panic。
	mu        sync.Mutex
	closed    bool
	closing   chan struct{}
	senders   sync.WaitGroup
	closeOnce sync.Once
}

// NewChatRecordsData 启动固定 worker；obs 可为 nil（不采集指标）。
func NewChatRecordsData(db *sql.DB, obs RecordsObserver) IChatRecordsData {
	if obs == nil {
		obs = nopRecordsObserver{}
	}
	d := &chatRecordsData{
		db:      db,
		queue:   make(chan *ChatRecord, recordQueueSize),
		obs:     obs,
		closing: make(chan struct{}),
	}
	d.insert = d.insertDB
	d.wg.Add(recordWorkers)
	for i := 0; i < recordWorkers; i++ {
		go d.worker()
	}
	return d
}

func (d *chatRecordsData) worker() {
	defer d.wg.Done()
	for cr := range d.queue {
		d.obs.QueueLength(len(d.queue))
		ctx, cancel := context.WithTimeout(context.Background(), recordInsertTO)
		start := time.Now()
		_, err := d.insert(ctx, cr)
		d.obs.InsertDuration(time.Since(start).Seconds())
		if err != nil {
			d.obs.InsertFailure(insertFailureReason(err))
			// 不打印用户问题/答案/关键词，避免敏感内容进入日志。
			log.ErrorF("chat_records insert failed: %v", err)
		} else {
			d.obs.RecordPersisted()
		}
		cancel()
	}
}

func (d *chatRecordsData) insertDB(ctx context.Context, cr *ChatRecord) (int64, error) {
	const q = "insert into chat_records(user_msg,user_msg_tokens,user_msg_keywords,ai_msg,ai_msg_tokens,req_tokens,create_at)values(?,?,?,?,?,?,?)"
	start := time.Now()
	res, err := d.db.ExecContext(ctx, q, cr.UserMsg, cr.UserMsgTokens,
		strings.Join(cr.UserMsgKeywords, ","), cr.AIMsg, cr.AIMsgTokens,
		cr.ReqTokens, cr.CreateAt)
	if err != nil {
		d.obs.DBQuery("insert_record", "error", time.Since(start).Seconds())
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		d.obs.DBQuery("insert_record", "error", time.Since(start).Seconds())
		return 0, fmt.Errorf("last_insert_id: %w", err)
	}
	d.obs.DBQuery("insert_record", "ok", time.Since(start).Seconds())
	return id, nil
}

func enqueueFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrRecordsClosed):
		return "closed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "other"
	}
}

func insertFailureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "db_error"
	}
}

// Add 仅入队；调用方用带超时的 ctx 限制背压时间。
func (d *chatRecordsData) Add(ctx context.Context, cr *ChatRecord) error {
	if cr == nil {
		return errors.New("nil chat record")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 注册在途发送者与 Close 的关闭动作由同一把锁串行化：
	// Close 置位后不会再有新的 Add 通过检查，Wait 期间也不会漏掉发送者。
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		d.obs.EnqueueFailure("closed")
		return ErrRecordsClosed
	}
	d.senders.Add(1)
	d.mu.Unlock()
	defer d.senders.Done()

	select {
	case d.queue <- cr:
		d.obs.QueueLength(len(d.queue))
		return nil
	case <-ctx.Done():
		d.obs.EnqueueFailure(enqueueFailureReason(ctx.Err()))
		return ctx.Err()
	case <-d.closing:
		d.obs.EnqueueFailure("closed")
		return ErrRecordsClosed
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
		d.mu.Lock()
		d.closed = true
		close(d.closing)
		d.mu.Unlock()
		d.senders.Wait() // 在途 Add 先退出，之后 close(queue) 才安全
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
