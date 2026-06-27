//! 运行时日志（FR-107，ADR-0029）：把 tracing 运行日志落盘到 `{data_dir}/logs/app.log`，
//! 并提供"日志行 → 结构化条目"的纯解析、tail / 级别过滤逻辑供读取端点复用。
//!
//! 设计（严格照 ADR-0029）：
//! - **载体是文件、不落库**：运行日志是高频技术流水，写文件而非 SQLite（与审计 FR-31 业务留痕严格区分）。
//! - **纯函数解析**：`parse_log_line` / `parse_level` / `tail_filter` 无 IO、无副作用，便于穷举单测；
//!   读文件的 IO 留在 `api` 侧薄封装。无法识别级别的行不丢、归无级别、原文进 message。
//! - **简单滚动**：`RollingFileWriter` 单文件追加 + 单次大小滚动（`app.log` → `app.log.1`），用 std 实现，
//!   不引 `tracing-appender`（简单优先 / 优先不引依赖）。

use std::fs::{File, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use serde::Serialize;

/// 日志子目录名（位于数据目录下）。
const LOGS_SUBDIR: &str = "logs";
/// 主日志文件名。
const LOG_FILE_NAME: &str = "app.log";
/// 滚动后的历史文件名（仅保留 1 个）。
const ROLLED_FILE_NAME: &str = "app.log.1";
/// 单文件大小上限（字节）：超过即滚动一次。默认 8 MiB，够 P2 用，约束读取端点单次读量。
const MAX_LOG_BYTES: u64 = 8 * 1024 * 1024;

/// 日志级别（对齐 tracing 的五级）。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LogLevel {
    Error,
    Warn,
    Info,
    Debug,
    Trace,
}

impl LogLevel {
    /// 级别的规范大写串（用于序列化与展示）。
    pub fn as_str(self) -> &'static str {
        match self {
            LogLevel::Error => "ERROR",
            LogLevel::Warn => "WARN",
            LogLevel::Info => "INFO",
            LogLevel::Debug => "DEBUG",
            LogLevel::Trace => "TRACE",
        }
    }
}

/// 解析级别字符串（大小写不敏感）；无法识别返回 `None`。
pub fn parse_level(raw: &str) -> Option<LogLevel> {
    match raw.trim().to_ascii_uppercase().as_str() {
        "ERROR" => Some(LogLevel::Error),
        "WARN" | "WARNING" => Some(LogLevel::Warn),
        "INFO" => Some(LogLevel::Info),
        "DEBUG" => Some(LogLevel::Debug),
        "TRACE" => Some(LogLevel::Trace),
        _ => None,
    }
}

/// 一条结构化日志条目（对外 DTO）：级别 / 时间戳缺省以 `null` 序列化，原文行保底进 `message`。
#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct LogEntry {
    /// 时间戳（RFC3339 字符串）；无法识别为 `None`。
    pub timestamp: Option<String>,
    /// 级别规范大写串（ERROR/WARN/INFO/DEBUG/TRACE）；无法识别为 `None`。
    pub level: Option<String>,
    /// 消息正文（含 span / target 与字段，去掉已识别的时间戳与级别前缀）。
    pub message: String,
}

/// 判断一个 token 是否像 tracing 默认 fmt 的时间戳（RFC3339，含 `T`、以 `Z` 收尾）。
///
/// 不做完整 RFC3339 校验（避免引时间库做严格解析）：只判别形态，足以从行首切出时间戳。
fn looks_like_timestamp(token: &str) -> bool {
    token.len() >= 20
        && token.ends_with('Z')
        && token.contains('T')
        && token.as_bytes()[0].is_ascii_digit()
}

/// 解析单行 tracing 默认 fmt 日志为结构化条目（纯函数）。
///
/// 默认行形如 `2026-06-27T08:00:00.123456Z  INFO target: message...`：
/// 时间戳为首 token、级别为次 token、其余为消息。任一不匹配则降级——
/// 识别不出时间戳就整行作消息、识别不出级别就保留时间戳后整段作消息，**绝不丢行**。
pub fn parse_log_line(line: &str) -> LogEntry {
    let trimmed = line.trim_end_matches(['\r', '\n']);
    let rest = trimmed.trim_start();

    // 尝试切出时间戳（首 token）
    let (timestamp, after_ts) = match rest.split_once(char::is_whitespace) {
        Some((first, tail)) if looks_like_timestamp(first) => {
            (Some(first.to_string()), tail.trim_start())
        }
        _ => (None, rest),
    };

    // 尝试切出级别（紧随的 token）
    if let Some((level_tok, tail)) = after_ts.split_once(char::is_whitespace) {
        if let Some(level) = parse_level(level_tok) {
            return LogEntry {
                timestamp,
                level: Some(level.as_str().to_string()),
                message: tail.trim_start().to_string(),
            };
        }
    } else if let Some(level) = parse_level(after_ts) {
        // 只有级别、无后续消息
        return LogEntry {
            timestamp,
            level: Some(level.as_str().to_string()),
            message: String::new(),
        };
    }

    // 级别识别失败：保留已识别的时间戳（若有），其余整段进消息，不丢
    LogEntry {
        timestamp,
        level: None,
        message: after_ts.to_string(),
    }
}

/// 对全部日志行做"解析 → 可选级别精确过滤 → tail（最新在前）→ offset/limit 切片"（纯函数）。
///
/// 返回 `(本页条目, 过滤后总数)`：
/// - `level` 为 `Some` 时只保留该级别（精确匹配）；无级别行在有过滤时被排除。
/// - tail 语义：结果按最新在前排序（输入按时间先后，故反转）。
/// - `offset` 从最新行起向更旧偏移；越界返回空页但 `total` 仍为过滤后总数。
pub fn tail_filter(
    lines: &[String],
    level: Option<LogLevel>,
    offset: usize,
    limit: usize,
) -> (Vec<LogEntry>, usize) {
    // 解析 + 过滤；保持原始时间先后次序
    let mut entries: Vec<LogEntry> = lines
        .iter()
        .map(|l| parse_log_line(l))
        .filter(|e| match level {
            None => true,
            Some(want) => e.level.as_deref() == Some(want.as_str()),
        })
        .collect();

    let total = entries.len();
    // 最新在前
    entries.reverse();

    let page: Vec<LogEntry> = entries.into_iter().skip(offset).take(limit).collect();
    (page, total)
}

/// 日志目录路径：`{data_dir}/logs`。
pub fn logs_dir(data_dir: &Path) -> PathBuf {
    data_dir.join(LOGS_SUBDIR)
}

/// 主日志文件路径：`{data_dir}/logs/app.log`（init 端写、读取端读，单一来源避免魔法字符串重复）。
pub fn log_file_path(data_dir: &Path) -> PathBuf {
    logs_dir(data_dir).join(LOG_FILE_NAME)
}

/// 读取日志文件全部行；文件不存在 / 读失败均返回空集合（端点据此返空列表，不报错）。
///
/// 文件大小受 `RollingFileWriter` 滚动上限约束，单次读全量可接受（简单优先，不做流式倒读）。
pub fn read_log_lines(path: &Path) -> Vec<String> {
    match std::fs::read_to_string(path) {
        Ok(content) => content.lines().map(|l| l.to_string()).collect(),
        Err(_) => Vec::new(),
    }
}

/// 把首启随机管理员口令的一次性提示**直接写到给定 writer（生产为 stdout）**（FR-59，安全红线）。
///
/// 为何不走 `tracing`：运行日志经 `tracing` 文件 sink 落盘到 `{data_dir}/logs/app.log` 且经「系统日志」页
/// 可查（FR-107，ADR-0029）。口令一旦经 `tracing` 打印就会被文件 sink 捕获、写进可查日志——违反
/// 「凭据/密码绝不进日志」红线（架构不变量 §3）。故口令明文仅经本函数直接写 stdout 提示运维（首启一次），
/// 从源头上不进入任何 `tracing` sink；非密上下文（已创建首个管理员）仍可经 `tracing` 记录。
pub fn write_bootstrap_password_notice<W: Write>(
    out: &mut W,
    username: &str,
    password: &str,
) -> io::Result<()> {
    writeln!(
        out,
        "================ 首启管理员初始口令（仅本次显示，请妥善保管并尽快登录后修改）================"
    )?;
    writeln!(out, "  用户名: {username}")?;
    writeln!(out, "  初始口令: {password}")?;
    writeln!(
        out,
        "  注意: 该口令不入库、不写入运行日志文件，仅在此标准输出提示一次；如未记下需重置数据重新引导。"
    )?;
    writeln!(
        out,
        "================================================================================"
    )?;
    out.flush()
}

/// 单文件 + 单次大小滚动的日志 writer（FR-107，ADR-0029）。
///
/// 写入前若现有文件超过 `max_bytes` 则滚动一次（`app.log` → `app.log.1`，旧 `.1` 覆盖），
/// 再续写新 `app.log`。写与滚动经内部 `Mutex` 串行化（本地小 IO，开销可忽略）。
/// 用 std 文件 API 实现，不引第三方 appender。
pub struct RollingFileWriter {
    inner: Mutex<RollingState>,
}

/// 滚动 writer 的可变内部态（持锁访问）。
struct RollingState {
    /// 主日志文件路径。
    path: PathBuf,
    /// 滚动历史文件路径。
    rolled: PathBuf,
    /// 当前打开的追加句柄。
    file: File,
    /// 当前文件已写字节数（用于判滚动，免每次 stat）。
    written: u64,
    /// 滚动阈值。
    max_bytes: u64,
}

impl RollingFileWriter {
    /// 在 `dir` 下创建 / 打开 `app.log`（追加模式），用默认大小上限。
    ///
    /// 调用前应确保 `dir` 已存在（由 init 端创建数据目录 / 日志目录）。
    pub fn new(dir: &Path) -> io::Result<Self> {
        Self::with_max_bytes(dir, MAX_LOG_BYTES)
    }

    /// 同 `new`，但显式指定大小上限（供测试注入小阈值验证滚动）。
    pub fn with_max_bytes(dir: &Path, max_bytes: u64) -> io::Result<Self> {
        let path = dir.join(LOG_FILE_NAME);
        let rolled = dir.join(ROLLED_FILE_NAME);
        let file = OpenOptions::new().create(true).append(true).open(&path)?;
        let written = file.metadata().map(|m| m.len()).unwrap_or(0);
        Ok(Self {
            inner: Mutex::new(RollingState {
                path,
                rolled,
                file,
                written,
                max_bytes,
            }),
        })
    }
}

impl RollingState {
    /// 若当前文件超过阈值则滚动一次：关旧句柄 → 重命名 → 新建空文件续写。
    fn roll_if_needed(&mut self) -> io::Result<()> {
        if self.written < self.max_bytes {
            return Ok(());
        }
        // 旧 .1 让位给本次滚动；重命名当前文件为 .1；重开空的主文件
        let _ = std::fs::remove_file(&self.rolled);
        std::fs::rename(&self.path, &self.rolled)?;
        self.file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)?;
        self.written = 0;
        Ok(())
    }
}

impl Write for &RollingFileWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let mut state = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        state.roll_if_needed()?;
        let n = state.file.write(buf)?;
        state.written += n as u64;
        Ok(n)
    }

    fn flush(&mut self) -> io::Result<()> {
        let mut state = self
            .inner
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        state.file.flush()
    }
}

/// 为 tracing 的 `fmt` 层提供 `MakeWriter`：每次事件取一份对 writer 的可写借用。
///
/// `&RollingFileWriter` 实现了 `Write`（内部 `Mutex` 串行化），故 `MakeWriter` 直接返回引用即可。
impl<'a> tracing_subscriber::fmt::MakeWriter<'a> for RollingFileWriter {
    type Writer = &'a RollingFileWriter;

    fn make_writer(&'a self) -> Self::Writer {
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn 解析级别_大小写不敏感且识别别名() {
        assert_eq!(parse_level("error"), Some(LogLevel::Error));
        assert_eq!(parse_level("  WARN "), Some(LogLevel::Warn));
        assert_eq!(parse_level("warning"), Some(LogLevel::Warn));
        assert_eq!(parse_level("Info"), Some(LogLevel::Info));
        assert_eq!(parse_level("DEBUG"), Some(LogLevel::Debug));
        assert_eq!(parse_level("trace"), Some(LogLevel::Trace));
        assert_eq!(parse_level("verbose"), None);
        assert_eq!(parse_level(""), None);
    }

    #[test]
    fn 解析正常行_取出时间戳级别与消息() {
        let line = "2026-06-27T08:00:00.123456Z  INFO jianartifact::api: 配置加载完成";
        let entry = parse_log_line(line);
        assert_eq!(
            entry.timestamp.as_deref(),
            Some("2026-06-27T08:00:00.123456Z")
        );
        assert_eq!(entry.level.as_deref(), Some("INFO"));
        assert_eq!(entry.message, "jianartifact::api: 配置加载完成");
    }

    #[test]
    fn 解析各级别行() {
        for (raw, want) in [
            ("2026-06-27T08:00:00.000000Z ERROR m: boom", "ERROR"),
            ("2026-06-27T08:00:00.000000Z  WARN m: careful", "WARN"),
            ("2026-06-27T08:00:00.000000Z  INFO m: ok", "INFO"),
            ("2026-06-27T08:00:00.000000Z DEBUG m: detail", "DEBUG"),
            ("2026-06-27T08:00:00.000000Z TRACE m: trace", "TRACE"),
        ] {
            assert_eq!(
                parse_log_line(raw).level.as_deref(),
                Some(want),
                "行: {raw}"
            );
        }
    }

    #[test]
    fn 解析异常行_不丢行_归无级别() {
        // 完全不符合格式的行：整行进消息、级别与时间戳为 None
        let entry = parse_log_line("这是一条没有任何格式的随意输出");
        assert_eq!(entry.timestamp, None);
        assert_eq!(entry.level, None);
        assert_eq!(entry.message, "这是一条没有任何格式的随意输出");

        // 有时间戳但级别不可识别：保留时间戳、级别 None、其余进消息
        let entry2 = parse_log_line("2026-06-27T08:00:00.000000Z NOTICE 自定义级别");
        assert_eq!(
            entry2.timestamp.as_deref(),
            Some("2026-06-27T08:00:00.000000Z")
        );
        assert_eq!(entry2.level, None);
        assert_eq!(entry2.message, "NOTICE 自定义级别");
    }

    #[test]
    fn 解析空行不panic() {
        let entry = parse_log_line("");
        assert_eq!(entry.level, None);
        assert_eq!(entry.message, "");
    }

    /// 构造测试用日志行（按时间先后）。
    fn 行集() -> Vec<String> {
        vec![
            "2026-06-27T08:00:01.000000Z  INFO m: 一".to_string(),
            "2026-06-27T08:00:02.000000Z ERROR m: 二".to_string(),
            "2026-06-27T08:00:03.000000Z  WARN m: 三".to_string(),
            "2026-06-27T08:00:04.000000Z  INFO m: 四".to_string(),
            "2026-06-27T08:00:05.000000Z ERROR m: 五".to_string(),
        ]
    }

    #[test]
    fn tail_最新在前() {
        let lines = 行集();
        let (page, total) = tail_filter(&lines, None, 0, 10);
        assert_eq!(total, 5);
        assert_eq!(page.len(), 5);
        // 最新（五）在前
        assert_eq!(page[0].message, "m: 五");
        assert_eq!(page[4].message, "m: 一");
    }

    #[test]
    fn tail_级别精确过滤() {
        let lines = 行集();
        let (page, total) = tail_filter(&lines, Some(LogLevel::Error), 0, 10);
        assert_eq!(total, 2, "仅两条 ERROR");
        assert_eq!(page.len(), 2);
        assert!(page.iter().all(|e| e.level.as_deref() == Some("ERROR")));
        // 最新的 ERROR（五）在前
        assert_eq!(page[0].message, "m: 五");
        assert_eq!(page[1].message, "m: 二");
    }

    #[test]
    fn tail_分页_offset与limit() {
        let lines = 行集();
        // 取第 2、3 新（跳过最新 1 条，要 2 条）→ 四、三
        let (page, total) = tail_filter(&lines, None, 1, 2);
        assert_eq!(total, 5);
        assert_eq!(page.len(), 2);
        assert_eq!(page[0].message, "m: 四");
        assert_eq!(page[1].message, "m: 三");
    }

    #[test]
    fn tail_offset越界_返空但total正确() {
        let lines = 行集();
        let (page, total) = tail_filter(&lines, None, 99, 10);
        assert_eq!(total, 5);
        assert!(page.is_empty());
    }

    #[test]
    fn tail_空输入_返空() {
        let (page, total) = tail_filter(&[], None, 0, 10);
        assert_eq!(total, 0);
        assert!(page.is_empty());
    }

    #[test]
    fn 路径助手_落在数据目录下() {
        let dir = Path::new("/data");
        assert_eq!(logs_dir(dir), Path::new("/data/logs"));
        assert_eq!(log_file_path(dir), Path::new("/data/logs/app.log"));
    }

    #[test]
    fn 读取不存在文件_返空集合() {
        let lines = read_log_lines(Path::new("/绝不存在的目录/app.log"));
        assert!(lines.is_empty());
    }

    #[test]
    fn 滚动写_超阈值后旧内容进点一文件() {
        let dir = tempfile::tempdir().unwrap();
        let writer = RollingFileWriter::with_max_bytes(dir.path(), 64).unwrap();
        // 写入超过 64 字节，触发至少一次滚动
        let mut w = &writer;
        for i in 0..20 {
            writeln!(w, "第{i}行日志内容占位填充字节数").unwrap();
        }
        w.flush().unwrap();

        let main = dir.path().join("app.log");
        let rolled = dir.path().join("app.log.1");
        assert!(main.exists(), "主文件应存在");
        assert!(rolled.exists(), "超阈值后应产生滚动历史文件 app.log.1");
        // 主文件大小应已被滚动重置过（不会一直线性增长到 20 行全部）
        let main_len = std::fs::metadata(&main).unwrap().len();
        assert!(main_len <= 64 + 128, "主文件应在阈值附近，不应累计全部内容");
    }

    #[test]
    fn 滚动写_未超阈值不滚动() {
        let dir = tempfile::tempdir().unwrap();
        let writer = RollingFileWriter::with_max_bytes(dir.path(), 1024 * 1024).unwrap();
        let mut w = &writer;
        writeln!(w, "一行").unwrap();
        w.flush().unwrap();
        assert!(dir.path().join("app.log").exists());
        assert!(!dir.path().join("app.log.1").exists(), "未超阈值不应滚动");
    }

    #[test]
    fn 首启口令提示_含口令明文且写入给定writer() {
        // 口令明文须出现在给运维的 stdout 提示里，否则首启无法登录（可用性不破）
        let mut buf: Vec<u8> = Vec::new();
        let password = "Zq7Wv3Nx9Lm2Kp5T-唯一口令";
        write_bootstrap_password_notice(&mut buf, "admin", password).unwrap();
        let text = String::from_utf8(buf).unwrap();
        assert!(text.contains("admin"), "提示应含用户名");
        assert!(text.contains(password), "提示必须含口令明文供运维首次登录");
    }
}
