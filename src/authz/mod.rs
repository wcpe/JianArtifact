//! 授权判定（ADR-0004）：综合全局角色、仓库可见性与每仓库读写 ACL，
//! 对"某身份能否对某仓库执行读 / 写"给出放行 / 拒绝结论。
//!
//! 本模块核心是**纯函数** [`authorize`]：无副作用、不触 DB / IO，仅依据传入的
//! 身份与仓库视图判定，便于穷举测试（鉴权判定矩阵是本项目 #1 高风险区）。
//! ACL 的查库由上层（api）完成后装入 [`RepoView`]，本层只做判定。

use crate::auth::AuthIdentity;
use crate::meta::{Permission, Visibility};

/// 对仓库的操作类别。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Action {
    /// 读操作（下载 / 浏览 / 详情）。
    Read,
    /// 写操作（上传 / 删除）。
    Write,
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
/// `caller_can_read` / `caller_can_write` 表示**当前调用方**是否在该仓库的 ACL 中
/// 命中读 / 写授权（由上层据身份查 `repo_acl` 后填入）；匿名调用方两者恒为 false。
/// 写授权天然蕴含读授权——`from_permissions` 会据此把 write 也视为可读。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RepoView {
    /// 仓库可见性。
    pub visibility: Visibility,
    /// 调用方是否对该仓库有读权限（命中 read 或 write ACL）。
    pub caller_can_read: bool,
    /// 调用方是否对该仓库有写权限（命中 write ACL）。
    pub caller_can_write: bool,
}

impl RepoView {
    /// 据可见性与调用方在该仓库上的 ACL 权限集合构造视图。
    ///
    /// 写权限蕴含读权限：命中 write 即视为可读，符合"能写必能读"的常识。
    pub fn from_permissions(visibility: Visibility, perms: &[Permission]) -> Self {
        let can_write = perms.contains(&Permission::Write);
        // write 蕴含 read：有写必能读
        let can_read = can_write || perms.contains(&Permission::Read);
        Self {
            visibility,
            caller_can_read: can_read,
            caller_can_write: can_write,
        }
    }
}

/// 授权判定纯函数：综合全局角色 × 可见性 × 每仓库 ACL × 操作给出结论。
///
/// 规则（ADR-0004）：
/// - 管理员：对任意仓库读写一律放行。
/// - public 仓库：任意身份（含匿名）可读；写需命中写 ACL（或管理员）。
/// - private 仓库：仅命中读 / 写 ACL 的用户（或管理员）可读，命中写 ACL（或管理员）可写；
///   其余（含匿名、无 ACL 的登录用户）一律拒绝。
/// - 写操作必须命中写 ACL 或管理员；只有读权限不得越权写。
///
/// 无副作用、不触 DB / IO，可对全组合穷举测试。
pub fn authorize(identity: &AuthIdentity, repo: &RepoView, action: Action) -> Decision {
    // 管理员对任意仓库读写一律放行
    if identity.is_admin() {
        return Decision::Allow;
    }

    match action {
        Action::Read => authorize_read(identity, repo),
        Action::Write => authorize_write(identity, repo),
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

/// 写判定：须已认证且命中写 ACL（管理员已在上层放行）。
///
/// 显式要求 `is_authenticated`，是纵深防御——即便上层误把非空 ACL 装进匿名视图，
/// 也绝不放行匿名写，杜绝任何身份通道绕过写权限边界。
fn authorize_write(identity: &AuthIdentity, repo: &RepoView) -> Decision {
    if identity.is_authenticated() && repo.caller_can_write {
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
    }

    #[test]
    fn 管理员对任意仓库读写放行() {
        for vis in [Visibility::Public, Visibility::Private] {
            for action in [Action::Read, Action::Write] {
                // 管理员即便无任何 ACL 也放行
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
            // 真实链路中匿名的 RepoView ACL 必为空，这里直接断言判定层拒绝
            assert_eq!(authorize(&匿名(), &v, Action::Read), Decision::Deny);
            assert_eq!(authorize(&匿名(), &v, Action::Write), Decision::Deny);
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
                let has_read_acl = acl.contains(&Permission::Read) || acl.contains(&Permission::Write);
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
}
