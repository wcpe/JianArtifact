//! 可配置 WAF 规则引擎（FR-55，ADR-0008 的「可配置 WAF 规则引擎」部分）。
//!
//! 仅做应用层（L7）请求模式匹配与阻断：按有序规则对请求的 **method / path / query / 指定 header**
//! 做三类匹配——**字面（literal，子串包含）/ 通配（wildcard，`*`/`?`）/ 正则（regex）**，
//! **首个命中生效**：命中 `block` 即在进入业务前返回 `403`，命中 `allow` 即放行并短路后续规则。
//!
//! 设计要点（对齐 testing-and-quality §2.7）：
//! - **规则启动期编译一次**：正则在构造 [`WafRuleSet`] 时预编译、通配模式转译为锚定正则；热路径只做
//!   匹配、不重复编译。**非法规则记 WARN 跳过、不阻断启动**（坏规则不致命，其余规则照常生效）。
//! - **热路径低开销**：未启用（`enabled=false`）或空规则集走零开销快路径直接放行；启用时按序匹配，
//!   命中即短路，无锁、无 IO、无分配（仅取请求属性的 `&str` 做匹配）。
//! - **防误杀**：默认空规则集 + 关闭，不影响现有行为、不误杀正常包管理器请求；规则与启用由运维显式承担。
//! - **不依赖 IP**：WAF 按请求属性（method/path/query/header）匹配，与来源 IP 无关；不读 / 不采信 XFF
//!   做任何 IP 相关判定（与其他 L7 防护「不采信可伪造头做 IP 项」一致）。
//! - **配置即时生效**：规则集随 `AppState` 经 `Arc` 共享；配置热替换后重建规则集，下个请求即按新规则判定。

use axum::{
    extract::{Request, State},
    http::{HeaderMap, Method, StatusCode},
    middleware::Next,
    response::{IntoResponse, Response},
    Json,
};
use regex_lite::Regex;
use serde_json::json;

use crate::config::{WafConfig, WafRuleConfig};
use crate::metrics_keys as keys;

use super::alerts::ProtectionDimension;
use super::AppState;

/// 规则匹配的请求属性字段。
#[derive(Debug, Clone)]
enum WafField {
    /// 请求方法（如 `GET` / `POST`）。
    Method,
    /// 请求路径（不含查询串）。
    Path,
    /// 请求查询串（`?` 之后部分，无查询串时视作空串）。
    Query,
    /// 指定名称的请求头（名称大小写不敏感）；缺失该头时视作空串。
    Header(String),
}

/// 规则命中后的动作。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum WafAction {
    /// 阻断：返回 403，不进入业务。
    Block,
    /// 放行：短路后续规则，直接进入业务（用于给合法模式开豁免口子）。
    Allow,
}

/// 匹配器：把规则的 `pattern` + `match_type` 编译为可执行的匹配逻辑。
///
/// `Literal` 走子串包含（标准 WAF 字面匹配语义，便于匹配字段内的某段特征）；`Wildcard` 把
/// `*`/`?` 转译为锚定正则；`Regex` 直接用 `regex_lite::Regex`（非锚定，子串搜索）。
#[derive(Debug, Clone)]
enum Matcher {
    /// 字面子串包含匹配。
    Literal(String),
    /// 通配 / 正则匹配（通配模式构造时已转译为锚定正则）。
    Pattern(Regex),
}

impl Matcher {
    /// 对目标字符串求值是否命中。
    fn is_match(&self, target: &str) -> bool {
        match self {
            Matcher::Literal(s) => target.contains(s.as_str()),
            Matcher::Pattern(re) => re.is_match(target),
        }
    }
}

/// 编译后的单条规则：字段 + 匹配器 + 动作。
#[derive(Debug, Clone)]
struct CompiledRule {
    /// 匹配的请求属性字段。
    field: WafField,
    /// 字段值的匹配器。
    matcher: Matcher,
    /// 命中后的动作。
    action: WafAction,
}

impl CompiledRule {
    /// 从配置项编译一条规则；字段 / 匹配类型 / 动作非法或正则无法编译时返回 `None`（由调用方记 WARN 跳过）。
    fn compile(cfg: &WafRuleConfig) -> Option<Self> {
        let field = parse_field(cfg)?;
        let action = parse_action(&cfg.action)?;
        let matcher = parse_matcher(&cfg.match_type, &cfg.pattern)?;
        Some(Self {
            field,
            matcher,
            action,
        })
    }

    /// 按本规则字段从请求属性取值并匹配；命中返回其动作。
    fn evaluate(&self, method: &Method, path: &str, query: &str, headers: &HeaderMap) -> bool {
        let target = match &self.field {
            WafField::Method => method.as_str(),
            WafField::Path => path,
            WafField::Query => query,
            // 缺失该头视作空串参与匹配（不会误命中非空 pattern）
            WafField::Header(name) => headers
                .get(name.as_str())
                .and_then(|v| v.to_str().ok())
                .unwrap_or(""),
        };
        self.matcher.is_match(target)
    }
}

/// 编译后的 WAF 规则集：随 `AppState` 经 `Arc` 共享。
///
/// 构造时一次性编译全部规则（非法项记 WARN 跳过），热路径仅按序匹配。`enabled=false` 或规则为空时
/// [`Self::evaluate`] 恒返回 `None`（快路径放行）。
#[derive(Debug, Clone, Default)]
pub struct WafRuleSet {
    /// 是否启用（来自配置；空规则集即便启用也等同放行）。
    enabled: bool,
    /// 有序编译规则；按序匹配、首个命中生效。
    rules: Vec<CompiledRule>,
}

impl WafRuleSet {
    /// 从配置编译规则集：逐条编译，非法规则记 WARN 跳过、不阻断启动。
    pub fn from_config(cfg: &WafConfig) -> Self {
        let mut rules = Vec::with_capacity(cfg.rules.len());
        for (idx, rule) in cfg.rules.iter().enumerate() {
            match CompiledRule::compile(rule) {
                Some(c) => rules.push(c),
                None => tracing::warn!(
                    序号 = idx,
                    字段 = %rule.field,
                    匹配类型 = %rule.match_type,
                    动作 = %rule.action,
                    "WAF 规则非法，已跳过该条"
                ),
            }
        }
        Self {
            enabled: cfg.enabled,
            rules,
        }
    }

    /// 是否处于生效态（已启用且至少有一条有效规则）；否则热路径走零开销快路径。
    fn is_active(&self) -> bool {
        self.enabled && !self.rules.is_empty()
    }

    /// 按序匹配请求属性，返回**首个命中**规则的动作；无命中返回 `None`。
    ///
    /// 纯函数（不读全局态），便于穷举测试各字段 / 各匹配类型 / 规则顺序。
    fn evaluate(
        &self,
        method: &Method,
        path: &str,
        query: &str,
        headers: &HeaderMap,
    ) -> Option<WafAction> {
        if !self.is_active() {
            return None;
        }
        self.rules
            .iter()
            .find(|r| r.evaluate(method, path, query, headers))
            .map(|r| r.action)
    }
}

/// 解析规则字段：`method` / `path` / `query` / `header`（后者须配 `header_name`）。
fn parse_field(cfg: &WafRuleConfig) -> Option<WafField> {
    match cfg.field.to_ascii_lowercase().as_str() {
        "method" => Some(WafField::Method),
        "path" => Some(WafField::Path),
        "query" => Some(WafField::Query),
        "header" => {
            // header 字段必须指定非空头名，否则该条非法
            let name = cfg.header_name.as_ref()?.trim();
            if name.is_empty() {
                None
            } else {
                Some(WafField::Header(name.to_string()))
            }
        }
        _ => None,
    }
}

/// 解析命中动作：`block` / `allow`。
fn parse_action(action: &str) -> Option<WafAction> {
    match action.to_ascii_lowercase().as_str() {
        "block" => Some(WafAction::Block),
        "allow" => Some(WafAction::Allow),
        _ => None,
    }
}

/// 按匹配类型把 `pattern` 编译为匹配器：`literal` / `wildcard` / `regex`。
///
/// `wildcard` 把 `*`（任意多字符）/ `?`（任意单字符）转译为**锚定**正则（整体匹配整个字段值），
/// 其余字符转义为字面；`regex` 直接编译为非锚定正则（子串搜索）。正则编译失败返回 `None`。
fn parse_matcher(match_type: &str, pattern: &str) -> Option<Matcher> {
    match match_type.to_ascii_lowercase().as_str() {
        "literal" => Some(Matcher::Literal(pattern.to_string())),
        "wildcard" => Regex::new(&wildcard_to_regex(pattern))
            .ok()
            .map(Matcher::Pattern),
        "regex" => Regex::new(pattern).ok().map(Matcher::Pattern),
        _ => None,
    }
}

/// 把通配模式（`*` 任意多字符、`?` 任意单字符）转译为锚定正则字符串。
///
/// 通配符以外的字符全部转义，确保只有 `*`/`?` 具备通配语义；用 `^...$` 锚定，使通配模式整体匹配
/// 整个字段值（如 `/admin/*` 匹配以 `/admin/` 开头的完整路径）。
fn wildcard_to_regex(pattern: &str) -> String {
    let mut re = String::with_capacity(pattern.len() + 4);
    re.push('^');
    for ch in pattern.chars() {
        match ch {
            '*' => re.push_str(".*"),
            '?' => re.push('.'),
            // 其余字符按正则字面转义，避免被当作元字符
            other => {
                if regex_syntax_special(other) {
                    re.push('\\');
                }
                re.push(other);
            }
        }
    }
    re.push('$');
    re
}

/// 判断字符是否为正则元字符（通配转译时需转义，使其按字面匹配）。
fn regex_syntax_special(ch: char) -> bool {
    matches!(
        ch,
        '.' | '+' | '(' | ')' | '|' | '[' | ']' | '{' | '}' | '^' | '$' | '\\' | '/'
    )
}

/// WAF 中间件：置于请求热路径前端（与其他 L7 防护同层），在进入业务前按规则匹配。
///
/// 未启用或空规则集时走零开销快路径直接放行；否则按 method / path / query / header 有序匹配，
/// 首个命中 `block` 返回 403、首个命中 `allow` 放行；无命中亦放行。WAF 不依赖来源 IP。
pub async fn waf_layer(State(state): State<AppState>, request: Request, next: Next) -> Response {
    let rules = &state.waf_rules;
    // 快路径：未启用 / 空规则集，零匹配开销直接放行
    if !rules.is_active() {
        return next.run(request).await;
    }

    let method = request.method().clone();
    let uri = request.uri();
    let path = uri.path().to_string();
    let query = uri.query().unwrap_or("").to_string();
    let headers = request.headers().clone();

    if let Some(WafAction::Block) = rules.evaluate(&method, &path, &query, &headers) {
        // 仅记命中阻断动作，不含可能含敏感信息的完整查询 / 头值
        tracing::warn!(方法 = %method, 路径 = %path, "WAF 规则命中，已阻断请求");
        // FR-56：阻断计数 + 告警评估（不以规则模式串作标签，守低基数）；热路径只做原子累加
        metrics::counter!(keys::WAF_BLOCKED_TOTAL).increment(1);
        state.alert_engine.record(
            ProtectionDimension::Waf,
            &state.config.protection.alerts,
            std::time::Instant::now(),
        );
        return forbidden();
    }
    // 命中 allow 或无命中：放行
    next.run(request).await
}

/// 构造 403 响应：统一错误体（与 `ApiError::Forbidden` 同形），不泄露命中规则细节。
fn forbidden() -> Response {
    let body = Json(json!({
        "error": {
            "code": "forbidden",
            "message": "请求被 WAF 规则拦截",
        }
    }));
    (StatusCode::FORBIDDEN, body).into_response()
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderValue;

    /// 便捷：构造一条规则配置。
    fn 规则(
        field: &str,
        header_name: Option<&str>,
        pattern: &str,
        mt: &str,
        action: &str,
    ) -> WafRuleConfig {
        WafRuleConfig {
            field: field.to_string(),
            header_name: header_name.map(String::from),
            pattern: pattern.to_string(),
            match_type: mt.to_string(),
            action: action.to_string(),
        }
    }

    /// 便捷：用规则集对给定属性求值。
    fn 求值(
        rs: &WafRuleSet,
        method: &str,
        path: &str,
        query: &str,
        headers: &[(&str, &str)],
    ) -> Option<WafAction> {
        let m = Method::from_bytes(method.as_bytes()).unwrap();
        let mut h = HeaderMap::new();
        for (k, v) in headers {
            h.insert(
                axum::http::HeaderName::from_bytes(k.as_bytes()).unwrap(),
                HeaderValue::from_str(v).unwrap(),
            );
        }
        rs.evaluate(&m, path, query, &h)
    }

    /// 由若干规则构造启用态规则集。
    fn 启用规则集(rules: Vec<WafRuleConfig>) -> WafRuleSet {
        WafRuleSet::from_config(&WafConfig {
            enabled: true,
            rules,
        })
    }

    // ===== 通配转译纯函数 =====

    #[test]
    fn 通配转译锚定且转义元字符() {
        // /admin/* → ^\/admin\/.*$，其中 / 被转义、* 转为 .*，整体锚定
        let re = wildcard_to_regex("/admin/*");
        assert_eq!(re, "^\\/admin\\/.*$");
        let compiled = Regex::new(&re).unwrap();
        assert!(compiled.is_match("/admin/users"));
        assert!(!compiled.is_match("/public/admin/x"), "锚定后不应子串命中");
    }

    #[test]
    fn 通配问号匹配单字符() {
        let re = Regex::new(&wildcard_to_regex("a?c")).unwrap();
        assert!(re.is_match("abc"));
        assert!(!re.is_match("ac"), "? 须恰好匹配一个字符");
        assert!(!re.is_match("abbc"));
    }

    // ===== 各字段匹配 =====

    #[test]
    fn 字段_method_匹配() {
        let rs = 启用规则集(vec![规则("method", None, "DELETE", "literal", "block")]);
        assert_eq!(求值(&rs, "DELETE", "/x", "", &[]), Some(WafAction::Block));
        assert_eq!(求值(&rs, "GET", "/x", "", &[]), None);
    }

    #[test]
    fn 字段_path_匹配() {
        let rs = 启用规则集(vec![规则("path", None, "/admin/*", "wildcard", "block")]);
        assert_eq!(
            求值(&rs, "GET", "/admin/secret", "", &[]),
            Some(WafAction::Block)
        );
        assert_eq!(求值(&rs, "GET", "/public/x", "", &[]), None);
    }

    #[test]
    fn 字段_query_匹配() {
        let rs = 启用规则集(vec![规则(
            "query",
            None,
            "drop\\s+table",
            "regex",
            "block",
        )]);
        assert_eq!(
            求值(&rs, "GET", "/x", "q=drop  table", &[]),
            Some(WafAction::Block)
        );
        assert_eq!(求值(&rs, "GET", "/x", "q=hello", &[]), None);
    }

    #[test]
    fn 字段_header_按名匹配且大小写不敏感() {
        let rs = 启用规则集(vec![规则(
            "header",
            Some("User-Agent"),
            "badbot",
            "literal",
            "block",
        )]);
        // 头名大小写不敏感（HeaderMap 本身忽略大小写）
        assert_eq!(
            求值(&rs, "GET", "/x", "", &[("user-agent", "evil-badbot/1.0")]),
            Some(WafAction::Block)
        );
        assert_eq!(
            求值(&rs, "GET", "/x", "", &[("user-agent", "curl/8.0")]),
            None
        );
    }

    #[test]
    fn 缺失header视作空串不误命中() {
        let rs = 启用规则集(vec![规则(
            "header",
            Some("X-Custom"),
            "x",
            "literal",
            "block",
        )]);
        // 请求不带该头：空串不含 "x"，不命中
        assert_eq!(求值(&rs, "GET", "/p", "", &[]), None);
    }

    // ===== 三种匹配类型 =====

    #[test]
    fn 字面匹配走子串包含() {
        let rs = 启用规则集(vec![规则("path", None, "etc/passwd", "literal", "block")]);
        assert_eq!(
            求值(&rs, "GET", "/files/../etc/passwd", "", &[]),
            Some(WafAction::Block)
        );
    }

    #[test]
    fn 正则匹配走子串搜索() {
        let rs = 启用规则集(vec![规则("path", None, "\\.\\./", "regex", "block")]);
        assert_eq!(求值(&rs, "GET", "/a/../b", "", &[]), Some(WafAction::Block));
        assert_eq!(求值(&rs, "GET", "/a/b", "", &[]), None);
    }

    // ===== 规则顺序：首个命中生效 =====

    #[test]
    fn 首个命中生效_allow在前短路block() {
        // 先 allow 命中即放行，不再看后面的 block
        let rs = 启用规则集(vec![
            规则("path", None, "/admin/health", "literal", "allow"),
            规则("path", None, "/admin/*", "wildcard", "block"),
        ]);
        assert_eq!(
            求值(&rs, "GET", "/admin/health", "", &[]),
            Some(WafAction::Allow)
        );
        // 不命中 allow 的 /admin 路径仍被后面的 block 规则拦
        assert_eq!(
            求值(&rs, "GET", "/admin/users", "", &[]),
            Some(WafAction::Block)
        );
    }

    #[test]
    fn 首个命中生效_block在前优先() {
        let rs = 启用规则集(vec![
            规则("path", None, "/x", "literal", "block"),
            规则("path", None, "/x", "literal", "allow"),
        ]);
        // 两条都能命中，首条 block 生效
        assert_eq!(求值(&rs, "GET", "/x", "", &[]), Some(WafAction::Block));
    }

    // ===== 启用 / 空规则 / 非法规则 =====

    #[test]
    fn 未启用时不匹配() {
        let rs = WafRuleSet::from_config(&WafConfig {
            enabled: false,
            rules: vec![规则("path", None, "/x", "literal", "block")],
        });
        assert!(!rs.is_active());
        assert_eq!(求值(&rs, "GET", "/x", "", &[]), None);
    }

    #[test]
    fn 空规则集即便启用也放行() {
        let rs = 启用规则集(vec![]);
        assert!(!rs.is_active());
        assert_eq!(求值(&rs, "GET", "/x", "", &[]), None);
    }

    #[test]
    fn 非法正则规则启动跳过不阻断其余() {
        // 第一条正则非法（未闭合括号）→ 跳过；第二条合法 → 生效
        let rs = 启用规则集(vec![
            规则("path", None, "(unclosed", "regex", "block"),
            规则("path", None, "/blocked", "literal", "block"),
        ]);
        // 仅剩一条有效规则
        assert_eq!(rs.rules.len(), 1);
        assert_eq!(
            求值(&rs, "GET", "/blocked", "", &[]),
            Some(WafAction::Block)
        );
    }

    #[test]
    fn 非法字段或动作或缺header名被跳过() {
        let rs = 启用规则集(vec![
            规则("bogus", None, "x", "literal", "block"), // 非法字段
            规则("path", None, "x", "literal", "destroy"), // 非法动作
            规则("path", None, "x", "fuzzy", "block"),    // 非法匹配类型
            规则("header", None, "x", "literal", "block"), // header 缺 header_name
        ]);
        assert_eq!(rs.rules.len(), 0, "全部非法规则应被跳过");
    }
}

// ============ 中间件端到端测试（经真实路由）============
#[cfg(test)]
mod middleware_tests {
    use super::super::tests::测试用状态;
    use super::super::{build_router, AppState};
    use super::WafRuleSet;
    use crate::config::{WafConfig, WafRuleConfig};
    use axum::body::Body;
    use axum::http::{Request, StatusCode};
    use std::sync::Arc;
    use tower::ServiceExt;

    /// 便捷：构造一条规则配置。
    fn 规则(
        field: &str,
        header_name: Option<&str>,
        pattern: &str,
        mt: &str,
        action: &str,
    ) -> WafRuleConfig {
        WafRuleConfig {
            field: field.to_string(),
            header_name: header_name.map(String::from),
            pattern: pattern.to_string(),
            match_type: mt.to_string(),
            action: action.to_string(),
        }
    }

    /// 以给定 WAF 配置定制测试状态（编译规则集注入 AppState）。
    async fn waf状态(enabled: bool, rules: Vec<WafRuleConfig>) -> (AppState, tempfile::TempDir) {
        let (mut state, dir) = 测试用状态().await;
        let cfg = WafConfig { enabled, rules };
        state.waf_rules = Arc::new(WafRuleSet::from_config(&cfg));
        (state, dir)
    }

    /// 用给定方法 / URI / 头发请求，返回状态码。
    async fn 打(
        app: axum::Router,
        method: &str,
        uri: &str,
        headers: &[(&str, &str)],
    ) -> StatusCode {
        let mut builder = Request::builder().method(method).uri(uri);
        for (k, v) in headers {
            builder = builder.header(*k, *v);
        }
        let req = builder.body(Body::empty()).unwrap();
        app.oneshot(req).await.unwrap().status()
    }

    #[tokio::test]
    async fn 未启用时正常请求放行不误杀() {
        // 关闭 WAF：即便有 block 规则也不生效（防误杀基线）
        let (state, _dir) = waf状态(
            false,
            vec![规则("path", None, "/health", "literal", "block")],
        )
        .await;
        let app = build_router(state);
        assert_eq!(打(app, "GET", "/health", &[]).await, StatusCode::OK);
    }

    #[tokio::test]
    async fn 空规则集正常包管理器请求不误杀() {
        // 启用但空规则集：正常请求一律放行
        let (state, _dir) = waf状态(true, vec![]).await;
        let app = build_router(state);
        // 模拟包管理器拉取健康检查与普通路径，均不应被拦
        assert_eq!(打(app.clone(), "GET", "/health", &[]).await, StatusCode::OK);
        assert_ne!(
            打(app, "GET", "/maven-hosted/a/b.jar", &[]).await,
            StatusCode::FORBIDDEN
        );
    }

    #[tokio::test]
    async fn 命中block规则返回403() {
        let (state, _dir) = waf状态(
            true,
            vec![规则("path", None, "/admin/*", "wildcard", "block")],
        )
        .await;
        let app = build_router(state);
        assert_eq!(
            打(app, "GET", "/admin/secret", &[]).await,
            StatusCode::FORBIDDEN
        );
    }

    #[tokio::test]
    async fn 未命中规则放行进入业务() {
        // 规则只拦 /admin/*，正常 /health 应放行进入业务（200）
        let (state, _dir) = waf状态(
            true,
            vec![规则("path", None, "/admin/*", "wildcard", "block")],
        )
        .await;
        let app = build_router(state);
        assert_eq!(打(app, "GET", "/health", &[]).await, StatusCode::OK);
    }

    #[tokio::test]
    async fn 命中allow规则短路放行() {
        // allow 在前命中即放行，即便后面有 block 命中同路径
        let (state, _dir) = waf状态(
            true,
            vec![
                规则("path", None, "/health", "literal", "allow"),
                规则("path", None, "/health", "literal", "block"),
            ],
        )
        .await;
        let app = build_router(state);
        assert_eq!(打(app, "GET", "/health", &[]).await, StatusCode::OK);
    }

    #[tokio::test]
    async fn header字段匹配阻断() {
        let (state, _dir) = waf状态(
            true,
            vec![规则(
                "header",
                Some("User-Agent"),
                "sqlmap",
                "literal",
                "block",
            )],
        )
        .await;
        let app = build_router(state.clone());
        // 命中恶意 UA
        assert_eq!(
            打(app, "GET", "/health", &[("user-agent", "sqlmap/1.5")]).await,
            StatusCode::FORBIDDEN
        );
        // 正常 UA 放行
        let app2 = build_router(state);
        assert_eq!(
            打(app2, "GET", "/health", &[("user-agent", "Maven/3.9")]).await,
            StatusCode::OK
        );
    }

    #[tokio::test]
    async fn method字段匹配阻断() {
        let (state, _dir) = waf状态(
            true,
            vec![规则("method", None, "TRACE", "literal", "block")],
        )
        .await;
        let app = build_router(state);
        assert_eq!(
            打(app, "TRACE", "/health", &[]).await,
            StatusCode::FORBIDDEN
        );
    }

    #[tokio::test]
    async fn query字段正则匹配阻断() {
        // 对原始查询串做正则匹配（WAF 不解码，按字段原文匹配）；用 `+` 作为查询中的合法分隔符
        let (state, _dir) = waf状态(
            true,
            vec![规则("query", None, "(?i)union.+select", "regex", "block")],
        )
        .await;
        let app = build_router(state);
        assert_eq!(
            打(app, "GET", "/health?q=1+UNION+SELECT+2", &[]).await,
            StatusCode::FORBIDDEN
        );
    }
}
