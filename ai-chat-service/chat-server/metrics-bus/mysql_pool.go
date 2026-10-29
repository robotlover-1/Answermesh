package metrics_bus

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// MySQLPoolCollector 在 scrape 时读取 database/sql 的 DBStats，
// 开销很低，不会额外查询数据库。db 用函数注入：启动早期尚未初始化时不报错。
type MySQLPoolCollector struct {
	db func() *sql.DB

	open        *prometheus.Desc
	inUse       *prometheus.Desc
	idle        *prometheus.Desc
	maxOpen     *prometheus.Desc
	waitCount   *prometheus.Desc
	waitSeconds *prometheus.Desc
	idleClosed  *prometheus.Desc
	idleTimeOut *prometheus.Desc
	lifeTimeEnd *prometheus.Desc
}

func NewMySQLPoolCollector(db func() *sql.DB) *MySQLPoolCollector {
	d := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(NAMESPACE+"_"+SUBSYSTEM+"_"+name, help,
			nil, map[string]string{"app": "ai_chat"})
	}
	return &MySQLPoolCollector{
		db:          db,
		open:        d("mysql_pool_open_connections", "连接池当前打开的连接数。"),
		inUse:       d("mysql_pool_in_use_connections", "连接池当前被占用的连接数。"),
		idle:        d("mysql_pool_idle_connections", "连接池当前空闲连接数。"),
		maxOpen:     d("mysql_pool_max_open_connections", "连接池 MaxOpenConns 配置值。"),
		waitCount:   d("mysql_pool_wait_count_total", "为获取连接而累计等待的次数。"),
		waitSeconds: d("mysql_pool_wait_duration_seconds_total", "为获取连接而累计等待的秒数。"),
		idleClosed:  d("mysql_pool_max_idle_closed_total", "因 MaxIdleConns 被关闭的累计连接数。"),
		idleTimeOut: d("mysql_pool_max_idle_time_closed_total", "因 MaxIdleTime 被关闭的累计连接数。"),
		lifeTimeEnd: d("mysql_pool_max_lifetime_closed_total", "因 MaxLifetime 被关闭的累计连接数。"),
	}
}

func (c *MySQLPoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		c.open, c.inUse, c.idle, c.maxOpen,
		c.waitCount, c.waitSeconds, c.idleClosed, c.idleTimeOut, c.lifeTimeEnd,
	} {
		ch <- d
	}
}

func (c *MySQLPoolCollector) Collect(ch chan<- prometheus.Metric) {
	db := c.db()
	if db == nil {
		return
	}
	st := db.Stats()
	g := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)
	}
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	g(c.open, float64(st.OpenConnections))
	g(c.inUse, float64(st.InUse))
	g(c.idle, float64(st.Idle))
	g(c.maxOpen, float64(st.MaxOpenConnections))
	counter(c.waitCount, float64(st.WaitCount))
	counter(c.waitSeconds, st.WaitDuration.Seconds())
	counter(c.idleClosed, float64(st.MaxIdleClosed))
	counter(c.idleTimeOut, float64(st.MaxIdleTimeClosed))
	counter(c.lifeTimeEnd, float64(st.MaxLifetimeClosed))
}
