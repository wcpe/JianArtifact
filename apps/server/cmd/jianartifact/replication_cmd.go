package main

import (
	"fmt"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// replicationCmd 分发 replication 子命令：status / start / stop（FR-86，FR-88 改造）。
// 对端配置（URL/令牌）存 setting 表（web 可配置，FR-88），本命令只管理自动同步开关并展示状态。
func replicationCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：jianartifact replication <status|start|stop>")
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载配置：%w", err)
	}
	svc, err := openServices(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = svc.db.Close() }()

	switch args[0] {
	case "status":
		return replicationStatus(svc)
	case "start":
		return setReplicationEnabled(svc, true)
	case "stop":
		return setReplicationEnabled(svc, false)
	default:
		return fmt.Errorf("未知 replication 子命令：%s（支持 status / start / stop）", args[0])
	}
}

// replicationStatus 打印复制对端配置与同步状态（FR-86，FR-88：对端自 setting 读取）。
func replicationStatus(svc *appServices) error {
	settings := repository.NewSettingRepo(svc.db)
	st := svc.replSvc.ClusterStatus()

	fmt.Printf("节点 ID：%s\n", st.NodeID)
	if st.PeerURL == "" {
		fmt.Println("复制对端：未配置（可在 web 管理端「集群」页配置）")
	} else {
		fmt.Printf("复制对端：%s\n", st.PeerURL)
		fmt.Printf("同步令牌：%s\n", boolLabel(st.TokenSet))
	}
	fmt.Printf("自动同步：%s\n", boolLabel(st.Enabled))

	if st.PeerURL != "" {
		if st.HasWatermark {
			fmt.Printf("对端水位：%d\n", st.Watermark)
		} else {
			fmt.Println("对端水位：未同步（启用自动同步或手动立即同步后开始）")
		}
	}
	if st.LastSyncAt != "" {
		fmt.Printf("最近同步：%s\n", st.LastSyncAt)
	} else {
		fmt.Println("最近同步：从未")
	}
	if st.LastError != "" {
		fmt.Printf("最近错误：%s\n", st.LastError)
	}
	_ = settings
	return nil
}

// setReplicationEnabled 设置自动同步开关（FR-86）。
func setReplicationEnabled(svc *appServices, enabled bool) error {
	v := "false"
	action := "停用"
	if enabled {
		v = "true"
		action = "启用"
	}
	if err := repository.NewSettingRepo(svc.db).Set(domain.SettingKeyReplEnabled, v); err != nil {
		return fmt.Errorf("写入同步开关：%w", err)
	}
	fmt.Printf("已%s自动同步（生效于服务进程下一轮轮询）。\n", action)
	return nil
}

// boolLabel 把布尔值转为「是 / 否」中文标签。
func boolLabel(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
