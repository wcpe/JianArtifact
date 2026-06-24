//! 授权判定（ADR-0004 + ADR-0007）：综合全局角色、仓库可见性与每仓库 ACL，
//! 对"某身份能否对某仓库执行某动作（读 / 写 / 删 / 管理）"给出放行 / 拒绝结论。
//!
//! 本模块核心是**纯函数** [`authorize`]：无副作用、不触 DB / IO，仅依据传入的
//! 身份与仓库视图判定，便于穷举测试（鉴权判定矩阵是本项目 #1 高风险区）。
//! ACL 的查库由上层（api）完成后装入 [`RepoView`]，本层只做判定。

use crate::auth::AuthIdentity;
use crate::meta::{Permission, Visibility};

/// 对仓库的操作类别（四级动作，FR-48 / ADR-0007）。
///
/// 动作自低到高为 Read < Write < Delete < Admin；判定时高动作蕴含低动作的能力。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Action {
    /// 读操作（下载 / 浏览 / 详情）。
    Read,
    /// 写操作（上传 / 发布 / 覆盖）。
    Write,
    /// 删除操作（删除制品 / 缓存）。
    Delete,
    /// 仓库级管理操作（配置 / 删除仓库 / 维护其 ACL）。
    Admin,
}

/// 授权判定结论。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Decision {
    /// 放行。
    Allow,
    /// 拒绝。
    Deny,
}

impl Decision {
    /// 是否放行。
    pub fn is_allowed(self) -> bool {
        matches!(self, Decision::Allow)
    }
}

/// 判定所需的仓库视图：可见性 + 调用方在该仓库上的 ACL 命中情况。
///
/// `caller_can_*` 表示**当前调用方**是否在该仓库的 ACL 中具备对应动作的能力（由上层据身份
/// 查 `repo_acl` 后填入）；匿名调用方四者恒为 false。
/// 四级动作存在蕴含关系（read < write < delete < admin）：高动作蕴含全部低动作——
/// `from_permissions` 据此把高动作授权下沉为低动作能力（如命中 admin 即四项能力全具备）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RepoView {
    /// 仓库可见性。
    pub visibility: Visibility,
    /// 调用方是否对该仓库有读能力（命中 read / write / delete / admin 任一 ACL）。
    pub caller_can_read: bool,
    /// 调用方是否对该仓库有写能力（命中 write / delete / admin 任一 ACL）。
    pub caller_can_write: bool,
    /// 调用方是否对该仓库有删除能力（命中 delete / admin ACL）。
    pub caller_can_delete: bool,
    /// 调用方是否对该仓库有仓库级管理能力（命中 admin ACL）。
    pub caller_can_admin: bool,
}

impl RepoView {
    /// 据可见性与调用方在该仓库上的 ACL 权限集合构造视图。
    ///
    /// 按动作蕴含关系（read < write < delete < admin）下沉能力：高动作蕴含全部低动作，
    /// 符合"能管理必能删、能删必能写、能写必能读"的常识。
    pub fn from_permissions(visibility: Visibility, perms: &[Permission]) -> Self {
        let has = |p: Permission| perms.contains(&p);
        // 自高到低逐级蕴含：admin ⊇ delete ⊇ write ⊇ read
        let can_admin = has(Permission::Admin);
        let can_delete = can_admin || has(Permission::Delete);
        let can_write = can_delete || has(Permission::Write);
        let can_read = can_write || has(Permission::Read);
        Self {
            visibility,
            caller_can_read: can_read,
            caller_can_write: can_write,
            caller_can_delete: can_delete,
            caller_can_admin: can_admin,
        }
    }
}

/// 授权判定纯函数：综合全局角色 × 可见性 × 每仓库 ACL × 操作给出结论。
///
/// 规则（ADR-0004 + ADR-0007 四级动作）：
/// - 全局管理员：对任意仓库任意动作一律放行。
/// - public 仓库：任意身份（含匿名）可读；写 / 删 / 管理需命中对应（或更高）动作 ACL。
/// - private 仓库：仅命中读（或更高）动作 ACL 的用户（或全局管理员）可读；
///   其余（含匿名、无 ACL 的登录用户）一律拒绝。
/// - 写 / 删 / 管理动作必须已认证且命中对应（或更高）动作 ACL 或为全局管理员；
///   低动作能力不得越权执行高动作（如仅读不得写，仅写不得删）。
///
/// 无副作用、不触 DB / IO，可对全组合穷举测试。
pub fn authorize(identity: &AuthIdentity, repo: &RepoView, action: Action) -> Decision {
    // 全局管理员对任意仓库任意动作一律放行
    if identity.is_admin() {
        return Decision::Allow;
    }

    match action {
        Action::Read => authorize_read(identity, repo),
        // 写 / 删 / 管理均为变更类动作：须已认证且具备对应（或更高）动作能力
        Action::Write => authorize_capability(identity, repo.caller_can_write),
        Action::Delete => authorize_capability(identity, repo.caller_can_delete),
        Action::Admin => authorize_capability(identity, repo.caller_can_admin),
    }
}

/// 读判定：public 任意可读；private 需命中读 / 写 ACL。
fn authorize_read(identity: &AuthIdentity, repo: &RepoView) -> Decision {
    match repo.visibility {
        // 公开仓库：匿名与任意登录用户均可读
        Visibility::Public => Decision::Allow,
        // 私有仓库：匿名一律拒绝；登录用户须命中读（或写蕴含读）ACL
        Visibility::Private => {
            if identity.is_authenticated() && repo.caller_can_read {
                Decision::Allow
            } else {
                Decision::Deny
            }
        }
    }
}

/// 变更类动作判定（写 / 删 / 管理）：须已认证且具备该动作能力（全局管理员已在上层放行）。
///
/// `has_capability` 为调用方对目标动作的能力（已含蕴含下沉，如命中 admin 则删 / 写能力均为 true）。
/// 显式要求 `is_authenticated`，是纵深防御——即便上层误把非空 ACL 装进匿名视图，
/// 也绝不放行匿名变更，杜绝任何身份通道绕过权限边界。
fn authorize_capability(identity: &AuthIdentity, has_capability: bool) -> Decision {
    if identity.is_authenticated() && has_capability {
        Decision::Allow
    } else {
        Decision::Deny
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::auth::AuthUser;
    use crate::meta::Role;

    /// 构造匿名身份。
    fn 匿名() -> AuthIdentity {
        AuthIdentity::Anonymous
    }

    /// 构造普通登录用户身份。
    fn 普通用户() -> AuthIdentity {
        AuthIdentity::Authenticated(AuthUser {
            user_id: "u1".to_string(),
            username: "u1".to_string(),
            role: Role::User,
        })
    }

    /// 构造管理员身份。
    fn 管理员() -> AuthIdentity {
        AuthIdentity::Authenticated(AuthUser {
            user_id: "a1".to_string(),
            username: "a1".to_string(),
            role: Role::Admin,
        })
    }

    /// 据可见性与 ACL 权限集合构造视图。
    fn 视图(vis: Visibility, perms: &[Permission]) -> RepoView {
        RepoView::from_permissions(vis, perms)
    }

    #[test]
    fn 写权限蕴含读权限() {
        let v = 视图(Visibility::Private, &[Permission::Write]);
        assert!(v.caller_can_read);
        assert!(v.caller_can_write);
        // 仅 write 不蕴含删 / 管理
        assert!(!v.caller_can_delete);
        assert!(!v.caller_can_admin);
    }

    /// 动作蕴含下沉：admin ⊇ delete ⊇ write ⊇ read，逐级断言能力具备与否。
    #[test]
    fn 四级动作蕴含下沉() {
        // 仅 read：只读，写 / 删 / 管理均无
        let r = 视图(Visibility::Private, &[Permission::Read]);
        assert!(
            r.caller_can_read && !r.caller_can_write && !r.caller_can_delete && !r.caller_can_admin
        );

        // delete：蕴含写与读，但不蕴含管理
        let d = 视图(Visibility::Private, &[Permission::Delete]);
        assert!(
            d.caller_can_read && d.caller_can_write && d.caller_can_delete && !d.caller_can_admin
        );

        // admin：四项能力全具备
        let a = 视图(Visibility::Private, &[Permission::Admin]);
        assert!(
            a.caller_can_read && a.caller_can_write && a.caller_can_delete && a.caller_can_admin
        );

        // 无任何 ACL：四项能力全无
        let none = 视图(Visibility::Private, &[]);
        assert!(
            !none.caller_can_read
                && !none.caller_can_write
                && !none.caller_can_delete
                && !none.caller_can_admin
        );
    }

    #[test]
    fn 管理员对任意仓库任意动作放行() {
        for vis in [Visibility::Public, Visibility::Private] {
            for action in [Action::Read, Action::Write, Action::Delete, Action::Admin] {
                // 全局管理员即便无任何 ACL 也放行
                let v = 视图(vis, &[]);
                assert_eq!(
                    authorize(&管理员(), &v, action),
                    Decision::Allow,
                    "管理员 {vis:?} {action:?} 应放行"
                );
            }
        }
    }

    #[test]
    fn 公开仓库匿名与任意登录用户可读() {
        let v = 视图(Visibility::Public, &[]);
        assert_eq!(authorize(&匿名(), &v, Action::Read), Decision::Allow);
        assert_eq!(authorize(&普通用户(), &v, Action::Read), Decision::Allow);
    }

    #[test]
    fn 公开仓库写需命中写_acl() {
        // 无写 ACL：匿名、仅读用户、无 ACL 用户写均拒绝
        let no_write = 视图(Visibility::Public, &[Permission::Read]);
        assert_eq!(authorize(&匿名(), &no_write, Action::Write), Decision::Deny);
        assert_eq!(
            authorize(&普通用户(), &no_write, Action::Write),
            Decision::Deny
        );
        // 命中写 ACL：放行
        let with_write = 视图(Visibility::Public, &[Permission::Write]);
        assert_eq!(
            authorize(&普通用户(), &with_write, Action::Write),
            Decision::Allow
        );
    }

    #[test]
    fn 私有仓库对匿名一律拒绝() {
        for perms in [
            vec![],
            vec![Permission::Read],
            vec![Permission::Write],
            vec![Permission::Read, Permission::Write],
        ] {
            let v = 视图(Visibility::Private, &perms);
            // 匿名永远拿不到 ACL（caller_can_* 据匿名身份恒 false），此处仅防御性穷举
            // 真实链路中匿名的 RepoView ACL 必为空，这里直接断言判定层对四级动作一律拒绝
            for action in [Action::Read, Action::Write, Action::Delete, Action::Admin] {
                assert_eq!(
                    authorize(&匿名(), &v, action),
                    Decision::Deny,
                    "私有仓库匿名 {action:?} 应拒绝"
                );
            }
        }
    }

    #[test]
    fn 私有仓库仅命中读_acl_可读不可写() {
        let v = 视图(Visibility::Private, &[Permission::Read]);
        assert_eq!(authorize(&普通用户(), &v, Action::Read), Decision::Allow);
        // 仅读不得越权写
        assert_eq!(authorize(&普通用户(), &v, Action::Write), Decision::Deny);
    }

    #[test]
    fn 私有仓库命中写_acl_可读可写() {
        let v = 视图(Visibility::Private, &[Permission::Write]);
        assert_eq!(authorize(&普通用户(), &v, Action::Read), Decision::Allow);
        assert_eq!(authorize(&普通用户(), &v, Action::Write), Decision::Allow);
    }

    #[test]
    fn 私有仓库无_acl_的登录用户读写均拒() {
        let v = 视图(Visibility::Private, &[]);
        assert_eq!(authorize(&普通用户(), &v, Action::Read), Decision::Deny);
        assert_eq!(authorize(&普通用户(), &v, Action::Write), Decision::Deny);
    }

    /// 鉴权判定矩阵全组合穷举：visibility × role × ACL × action 逐格断言。
    ///
    /// 这是 ADR-0004 与 testing-and-quality §2.1 要求的 #1 高风险区核心覆盖。
    #[test]
    fn 鉴权判定矩阵全组合() {
        // ACL 组合：none / read / write / both
        let acl_sets: [&[Permission]; 4] = [
            &[],
            &[Permission::Read],
            &[Permission::Write],
            &[Permission::Read, Permission::Write],
        ];

        for vis in [Visibility::Public, Visibility::Private] {
            for acl in acl_sets {
                let has_read_acl =
                    acl.contains(&Permission::Read) || acl.contains(&Permission::Write);
                let has_write_acl = acl.contains(&Permission::Write);

                // ---- 匿名：匿名永远不带 ACL，故视图按空 ACL 构造 ----
                let anon_view = 视图(vis, &[]);
                let expect_anon_read = matches!(vis, Visibility::Public);
                assert_eq!(
                    authorize(&匿名(), &anon_view, Action::Read).is_allowed(),
                    expect_anon_read,
                    "匿名 读 vis={vis:?}"
                );
                // 匿名写：任何可见性都拒
                assert_eq!(
                    authorize(&匿名(), &anon_view, Action::Write),
                    Decision::Deny,
                    "匿名 写 vis={vis:?}"
                );

                // ---- 普通用户：按 ACL 命中情况 ----
                let user_view = 视图(vis, acl);
                let expect_user_read = match vis {
                    // 公开：任意登录用户可读
                    Visibility::Public => true,
                    // 私有：须命中读（或写蕴含读）ACL
                    Visibility::Private => has_read_acl,
                };
                assert_eq!(
                    authorize(&普通用户(), &user_view, Action::Read).is_allowed(),
                    expect_user_read,
                    "普通用户 读 vis={vis:?} acl={acl:?}"
                );
                // 写：无论可见性，须命中写 ACL
                assert_eq!(
                    authorize(&普通用户(), &user_view, Action::Write).is_allowed(),
                    has_write_acl,
                    "普通用户 写 vis={vis:?} acl={acl:?}"
                );

                // ---- 管理员：读写一律放行 ----
                let admin_view = 视图(vis, acl);
                assert_eq!(
                    authorize(&管理员(), &admin_view, Action::Read),
                    Decision::Allow,
                    "管理员 读 vis={vis:?} acl={acl:?}"
                );
                assert_eq!(
                    authorize(&管理员(), &admin_view, Action::Write),
                    Decision::Allow,
                    "管理员 写 vis={vis:?} acl={acl:?}"
                );
            }
        }
    }

    /// 四级动作鉴权判定矩阵全组合穷举（FR-48 / ADR-0007）：
    /// visibility × 身份（匿名 / 普通用户 / 全局管理员） × ACL 全子集（4 动作的 16 种组合） × 4 动作。
    ///
    /// 期望结论按动作蕴含关系（read < write < delete < admin）独立推导，逐格断言放行 / 拒绝，
    /// 覆盖 testing-and-quality §2.1 鉴权矩阵在四级动作维度的全组合。
    #[test]
    fn 四级动作鉴权矩阵全组合() {
        let all = [
            Permission::Read,
            Permission::Write,
            Permission::Delete,
            Permission::Admin,
        ];
        let actions = [Action::Read, Action::Write, Action::Delete, Action::Admin];

        for vis in [Visibility::Public, Visibility::Private] {
            // 枚举 ACL 全子集：用 4 位掩码取 all 的子集（共 16 种）
            for mask in 0u8..16 {
                let acl: Vec<Permission> = all
                    .iter()
                    .enumerate()
                    .filter(|(i, _)| mask & (1 << i) != 0)
                    .map(|(_, p)| *p)
                    .collect();

                // 据蕴含关系独立推导各动作能力（与被测实现解耦，避免同源错误）
                let can_admin = acl.contains(&Permission::Admin);
                let can_delete = can_admin || acl.contains(&Permission::Delete);
                let can_write = can_delete || acl.contains(&Permission::Write);
                let can_read = can_write || acl.contains(&Permission::Read);
                let capability = |a: Action| match a {
                    Action::Read => can_read,
                    Action::Write => can_write,
                    Action::Delete => can_delete,
                    Action::Admin => can_admin,
                };

                let user_view = 视图(vis, &acl);
                let admin_view = 视图(vis, &acl);

                for action in actions {
                    // ---- 匿名：永远不带 ACL，公开仅可读，其余一律拒 ----
                    let anon_view = 视图(vis, &[]);
                    let expect_anon = matches!((vis, action), (Visibility::Public, Action::Read));
                    assert_eq!(
                        authorize(&匿名(), &anon_view, action).is_allowed(),
                        expect_anon,
                        "匿名 {action:?} vis={vis:?}"
                    );

                    // ---- 普通用户：读受可见性影响，变更类动作须具备对应（或更高）能力 ----
                    let expect_user = match action {
                        // 公开仓库任意登录用户可读；私有须具备读能力
                        Action::Read => matches!(vis, Visibility::Public) || can_read,
                        // 写 / 删 / 管理：与可见性无关，须具备对应能力
                        _ => capability(action),
                    };
                    assert_eq!(
                        authorize(&普通用户(), &user_view, action).is_allowed(),
                        expect_user,
                        "普通用户 {action:?} vis={vis:?} acl={acl:?}"
                    );

                    // ---- 全局管理员：任意动作一律放行 ----
                    assert_eq!(
                        authorize(&管理员(), &admin_view, action),
                        Decision::Allow,
                        "管理员 {action:?} vis={vis:?} acl={acl:?}"
                    );
                }
            }
        }
    }
}
