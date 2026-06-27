//! 复现并回归守护：首启随机管理员口令绝不进可查的运行日志文件（FR-59 × FR-107，安全红线）。
//!
//! 背景：FR-59 首启无管理员时生成随机口令提示运维；FR-107（ADR-0029）给运行日志增设文件 sink
//! 落盘到 `{data_dir}/logs/app.log` 并经「系统日志」页可查。若口令经 `tracing` 打印，会被文件 sink
//! 捕获、写进可查日志文件——违反「凭据/密码绝不进日志」红线（架构不变量 §3、ADR-0029 §22）。
//!
//! 本测试用与生产一致的文件 sink 装配（`RollingFileWriter` + fmt 层），运行首启口令提示路径，
//! 断言：口令**出现在给运维的 stdout 提示**、但**不出现在落盘日志文件**（即系统日志页可查内容）。

use jianartifact::logs::{self, RollingFileWriter};
use tracing::warn;
use tracing_subscriber::fmt::MakeWriter;
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::Layer;

// 经 thread_local 收集本测试线程内写入「stdout 提示」通道的字节。
thread_local! {
    static STDOUT_BUF: std::cell::RefCell<Vec<u8>> = const { std::cell::RefCell::new(Vec::new()) };
}

/// 写入本测试线程 stdout 提示缓冲的句柄（fmt 层每次事件取一份，故落到共享 thread_local）。
struct 线程缓冲句柄;
impl std::io::Write for 线程缓冲句柄 {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        STDOUT_BUF.with(|b| b.borrow_mut().extend_from_slice(buf));
        Ok(buf.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// 把日志事件捕获进线程缓冲的 `MakeWriter`，模拟「给运维的 stdout 提示」通道，便于断言口令可见。
#[derive(Clone, Default)]
struct 缓冲写;
impl<'a> MakeWriter<'a> for 缓冲写 {
    type Writer = 线程缓冲句柄;
    fn make_writer(&'a self) -> Self::Writer {
        线程缓冲句柄
    }
}

/// 读取本测试线程缓冲（stdout 提示通道）的全部文本。
fn 读stdout提示() -> String {
    STDOUT_BUF.with(|b| String::from_utf8_lossy(&b.borrow()).into_owned())
}

#[test]
fn 首启随机口令进stdout提示但不进可查日志文件() {
    let dir = tempfile::tempdir().unwrap();
    let logs_dir = logs::logs_dir(dir.path());
    std::fs::create_dir_all(&logs_dir).unwrap();

    // 与生产 install_file_logging 一致：文件 fmt 层（关 ANSI）+ RollingFileWriter；
    // 另叠一层把同样的事件喂进「stdout 提示」缓冲，模拟生产的 stdout 通道。
    let writer = RollingFileWriter::new(&logs_dir).unwrap();
    let file_layer = tracing_subscriber::fmt::layer()
        .with_writer(writer)
        .with_ansi(false)
        .boxed();
    let stdout_layer = tracing_subscriber::fmt::layer()
        .with_writer(缓冲写)
        .with_ansi(false)
        .boxed();
    let subscriber = tracing_subscriber::registry()
        .with(file_layer)
        .with(stdout_layer);

    // 唯一标识本次随机口令的明文（足够独特，避免与其他日志文本巧合匹配）
    let password = "Zq7Wv3Nx9Lm2Kp5T-唯一口令";
    let username = "admin";

    // 在作用域内设为默认订阅者，运行首启口令提示路径（被测真实行为）
    {
        let _guard = subscriber.set_default();
        // 生产路径：把随机口令提示给运维（仅首启一次）。
        // 经 logs 模块统一入口直接写 stdout，确保口令只进 stdout 提示、不经 tracing 进文件 sink。
        let mut out = 线程缓冲句柄;
        logs::write_bootstrap_password_notice(&mut out, username, password).unwrap();
        // 非密上下文仍可进运行日志（便于运维知道发生了首启引导），但绝不含口令字段
        warn!(
            用户名 = %username,
            "已创建首个管理员并生成随机初始口令，口令仅在标准输出提示一次，请妥善保管并尽快改密"
        );
    }

    // 断言一：口令出现在给运维的 stdout 提示（首启可用性不破）
    let stdout_text = 读stdout提示();
    assert!(
        stdout_text.contains(password),
        "随机口令必须出现在 stdout 提示，否则运维无法首次登录；实际 stdout 提示：\n{stdout_text}"
    );

    // 断言二：口令绝不出现在落盘日志文件（即系统日志页可查内容）
    let file_path = logs::log_file_path(dir.path());
    let file_lines = logs::read_log_lines(&file_path);
    let file_text = file_lines.join("\n");
    assert!(
        !file_text.contains(password),
        "随机口令绝不能进可查的运行日志文件（违反凭据不进日志红线）；实际落盘日志：\n{file_text}"
    );

    // 断言三：系统日志解析视角（页面展示口径）同样看不到口令
    let (entries, _total) = logs::tail_filter(&file_lines, None, 0, 1000);
    let parsed_text: String = entries
        .into_iter()
        .map(|e| e.message)
        .collect::<Vec<_>>()
        .join("\n");
    assert!(
        !parsed_text.contains(password),
        "随机口令绝不能经系统日志页展示出来；实际解析后内容：\n{parsed_text}"
    );
}
