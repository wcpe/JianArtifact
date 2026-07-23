package discover

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BlobAsset 表示 Nexus blob store 中一项可迁移资产。
type BlobAsset struct {
	Repo      string
	Path      string // @BlobStore.blob-name（去前导 /）
	BytesPath string
}

// EnumProgress 枚举过程回调：found 为已解析的有效资产数，repo 为当前仓。
type EnumProgress func(found int64, repo string)

// EnumerateNexusBlobAssets 按仓库精确枚举 blob 资产（无进度回调）。
func EnumerateNexusBlobAssets(contentRoot string, repos []string) ([]BlobAsset, error) {
	return EnumerateNexusBlobAssetsWithProgress(context.Background(), contentRoot, repos, nil)
}

// EnumerateNexusBlobAssetsWithProgress 流式枚举：grep 每命中一行即解析并回调，避免长时间 0 进度。
// 跳过 deleted=true；repo 名必须完全相等（避免 r3d 匹配 r3d-mixed）。
func EnumerateNexusBlobAssetsWithProgress(ctx context.Context, contentRoot string, repos []string, onProg EnumProgress) ([]BlobAsset, error) {
	if contentRoot == "" || len(repos) == 0 {
		return nil, nil
	}
	allow := make(map[string]bool, len(repos))
	ordered := make([]string, 0, len(repos))
	for _, r := range repos {
		if r != "" && !allow[r] {
			allow[r] = true
			ordered = append(ordered, r)
		}
	}
	if len(allow) == 0 {
		return nil, nil
	}

	var out []BlobAsset
	var found int64
	lastProg := time.Time{}
	emit := func(repo string, force bool) {
		if onProg == nil {
			return
		}
		now := time.Now()
		if !force && !lastProg.IsZero() && now.Sub(lastProg) < 400*time.Millisecond {
			return
		}
		lastProg = now
		onProg(found, repo)
	}

	for _, repo := range ordered {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		emit(repo, true)

		// 优先：流式 grep，每行立刻解析（进度实时上涨）
		streamed, err := streamGrepAndCollect(ctx, contentRoot, repo, func(asset BlobAsset) {
			out = append(out, asset)
			found++
			emit(repo, false)
		})
		if err != nil {
			return out, err
		}
		if streamed {
			emit(repo, true)
			continue
		}

		// 回退 Walk：无 grep 或启动失败
		paths, err := walkPropertyFiles(ctx, contentRoot, repo)
		if err != nil {
			return out, err
		}
		for _, propPath := range paths {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			asset, ok := parseBlobAsset(propPath, repo)
			if !ok {
				continue
			}
			out = append(out, asset)
			found++
			emit(repo, false)
		}
		emit(repo, true)
	}
	return out, nil
}

// CountNexusBlobAssets 按仓统计可迁移资产数。
func CountNexusBlobAssets(contentRoot string, repo string) (int64, error) {
	items, err := EnumerateNexusBlobAssets(contentRoot, []string{repo})
	if err != nil {
		return 0, err
	}
	return int64(len(items)), nil
}

// streamGrepAndCollect 流式 grep；ok=false 表示应回退 Walk。
func streamGrepAndCollect(ctx context.Context, contentRoot, repo string, onHit func(BlobAsset)) (bool, error) {
	needle := "@Bucket.repo-name=" + repo
	cmd := exec.CommandContext(ctx, "grep", "-rl", "--include=*.properties", needle, contentRoot)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, nil
	}
	if err := cmd.Start(); err != nil {
		return false, nil
	}

	sc := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return true, err
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		asset, ok := parseBlobAsset(line, repo)
		if ok {
			onHit(asset)
		}
	}
	waitErr := cmd.Wait()
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			// 无匹配
			return true, nil
		}
		// 其它错误且一行都没读到 → 回退 Walk
		return false, nil
	}
	return true, sc.Err()
}

func parseBlobAsset(propPath, repo string) (BlobAsset, bool) {
	meta, err := readBlobProperties(propPath)
	if err != nil {
		return BlobAsset{}, false
	}
	if meta["@Bucket.repo-name"] != repo {
		return BlobAsset{}, false
	}
	if meta["deleted"] == "true" {
		return BlobAsset{}, false
	}
	blobName := meta["@BlobStore.blob-name"]
	if blobName == "" {
		return BlobAsset{}, false
	}
	bytesPath := strings.TrimSuffix(propPath, ".properties") + ".bytes"
	if _, err := os.Stat(bytesPath); err != nil {
		return BlobAsset{}, false
	}
	return BlobAsset{
		Repo:      repo,
		Path:      strings.TrimPrefix(blobName, "/"),
		BytesPath: bytesPath,
	}, true
}

func walkPropertyFiles(ctx context.Context, contentRoot, repo string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(contentRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(d.Name(), ".properties") {
			return nil
		}
		meta, err := readBlobProperties(path)
		if err != nil {
			return nil
		}
		if meta["@Bucket.repo-name"] != repo {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	return paths, err
}
