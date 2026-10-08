package api

import (
	"testing"

	"github.com/wcpe/jianartifact/apps/server/internal/repository"
)

// TestUserGroupAuditMapping 用户组审计动作的归类（FR-36）：
// 组操作的 target.kind 归到 user（与账号同族、不落 other），删组与改组成员属高危。
//
// 本阶段只备常量与映射（HTTP handler 在阶段三），此用例守住「映射已接线」这件事，
// 避免阶段三上线后组操作在审计看板上既认不出类型、也不进高危视图。
func TestUserGroupAuditMapping(t *testing.T) {
	t.Run("target kind", func(t *testing.T) {
		got := toAPIAuditTarget(repository.ObservabilityEvent{
			Action:     AuditActionGroupCreate,
			EntityType: EntityTypeUserGroup,
			EntityKey:  "release-team",
		})
		if got.Kind != AuditTargetUser {
			t.Errorf("组操作 target.kind = %q，期望 %q（不应落 other）", got.Kind, AuditTargetUser)
		}
		if got.Label != "release-team" {
			t.Errorf("组操作 label = %q，期望组名", got.Label)
		}
	})

	// 高危：删组与成员增删改变的是「授权面」，须与 acl.set 同级进入高危视图。
	highRisk := []string{
		AuditActionGroupDelete,
		AuditActionGroupMemberAdd,
		AuditActionGroupMemberRmv,
	}
	for _, action := range highRisk {
		if !isHighRisk(repository.ObservabilityEvent{Action: action, EntityType: EntityTypeUserGroup}) {
			t.Errorf("%s 应属高危动作", action)
		}
	}

	// 建组 / 改组名不进高危：它们本身不改变任何人的有效权限，
	// 把常规 CRUD 标成高危会让高危视图失去筛选价值。
	for _, action := range []string{AuditActionGroupCreate, AuditActionGroupUpdate} {
		if isHighRisk(repository.ObservabilityEvent{Action: action, EntityType: EntityTypeUserGroup}) {
			t.Errorf("%s 不应属高危动作", action)
		}
	}
}
