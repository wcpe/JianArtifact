package archive

import "sort"

// BlobSetDiff 返回存在于 cur 但不在 base 中的 blob。
// 这是增量差包的核心：基线包已携带的 blob 无需重复传输。
func BlobSetDiff(base, cur BlobIndex) BlobIndex {
	baseSet := base.Hashes()
	out := make(BlobIndex, 0, len(cur))
	for _, ref := range cur {
		if _, ok := baseSet[ref.Hash]; ok {
			continue
		}
		out = append(out, ref)
	}
	return NewBlobIndex(out)
}

// MissingFrom 返回本索引中尚未存在于 have 的 blob。
// 导入端用它判断还缺哪些 blob 才能真正完成还原。
func (idx BlobIndex) MissingFrom(have BlobIndex) BlobIndex {
	return BlobSetDiff(have, idx)
}

// Summary 返回集合摘要（规模与索引 sha256）。索引按哈希升序渲染，故摘要与
// 输入顺序无关——同一集合无论求差先后都得到稳定摘要，是增量核对的基础。
func (idx BlobIndex) Summary() BlobSetSummary {
	return BlobSetSummary{
		Count:       len(idx),
		TotalBytes:  idx.TotalBytes(),
		IndexSHA256: idx.SHA256(),
	}
}

// Union 返回两个集合的并集：按哈希去重（同哈希保留任意一次出现），并对结果
// 确定性排序。输入顺序不影响输出，因此链式派生（差包之上再差包）的摘要稳定。
func (idx BlobIndex) Union(other BlobIndex) BlobIndex {
	seen := make(map[string]struct{}, len(idx)+len(other))
	out := make(BlobIndex, 0, len(idx)+len(other))
	add := func(refs BlobIndex) {
		for _, ref := range refs {
			if _, ok := seen[ref.Hash]; ok {
				continue
			}
			seen[ref.Hash] = struct{}{}
			out = append(out, ref)
		}
	}
	add(idx)
	add(other)
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}
