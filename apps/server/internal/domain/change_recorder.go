package domain

// ChangeRecorder 记录实体变更。
//
// 复制/集群退役（FR-138）后，repl_change 变更日志不再写入——它唯一的消费方是
// 已退役的复制拉取通道。接口与注入点保留，是为了避免为摘除而改动 9 个写路径
// 服务的字段与构造；具体实现换成了 NoopChangeRecorder。
//
// 注意：**原子 operation 信封不受影响**——asset_mutation 的 ApplyWithOutbox* 仍把
// 制品操作写入 replication_operation_outbox（同一事务），那是制品操作与审计的真源，
// 与本接口无关。
type ChangeRecorder interface {
	Record(entityType, entityKey, op string, data any) error
}

// NoopChangeRecorder 是复制退役后的空实现：不再产生 repl_change 记录。
type NoopChangeRecorder struct{}

// Record 什么都不做，恒返回 nil（写路径不能因记账失败而中断）。
func (NoopChangeRecorder) Record(string, string, string, any) error { return nil }
