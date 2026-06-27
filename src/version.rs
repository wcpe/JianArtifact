//! 二进制构建版本号解析（修复 prerelease 自更新版本号不收敛）。
//!
//! `CARGO_PKG_VERSION` 只含基线版本（如 `0.4.0`），预发布的 `dev.N.sha` 仅存在于 GitHub Release
//! 名 / 资产名、从未编进二进制——导致 prerelease 通道自更新后「当前版本」纹丝不动、`current` 恒为
//! `0.4.0`、与 `latest=0.4.0-dev.N.sha` 永远不相等而一直显示「有可用更新」，永不收敛。
//!
//! 修复：CI 发布时经环境变量 `JIANARTIFACT_BUILD_VERSION` 注入完整版本串（预发布
//! `{cargo版本}-dev.{run}.{sha}`、tag 取版本），编译期由 [`option_env!`] 读入；未注入（本地开发
//! 或 CI 测试任务）时回退 `CARGO_PKG_VERSION`，行为与修复前一致。全部「当前版本」展示点
//! （在线更新检查 / 应用、`/health`、设置页、clap `--version`）统一经 [`build_version`] 取值。

/// 在「CI 注入的完整版本」与「Cargo 基线版本」间择取（纯函数，可测）。
///
/// 注入值为 `Some` 且非空白时取其裁剪后的值；否则（`None` 或全空白）回退 Cargo 版本。
fn resolve_build_version<'a>(injected: Option<&'a str>, cargo: &'a str) -> &'a str {
    match injected {
        Some(v) if !v.trim().is_empty() => v.trim(),
        _ => cargo,
    }
}

/// 当前二进制的展示版本：优先 CI 注入的完整版本串，回退 `CARGO_PKG_VERSION`。
pub fn build_version() -> &'static str {
    resolve_build_version(
        option_env!("JIANARTIFACT_BUILD_VERSION"),
        env!("CARGO_PKG_VERSION"),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 注入非空时取注入的完整版本串() {
        // 复现 bug：预发布二进制须自报 CI 注入的 dev.N.sha，而非基线 0.4.0
        assert_eq!(
            resolve_build_version(Some("0.4.0-dev.8.4488ab2"), "0.4.0"),
            "0.4.0-dev.8.4488ab2"
        );
    }

    #[test]
    fn 未注入时回退_cargo_版本() {
        assert_eq!(resolve_build_version(None, "0.4.0"), "0.4.0");
    }

    #[test]
    fn 注入空白时回退_cargo_版本() {
        assert_eq!(resolve_build_version(Some(""), "0.4.0"), "0.4.0");
        assert_eq!(resolve_build_version(Some("  \n"), "0.4.0"), "0.4.0");
    }

    #[test]
    fn 注入值首尾空白被裁剪() {
        assert_eq!(
            resolve_build_version(Some(" 0.4.0-dev.9.abcdef0 \n"), "0.4.0"),
            "0.4.0-dev.9.abcdef0"
        );
    }

    #[test]
    fn 测试构建未注入环境时_build_version_等于_cargo_版本() {
        // CI 测试任务（ci.yml）不注入 JIANARTIFACT_BUILD_VERSION，应回退 CARGO_PKG_VERSION
        assert_eq!(build_version(), env!("CARGO_PKG_VERSION"));
    }
}
