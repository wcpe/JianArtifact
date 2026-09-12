// Command release-assets 生成 JianArtifact 的跨平台发布资产。
//
// 发布逻辑集中在 Go 工具中，供 Windows PowerShell、Unix shell、Make 和 Task
// 共用，避免 Windows 发布依赖 WSL 或与 CI 使用不同的目标清单。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type target struct {
	goos   string
	goarch string
}

var releaseTargets = []target{
	{goos: "linux", goarch: "amd64"},
	{goos: "linux", goarch: "arm64"},
	{goos: "windows", goarch: "amd64"},
	{goos: "darwin", goarch: "amd64"},
	{goos: "darwin", goarch: "arm64"},
}

func main() {
	root := flag.String("root", ".", "仓库根目录")
	version := flag.String("version", "", "发布版本；为空时读取 VERSION")
	output := flag.String("output", "dist/release", "发布资产输出目录")
	flag.Parse()

	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		fail(err)
	}
	versionValue := strings.TrimSpace(*version)
	if versionValue == "" {
		versionValue, err = readVersion(rootAbs)
		if err != nil {
			fail(err)
		}
	}
	if strings.ContainsAny(versionValue, `/\\`) || versionValue == "." || versionValue == ".." {
		fail(fmt.Errorf("版本号含非法路径字符"))
	}

	outputDir := *output
	if !filepath.IsAbs(outputDir) {
		outputDir = filepath.Join(rootAbs, outputDir)
	}
	if err := buildRelease(rootAbs, outputDir, versionValue); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "发布资产构建失败：", err)
	os.Exit(1)
}

func readVersion(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return "", fmt.Errorf("读取 VERSION：%w", err)
	}
	version := strings.TrimSpace(string(b))
	if version == "" {
		return "", errors.New("VERSION 不能为空")
	}
	return version, nil
}

func buildRelease(root, output, version string) error {
	if err := os.MkdirAll(output, 0o755); err != nil {
		return fmt.Errorf("创建发布目录：%w", err)
	}
	if err := run(root, "pnpm", "install", "--frozen-lockfile"); err != nil {
		return err
	}
	if err := run(root, "pnpm", "build"); err != nil {
		return err
	}

	webDist := filepath.Join(root, "apps", "server", "web", "dist")
	if err := os.RemoveAll(webDist); err != nil {
		return fmt.Errorf("清理内嵌前端目录：%w", err)
	}
	if err := copyDir(filepath.Join(root, "apps", "web", "dist"), webDist); err != nil {
		return fmt.Errorf("同步内嵌前端资源：%w", err)
	}

	ldflags := "-s -w -X main.version=" + version
	assetNames := make([]string, 0, len(releaseTargets))
	for _, target := range releaseTargets {
		name := fmt.Sprintf("jianartifact-%s-%s-%s", version, target.goos, target.goarch)
		if target.goos == "windows" {
			name += ".exe"
		}
		path := filepath.Join(output, name)
		if err := buildOne(root, path, target, ldflags); err != nil {
			return err
		}
		assetNames = append(assetNames, name)
	}
	return writeChecksums(output, assetNames)
}

func buildOne(root, output string, target target, ldflags string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", output, "./apps/server/cmd/jianartifact")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.goos, "GOARCH="+target.goarch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("构建 %s/%s：%s\n", target.goos, target.goarch, filepath.Base(output))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("构建 %s/%s：%w", target.goos, target.goarch, err)
	}
	return nil
}

func run(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("执行 %s：%w", name, err)
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if err := copyFile(path, target, info.Mode().Perm()); err != nil {
			return err
		}
		return nil
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writeChecksums(output string, names []string) error {
	var b strings.Builder
	for _, name := range names {
		path := filepath.Join(output, name)
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("打开发布资产 %s：%w", name, err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("计算 %s 校验和：%w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭发布资产 %s：%w", name, closeErr)
		}
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(hash.Sum(nil)), name)
	}
	if err := os.WriteFile(filepath.Join(output, "SHA256SUMS"), []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("写入 SHA256SUMS：%w", err)
	}
	return nil
}
