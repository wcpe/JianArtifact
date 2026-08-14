package main

import (
	"fmt"

	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// replicationCmd 分发 replication 子命令：status / start / stop（FR-86）。
// 对端 URL 与令牌来自环境变量（JIAN_SYNC_PEER_URL / JIAN_SYNC_TOKEN），不在运行时编辑；
// 本命令管理同步调度启停开关（setting repl:enabled）并展示同步状态。
func replicationCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：jianartifact replication <status|start|stop>")
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载配置：%w", err)
	}
	if cfg.SyncPeerURL == "" {
		return fmt.Errorf("未配置复制对端（缺少环境变量 %s）；配置后本命令方可使用", config.EnvSyncPeerURL)
	}
	svc, err := openServices(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = svc.db.Close() }()

	switch args[0] {
	case "status":
		return replicationStatus(cfg, svc)
	case "start":
		return setReplicationEnabled(svc, true)
	case "stop":
		return setReplicationEnabled(svc, false)
	default:
		return fmt.Errorf("未知 replication 子命令：%s（支持 status / start / stop）", args[0])
	}
}

// replicationStatus 打印复制对端配置与同步状态（FR-86）。
func replicationStatus(cfg *config.Config, svc *appServices) error {
	settings := repository.NewSettingRepo(svc.db)

	fmt.Printf("节点 ID：%s\n", svc.replSvc.NodeID())
	fmt.Printf("复制对端：%s\n", cfg.SyncPeerURL)
	fmt.Printf("同步令牌：%s\n", boolLabel(cfg.SyncToken != ""))

	enabled, _ := settings.Get(domain.SettingKeyReplEnabled)
	fmt.Printf("同步调度：%s\n", boolLabel(enabled != "false"))

	watermark, _ := settings.Get(domain.ReplicationWatermarkKey(cfg.SyncPeerURL))
	if watermark == "" {
		fmt.Println("对端水位：未同步（首次启动将全量初始化）")
	} else {
		fmt.Printf("对端水位：%s\n", watermark)
	}

	lastSync, _ := settings.Get(domain.SettingKeyReplLastSync)
	if lastSync != "" {
		fmt.Printf("最近同步：%s\n", lastSync)
	} else {
		fmt.Println("最近同步：从未")
	}
	if lastErr, _ := settings.Get(domain.SettingKeyReplLastError); lastErr != "" {
		fmt.Printf("最近错误：%s\n", lastErr)
	}
	return nil
}

// setReplicationEnabled 设置同步调度启停开关（FR-86）。
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
	fmt.Printf("已%s复制同步调度（生效于服务进程下一轮轮询）。\n", action)
	return nil
}

// boolLabel 把布尔值转为「是 / 否」中文标签。
func boolLabel(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
