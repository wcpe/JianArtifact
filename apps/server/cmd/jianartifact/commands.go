package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/wcpe/jianartifact/apps/server/internal/api"
	"github.com/wcpe/jianartifact/apps/server/internal/config"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// adminCmd 分发 admin 子命令：reset / backfill-checksums。
func adminCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：jianartifact admin <reset|backfill-checksums> [参数]")
	}
	switch args[0] {
	case "reset":
		return adminReset(args[1:])
	case "backfill-checksums":
		return adminBackfillChecksums(args[1:])
	default:
		return fmt.Errorf("未知 admin 子命令：%s（支持 reset / backfill-checksums）", args[0])
	}
}

// adminBackfillChecksums 离线回填 asset 表中缺失的 sha1/md5：从内容寻址 blob 流式计算并写库。
// --batch 控制单批上限；--all 循环直到无剩余（或本批无更新）。
func adminBackfillChecksums(args []string) error {
	fs := flag.NewFlagSet("admin backfill-checksums", flag.ContinueOnError)
	batch := fs.Int("batch", 500, "单批最多处理条数")
	all := fs.Bool("all", false, "循环处理直至无剩余")
	if err := fs.Parse(args); err != nil {
		return err
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

	totalUpdated, totalSkipped := 0, 0
	for {
		res, err := svc.assetSvc.BackfillChecksums(*batch)
		if err != nil {
			return fmt.Errorf("回填校验和：%w", err)
		}
		totalUpdated += res.Updated
		totalSkipped += res.Skipped
		fmt.Printf("本批：扫描 %d，更新 %d，跳过 %d，剩余 %d\n",
			res.Scanned, res.Updated, res.Skipped, res.Remaining)
		if !*all || res.Remaining == 0 || res.Scanned == 0 {
			break
		}
		// 本批全跳过且仍有剩余 → 避免死循环（blob 缺失无法补）
		if res.Updated == 0 {
			fmt.Println("本批无成功更新，停止（可能 blob 缺失）。")
			break
		}
	}
	fmt.Printf("合计：更新 %d，跳过 %d。\n", totalUpdated, totalSkipped)
	return nil
}

// adminReset 离线直连 SQLite 重置 / 创建管理员账号与口令：
// 该用户名不存在则以 admin 角色创建；已存在则重置口令并确保 admin / active。
// 未提供 --password 时交互式安全录入（不回显）。用于账号锁死后的恢复。
func adminReset(args []string) error {
	fs := flag.NewFlagSet("admin reset", flag.ContinueOnError)
	username := fs.String("username", "admin", "管理员用户名")
	password := fs.String("password", "", "管理员口令；留空则交互式安全录入")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pw := *password
	if pw == "" {
		entered, err := readPasswordTwice()
		if err != nil {
			return err
		}
		pw = entered
	}
	if pw == "" {
		return errors.New("口令不能为空")
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

	existing, err := svc.users.GetByUsername(*username)
	switch {
	case err == nil:
		if err := svc.userSvc.ChangePassword(existing.ID, pw); err != nil {
			return fmt.Errorf("重置口令：%w", err)
		}
		if _, err := svc.userSvc.Update(existing.ID, "admin", "active"); err != nil {
			return fmt.Errorf("恢复管理员角色 / 状态：%w", err)
		}
		fmt.Printf("已重置管理员 %q 的口令。\n", *username)
	case errors.Is(err, repository.ErrNotFound):
		if _, err := svc.userSvc.Create(*username, pw, "admin"); err != nil {
			return fmt.Errorf("创建管理员：%w", err)
		}
		fmt.Printf("已创建管理员 %q。\n", *username)
	default:
		return fmt.Errorf("查询用户：%w", err)
	}
	return nil
}

// readPasswordTwice 从终端安全读取口令两次并校验一致（不回显）。
func readPasswordTwice() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("非交互式终端，请通过 --password 提供口令")
	}
	fmt.Fprint(os.Stderr, "请输入新口令：")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("读取口令：%w", err)
	}
	fmt.Fprint(os.Stderr, "请再次输入：")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("读取口令：%w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("两次输入的口令不一致")
	}
	return string(first), nil
}

// statusCmd 打印运行时 / 静态状态：服务在跑则在线探测 /api/v1/status，
// 否则回退输出版本与解析后的配置（data 目录、DB 路径、监听地址、迁移版本）。
func statusCmd() error {
	if probeOnlineStatus() {
		return nil
	}
	return printOfflineStatus()
}

// probeOnlineStatus 尝试在线探测本地 /api/v1/status；成功打印并返回 true。
func probeOnlineStatus() bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(statusURL())
	if err != nil {
		return false
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var info api.StatusInfo
	if err := decodeJSON(resp.Body, &info); err != nil {
		return false
	}
	fmt.Println("状态：运行中")
	fmt.Printf("  版本：%s\n", info.Version)
	fmt.Printf("  就绪：%t\n", info.Ready)
	fmt.Printf("  已初始化：%t\n", info.Initialized)
	fmt.Printf("  迁移版本：%s\n", info.MigrationVersion)
	fmt.Printf("  用户数：%d\n", info.UserCount)
	return true
}

// printOfflineStatus 输出版本与解析后的配置（服务未运行时）。
func printOfflineStatus() error {
	fmt.Println("状态：未运行（离线）")
	fmt.Printf("  版本：%s\n", version)
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("加载配置：%w", err)
	}
	fmt.Printf("  data 目录：%s\n", cfg.DataDir)
	fmt.Printf("  DB 路径：%s\n", cfg.DBPath)
	fmt.Printf("  监听地址：%s\n", cfg.HTTPAddr)

	if db, err := openServices(cfg); err == nil {
		defer func() { _ = db.db.Close() }()
		if v, err := db.db.CurrentVersion(); err == nil {
			fmt.Printf("  迁移版本：%s\n", v)
		}
		if n, err := db.userSvc.Count(); err == nil {
			fmt.Printf("  用户数：%d\n", n)
		}
	}
	return nil
}

// statusURL 组装本地状态探测地址。
func statusURL() string {
	addr := listenAddr()
	host, port := "127.0.0.1", "8080"
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if addr[:i] != "" {
			host = addr[:i]
		}
		if addr[i+1:] != "" {
			port = addr[i+1:]
		}
	}
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s/api/v1/status", host, port)
}

// decodeJSON 解析 JSON 响应体到 dst。
func decodeJSON(r io.Reader, dst any) error {
	return json.NewDecoder(bufio.NewReader(r)).Decode(dst)
}
