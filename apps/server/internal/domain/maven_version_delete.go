package domain

// 本文件实现 Maven 版本级**软删除**：把版本目录下的资产移入资产回收站，
// 保留 blob 但让它们从读/搜索/配额视图中消失，可随时恢复。
//
// 四条必须守住的口径：
//
//  1. **blob 绝不在这里释放**。软删除只动元数据；blob 的存活由引用计数决定
//     （见 AssetMutationRepo.CountBlobReferences，它已把回收站引用算作活引用）。
//     任何"顺手清一下 blob"的优化都会让恢复拿到一条指向空内容的资产行。
//
//  2. **写入回收站与从 asset 表删除必须在同一事务**。否则中间崩溃会留下
//     "元数据没了、回收站也没有"的孤儿状态——blob 仍占盘但既不可见也不可恢复。
//     实现上用 MutationCompletionHook（在删除之后、提交之前于同一 tx 内执行）。
//
//  3. **分批提交，不受单次 500 条上限约束**。SNAPSHOT 版本累积几十个历史构建后
//     文件数轻易过千；沿用 maxAssetOperationItems 会让用户永远删不掉最该删的版本。
//     每批自身是一个完整的原子操作，批与批之间不保证原子性——
//     这是有意的取舍：宁可"删了 800/1000 并能看到已完成的部分"，
//     也不要"一个都删不掉"，且残留状态可见、可继续删、可恢复。
//
//  4. **先预览后执行，两者共用同一份计划**。DeleteMavenVersion 内部重新计算计划后，
//     用调用方回传的路径做**子集校验**：不在计划里的路径导致整批拒绝，
//     不做部分信任——否则前端一个 bug 就能删掉用户没看见的文件。

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// MavenVersionDeleteResult 是一次版本级软删除的结果。
type MavenVersionDeleteResult struct {
	OperationID string `json:"operationId"`
	Repository  string `json:"repository"`
	// Trashed 是移入回收站的文件数，Batches 是实际提交的批次数。
	Trashed int   `json:"trashed"`
	Batches int   `json:"batches"`
	Bytes   int64 `json:"bytes"`
	// ArchivePath 是整版删除后被清理的目录（用于提示"该版本目录已空"）。
	ArchivePath string `json:"archivePath"`
}

// ErrAssetTrashUnavailable 表示资产回收站未装配，版本级软删除不可用。
var ErrAssetTrashUnavailable = errors.New("资产回收站未启用，无法执行可恢复的版本删除")

// PlanMavenVersion 返回某个 Maven 坐标版本的完整清理计划（预览用）。
func (s *AssetService) PlanMavenVersion(repoName string, coord MavenVersionCoordinate) (*MavenVersionPlan, error) {
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, fmt.Errorf("%w（当前为 %s 仓库）", ErrRepositoryNotWritable, repo.Type)
	}
	if repo.Format != "maven" {
		return nil, fmt.Errorf("%w: 仅 Maven 仓库支持版本级清理", ErrOperationUnsupported)
	}
	return s.planMavenVersion(repo, coord)
}

// DeleteMavenVersion 把一个 Maven 坐标版本移入资产回收站。
//
// paths 为调用方（前端）确认过的待删路径集合；为空表示"删掉计划里的全部文件"。
// 非空时必须是计划路径的子集，否则整批拒绝（见文件头口径 4）。
func (s *AssetService) DeleteMavenVersion(repoName string, coord MavenVersionCoordinate, paths []string, actor string) (*MavenVersionDeleteResult, error) {
	if err := s.requireBusinessWrite(); err != nil {
		return nil, err
	}
	if s.assetTrash == nil {
		return nil, ErrAssetTrashUnavailable
	}
	if s.mutator == nil {
		return nil, fmt.Errorf("%w: 资产事务引擎未就绪", ErrConflict)
	}
	repo, err := s.repos.GetByName(repoName)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if repo.Type != "hosted" {
		return nil, fmt.Errorf("%w（当前为 %s 仓库）", ErrRepositoryNotWritable, repo.Type)
	}
	if repo.Format != "maven" {
		return nil, fmt.Errorf("%w: 仅 Maven 仓库支持版本级清理", ErrOperationUnsupported)
	}

	plan, err := s.planMavenVersion(repo, coord)
	if err != nil {
		return nil, err
	}
	if !plan.Exists {
		return nil, ErrMavenVersionEmpty
	}

	selected, metadataToRebuild := selectMavenPlanFiles(plan)
	if len(selected) == 0 {
		return nil, ErrMavenVersionEmpty
	}

	// 子集校验：前端只允许删掉它在预览里看见的文件。
	if len(paths) > 0 {
		allowed := make(map[string]struct{}, len(selected))
		for _, file := range selected {
			allowed[file.Path] = struct{}{}
		}
		for _, p := range paths {
			if _, ok := allowed[p]; !ok {
				return nil, fmt.Errorf("%w: 路径 %s 不在该版本的清理计划内", ErrValidation, p)
			}
		}
		// 按前端确认的子集收敛（保持计划顺序，便于结果可复现）。
		confirmed := make(map[string]struct{}, len(paths))
		for _, p := range paths {
			confirmed[p] = struct{}{}
		}
		narrowed := make([]MavenVersionFile, 0, len(paths))
		for _, file := range selected {
			if _, ok := confirmed[file.Path]; ok {
				narrowed = append(narrowed, file)
			}
		}
		selected = narrowed
	}

	operationID := newOperationID()
	result := &MavenVersionDeleteResult{
		OperationID: operationID,
		Repository:  repoName,
		ArchivePath: plan.Directory,
	}

	// 分批提交：每批是一个独立原子操作。
	for start := 0; start < len(selected); start += maxAssetOperationItems {
		end := start + maxAssetOperationItems
		if end > len(selected) {
			end = len(selected)
		}
		batch := selected[start:end]
		if err := s.trashMavenBatch(repo, coord, batch, operationID, actor); err != nil {
			// 已提交的批次保留（可见、可恢复、可继续删），只上报失败。
			return result, err
		}
		result.Trashed += len(batch)
		result.Batches++
		for _, file := range batch {
			result.Bytes += file.Size
		}
	}

	// 只删单个构建时，版本级 metadata 必须按剩余构建重写：
	// 留着旧 metadata 会让客户端反复请求已删除的构建文件。
	if len(metadataToRebuild) > 0 && len(selected) > 0 {
		if err := s.rebuildSnapshotMetadataAfterBuildDelete(repo, coord, metadataToRebuild); err != nil {
			return result, err
		}
	}

	return result, nil
}

// trashMavenBatch 把一批文件原子地移入回收站：同一事务内先记快照，再从 asset 表删除。
func (s *AssetService) trashMavenBatch(repo *repository.Repository, coord MavenVersionCoordinate, batch []MavenVersionFile, operationID, actor string) error {
	items := make([]repository.AssetMutationItem, 0, len(batch))
	entries := make([]repository.AssetTrashEntry, 0, len(batch))
	scope := repository.AssetTrashScopeVersion
	if coord.BuildID != "" {
		scope = repository.AssetTrashScopeSnapshotBuild
	}
	for _, file := range batch {
		// 必须读库取**完整**资产作为前镜像，而不是从计划里拼：
		// content_type / sha1 / md5 在计划里没有取值，用空字符串填充会让恢复出的制品
		// 校验和全空、类型退化——用户点的是"恢复"，不是一个"内容碰巧相同"的新文件。
		// 同一份快照同时用于事务内的并发校验与回收站记录，两者必须一致。
		before, err := s.assets.GetByPath(repo.ID, file.Path)
		if err != nil {
			return mapNotFound(err)
		}
		items = append(items, repository.AssetMutationItem{
			RepositoryID: repo.ID,
			Path:         file.Path,
			Before:       before,
		})
		entries = append(entries, repository.AssetTrashEntry{
			RepositoryID: repo.ID,
			Path:         before.Path,
			BlobHash:     before.BlobHash,
			Size:         before.Size,
			ContentType:  before.ContentType,
			Sha1:         before.Sha1,
			Md5:          before.Md5,
			CreatedAt:    before.CreatedAt,
			UpdatedAt:    before.UpdatedAt,
			Scope:        scope,
			Actor:        actor,
			OperationID:  operationID,
		})
	}

	// 钩子在删除之后、提交之前于同一事务内执行：把快照写入回收站。
	// 任一步失败整体回滚，不会留下"删了但没进回收站"的孤儿。
	hook := func(tx *sqlx.Tx) error {
		return repository.InsertAssetTrashStaged(tx, entries)
	}
	// 必须走 KeepBlobs 变体：常规路径会在删除前把"不再被引用"的 blob 搬进隔离区，
	// 而软删除的定义正是"保留内容以便恢复"。走常规路径会让恢复拿到指向隔离区的资产行。
	if _, err := s.mutator.ApplyWithOperationIDAndHookKeepBlobs(operationID, items, hook); err != nil {
		return err
	}
	return nil
}

// rebuildSnapshotMetadataAfterBuildDelete 在删除某次 SNAPSHOT 构建后重写版本级 metadata。
//
// 这里刻意**不缩小 metadata 的语义**：版本级 maven-metadata.xml 描述的是"该 SNAPSHOT
// 版本当前指向哪次构建"（<snapshot><timestamp><buildNumber>），不是历史构建列表。
// 因此只有当被删的这次构建正是 metadata 当前所指时才移除这些文件——
// 若 metadata 指向的是仍存在的另一次构建，删掉它会让客户端连"当前构建"都解析不到，
// 原 -SNAPSHOT 路径对全部剩余构建一起 404。
func (s *AssetService) rebuildSnapshotMetadataAfterBuildDelete(repo *repository.Repository, coord MavenVersionCoordinate, metadata []MavenVersionFile) error {
	pointed := s.snapshotMetadataPointedBuild(repo, metadata)
	if pointed != "" && pointed != coord.BuildID {
		// metadata 指向的是另一次仍然存在的构建：保持原样，不动 metadata。
		return nil
	}
	paths := make([]string, 0, len(metadata))
	for _, file := range metadata {
		paths = append(paths, file.Path)
	}
	return s.deleteAssetsDeadOn(repo, paths)
}

// snapshotMetadataPointedBuild 读取版本级 maven-metadata.xml 里 <snapshot> 指向的
// <timestamp>-<buildNumber>。文件缺失、读取失败或字段不完整都返回空串，
// 调用方按"无法证伪"处理（保守维持原删除语义，不留下指向已删构建的指针）。
func (s *AssetService) snapshotMetadataPointedBuild(repo *repository.Repository, metadata []MavenVersionFile) string {
	for _, file := range metadata {
		if !strings.HasSuffix(file.Path, "maven-metadata.xml") {
			continue
		}
		asset, err := s.assets.GetByPath(repo.ID, file.Path)
		if err != nil || asset == nil {
			continue
		}
		rc, _, err := s.blobs.Open(asset.BlobHash)
		if err != nil {
			continue
		}
		// 流式解析而非整体读入：超大伪造 metadata 不再撑爆内存（限长兜底 CPU），
		// 指向存活构建的合法大文件也不会被误判成「指向不明」而删掉。
		ts, bn := parseSnapshotPointedBuild(io.LimitReader(rc, 8<<20))
		_ = rc.Close()
		if ts == "" || bn == "" {
			continue
		}
		return ts + "-" + bn
	}
	return ""
}

// parseSnapshotPointedBuild 流式读出 maven-metadata.xml 里 versioning>snapshot 的
// timestamp 与 buildNumber，读到两个值立即返回。输入可能被限长截断或损坏，
// 此时返回已解析到的部分（可能为空串），由调用方决定跳过。
func parseSnapshotPointedBuild(r io.Reader) (timestamp, buildNumber string) {
	dec := xml.NewDecoder(r)
	var depth []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return timestamp, buildNumber
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth = append(depth, t.Name.Local)
		case xml.EndElement:
			if len(depth) > 0 {
				depth = depth[:len(depth)-1]
			}
		case xml.CharData:
			if len(depth) == 4 && depth[1] == "versioning" && depth[2] == "snapshot" {
				switch depth[3] {
				case "timestamp":
					if timestamp == "" {
						timestamp = strings.TrimSpace(string(t))
					}
				case "buildNumber":
					if buildNumber == "" {
						buildNumber = strings.TrimSpace(string(t))
					}
				}
			}
		}
		if timestamp != "" && buildNumber != "" {
			return timestamp, buildNumber
		}
	}
}

// deleteAssetsDeadOn 硬删除一批**派生文件**（如 metadata），不进回收站。
//
// 之所以不进回收站：这些文件的内容由其它文件派生而来，恢复它们没有独立意义——
// 真正的数据在构建文件身上，而那几个已经在回收站里了。把派生文件也塞进回收站
// 会让「恢复」需要用户手工挑选正确组合，反而更容易恢复出不一致的状态。
func (s *AssetService) deleteAssetsDeadOn(repo *repository.Repository, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	items := make([]repository.AssetMutationItem, 0, len(paths))
	for _, p := range paths {
		// 重新读取当前值作为前镜像：这些文件可能在本轮删除前已被别的操作改动。
		current, err := s.assets.GetByPath(repo.ID, p)
		if err != nil {
			continue
		}
		items = append(items, repository.AssetMutationItem{
			RepositoryID: repo.ID,
			Path:         p,
			Before:       current,
		})
	}
	if len(items) == 0 {
		return nil
	}
	_, err := s.mutator.ApplyWithOperationID(newOperationID(), items)
	return err
}
