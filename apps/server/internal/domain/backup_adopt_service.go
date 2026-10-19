package domain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wcpe/jianartifact/apps/server/internal/archive"
	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// 未管理备份包（FR-40 追加八）。
//
// # 为什么需要
//
// `${JIAN_DATA_DIR}/backups/` 里的包体与数据库登记是两套独立的持久化：包体是文件，
// 登记在 SQLite。二者可能失步——文件在、登记没了。历史实例上真实出现过这种残留
// （不经过 BackupService.Delete 的删除路径，或早期版本直接改库）。这些包在界面上
// 完全不可见：列表读的是登记表，于是磁盘被占着却既不能用也不能删。
//
// # 与孤儿回收的分工
//
// 「未管理」不等于「该删」。本服务把它们列出来交给用户判断：要的导入（重建登记），
// 不要的交给 backup-orphan-cleanup 按宽限期回收。两者必须都基于同一份差集计算，
// 否则会出现"刚导入的包被判定为孤儿删掉"的竞态。
//
// # 护栏
//
//   - 只处理 `<id>.tar.gz`；`.index` 作为附属跟随包体，`.tar.gz.snap` 是生成中临时
//     文件（由生成流程的 defer 自清理），一律不碰。
//   - 包标识一律以**归档内 manifest.packageId** 为准，而不是文件名。文件名可以被
//     人工改名，manifest 才是包的真实身份；两者不一致时以 manifest 为准并在结果里
//     标出，避免出现"登记了一个名字、实际是另一个包"的错配。
//   - 非法包（读不出 manifest / Validate 失败）不导入，但如实报告原因。
type BackupAdoptService struct {
	backups *BackupService
	repo    *repository.BackupPackageRepo
	now     func() time.Time
}

// NewBackupAdoptService 装配未管理包服务。
func NewBackupAdoptService(backups *BackupService, repo *repository.BackupPackageRepo) *BackupAdoptService {
	return &BackupAdoptService{
		backups: backups,
		repo:    repo,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// UnmanagedBackup 是一个"磁盘上有文件、登记里没有"的候选包。
type UnmanagedBackup struct {
	// FileID 是**文件名**推导出的标识（等价于磁盘上的 <FileID>.tar.gz）。
	// 界面用它定位条目；真正导入时仍以归档内 manifest 为准。
	FileID string `json:"fileId"`
	// PackageID 是归档内 manifest 声明的标识；非法包为空。
	PackageID string `json:"packageId"`
	// SizeBytes 是包体文件的字节数。
	SizeBytes int64 `json:"sizeBytes"`
	// ModifiedAt 是包体文件的最后修改时间（RFC3339，UTC）。
	ModifiedAt string `json:"modifiedAt"`
	// HasSidecar 表示同目录下是否存在 `<FileID>.index`。
	HasSidecar bool `json:"hasSidecar"`
	// Valid 表示归档能被打开且 manifest 通过 Validate。
	Valid bool `json:"valid"`
	// Reason 是 Valid=false 时的原因；Valid=true 时为空。
	Reason string `json:"reason,omitempty"`
	// Renamed 表示文件名与 manifest.packageId 不一致（导入将以 manifest 为准）。
	Renamed bool `json:"renamed"`
}

// AdoptedBackup 是单个包的导入结果。
type AdoptedBackup struct {
	// FileID 是请求里给的文件标识（界面回显用）。
	FileID string `json:"fileId"`
	// PackageID 是最终登记的标识。
	PackageID string `json:"packageId"`
	// Adopted 表示是否成功建立登记。
	Adopted bool `json:"adopted"`
	// Skipped 表示未执行导入（已登记、非法包等），Reason 说明原因。
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason,omitempty"`
}

// AdoptResult 汇总一次批量导入。
type AdoptResult struct {
	Items      []AdoptedBackup `json:"items"`
	Adopted    int             `json:"adopted"`
	Skipped    int             `json:"skipped"`
	Failed     int             `json:"failed"`
	Candidates int             `json:"-"`
}

// Summary 输出中文摘要（作业与日志用）。
func (r AdoptResult) Summary() string {
	return fmt.Sprintf("候选 %d 个 / 导入 %d 个 / 跳过 %d 个 / 失败 %d 个",
		r.Candidates, r.Adopted, r.Skipped, r.Failed)
}

// backupSidecarSuffix / backupPackageSuffix 与 BackupService 的路径约定保持一致。
const (
	backupPackageSuffix = ".tar.gz"
	backupSidecarSuffix = ".index"
	// backupSnapshotSuffix 是生成中的一致性快照临时后缀，清理与扫描都要跳过。
	backupSnapshotSuffix = ".snap"
)

// Scan 列出所有未管理的备份包（磁盘有包体、登记里无同 id 记录）。
//
// 已登记的包不进入结果：它们正常出现在列表里，重复暴露只会让用户困惑。
func (s *BackupAdoptService) Scan() ([]UnmanagedBackup, error) {
	if s == nil || s.backups == nil || s.repo == nil {
		return nil, nil
	}
	entries, err := os.ReadDir(s.backups.Dir())
	if err != nil {
		if os.IsNotExist(err) {
			// 数据目录尚未创建（全新实例）：没有候选是正常状态，不是错误。
			return []UnmanagedBackup{}, nil
		}
		return nil, fmt.Errorf("读取备份目录：%w", err)
	}

	registered, err := s.registeredIDs()
	if err != nil {
		return nil, err
	}

	// 先建好同名 .index 的集合，便于标注 HasSidecar。
	sidecars := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), backupSidecarSuffix) {
			continue
		}
		sidecars[strings.TrimSuffix(e.Name(), backupSidecarSuffix)] = struct{}{}
	}

	out := make([]UnmanagedBackup, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// 只认 <id>.tar.gz；快照临时文件与侧车不在候选范围。
		if !strings.HasSuffix(name, backupPackageSuffix) || strings.HasSuffix(name, backupSnapshotSuffix) {
			continue
		}
		fileID := strings.TrimSuffix(name, backupPackageSuffix)
		if fileID == "" {
			continue
		}
		// 以文件名为准先筛掉已登记的同名包；manifest 改名的情况在下面单独处理。
		if _, ok := registered[fileID]; ok {
			continue
		}

		item := UnmanagedBackup{FileID: fileID}
		if info, statErr := e.Info(); statErr == nil {
			item.SizeBytes = info.Size()
			item.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
		}
		if _, ok := sidecars[fileID]; ok {
			item.HasSidecar = true
		}

		m, ok := s.backups.readPackageFile(fileID)
		if !ok {
			item.Valid = false
			item.Reason = "归档无法打开或不是本产品的节点备份包"
			out = append(out, item)
			continue
		}
		item.Valid = true
		item.PackageID = m.PackageID
		item.Renamed = m.PackageID != fileID
		// 文件名与 manifest 不同但 manifest 的 id 已登记：这不是新包，跳过。
		if _, already := registered[m.PackageID]; already {
			continue
		}
		out = append(out, item)
	}

	// 稳定排序：大包优先无意义，按文件名（内含时间戳）升序最可预期。
	sort.Slice(out, func(i, j int) bool { return out[i].FileID < out[j].FileID })
	return out, nil
}

// Adopt 把指定的未管理包导入登记表。
//
// 单个包失败不中断整批：返回的结果逐项标注，调用方按 Items 展示。
// 已登记的 id 计为 Skipped（幂等：重复导入同一批不会报错）。
func (s *BackupAdoptService) Adopt(fileIDs []string) (AdoptResult, error) {
	var result AdoptResult
	if s == nil || s.backups == nil || s.repo == nil {
		return result, nil
	}
	result.Candidates = len(fileIDs)
	if len(fileIDs) == 0 {
		return result, nil
	}

	registered, err := s.registeredIDs()
	if err != nil {
		return result, err
	}

	for _, fileID := range fileIDs {
		item := AdoptedBackup{FileID: fileID}
		// 文件名白名单：拒绝路径穿越（../）与空串。
		if !safeBackupFileID(fileID) {
			item.Skipped = true
			item.Reason = "非法的包标识"
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		if _, ok := registered[fileID]; ok {
			item.PackageID = fileID
			item.Skipped = true
			item.Reason = "该包已在管理列表中"
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}

		m, ok := s.backups.readPackageFile(fileID)
		if !ok {
			item.Skipped = true
			item.Reason = "归档无法打开或不是本产品的节点备份包"
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		item.PackageID = m.PackageID
		if _, dup := registered[m.PackageID]; dup {
			item.Skipped = true
			item.Reason = "包内标识对应的包已在管理列表中"
			result.Skipped++
			result.Items = append(result.Items, item)
			continue
		}
		// 文件名与 manifest 不一致：需要把包体改名到 manifest 声明的 id，
		// 否则登记行指向一个不存在的路径（PackagePath 由 id 拼出）。
		if m.PackageID != fileID {
			if err := s.renamePackage(fileID, m.PackageID); err != nil {
				item.Skipped = true
				item.Reason = fmt.Sprintf("包内标识与文件名不一致且改名失败：%v", err)
				result.Failed++
				result.Items = append(result.Items, item)
				continue
			}
		}

		if err := s.register(&m); err != nil {
			item.Reason = err.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			continue
		}
		// 侧车缺失时补写：后续生成差包需要它，缺了会让该包无法作为基线。
		if err := s.ensureSidecar(m.PackageID); err != nil {
			// 侧车失败不回滚登记：包已可用（校验/演练/恢复都不依赖侧车），
			// 只是暂时不能作为差包基线。如实记载但不判为导入失败。
			item.Reason = fmt.Sprintf("已导入，但补写侧车索引失败：%v", err)
		}
		item.Adopted = true
		result.Adopted++
		registered[m.PackageID] = struct{}{}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

// registeredIDs 翻页取回全部已登记标识。
//
// 不能复用 ListRetentionCandidates：它写死了 origin='auto' AND status='done'，
// 拿它做差集会把手动包与生成中的包全部误判为"未管理"。
func (s *BackupAdoptService) registeredIDs() (map[string]struct{}, error) {
	const pageSize = 500
	ids := make(map[string]struct{})
	for offset := 0; ; offset += pageSize {
		items, err := s.repo.List(pageSize, offset)
		if err != nil {
			return nil, fmt.Errorf("读取备份登记：%w", err)
		}
		for _, it := range items {
			ids[it.PackageID] = struct{}{}
		}
		if len(items) < pageSize {
			break
		}
	}
	return ids, nil
}

// register 以归档内 manifest 为依据补建登记行（未管理包导入用）。
func (s *BackupAdoptService) register(m *archive.Manifest) error {
	return s.registerWith(m, adoptLabel)
}

// registerWith 以归档内 manifest 为依据补建登记行，备注由调用方指定。
//
// status 固定 done、origin 固定 manual：这个包已经完整落盘，把"来源"记成自动会让
// 保留策略把它当成周期备份淘汰掉——手工放进来/手工导入的包必须由人决定去留。
func (s *BackupAdoptService) registerWith(m *archive.Manifest, label string) error {
	item := repository.BackupPackage{
		PackageID:       m.PackageID,
		Mode:            string(m.Mode),
		BasePackageID:   m.BasePackageID,
		Status:          repository.BackupStatusDone,
		Label:           label,
		SizeBytes:       packageSize(s.backups.PackagePath(m.PackageID)),
		NodeID:          m.NodeID,
		AppVersion:      m.AppVersion,
		DBSchemaVersion: m.DBSchemaVersion,
		CreatedAt:       m.CreatedAt.UTC().Format(time.RFC3339Nano),
		FinishedAt:      strPtr(m.CreatedAt.UTC().Format(time.RFC3339Nano)),
		Origin:          repository.BackupOriginManual,
	}
	return s.repo.Create(item)
}

// adoptLabel 是导入包的默认备注，让用户在列表里一眼看出它从哪来。
const adoptLabel = "导入的未管理包"

// uploadLabel 是上传入库的默认备注，与"导入"区分开：一个来自本机运维，一个来自浏览器。
const uploadLabel = "上传的备份包"

// AdoptUploadedPackage 是一次上传入库的结果。
type AdoptUploadedPackage struct {
	// PackageID 是最终登记的包标识（取自归档内 manifest）。
	PackageID string
	// Blobs 是包内 blob 数量。
	Blobs int
}

// AdoptUploaded 把一次分片上传组装出来的归档搬进备份列表并登记（FR-40 追加八）。
//
// 与 Adopt 的区别：来源归档不在 backups/ 下（在 backup-uploads/<id>/package.tar.gz），
// 且不需要"未管理"判定——它还没被登记过。因此这里做的是：校验 manifest → 搬运到
// backups/<packageId>.tar.gz → 补侧车 → 登记行。
//
// 同名包已存在时**明确拒绝**而不是覆盖：覆盖会让一个已登记、可能正在被引用的包
// 在用户不知情的情况下被换掉，那比上传失败危险得多。
func (s *BackupAdoptService) AdoptUploaded(sourcePath string) (AdoptUploadedPackage, error) {
	if s == nil || s.backups == nil || s.repo == nil {
		return AdoptUploadedPackage{}, fmt.Errorf("导入服务未启用")
	}
	_, m, err := archive.Open(sourcePath)
	if err != nil {
		return AdoptUploadedPackage{}, fmt.Errorf("%w：归档无法打开（%v）", ErrValidation, err)
	}
	if err := m.Validate(); err != nil {
		return AdoptUploadedPackage{}, fmt.Errorf("%w：%v", ErrValidation, err)
	}
	if _, err := s.repo.Get(m.PackageID); err == nil {
		return AdoptUploadedPackage{}, fmt.Errorf("%w：备份列表中已存在同名包 %s", ErrValidation, m.PackageID)
	} else if !errors.Is(err, repository.ErrNotFound) {
		return AdoptUploadedPackage{}, fmt.Errorf("查询备份登记：%w", err)
	}

	// 归档搬进 backups/：用 rename 同盘移动；跨设备时退化为复制+删除。
	// 目录可能尚不存在（全新实例第一次走上传通道时 backups/ 还没被 prepare 创建），
	// 因此先确保存在——否则 moveFile 会以"找不到路径"失败。
	if err := os.MkdirAll(s.backups.Dir(), 0o750); err != nil {
		return AdoptUploadedPackage{}, fmt.Errorf("创建备份目录：%w", err)
	}
	dst := s.backups.PackagePath(m.PackageID)
	if err := moveFile(sourcePath, dst); err != nil {
		return AdoptUploadedPackage{}, fmt.Errorf("保存备份包：%w", err)
	}
	// 侧车必须与包体一起就位：它把"算基线 blob 集合"从扫描整包降到读索引，
	// 缺了会让这个包无法作为差包基线。
	if idx, idxErr := s.backups.readPackageBlobIndex(m.PackageID); idxErr == nil {
		if err := s.backups.writeSidecar(m.PackageID, idx); err != nil {
			return AdoptUploadedPackage{}, fmt.Errorf("写侧车索引：%w", err)
		}
	}
	if err := s.registerWith(&m, uploadLabel); err != nil {
		// 登记失败则把刚搬进来的文件撤掉，避免留下一个"未管理"的新孤儿。
		_ = os.Remove(dst)
		_ = os.Remove(s.backups.SidecarPath(m.PackageID))
		return AdoptUploadedPackage{}, err
	}
	return AdoptUploadedPackage{PackageID: m.PackageID, Blobs: m.Blobs.Count}, nil
}

// moveFile 优先用 rename（同盘零拷贝）；跨设备时退回"复制后删源"。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	_ = os.Remove(src)
	return nil
}

// ensureSidecar 在侧车缺失时用归档内的 blobs.index 补写。
func (s *BackupAdoptService) ensureSidecar(packageID string) error {
	if _, err := os.Stat(s.backups.SidecarPath(packageID)); err == nil {
		return nil
	}
	idx, err := s.backups.readPackageBlobIndex(packageID)
	if err != nil {
		return err
	}
	return s.backups.writeSidecar(packageID, idx)
}

// renamePackage 把包体与侧车改名到 manifest 声明的 id。
func (s *BackupAdoptService) renamePackage(from, to string) error {
	src := filepath.Join(s.backups.Dir(), from+backupPackageSuffix)
	dst := s.backups.PackagePath(to)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("目标位置已存在同名包 %s", to)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("改名包体：%w", err)
	}
	oldSidecar := filepath.Join(s.backups.Dir(), from+backupSidecarSuffix)
	if _, err := os.Stat(oldSidecar); err == nil {
		if err := os.Rename(oldSidecar, s.backups.SidecarPath(to)); err != nil {
			// 侧车改名失败不影响包体，记录后继续（ensureSidecar 会补）。
			return nil
		}
	}
	return nil
}

// safeBackupFileID 拒绝路径分隔符与控制字符，防止越出备份目录。
func safeBackupFileID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return false
	}
	return !strings.ContainsAny(id, "\x00\n\r\t")
}

// packageSize 返回包体字节数；读不到返回 0（登记仍可完成，不影响可用性）。
func packageSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func strPtr(s string) *string { return &s }
