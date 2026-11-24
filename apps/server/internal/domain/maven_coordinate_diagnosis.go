package domain

// 本文件实现跨仓库的**同坐标诊断**。
//
// 要解决的问题（来自一次真实事故的复盘）：
//   同一个 Maven 坐标 `group:artifact:version` 同时存在于 releases 与 snapshots 两个
//   仓库里，聚合仓按成员顺序把 releases 那份旧制品先返回，客户端拿到的是旧内容——
//   而两份文件大小只差十几 KB，靠肉眼与"能下载成功"完全发现不了。
//
// 关键事实：**变换 group 的成员顺序不能解决它**，只能把"哪个被遮蔽"换到另一边。
// 真正要做的是让这个坐标只指向正确的那个来源，而前提是先**看得见**重复。
//
// 因此这里不做任何自动修复，只做两件事：
//   1. 列出该坐标在哪些仓库存在、各自的文件数与字节；
//   2. 给出**内容指纹**（按路径排序后对 blob 哈希求摘要），
//      让"两份内容相同"与"两份内容不同"一眼可分——前者是无害的冗余，
//      后者才是会出事故的遮蔽。

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// CoordinateDuplicateSource 是该坐标在某个仓库里的存在情况。
type CoordinateDuplicateSource struct {
	Repository string `json:"repository"`
	Type       string `json:"type"`
	// FileCount / Bytes 是该版本目录下的文件数与字节合计。
	FileCount int   `json:"fileCount"`
	Bytes     int64 `json:"bytes"`
	// Fingerprint 是内容指纹：按 path 排序后对「path + blobHash + size」求 sha256。
	// 两份指纹相同 = 内容与布局完全一致（无害冗余）；不同 = 真遮蔽风险。
	Fingerprint string `json:"fingerprint"`
	// SnapshotBuilds 仅对 SNAPSHOT 版本给出：该仓里有哪几次时间戳构建。
	SnapshotBuilds []string `json:"snapshotBuilds,omitempty"`
	// HasLiteralSnapshot 表示存在字面 `-SNAPSHOT` 文件（Gradle 直接发布形态）。
	// 它没有时间戳，是不可复现的覆盖写，通常是应被清理的那个。
	HasLiteralSnapshot bool `json:"hasLiteralSnapshot"`
}

// CoordinateDuplicateReport 是同坐标诊断报告。
type CoordinateDuplicateReport struct {
	Coordinate MavenVersionCoordinate      `json:"coordinate"`
	Sources    []CoordinateDuplicateSource `json:"sources"`
	// Identical 表示所有来源的内容指纹一致（纯冗余，删除任一副本都安全）。
	Identical bool `json:"identical"`
	// DistinctFingerprints 是不同指纹的数量；>1 表示存在真遮蔽风险。
	DistinctFingerprints int `json:"distinctFingerprints"`
	// Warning 是给运维的自然语言结论，避免用户自己从数字里推导。
	Warning string `json:"warning,omitempty"`
}

// DiagnoseMavenCoordinate 扫描全部 Maven hosted 仓库，报告该坐标的重复情况。
func (s *AssetService) DiagnoseMavenCoordinate(coord MavenVersionCoordinate) (*CoordinateDuplicateReport, error) {
	if err := coord.Validate(); err != nil {
		return nil, err
	}
	// Maven 仓库数量是个位数到几十，一次全取比逐页翻更简单也更快；
	// 上限 500 与仓库管理的分页上限一致，不会漏掉可管理的仓库。
	repos, err := s.repos.List(500, 0)
	if err != nil {
		return nil, err
	}
	report := &CoordinateDuplicateReport{Coordinate: coord}
	seenFingerprints := make(map[string]struct{})

	for i := range repos {
		repo := &repos[i]
		// 只看 Maven hosted：proxy 仓的"存在"取决于上游当时状态，
		// group 仓本身不持有文件（它的内容来自成员），把这两类算进来会让
		// 报告出现"同一份内容被数了三次"的假重复。
		if repo.Format != "maven" || repo.Type != "hosted" {
			continue
		}
		plan, planErr := s.planMavenVersion(repo, coord)
		if planErr != nil || !plan.Exists {
			continue
		}
		source := CoordinateDuplicateSource{
			Repository:         repo.Name,
			Type:               repo.Type,
			FileCount:          plan.TotalFiles,
			Bytes:              plan.TotalBytes,
			Fingerprint:        mavenVersionFingerprint(plan),
			HasLiteralSnapshot: plan.LiteralSnapshotFiles > 0,
		}
		for _, build := range plan.SnapshotBuilds {
			source.SnapshotBuilds = append(source.SnapshotBuilds, build.BuildID)
		}
		report.Sources = append(report.Sources, source)
		seenFingerprints[source.Fingerprint] = struct{}{}
	}

	report.DistinctFingerprints = len(seenFingerprints)
	report.Identical = len(seenFingerprints) <= 1
	if len(report.Sources) > 1 && !report.Identical {
		report.Warning = "同一坐标在多个仓库存在且内容不同：聚合仓会按成员顺序遮蔽其中一份，" +
			"客户端可能长期拿到旧内容。请确认哪个来源正确并清理另一个。"
	}
	return report, nil
}

// mavenVersionFingerprint 计算版本目录的内容指纹。
//
// 组成项必须包含 path：同一份 blob 摆在不同路径下是不同的布局，
// 而 Maven 客户端是按路径取文件的，路径不同就不能算"内容相同"。
// 按 path 排序后再累加，保证指纹与遍历顺序无关。
func mavenVersionFingerprint(plan *MavenVersionPlan) string {
	files := make([]MavenVersionFile, len(plan.Files))
	copy(files, plan.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	h := sha256.New()
	for _, file := range files {
		// 用 \x00 分隔，避免"路径尾 + 哈希头"拼接产生歧义碰撞。
		h.Write([]byte(file.Path))
		h.Write([]byte{0})
		h.Write([]byte(file.BlobHash))
		h.Write([]byte{0})
		h.Write([]byte(strconv.FormatInt(file.Size, 10)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ListMavenHostedRepos 返回可用于版本清理的仓库（maven + hosted）。
func (s *AssetService) ListMavenHostedRepos() ([]repository.Repository, error) {
	repos, err := s.repos.List(500, 0)
	if err != nil {
		return nil, err
	}
	out := make([]repository.Repository, 0, len(repos))
	for _, repo := range repos {
		if repo.Format == "maven" && repo.Type == "hosted" {
			out = append(out, repo)
		}
	}
	return out, nil
}
