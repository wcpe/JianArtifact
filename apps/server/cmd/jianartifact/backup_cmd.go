package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/domain"
	"golang.org/x/term"
)

// backupLinkTTL 约束与 Web 端 /link 保持一致，避免 CLI 签出超长有效期链接。
const (
	backupLinkCLIMinTTL = time.Minute
	backupLinkCLIMaxTTL = 24 * time.Hour
)

// backupCmd 执行备份包离线运维子命令。
func backupCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：jianartifact backup <create|list|verify|link|delete|import>")
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
	case "create":
		return backupCreate(svc, args[1:])
	case "list":
		return backupList(svc, args[1:])
	case "verify":
		return backupVerify(svc, args[1:])
	case "link":
		return backupLink(svc, cfg, args[1:])
	case "delete":
		return backupDelete(svc, args[1:])
	case "import":
		return backupImport(svc, cfg, args[1:])
	default:
		return fmt.Errorf("未知 backup 子命令：%s（支持 create / list / verify / link / delete / import）", args[0])
	}
}

// backupCreate 生成一个备份包并登记。
func backupCreate(svc *appServices, args []string) error {
	mode := optionalOption(args, "--mode")
	if mode == "" {
		mode = string(archive.ModeHot)
	}
	if mode != string(archive.ModeHot) && mode != string(archive.ModeFrozen) {
		return fmt.Errorf("--mode 只能是 hot 或 frozen，实际 %q", mode)
	}
	if mode == string(archive.ModeFrozen) {
		fmt.Fprintln(os.Stderr, "提示：frozen 模式假定本地写入已停止。CLI 无法冻结运行中的服务；")
		fmt.Fprintln(os.Stderr, "      若服务仍在运行，请改用 Web 的冻结窗口，或先停止服务再执行本命令。")
	}

	base := optionalOption(args, "--base")
	if base != "" {
		fmt.Fprintf(os.Stderr, "提示：以基线包 %s 生成增量差包，只携带新增 blob，缩短传输窗口。\n", base)
	}

	started := time.Now()
	rec, err := svc.backupSvc.Generate(context.Background(), domain.CreateBackupOptions{
		Mode:          archive.Mode(mode),
		Label:         optionalOption(args, "--label"),
		BasePackageID: base,
	})
	if err != nil {
		return fmt.Errorf("生成备份包：%w", err)
	}
	fmt.Printf("已生成备份包 %s\n", rec.PackageID)
	fmt.Printf("  模式：%s\n", rec.Mode)
	if rec.BasePackageID != "" {
		fmt.Printf("  基线：%s（增量差包）\n", rec.BasePackageID)
	}
	fmt.Printf("  大小：%d 字节\n", rec.SizeBytes)
	fmt.Printf("  耗时：%s\n", time.Since(started).Round(time.Millisecond))
	fmt.Printf("  路径：%s\n", svc.backupSvc.PackagePath(rec.PackageID))
	return nil
}

// backupList 列出本机备份包。
func backupList(svc *appServices, args []string) error {
	items, err := svc.backupSvc.List(500, 0)
	if err != nil {
		return fmt.Errorf("读取备份列表：%w", err)
	}
	if flagPresent(args, "--json") {
		data, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return fmt.Errorf("序列化备份列表：%w", err)
		}
		fmt.Println(string(data))
		return nil
	}
	if len(items) == 0 {
		fmt.Println("暂无备份包。")
		return nil
	}
	fmt.Printf("%-34s %-6s %-12s %14s  %s\n", "包标识", "模式", "状态", "大小(字节)", "创建时间")
	for _, it := range items {
		summary := it.ErrorSummary
		if summary != "" {
			summary = "  " + summary
		}
		base := ""
		if it.BasePackageID != "" {
			base = "  基线=" + it.BasePackageID
		}
		fmt.Printf("%-34s %-6s %-12s %14d  %s%s%s\n", it.PackageID, it.Mode, it.Status, it.SizeBytes, it.CreatedAt, summary, base)
	}
	return nil
}

// backupVerify 校验备份包；target 可以是已登记的包标识，也可以是归档文件路径
// （用于校验通过其他通道拿到的包）。
func backupVerify(svc *appServices, args []string) error {
	target := positionalArg(args)
	if target == "" {
		return fmt.Errorf("用法：jianartifact backup verify <包标识|归档路径> [--deep]")
	}
	deep := flagPresent(args, "--deep")

	path := target
	if _, err := svc.backupSvc.Get(target); err == nil {
		path = svc.backupSvc.PackagePath(target)
	} else if _, statErr := os.Stat(target); statErr != nil {
		return fmt.Errorf("既不是已登记的包标识，也不是可读文件：%s", target)
	}

	reader, manifest, err := archive.Open(path)
	if err != nil {
		return fmt.Errorf("打开备份包：%w", err)
	}
	if err := reader.Verify(deep); err != nil {
		return fmt.Errorf("校验未通过：%w", err)
	}
	level := "快速（校验 db 与索引摘要）"
	if deep {
		level = "深度（逐 blob 比对内容摘要）"
	}
	fmt.Printf("校验通过：%s\n", manifest.PackageID)
	fmt.Printf("  校验强度：%s\n", level)
	fmt.Printf("  制品 %d 件，blob %d 个（%d 字节）\n",
		manifest.Counts.Assets, manifest.Blobs.Count, manifest.Blobs.TotalBytes)
	return nil
}

// backupLink 签发带时效的下载链接，供新机器直接拉取。
func backupLink(svc *appServices, cfg *config.Config, args []string) error {
	packageID := positionalArg(args)
	if packageID == "" {
		return fmt.Errorf("用法：jianartifact backup link <包标识> [--ttl 30m] [--base https://对外地址]")
	}
	if _, err := svc.backupSvc.Get(packageID); err != nil {
		return err
	}

	ttl := 30 * time.Minute
	if raw := optionalOption(args, "--ttl"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("--ttl 解析失败（示例 30m / 2h）：%w", err)
		}
		ttl = parsed
	}
	if ttl < backupLinkCLIMinTTL {
		ttl = backupLinkCLIMinTTL
	}
	if ttl > backupLinkCLIMaxTTL {
		ttl = backupLinkCLIMaxTTL
	}

	base := strings.TrimRight(optionalOption(args, "--base"), "/")
	if base == "" {
		base = strings.TrimRight(cfg.PublicURL, "/")
	}
	if base == "" {
		return fmt.Errorf("缺少对外基址：请用 --base 指定（例如 https://maven.example.com）")
	}

	exp := time.Now().Add(ttl)
	signer := domain.NewBackupLinkSigner(cfg.JWTSecret)
	fmt.Printf("%s/api/v1/backups/%s/download?exp=%d&token=%s\n",
		base, packageID, exp.Unix(), signer.Sign(packageID, exp))
	fmt.Fprintf(os.Stderr, "有效期至 %s（%s 后失效）\n", exp.Format(time.RFC3339), ttl)
	return nil
}

// backupDelete 删除备份包。
func backupDelete(svc *appServices, args []string) error {
	packageID := positionalArg(args)
	if packageID == "" {
		return fmt.Errorf("用法：jianartifact backup delete <包标识>")
	}
	if err := svc.backupSvc.Delete(packageID); err != nil {
		return err
	}
	fmt.Printf("已删除备份包 %s\n", packageID)
	return nil
}

// backupImport 导入一个节点备份包：校验 → 暂存 → 合并 blob → 写待生效标记，重启后生效。
func backupImport(svc *appServices, cfg *config.Config, args []string) error {
	src := positionalArg(args)
	if src == "" {
		return fmt.Errorf("用法：jianartifact backup import <归档路径> [--overwrite] [--deep] [--yes]")
	}
	overwrite := flagPresent(args, "--overwrite")
	deep := flagPresent(args, "--deep")
	yes := flagPresent(args, "--yes")

	restore := domain.NewRestoreService(svc.db, cfg.DataDir, cfg.DBPath, cfg.BlobDir)

	// 目标非空需二次确认：仅交互式终端且未显式 --yes 时提问；无 TTY 时 --overwrite 即视为已确认。
	targetNonEmpty, err := restore.TargetNonEmpty()
	if err != nil {
		return fmt.Errorf("检查目标实例：%w", err)
	}
	if targetNonEmpty {
		if !overwrite {
			return fmt.Errorf("目标实例非空，导入将覆盖现有数据；若确认覆盖请加 --overwrite")
		}
		if !yes && isInteractiveTerminal() {
			if !confirmOverwrite() {
				return fmt.Errorf("已取消：未确认覆盖目标实例")
			}
		}
	}

	res, err := restore.Stage(context.Background(), domain.RestoreRequest{
		SourcePath: src,
		Origin:     "cli",
		Operator:   "cli",
		Overwrite:  overwrite,
		Deep:       deep,
	})
	if err != nil {
		// 失败路径由 Stage 保证不留标记、不留暂存目录。
		return fmt.Errorf("导入备份包：%w", err)
	}

	fmt.Printf("已暂存备份包 %s，待重启生效\n", res.Manifest.PackageID)
	fmt.Printf("  制品 %d 件，blob 合并 %d / 跳过 %d\n", res.Manifest.Counts.Assets, res.BlobMerged, res.BlobSkipped)
	fmt.Printf("  暂存路径：%s\n", res.PendingPath)
	fmt.Fprintln(os.Stderr, "提示：需重启服务后生效（下次启动自动完成数据库原子替换，并保留 pre-restore 回滚备份）。")
	fmt.Fprintf(os.Stderr, "回滚备份将落于 %s/pre-restore-<UTC时间戳>/%s\n",
		cfg.DataDir, filepath.Base(cfg.DBPath))
	return nil
}

// isInteractiveTerminal 报告标准输入是否为交互式终端（TTY）。
func isInteractiveTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// confirmOverwrite 向终端询问是否覆盖非空目标；非 y/yes 均视为拒绝。
func confirmOverwrite() bool {
	fmt.Fprint(os.Stderr, "目标实例非空，确认覆盖？[y/N] ")
	var ans string
	if _, err := fmt.Scanln(&ans); err != nil {
		return false
	}
	ans = strings.ToLower(strings.TrimSpace(ans))
	return ans == "y" || ans == "yes"
}

// optionalOption 读取形如 --key value 的可选参数；缺省返回空串。
func optionalOption(args []string, key string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key {
			return strings.TrimSpace(args[index+1])
		}
	}
	return ""
}

// flagPresent 判断布尔开关是否出现。
func flagPresent(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

// positionalArg 返回第一个位置参数，跳过开关及其取值（--key value 形式）。
func positionalArg(args []string) string {
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(arg, "--") {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg
	}
	return ""
}
