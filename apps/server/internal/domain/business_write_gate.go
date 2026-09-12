package domain

// BusinessWriteGate 在领域写路径执行持久化前决定是否允许本地业务写入。
// 复制退役后不再有角色派生的静态只读栅栏；nil 表示允许写入。
// 冻结窗口（FR-135）由 FreezeController 实现本接口并统一承载"停写"。
type BusinessWriteGate interface {
	RequireBusinessWrite() error
}

func requireBusinessWrite(gate BusinessWriteGate) error {
	if gate == nil {
		return nil
	}
	return gate.RequireBusinessWrite()
}
