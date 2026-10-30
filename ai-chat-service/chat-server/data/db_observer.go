package data

// DBObserver 由仓储层上报数据库指标。
// operation 必须是硬编码逻辑名（append_conversation/publish_knowledge/claim_outbox...），
// 禁止传真实 SQL 文本、主键或用户内容。
type DBObserver interface {
	DBQuery(operation, result string, seconds float64)
	TxRetry(reason string)
}

type nopDBObserver struct{}

func (nopDBObserver) DBQuery(string, string, float64) {}
func (nopDBObserver) TxRetry(string)                  {}

// DBOrNop 允许调用方不注入指标。
func DBOrNop(o DBObserver) DBObserver {
	if o == nil {
		return nopDBObserver{}
	}
	return o
}
