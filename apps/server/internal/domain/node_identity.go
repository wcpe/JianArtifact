package domain

import (
	"crypto/rand"
	"encoding/hex"
	"os"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// nodeIdentitySettingKey 是本节点唯一标识在 setting 表中的键。
// 与原 ReplicationService.NodeID 使用同一个键，保证既有部署的 nodeId 不变。
const nodeIdentitySettingKey = "node_id"

// NodeIdentity 提供本节点的稳定唯一标识。
//
// 复制退役后节点标识仍是备份包 manifest 与审计记录的必备字段，因此从
// ReplicationService 中独立出来。取值顺序（与退役前完全一致）：
//  1. 环境变量 JIAN_NODE_ID；
//  2. setting 表的 node_id；
//  3. 随机生成并持久化到 setting（失败不阻塞，下次调用重试）。
//
// 该值必须跨重启稳定：变更会让前后生成的备份包无法对账。
type NodeIdentity struct {
	settings *repository.SettingRepo
	cached   string
}

// NewNodeIdentity 装配节点标识服务。
func NewNodeIdentity(settings *repository.SettingRepo) *NodeIdentity {
	return &NodeIdentity{settings: settings}
}

// NodeID 返回本节点唯一标识（跨重启稳定）。
func (n *NodeIdentity) NodeID() string {
	if n.cached != "" {
		return n.cached
	}
	if id := os.Getenv("JIAN_NODE_ID"); id != "" {
		n.cached = id
		return id
	}
	if id, err := n.settings.Get(nodeIdentitySettingKey); err == nil && id != "" {
		n.cached = id
		return id
	}
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	id := hex.EncodeToString(raw[:])
	// 持久化失败不阻塞写路径（下次调用重试）；随机值保证并发下也唯一。
	_ = n.settings.Set(nodeIdentitySettingKey, id)
	n.cached = id
	return id
}
