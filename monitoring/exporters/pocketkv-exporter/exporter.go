package main

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "pocketkv"

type descSet struct {
	allocCalls     *prometheus.Desc
	freeCalls      *prometheus.Desc
	allocatedBytes *prometheus.Desc
	requestedBytes *prometheus.Desc
	fragmentation  *prometheus.Desc
	slabUtil       *prometheus.Desc
	classPages     *prometheus.Desc
	classChunks    *prometheus.Desc
	classFree      *prometheus.Desc
	classPageBytes *prometheus.Desc

	masterOffset  *prometheus.Desc
	appliedOffset *prometheus.Desc
	durableOffset *prometheus.Desc
	commitGap     *prometheus.Desc
	commitTimeout *prometheus.Desc
	commitDesync  *prometheus.Desc

	proxyCacheBytes *prometheus.Desc

	rdmaJobs            *prometheus.Desc
	rdmaCompleted       *prometheus.Desc
	rdmaFailed          *prometheus.Desc
	rdmaCQErrors        *prometheus.Desc
	rdmaCMRejects       *prometheus.Desc
	rdmaResourceRejects *prometheus.Desc
	rdmaConnectedJobs   *prometheus.Desc

	aofSubmitted      *prometheus.Desc
	aofWritten        *prometheus.Desc
	aofFatal          *prometheus.Desc
	aofOutstanding    *prometheus.Desc
	aofBatchCount     *prometheus.Desc
	aofBatchDur       *prometheus.Desc
	aofErrorCount     *prometheus.Desc
	aofMaxBatchAge    *prometheus.Desc
	replAckAge        *prometheus.Desc
	replSlotApplied   *prometheus.Desc
	replSlotDurable   *prometheus.Desc
	replSlotAckAge    *prometheus.Desc
	replSlotTransport *prometheus.Desc

	fullsyncTotal  *prometheus.Desc
	partialOKTotal *prometheus.Desc
	partialErr     *prometheus.Desc
	connectedSlave *prometheus.Desc
	replicas       *prometheus.Desc

	buildInfo *prometheus.Desc
}

func newDescSet() *descSet {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(namespace+"_"+name, help, labels, nil)
	}
	return &descSet{
		allocCalls:     d("allocator_alloc_calls_total", "分配器 alloc 调用次数（进程重启会归零）。"),
		freeCalls:      d("allocator_free_calls_total", "分配器 free 调用次数；不是当前空闲块数量。"),
		allocatedBytes: d("allocator_allocated_bytes", "分配器实际管理的内存字节数，不等于进程 RSS。"),
		requestedBytes: d("allocator_requested_bytes", "调用方申请的内存字节数。"),
		fragmentation:  d("allocator_fragmentation_ratio", "分配器内部碎片率（0~1）。"),
		slabUtil:       d("allocator_slab_utilization_ratio", "小对象页利用率 = current_small_inuse / total_small_page_bytes（0~1）。"),
		classPages:     d("allocator_class_pages", "每个 slab size class 的页数。", "size_class"),
		classChunks:    d("allocator_class_chunks", "每个 size class 的 chunk 总数。", "size_class"),
		classFree:      d("allocator_class_free_chunks", "每个 size class 的空闲 chunk 数。", "size_class"),
		classPageBytes: d("allocator_class_page_bytes", "每个 size class 的页占用字节数。", "size_class"),

		masterOffset:  d("replication_master_offset_bytes", "master_repl_offset（字节）。"),
		appliedOffset: d("replication_applied_offset_bytes", "Slave 已应用的复制 offset（字节）。"),
		durableOffset: d("replication_durable_offset_bytes", "Slave 已持久化的复制 offset（字节）。"),
		commitGap:     d("commit_gap_total", "Commit Gate 观测到的复制缺口次数。"),
		commitTimeout: d("commit_timeout_total", "Commit Gate 超时次数。"),
		commitDesync:  d("commit_desync_total", "Commit Gate 失步次数。"),

		proxyCacheBytes: d("proxy_recovery_cache_bytes", "eBPF proxy 断线恢复缓存字节数。"),

		rdmaJobs:            d("rdma_fullsync_jobs", "RDMA fullsync job 数，按状态（active/sending/waiting）分类。", "state"),
		rdmaCompleted:       d("rdma_fullsync_completed_total", "完成的 RDMA fullsync job 总数。"),
		rdmaFailed:          d("rdma_fullsync_failed_total", "失败的 RDMA fullsync job 总数。"),
		rdmaCQErrors:        d("rdma_cq_errors_total", "RDMA CQ 错误总数。"),
		rdmaCMRejects:       d("rdma_cm_rejects_total", "RDMA CM 连接拒绝次数。"),
		rdmaResourceRejects: d("rdma_resource_rejects_total", "RDMA 资源预算拒绝次数。"),
		rdmaConnectedJobs:   d("rdma_connected_jobs", "当前已建立连接的 RDMA job 数。"),

		aofSubmitted:      d("aof_submitted_bytes", "AOF 已提交写入的字节偏移。"),
		aofWritten:        d("aof_written_bytes", "AOF 已完成 fsync 的字节偏移；差额为持久化滞后。"),
		aofFatal:          d("aof_fatal", "AOF 线程进入 fatal 状态（1=fatal，此后只进内存不落 AOF）。"),
		aofOutstanding:    d("aof_outstanding_batches", "AOF 在途批次（ready + in-flight）。"),
		aofBatchCount:     d("aof_batch_count_total", "完成的 AOF write+fsync 批次数。"),
		aofBatchDur:       d("aof_batch_duration_seconds_total", "AOF 批次 write+fsync 累计耗时；配合 _count_total 求平均。"),
		aofErrorCount:     d("aof_error_count_total", "AOF 写/fsync 错误累计次数。"),
		aofMaxBatchAge:    d("aof_max_batch_age_seconds", "观测到的最大攒批年龄（崩溃窗口分量）。"),
		replAckAge:        d("replication_min_ack_age_seconds", "所有副本中最早一次 REPLACK 距今的秒数（无副本时为 0）。"),
		replSlotApplied:   d("replica_applied_offset_bytes", "per-slot 副本已应用 offset（eBPF slot 0..7）。", "slot"),
		replSlotDurable:   d("replica_durable_offset_bytes", "per-slot 副本已持久化 offset（eBPF slot 0..7）。", "slot"),
		replSlotAckAge:    d("replica_ack_age_seconds", "per-slot 副本 REPLACK 年龄（秒）。", "slot"),
		replSlotTransport: d("replica_transport_state", "per-slot 副本传输状态（固定枚举，值恒为 1）。", "slot", "state"),

		fullsyncTotal:  d("repl_fullsync_total", "触发 fullsync 的总次数。"),
		partialOKTotal: d("repl_partialsync_ok_total", "partial resync 成功次数。"),
		partialErr:     d("repl_partialsync_err_total", "partial resync 失败次数。"),
		connectedSlave: d("connected_slaves", "当前连接的 Slave 数（master 视角）。"),
		replicas:       d("replicas", "当前副本数。"),

		buildInfo: d("build_info", "Exporter 构建信息（版本只使用发布版本/短 SHA）。", "version", "role"),
	}
}

// Exporter 实现 prometheus.Collector：每次抓取只发 INFO + MEMSTAT 两条只读命令。
type Exporter struct {
	client *respClient
	descs  *descSet

	version string
	role    string

	scrapeSuccess  prometheus.Gauge
	scrapeDuration prometheus.Gauge
	parseErrors    prometheus.Counter
}

func NewExporter(client *respClient, version, role string) *Exporter {
	return &Exporter{
		client:  client,
		descs:   newDescSet(),
		version: version,
		role:    role,
		scrapeSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: namespace + "_exporter_scrape_success",
			Help: "最近一次抓取 PocketKV 是否成功（1=成功，0=失败）。",
		}),
		scrapeDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: namespace + "_exporter_scrape_duration_seconds",
			Help: "最近一次 INFO+MEMSTAT 抓取耗时（秒）。",
		}),
		parseErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: namespace + "_exporter_parse_errors_total",
			Help: "PocketKV 响应中无法解析字段/行的累计次数。",
		}),
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	d := e.descs
	for _, desc := range []*prometheus.Desc{
		d.allocCalls, d.freeCalls, d.allocatedBytes, d.requestedBytes, d.fragmentation, d.slabUtil,
		d.classPages, d.classChunks, d.classFree, d.classPageBytes,
		d.masterOffset, d.appliedOffset, d.durableOffset,
		d.commitGap, d.commitTimeout, d.commitDesync,
		d.proxyCacheBytes,
		d.rdmaJobs, d.rdmaCompleted, d.rdmaFailed, d.rdmaCQErrors,
		d.rdmaCMRejects, d.rdmaResourceRejects, d.rdmaConnectedJobs,
		d.aofSubmitted, d.aofWritten, d.aofFatal, d.replAckAge,
		d.replSlotApplied, d.replSlotDurable, d.replSlotAckAge, d.replSlotTransport,
		d.aofOutstanding, d.aofBatchCount, d.aofBatchDur, d.aofErrorCount, d.aofMaxBatchAge,
		d.fullsyncTotal, d.partialOKTotal, d.partialErr, d.connectedSlave, d.replicas,
		d.buildInfo,
	} {
		if desc != nil {
			ch <- desc
		}
	}
	e.scrapeSuccess.Describe(ch)
	e.scrapeDuration.Describe(ch)
	e.parseErrors.Describe(ch)
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	infoText, memText, err := e.scrape()
	e.scrapeDuration.Set(time.Since(start).Seconds())
	if err != nil {
		e.scrapeSuccess.Set(0)
		e.scrapeSuccess.Collect(ch)
		e.scrapeDuration.Collect(ch)
		e.parseErrors.Collect(ch)
		// 抓取失败时无法从 INFO 取 role，使用启动参数中的可信角色。
		role := e.role
		if role != "master" && role != "slave" {
			role = "unknown"
		}
		e.emitBuildInfo(ch, role)
		return
	}
	info := parseKeyValue(infoText, e.noteParseError)
	mem := parseKeyValue(memText, e.noteParseError)
	e.scrapeSuccess.Set(1)

	role := e.role
	if role == "" || role == "auto" {
		role = info["role"]
	}
	if role != "master" && role != "slave" {
		role = "unknown"
	}
	e.emitBuildInfo(ch, role)

	e.collectAllocator(ch, mem)
	e.collectReplication(ch, info)
	e.collectRDMA(ch, info)
	e.collectAOF(ch, info)
	e.collectReplicaSlots(ch, info)
	e.collectProxy(ch, info)

	e.scrapeSuccess.Collect(ch)
	e.scrapeDuration.Collect(ch)
	e.parseErrors.Collect(ch)
}

func (e *Exporter) scrape() (string, string, error) {
	info, err := e.client.textCommand("INFO")
	if err != nil {
		return "", "", err
	}
	mem, err := e.client.textCommand("MEMSTAT")
	if err != nil {
		return "", "", err
	}
	return info, mem, nil
}

func (e *Exporter) noteParseError() {
	e.parseErrors.Inc()
}

func parseKeyValue(text string, note func()) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			k, v, ok = strings.Cut(line, "=")
		}
		if !ok || k == "" {
			if note != nil {
				note()
			}
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

func (e *Exporter) emitBuildInfo(ch chan<- prometheus.Metric, role string) {
	ch <- prometheus.MustNewConstMetric(e.descs.buildInfo, prometheus.GaugeValue, 1, e.version, role)
}

func (e *Exporter) emitUint(ch chan<- prometheus.Metric, m map[string]string, key string,
	d *prometheus.Desc, vt prometheus.ValueType, labels ...string) {
	raw, ok := m[key]
	if !ok {
		return
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		e.noteParseError()
		return
	}
	ch <- prometheus.MustNewConstMetric(d, vt, float64(v), labels...)
}

func (e *Exporter) emitInt(ch chan<- prometheus.Metric, m map[string]string, key string,
	d *prometheus.Desc, vt prometheus.ValueType, labels ...string) {
	raw, ok := m[key]
	if !ok {
		return
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		e.noteParseError()
		return
	}
	ch <- prometheus.MustNewConstMetric(d, vt, float64(v), labels...)
}

func (e *Exporter) emitFloat(ch chan<- prometheus.Metric, m map[string]string, key string,
	d *prometheus.Desc) {
	raw, ok := m[key]
	if !ok {
		return
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		e.noteParseError()
		return
	}
	ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v)
}

func (e *Exporter) collectAllocator(ch chan<- prometheus.Metric, mem map[string]string) {
	e.emitUint(ch, mem, "alloc_calls", e.descs.allocCalls, prometheus.CounterValue)
	e.emitUint(ch, mem, "free_calls", e.descs.freeCalls, prometheus.CounterValue)
	e.emitUint(ch, mem, "current_allocated_bytes", e.descs.allocatedBytes, prometheus.GaugeValue)
	e.emitUint(ch, mem, "current_requested_bytes", e.descs.requestedBytes, prometheus.GaugeValue)
	e.emitFloat(ch, mem, "internal_fragment_rate", e.descs.fragmentation)
	e.emitFloat(ch, mem, "page_utilization", e.descs.slabUtil)

	// class_<i>_{size,pages,total_chunks,free_chunks,page_bytes}
	type classStat struct {
		size      uint64
		pages     uint64
		chunks    uint64
		free      uint64
		pageBytes uint64
		seen      map[string]bool
	}
	classes := make(map[int]*classStat)
	for k, v := range mem {
		rest, ok := strings.CutPrefix(k, "class_")
		if !ok {
			continue
		}
		idxPart, field, ok := strings.Cut(rest, "_")
		if !ok {
			continue
		}
		idx, err := strconv.Atoi(idxPart)
		if err != nil || idx < 0 || idx >= 64 {
			e.noteParseError()
			continue
		}
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			e.noteParseError()
			continue
		}
		st := classes[idx]
		if st == nil {
			st = &classStat{seen: make(map[string]bool)}
			classes[idx] = st
		}
		if st.seen[field] {
			continue
		}
		st.seen[field] = true
		switch field {
		case "size":
			st.size = n
		case "pages":
			st.pages = n
		case "total_chunks":
			st.chunks = n
		case "free_chunks":
			st.free = n
		case "page_bytes":
			st.pageBytes = n
		}
	}
	idxs := make([]int, 0, len(classes))
	for i := range classes {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		st := classes[i]
		if st.size == 0 {
			continue
		}
		label := strconv.FormatUint(st.size, 10)
		ch <- prometheus.MustNewConstMetric(e.descs.classPages, prometheus.GaugeValue, float64(st.pages), label)
		ch <- prometheus.MustNewConstMetric(e.descs.classChunks, prometheus.GaugeValue, float64(st.chunks), label)
		ch <- prometheus.MustNewConstMetric(e.descs.classFree, prometheus.GaugeValue, float64(st.free), label)
		ch <- prometheus.MustNewConstMetric(e.descs.classPageBytes, prometheus.GaugeValue, float64(st.pageBytes), label)
	}
}

func (e *Exporter) collectReplication(ch chan<- prometheus.Metric, info map[string]string) {
	e.emitUint(ch, info, "master_repl_offset", e.descs.masterOffset, prometheus.GaugeValue)
	e.emitUint(ch, info, "slave_repl_applied_offset", e.descs.appliedOffset, prometheus.GaugeValue)
	e.emitUint(ch, info, "slave_repl_durable_offset", e.descs.durableOffset, prometheus.GaugeValue)
	e.emitUint(ch, info, "repl_commit_gap", e.descs.commitGap, prometheus.CounterValue)
	e.emitUint(ch, info, "repl_commit_timeout", e.descs.commitTimeout, prometheus.CounterValue)
	e.emitUint(ch, info, "repl_commit_desync", e.descs.commitDesync, prometheus.CounterValue)
	e.emitUint(ch, info, "repl_fullsync_count", e.descs.fullsyncTotal, prometheus.CounterValue)
	e.emitUint(ch, info, "repl_partialsync_ok_count", e.descs.partialOKTotal, prometheus.CounterValue)
	e.emitUint(ch, info, "repl_partialsync_err_count", e.descs.partialErr, prometheus.CounterValue)
	e.emitUint(ch, info, "connected_slaves", e.descs.connectedSlave, prometheus.GaugeValue)
	e.emitUint(ch, info, "replicas", e.descs.replicas, prometheus.GaugeValue)
}

func (e *Exporter) collectRDMA(ch chan<- prometheus.Metric, info map[string]string) {
	for key, state := range map[string]string{
		"rdma_fs_active_jobs":  "active",
		"rdma_fs_sending_jobs": "sending",
		"rdma_fs_waiting_jobs": "waiting",
	} {
		e.emitUint(ch, info, key, e.descs.rdmaJobs, prometheus.GaugeValue, state)
	}
	e.emitUint(ch, info, "rdma_fs_total_completed", e.descs.rdmaCompleted, prometheus.CounterValue)
	e.emitUint(ch, info, "rdma_fs_total_failed", e.descs.rdmaFailed, prometheus.CounterValue)
	e.emitUint(ch, info, "rdma_fs_cq_errors", e.descs.rdmaCQErrors, prometheus.CounterValue)
	e.emitUint(ch, info, "rdma_fs_cm_rejects", e.descs.rdmaCMRejects, prometheus.CounterValue)
	e.emitUint(ch, info, "rdma_fs_resource_rejects", e.descs.rdmaResourceRejects, prometheus.CounterValue)
	e.emitUint(ch, info, "rdma_fs_connected_jobs", e.descs.rdmaConnectedJobs, prometheus.GaugeValue)
}

func (e *Exporter) collectAOF(ch chan<- prometheus.Metric, info map[string]string) {
	e.emitInt(ch, info, "aof_submitted", e.descs.aofSubmitted, prometheus.GaugeValue)
	e.emitInt(ch, info, "aof_written", e.descs.aofWritten, prometheus.GaugeValue)
	e.emitInt(ch, info, "aof_fatal", e.descs.aofFatal, prometheus.GaugeValue)
	e.emitInt(ch, info, "aof_outstanding", e.descs.aofOutstanding, prometheus.GaugeValue)
	e.emitUint(ch, info, "aof_batch_count", e.descs.aofBatchCount, prometheus.CounterValue)
	e.emitUint(ch, info, "aof_error_count", e.descs.aofErrorCount, prometheus.CounterValue)
	if raw, ok := info["aof_batch_us_total"]; ok {
		if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
			ch <- prometheus.MustNewConstMetric(e.descs.aofBatchDur, prometheus.CounterValue, float64(v)/1e6)
		} else {
			e.noteParseError()
		}
	}
	if raw, ok := info["aof_max_batch_age_us"]; ok {
		if v, err := strconv.ParseFloat(raw, 64); err == nil {
			ch <- prometheus.MustNewConstMetric(e.descs.aofMaxBatchAge, prometheus.GaugeValue, v/1e6)
		} else {
			e.noteParseError()
		}
	}
	// INFO 中为毫秒，负数表示“无副本/无 ACK”，归一为 0 秒避免误告警。
	if raw, ok := info["replica_min_ack_age_ms"]; ok {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			e.noteParseError()
		} else {
			if v < 0 {
				v = 0
			}
			ch <- prometheus.MustNewConstMetric(e.descs.replAckAge, prometheus.GaugeValue, float64(v)/1000.0)
		}
	}
}

func (e *Exporter) collectProxy(ch chan<- prometheus.Metric, info map[string]string) {
	e.emitUint(ch, info, "ebpf_proxy_cache_bytes", e.descs.proxyCacheBytes, prometheus.GaugeValue)
}

func (e *Exporter) collectReplicaSlots(ch chan<- prometheus.Metric, info map[string]string) {
	type slotStat struct {
		applied, durable uint64
		ackAgeMS         int64
		transport        int64
		seen             bool
	}
	slots := make(map[int]*slotStat)
	for k, v := range info {
		rest, ok := strings.CutPrefix(k, "replica_slot_")
		if !ok {
			continue
		}
		idxPart, field, ok := strings.Cut(rest, "_")
		if !ok {
			continue
		}
		idx, err := strconv.Atoi(idxPart)
		if err != nil || idx < 0 || idx >= 64 {
			e.noteParseError()
			continue
		}
		st := slots[idx]
		if st == nil {
			st = &slotStat{}
			slots[idx] = st
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			e.noteParseError()
			continue
		}
		st.seen = true
		switch field {
		case "applied":
			st.applied = uint64(n)
		case "durable":
			st.durable = uint64(n)
		case "ack_age_ms":
			st.ackAgeMS = n
		case "transport":
			st.transport = n
		}
	}
	for idx, st := range slots {
		if !st.seen {
			continue
		}
		label := strconv.Itoa(idx)
		ch <- prometheus.MustNewConstMetric(e.descs.replSlotApplied, prometheus.GaugeValue, float64(st.applied), label)
		ch <- prometheus.MustNewConstMetric(e.descs.replSlotDurable, prometheus.GaugeValue, float64(st.durable), label)
		age := st.ackAgeMS
		if age < 0 {
			age = 0
		}
		ch <- prometheus.MustNewConstMetric(e.descs.replSlotAckAge, prometheus.GaugeValue, float64(age)/1000.0, label)
		ch <- prometheus.MustNewConstMetric(e.descs.replSlotTransport, prometheus.GaugeValue, 1, label, transportStateName(st.transport))
	}
}

func transportStateName(kind int64) string {
	switch kind {
	case 1:
		return "tcp"
	case 2:
		return "rdma"
	case 3:
		return "ebpf"
	case 4:
		return "kprobe-rdma"
	case 5:
		return "ebpf-tcp"
	default:
		return "unknown"
	}
}
