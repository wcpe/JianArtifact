# 变更日志

本项目所有重要变更记录于此。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## 未发布

> 本段登记**尚未开窗**的变更：`VERSION` 仍为 `0.11.0`，版本号待下一个开发窗口开启时统一确定，故此处不写具体版本。

### 新增

- **恢复进度、校验反馈与陈旧行（FR-40 追加七，前端）**：界面复核提出的四条"看不到反馈"与一条 regression，均由前端补齐。① **恢复不再是"只弹一个 toast"**：恢复受理后弹窗只告知"已受理"，用户不知道在跑、跑到哪、失败没有。新增 `RestoreProgressModal` 显示阶段（排队 / 拉取 / 校验与暂存 / 待重启 / 已生效 / 失败）、字节进度与 `blob` 计数——进度数据本就存在（`BackupImport` 带 `status` / `fetchedBytes` / `totalBytes`），此前只是没显示出来。**只有"拉取"阶段有真实字节数才画百分比进度条**，暂存与深校验一律"转圈 + 阶段文案"：那两步耗时与包体积同阶，编一个百分比比不画更误导。四条恢复通道（URL / 上传 / 本机包 / CLI）共用一个弹窗（差别只在 `origin`）。通道接法沿用既有决定——`UrlImportPanel` / `BackupUploadPanel` 此前明确拒绝过 `onImported` 回调（"同一件事有两个刷新通道，容易出现一个刷了另一个没刷的错位"），故仍是**同一个全局刷新事件**，只是把 `importId` 作为载荷捎带（`dispatchRefresh(importId)`）。CLI 通道没有前端调用点，退化为"列表里可见的一行"。② **校验 / 深度校验有弹窗了**：此前只是 `await` 一下再 toast。新增 `VerifyModal` 打开即列出**真实会执行的检查项**（顺序与 `archive.Reader.Verify` 的实现步骤一一对应：清单一致性 → 数据库快照 → 对象索引 → 归档完整性；深校验额外加逐对象内容摘要），完成后**就地展示结论**（通过与规模、耗时）而不是让结论随 toast 消失。**刻意不画百分比**：后端 `verify` 是同步接口（一次返回结论，无中间进度可拉，对比 drill 是 202 + 轮询），编出来的进度比不显示更不可信。**浅校验显式写明"不核对对象内容"**——否则"校验通过"会被读成"这份包没问题"，而它对"结构完好但内容被篡改"的包照样判通过。③ **一键重启入了界面**：新增 `RestartControl`，可用时「立即重启服务」为二次确认后的主按钮，受理后**轮询公开健康端点 `/api/v1/status` 探活**，成功即刷新数据；不可用（`supported:false`）时显示可复制命令与原因，**绝不假装成功**。探活首个请求延迟 1.5 秒发出——重启命令有约 2 秒延迟，立刻探必然还是旧进程，会让"服务已回来"在被杀掉之前误报成功；上限 60 秒，超时如实告知手动刷新。④ **恢复前建议冻结（不强制）**：恢复确认弹窗与进度弹窗的"待重启"区块各给一句说明 + 可点的冻结入口（复用 `WriteFreezeModal`），已冻结时改为提醒"重启后记得解冻"。**不强制的原因**：强制会让"忘记解冻"直接把实例变成只读，代价大于收益，而恢复本身是"暂存 + 重启时原子替换"，一致性已由机制保证。⑤ **修复「复制链接会失败」（regression，根因不是剪贴板）**：真实原因是**界面显示的是陈旧行**——自动备份每 30 秒生成新包并淘汰旧包，而**新增与淘汰都是终态变化**，不触发任何轮询，于是页面开着不动时列表永不更新，用户点到已消失的包、后端返 404「备份包不存在」，看着像功能坏了。两处修复：新增 `AUTO_LIST_REFRESH_MS = 10_000`（存在自动备份配置时按 10 秒慢节奏自动刷新，`hasRunning` 时由更快的 1.5 秒轮询覆盖、不叠加定时器）；新增 `handleStaleRow` 把 404 统一转为可操作提示并**先刷新列表再提示**（只提示不刷新的话用户很容易再点同一行、再失败一次），接入获取链接 / 校验 / 删除 / **用这个包恢复**四处（最后一处是实机验收时补上的：弹窗开着时包被淘汰，点「开始恢复」拿到 404，不接就退化成裸报「备份包不存在」）。测试：新增 25 条前端守卫（恢复进度轮询推进与终态停轮询、校验检查项与浅校验诚实说明、重启降级显示命令、冻结建议），**逐条变异验证**——去掉轮询 effect、去掉浅校验提示、重启降级改报错、去掉冻结建议、去掉终态停轮询门控、去掉重启二次确认，六处分别转红后还原。后端另加四条变异（见下条）。实机验收另发现两处原计划未预见的收尾缺口，已在同一次改动内补齐：① **恢复生效后弹窗不会收尾**——恢复会把整个数据库替换成包里的快照，那条导入记录随之消失，于是进度弹窗随后的轮询拿到 404；若按通用错误渲染成"读不到记录"，**说的与事实正好相反**（用户刚恢复成功）。现把这条 404 单独识别为「恢复已生效」收尾态（渲染在 `AsyncBoundary` 之外，否则通用错误态会把它挡掉）；走界面「立即重启服务」时探活成功即顺手关掉弹窗。两个分支覆盖两种重启方式（服务器手敲命令 / 界面点按钮），缺一不可。② **「待重启」说明弹窗缺一键重启**——`restore.pending` 不允许叠加，一次恢复暂存成功后后续恢复一律被 409 拒绝，而这个状态**跨页面刷新一直存在**：页面重开后再从橙色导入按钮进到那个说明弹窗时，原先只有一条要自己复制的命令，一键重启按钮只存在于"刚受理完那一刻"才打开的进度弹窗里——**用户真正需要它的场景里，按钮恰好不在**。现把 `RestartControl` 也放进该弹窗（保留可复制命令作为保底）。③ **「用这个包恢复」漏了陈旧行处置**——原计划已列出这一处，首轮实现只接了获取链接 / 校验 / 删除。实机踩到的场景很典型：用户看到列表 → 点开确认弹窗 → 读完说明再按「开始恢复」，这中间包已被淘汰（测试站 30 秒间隔 + 保留 2 份），后端返 404，不接就退化成裸报「备份包不存在」。顺带修正两条**既有**用例的查询作用域：它们用 `findByText(/^imp-\d{14}-\d{3}$/)` 找新导入记录，而进度弹窗会自动打开、也渲染同一个 `importId` ⇒ 断言因"找到多个元素"失败——**那是本功能正常工作的结果，不是回归**，已改为限定在事件表内查找。规格见 [`docs/specs/0.12.0-auto-backup-and-drill.md`](docs/specs/0.12.0-auto-backup-and-drill.md) §7.5。
- **一键重启服务（FR-40 追加七，后端）**：备份恢复后生效、搬迁流程的"起服"一步，此前都得登机器敲 `systemctl restart`。现新增 `POST /api/v1/maintenance/restart`（仅管理员，见 `docs/API.md`）。① **顺序契约：先回响应，再重启**——重启命令会终止当前进程，若先重启则 202 根本发不出去，调用方只能看到连接被重置，既分不清"重启成功"还是"网络抖动"，也拿不到降级信息；因此先排定一个延迟任务再立刻写出 **202**，真正执行在 `delayMs` 毫秒后（约 1.8 秒，留出响应送达与前端渲染的余量）。**看到 202 后连接断开是预期行为**。② **能力探测 + 失败降级（绝不假装成功）**：受理前**同步**预检——`LookPath` 查命令是否存在、`sudo -n systemctl restart --dry-run <unit>`（3 秒超时）验免密 sudo 与单元可操作性、单元名字符集校验。预检不通过时返回 **HTTP 200** + `supported:false` + `reason` + `command`（可复制到终端手动执行的等效命令），而**不是** 4xx/5xx：`supported:false` 是正常业务结果，前端要拿它渲染降级 UI，用错误码返回会把可操作信息埋进错误路径。预检必须前移的原因也在此——后台失败对调用方**不可观测**（进程随即重启、响应早已发出），无法区分"重启成功"与"命令根本没跑起来"。③ **新增两个环境变量**：`JIAN_RESTART_UNIT`（默认 `jianartifact`）与 `JIAN_RESTART_SCOPE`（默认 `system`，可选 `systemd-user` / `docker`，非法值回落 `system` 而非报错——作用域写错应当退化成最保守取值再交由预检如实降级）。`system` 下进程本身是 root 时不加 `sudo`；`docker` 作用域**明确不支持自动重启**（容器生命周期由外部编排掌握），只给宿主机可执行的命令。④ **命令注入防护（本功能最高风险点）**：单元名**只**从环境变量读取、**绝不**从请求体读取；且须匹配 `[A-Za-z0-9._@-]`，含 `;` / `$()` / 空格 / 换行等元字符时一律降级。字符集校验针对的**不是执行侧**（`exec.Command` 传参数组、不经 shell），而是 `command` 字段——它会被运维**复制到终端粘贴执行**，那一侧是真的有 shell 的，回显非法配置原文等于把一处配置错误变成可粘贴的注入载荷；因此非法时 `command` 只给说明性注释。⑤ 审计动作 `maintenance.service_restart`（受理记 `ok`、降级记 `failed`）。测试：接口层 8 条 + 领域层 8 条 + 配置 2 条，含「响应送出时重启尚未发生 → 延迟到期后才发生」的顺序断言（用桩记录两次调用时刻，而不是靠"先重启再回响应"——那会把测试进程杀掉）。**逐条变异验证**：把后台延迟改回同步执行、去掉单元名字符集校验、让预检失败改为抛 500、去掉 `requireAdmin` 守卫，四处分别转红后还原；其中去掉白名单后注入用例的响应直接变成 `supported:true` 与 `"command":"systemctl restart jianartifact; rm -rf /"`。⑥ **实机验收推翻"只试 sudo"（本条目内的缺陷修复）**：测试站（systemd 系统级单元 `User=admin`）实测发现，**仓库自带的单元带 `NoNewPrivileges=true`，该 flag 会阻止 sudo 提权**——`sudo -n systemctl restart` 直接失败于 `The "no new privileges" flag is set, which prevents sudo from running as root`；而 `systemctl restart`（无 sudo）走 polkit 又被拒于 `Interactive authentication required`。也就是说在**项目自己的部署形态**下，原实现"只预检 sudo"会让一键重启**永远 `supported:false`**：功能从未生效，却被读成"这台机器没配好"。修复：非 root 的 `system` 作用域改为**有序候选列表 + 逐个预检 + 取第一条通过的**——候选 A 免密 sudo（预检即 `sudo -n systemctl restart --dry-run <unit>`，`sudo -n` 本身是真实授权检查）；候选 B polkit（裸 `systemctl restart <unit>`，预检改用 **`pkcheck --action-id org.freedesktop.systemd1.manage-units --process <自身 pid>`**，仅退出码 0 算已授权，1 未授权 / 2 需要交互式认证都算不可用，`pkcheck` 缺失则视为不可用并点名说明）。**不能用 `systemctl … --dry-run` 作候选 B 的预检**：实测无任何 polkit 规则时它照样退出 0，是"看着可用、真跑必失败"的假阳性；`pkcheck` 的裁决与真实重启结果一致。两者都不可用时 `reason` **同时**给出两条路径各自的原因，运维据此判断该补 `NOPASSWD` 还是该补 polkit 规则（规则示例见 `docs/OPERATIONS.md`）。**同源不变量**：预检首次返回 `RestartPlan`（同时携带"是否可用、原因、选中的命令"），响应的 `command` 与 `RestartAfter` 真正执行的命令都取这**同一个值**——执行侧不再"重挑一次"（它在响应之后执行，重挑可能与预检结论不同，于是要么跑另一条命令、要么什么都不跑却已经回了 202）。测试：领域层 13 条（含 pkcheck 退出码表驱动、假阳性守卫、零值 `RestartPlan` 纵深防御）、接口层 6 条（含三种情形各一条"响应 command 与执行命令字符级一致"断言）；**逐条变异验证**：① 把候选 B 预检换回 `--dry-run`、② 回显固定候选 A、③ 执行侧固定 sudo、④ pkcheck 非 0 也算通过——四处分别转红后还原（①在无规则时报 `supported:true`，④直接把 202 的 `supported` 写成 `true`）。⑦ **实机验收发现"同一台机器上两条命令、其中一条照做必失败"（本条目内的第二处缺陷修复）**：测试站是**仓库自带的部署形态**——`deploy/systemd/jianartifact.service` 带 `NoNewPrivileges=true` 且已装 polkit 规则，`POST /api/v1/maintenance/restart` 如实返回 `supported:true` 与 `command: "systemctl restart jianartifact"`（**不带 sudo**，走 polkit，这条在该机器上真的可用）；但前端「待重启」说明弹窗里那条可复制命令是**硬编码**的 `sudo systemctl restart jianartifact`（`RESTART_COMMAND` 常量），而它因 `NoNewPrivileges` 阻止 sudo 提权在**同一台机器上必然失败**——于是同一个界面同时显示两条命令，照做的那条是陷阱。**根因不是"抄错了一条命令"，而是"命令本身不该由界面硬编码"**：命令随部署形态而变（root / 免密 sudo / polkit / systemd-user / docker），只有后端知道本机哪条真的可用。修复：在既有路径 `/api/v1/maintenance/restart` 下新增 **`GET`**（`operationId: getRestartCapability`，见 `docs/API.md`），**只读探测、绝不排定任何重启**——返回与 `POST` 相同的 `MaintenanceRestartResult`（`supported` / `command` / `reason` / `scope`）但**不含 `delayMs`**（没有受理就没有延迟可言）；探测口径与 `POST` 完全一致（同一个 `Probe`，并把 `RestartPlan` → `MaintenanceRestartResult` 的映射抽成共用函数，避免两处漂移），因此界面回显的 `command` 与 `POST` 受理后真正执行的命令同源。**为什么必须另开只读端点而不能拿 `POST` 当探测**：`POST` 在 `supported:true` 时会当场排定重启（`RestartAfter`），前端若"打开弹窗即调用它取 command"，就变成**打开弹窗就重启服务**——所以 handler **只调 `Probe()`、绝不调 `RestartAfter`**（注释里写明了这条理由），把"读能力"与"触发重启"在接口层彻底分开：读操作无论被轮询多少次都不会让进程重启。本端点**不写审计**（只读探测不改变任何状态，与作业清单查询一致；否则界面每次打开弹窗都会在审计库留一条记录，把真正的重启受理淹没），鉴权沿用 `requireAdmin`（401 / 403），依赖缺失 503。测试：接口层 6 条（401/403、可用时 200 + `supported:true` + 非空 command 且**零次 exec**、降级时 200 + `supported:false` + `reason` + 非空 command 且零次 exec、自带部署形态下回**不带 sudo** 的 polkit 命令、只读不写审计、未装配 503），其中"零次 exec"是**本端点最关键的不变量**，断言时刻刻意等过 `restartDelay` 之后再看（否则"已排定但还没到执行时刻"会被误判为通过）。**逐条变异验证**：① 在只读 handler 里也调 `RestartAfter`（模拟"顺手排定重启"）→ 零次 exec 断言转红（`只读探测排定了重启：探测端点绝不能触发重启（否则打开页面即重启）`），还原后转绿；② 去掉 `requireAdmin` → 401/403 用例转红（匿名拿到 `200` 且响应里带出 `"command":"sudo -n systemctl restart jianartifact","supported":true`），还原后转绿。⑧ **部署指引修正：推荐 rootless 用户级单元这一正规方案（仅文档，无代码改动）**：上一条把"polkit 规则"写成了自带单元下真正可用的路径，这个结论**建在一个偏离自带设计的部署形态上**——测试站当时用的是系统级单元（`/etc/systemd/system`，含 `User=admin` + `NoNewPrivileges=true`），而**仓库自带的 [`deploy/systemd/jianartifact.service`](deploy/systemd/jianartifact.service) 其实是 rootless 用户级单元**（`~/.config/systemd/user`，`systemctl --user` 管理），[`deploy/deploy.sh`](deploy/deploy.sh) 的默认 `SYSTEMD_SCOPE` 也是 `user`——**用户级才是仓库的正规形态，它不需要 sudo、不需要 polkit、也不受 `NoNewPrivileges=true` 影响**（该加固项限制提权，而用户级重启走用户自己的 systemd 实例），因此先前为了在偏离形态下跑通功能而加 polkit 规则，是**把问题解法建立在了错误的前提上**。现按**实机验证**重写 [`docs/OPERATIONS.md`](docs/OPERATIONS.md) §1.7 的一键重启章节：用与服务进程**完全相同的限制**（`systemd-run --uid=admin --property=NoNewPrivileges=true`）执行用户级操作，`systemctl --user list-units` 与 `systemctl --user restart <探针单元>` 均 **exit=0**，即该路径确凿可用；同时如实记下两个前提——**可达性**（服务进程环境里没有 `XDG_RUNTIME_DIR`，实测计数为 0，需在单元里显式给出 `Environment=XDG_RUNTIME_DIR=/run/user/<uid>`）与**常驻性**（用户未登录也要常驻必须 `loginctl enable-linger <user>`，即 lingering）。文档随之从"候选 A 免密 sudo / 候选 B polkit"改写为**四种部署形态按推荐度排序**：① rootless 用户级单元（`JIAN_RESTART_SCOPE=systemd-user`，推荐）② 系统级单元 + 免密 sudo（`system`，须 `NoNewPrivileges=false`，否则 sudo 报 `The "no new privileges" flag is set`）③ 系统级单元 + polkit 规则 ④ 容器 / Docker / K8s。**polkit 被明确降格为"不推荐、仅作最后手段"**：标注它是非常规做法、**权限边界比功能实际需要的大**，并给出**两种写法与各自代价**——宽版（`action.id == "…manage-units" && subject.user == "…"`）能生效但等于让该用户管理**任意**单元，窄版（再加 `action.lookup("unit") == "jianartifact.service" && action.lookup("verb") == "restart"`）更安全却**探测不出来**（非 root 进程无法向 `pkcheck` 传 unit / verb 细节，实测报 `Only trusted callers … can pass details`），后果是一键重启降级成"显示命令手动执行"；"更安全的规则"与"功能真的可用"在该形态下无法兼得，这正是它不该优先选的理由。测试站上那条 polkit 规则已删除。既有内容（顺序契约、能力探测与降级、命令注入防护、`pkcheck` 退出码语义、只读探测端点、已知近似）保留并同步更新。⑨ **执行路径改为「进程自替换」（`syscall.Exec`），不再调任何外部命令（本条目的第三处修正，也是前两处修正的收口）**：⑧ 得出的"推荐 rootless 用户级单元"结论虽然正确，但它仍把功能可用性**押在部署形态上**——实机验收发现，在"系统级单元 + `User=<非 root>` + `NoNewPrivileges=true`"这种形态下两条外部命令路径都会被拦住（`sudo -n systemctl restart` 报 `no new privileges flag is set`，裸 `systemctl restart` 走 polkit 被拒于 `Interactive authentication required`），只能靠一条**非常规的 polkit 规则**救场；而那条规则被否决——它的权限边界（让该用户能管理任意单元）比功能实际需要的大得多，属于为了"重启自己"去申请"重装系统"的权限。**根因是路径选错了**：`jianartifact` 本就有权执行自己的二进制（systemd 的 `ExecStart` 就是它），`syscall.Exec` 只是"同一个进程换一份新映像"，**不引入任何新权限**——于是 `NoNewPrivileges`、polkit、sudoers、linger 全都绕开了（它们不是被绕过，而是压根不需要）。现把执行路径整个换成自替换：① **不需要任何额外授权**，用户级单元 / 系统级单元（含 `NoNewPrivileges=true`）/ 容器内一律可用，部署形态不再是功能可用性的前提；② **`Probe` 判据随之换掉**——不再探测 sudo / polkit / `--dry-run`，改为"本平台是否支持自替换（`syscall.Exec` 仅 Unix）→ `os.Args[0]` 能否解析为可执行文件（`exec.LookPath`，如实判断相对路径与符号链接，不假装成功）→ 重启触发点是否已接线 → 单元名与作用域是否合法"四条，任一不满足即 `supported:false` + 点名原因的 `reason`（**不是报错**）；③ **删掉为外部命令服务的一整套机制**：候选列表、`restartCandidate`、`restartProbeMode`、`restartProbeFailure`、`pkcheck` 常量与 `restartPolkitAction`、`restartExitCoder` / `restartExitCode`、`withDryRun`、`RestartRunner` 与 `osRestartRunner`（不留死代码）；**保留**单元名白名单（`restartUnitPattern`）与 `restartConfigProblem` / `restartTruncate`——它们保护的已不是执行侧，而是 `command` 这条**会被复制到终端粘贴执行**的手工命令；④ **`command` 的语义在契约里也改了**（`api/openapi.yaml` 的 `MaintenanceRestartResult.command` description、两个端点的 description 与 `docs/API.md` / `docs/OPERATIONS.md` 同步）：它**不是本进程将要执行的命令**（本进程走自替换），而是"若你想手工重启，敲这条"，仍按 `JIAN_RESTART_SCOPE` 生成（`systemd-user` → `systemctl --user restart <unit>`；`system` 非 root → `sudo -n systemctl restart <unit>`；root → `systemctl restart <unit>`；`docker` → `docker restart <unit>`），仍始终返回、可直接复制；⑤ **平台适配**：`syscall.Exec` 是 Unix 专有接口，按仓库既有先例（`cmd/jianartifact/file_owner_unix.go` / `_windows.go`）新增 `internal/domain/restart_exec_unix.go`（`//go:build !windows`，真实 `syscall.Exec`）与 `restart_exec_windows.go`（`//go:build windows`，返回"本平台不支持"），Windows 上 `Probe()` 如实回 `supported:false`（开发机 `go build ./...` 照常过，正式部署是 Linux）；⑥ **`main.go` 的接线**：`wiring.go` 装配期把"该重启了"接成通道信号（缓冲 1，重复触发直接丢弃）注入领域服务，`main` 的 `select` 新增 `case <-restartCh` 分支，动作顺序是**先优雅关停 HTTP 与 TLS（复用既有 15s 上限的 `Shutdown`）、再 `syscall.Exec`**——反过来会有两个具体后果：新旧映像争抢同一端口（`Shutdown` 之后端口才是干净的），以及旧映像的在途请求与数据库连接随着 Exec 一起消失、没有任何收尾机会；**关停失败如实报错并返回、绝不静默 Exec**（否则"没关干净"这件事会被藏起来，新映像看到一份半关停的现场而日志一字不提）。自替换前打印一行明确日志，失败时错误里带上 `argv[0]` 与底层原因。**这条链路才是本功能的意义所在**：新映像会重跑整个启动流程，因而 `ApplyPendingRestore`（`main.go` 启动早期，在打开数据库连接之前）得以执行——备份恢复的落地就是靠这一次启动；⑦ **必须用 `os.Args[0]`、绝不能用 `os.Executable()`（实测结论，已写进代码注释）**：部署脚本用 `current` 符号链接指向具体 release 目录，实测进程状态为 `argv[0]` = `/opt/jianartifact/current/jianartifact`、`/proc/PID/exe` = `/opt/jianartifact/releases/<时间戳>/jianartifact`。探针实测：把 `current` 从 v1 切到 v2 后，用 `argv[0]` 自替换加载到 **v2**，而 `os.Executable()`（返回 `/proc/self/exe` 的内容，符号链接已解析）加载到 **v1**——也就是说改成后者会让"部署切换后点一键重启"**等于没有切换**，旧映像重新跑一遍，且没有任何报错。⑧ **实测可行的两条依据**：在 `NoNewPrivileges=true` + `ProtectSystem=strict` + `ProtectHome=read-only` + `PrivateTmp=true` 全套加固下自替换成功，且 **PID 不变**（27271 → 27271，不是 fork）、**PPID 仍为 1**（仍是 systemd 直属子进程，未脱离管控）、`status=0/SUCCESS`；即使**故意不关闭**监听器直接 Exec，新映像仍能成功重绑同一端口（Go 的 socket 默认 `CLOEXEC`，内核在 exec 时自动关闭旧 fd）。**两个如实记录的语义差异（文档同步写明）**：**① unit 状态不变**——systemd 视角上单元从未停止，PID 不变、`ActiveEnterTimestamp` / `NRestart` 不动，`systemctl status` 看不出重启过（判断是否真的重启请**看应用日志的启动横幅**）；这是**可以接受的**：本功能要的是"应用重新执行一遍启动流程"（备份恢复落地、搬迁起服），而不是"让 systemd 记一笔重启"；**② 存在短暂的中断窗口**（先优雅关停、再替换映像，通常几秒）。测试：领域层 15 条 + 接口层 13 条，判据全部改为自替换语义；**逐条变异验证**：① 让 `RestartAfter` 同步触发（不延迟）→ 领域层顺序断言转红（`RestartAfter 返回时不得已触发重启`）、接口层转红（`响应送出时不得已触发重启，实际 1 次`），还原转绿；② 让 `Probe()` 在 `os.Args[0]` 不可解析时仍返回 `supported:true` → 领域层转红（`argv[0] 解析不了时不得判定为可用，得 {Supported:true ...}`）、接口层转红（降级用例拿到 202 与 `"supported":true`）、只读探测转红，还原转绿；③ 让只读探测端点也触发自替换（模拟"打开页面即重启"）→ 转红（`只读探测排定了重启：探测端点绝不能触发重启`）**且新增的 `RestartAfter` 记录型断言一并转红**（`只读探测绝不得调用 RestartAfter，实际 1 次`），还原转绿；④ 把自替换改用 `os.Executable()` 而非 `os.Args[0]` → **首轮变异未被抓住**（开发机上 `os.Args[0]` 与 `os.Executable()` 恰好同值，断言恒真），已改为"把 `os.Args[0]` 换成与真实可执行文件明显不同的哨兵值"并正面断言"结果不得等于 `os.Executable()`"，同时把 `Probe` 那条用例改成**不注入 `Argv0`**（注入桩会把"默认值取自哪里"整个绕过），重跑后两条分别转红（`自替换必须用 os.Args[0]="/opt/jianartifact/current/jianartifact"，实际 "…\domain.test.exe"`），还原转绿。
- **用户组与更细粒度权限动作（FR-36）**：① **用户组**作为新的授权主体：新增 `user_group`（组名全局唯一、可选说明）与 `user_group_member`（复合主键 `(group_id, user_id)`，两侧外键均 `ON DELETE CASCADE`），一个用户可属多个组；组本身不产生权限，真正生效的是把组写进仓库 ACL，等价于给组内每个成员各写一条授权，且成员变动不必重改 ACL。② **动作集由三档扩为六档** `read` / `write` / `publish` / `delete` / `acl_manage` / `admin`，蕴含关系定义为「判定动作 → 能使之通过的已授权动作集合」：`read` ← `read`/`write`/`admin`、`publish` ← `publish`/`write`/`admin`、`write` ← `write`/`admin`、`delete` ← `delete`/`admin`、`acl_manage` ← `acl_manage`/`admin`、`admin` ← `admin`；**`write` 蕴含 `read` 与 `publish`，但不蕴含 `delete`**（`delete` 需单独授予，与「发 / 删分离」的取舍一致，见规格 §6），`publish` / `delete` / `acl_manage` 只蕴含自身、彼此互不满足且**都不蕴含 `read`**（只发发布权的人不应因此读全仓，管 ACL 不等于能读内容）。**未登记的动作一律按「只由自身满足」判定**，不会被宽泛授权放行。③ **判定主体**改为 `repository.Subject{UserID, GroupIDs}`：一次判定同时看「用户自身 ACL」与「所属任一组的 ACL」并取并集，组归属由 `RepositoryService.SubjectFor` 单点展开（避免某端点漏展开即成提权漏洞）；成员关系变化**即时生效**（无缓存、无快照）；**匿名主体不属于任何组**，沿用既有匿名与 public read 语义；全局管理员（role=admin）行为不变。④ **协议层统一**：各格式（Raw/Maven/npm/NuGet/OCI/Cargo/PyPI）的上传统一判定 `publish`，删除判定 `delete`；cargo yank 作为**索引状态变更**仍走 `write`（它既不上传新制品也不移除制品本体）。向后兼容由 `write ⊇ publish` 保证——存量 `write` 授权照旧可发布。删除权则由「仅全局管理员」放开为可授予的仓库级 `delete` 动作（见下方「变更」段）。⑤ **八个组管理端点**（均仅管理员，见 `docs/API.md`）：`GET/POST /api/v1/user-groups`、`GET/PATCH/DELETE /api/v1/user-groups/{id}`、`GET/POST /api/v1/user-groups/{id}/members`、`DELETE /api/v1/user-groups/{id}/members/{userId}`；建组重名 409，成员重复加入幂等，删组连带清理成员关系与该组的全部 ACL 条目（不留指向已消失组的悬空授权）。⑥ **审计**：新增动作 `group.create` / `group.update` / `group.delete` / `group.member.add` / `group.member.remove`，`entityType` 为 `user_group`。⑦ **范围外（本期不做）**：目录组 → 角色 / ACL 映射（LDAP / OIDC / SCIM）、组嵌套、路径级授权、以及**管理面的细粒度授权**——管理端点仍沿用既有 `IsAdmin()` 粗粒度守卫，`acl_manage` 未接到管理端点。规格见 [`docs/specs/0.12.0-user-groups-and-fine-grained-actions.md`](docs/specs/0.12.0-user-groups-and-fine-grained-actions.md)
- **上游断路器补强（FR-43）**：在 FR-112 已交付的 auto-block 基础上补齐三处。① **退避起始可配置**：新增环境变量 `JIAN_AUTO_BLOCK_BASE_SECONDS`（默认 `40` 秒）设定首次失败的阻止窗口时长，后续档位仍**每档翻倍**（40s→80s→160s，几何增长）；非法值（非数字 / 0 / 负数）回落默认且不报错，与既有数值类配置一致。**不做退避上限与抖动**——避免改变既有退避曲线形状。② **断路器运行指标**：新增 counter `jianartifact_upstream_block_events_total{reason}`（`reason` 为闭集枚举 `probe_failed` / `upstream_error`，闭集外取值忽略）与 gauge `jianartifact_upstream_blocked_repositories`（当前处于阻止态的仓库数，刻意**不带仓库名标签**以免标签基数随仓库数无界增长）；两者经既有 `/metrics` 暴露，且保持「无样本也输出 `# HELP` / `# TYPE`」的既有约定。③ **新增连接状态 `HALF_OPEN`**：`ConnectionStatus.status` 枚举由五值扩为六值，表示「阻止窗口已到期、后台正在试探上游」——半开期与 `AUTO_BLOCKED` 同属阻止态，但对外取值分开，管理界面据此区分「窗口内封锁」与「正在试探」（不再把半开期误报为自动阻止）；前端仓库列表/详情/状态面板/环形图与 devmock 均同步该枚举。规格见 [`docs/specs/0.12.0-upstream-circuit-breaker.md`](docs/specs/0.12.0-upstream-circuit-breaker.md)
- **周期化自动备份、保留策略与恢复演练（FR-40）**：把 v0.8.0 交付的**按需**备份包能力补齐无人值守与可验证两块。① **周期化自动备份**：新增环境变量 `JIAN_BACKUP_AUTO_INTERVAL`（秒，**默认 0 = 关闭**），>0 时经既有调度器注册 `backup-auto` 作业；每轮生成**完整热备份包**（不生成差包——无人值守下基线选择不可控且易形成长链），手动与自动共用同一生成槽位、撞车时跳过本轮且**不算失败**（正常竞争而非故障）。② **保留策略**（自动备份的隐性硬依赖，否则磁盘会被自己打满）：`JIAN_BACKUP_KEEP_LAST`（默认 7 份）与 `JIAN_BACKUP_RETENTION_DAYS`（默认 30 天）**同时生效、取更严格者**，只淘汰 `origin='auto'` 的包（**手动备份是人工决策产物，永不自动删除**），被差包引用为基线的包跳过而非报错；作为作业收尾步骤执行，首个自动备份生成后即应用一次。③ **来源可分辨**：迁移 `0047` 给 `backup_package` 加 `origin`（`manual`/`auto`，既有行按列默认值补为 `manual`，语义正确），列表据此区分。④ **恢复演练与可查报告**：对任意已完成包，在**隔离临时目录**内真解包、真校验——归档完整性（浅/深两级，深校验逐 blob 比对内容摘要）→ 解包 → 解出的 SQLite 跑 `PRAGMA quick_check` 并确认关键表可查询 → blob 规模与 manifest 比对；**逐项结论与耗时落库**为报告（迁移 `0047` 新增 `backup_verification` 表，随包级联删除），可查历史而非只有一次性响应。检查项**失败后仍继续后续项**，便于一次看清"哪里坏了"；演练失败同样落库（`ok:false` + 失败原因），不粉饰。⑤ **演练是纯只读路径**：绝不写 `restore-staging/` 与 `restore.pending`，因此不会在下次重启触发真正的数据替换——这是与真导入/恢复的关键区别，并有专门用例守护。⑥ **接口**：`POST /api/v1/backups/{id}/drill` 采用 **202 + 轮询**（深校验耗时与包体积同阶，同步等待会超时），省略请求体即按 **deep** 执行；`GET /api/v1/backups/{id}/verifications` 分页读报告；`BackupPackage` 加 `origin`（加性），新增 `BackupVerificationReport` / `BackupVerificationList` / `DrillBackupRequest` 等 schema；CLI 另有 `backup drill <包标识> [--level shallow|deep]`（同步等待，默认 deep）。⑦ **界面**：备份列表新增来源列，新增演练入口与报告查看（含历史）、自动备份与保留策略卡；配置卡**只读展示并如实标注环境变量与"间隔改动需重启生效"**，不提供就地编辑（后端本批未暴露该配置的读写端点，见 `docs/API.md`）。⑧ **范围外（本期不做）**：备份包加密与异地自动同步、自动备份的差包链管理、演练结果的趋势分析与告警联动、手动备份的自动保留。规格见 [`docs/specs/0.12.0-auto-backup-and-drill.md`](docs/specs/0.12.0-auto-backup-and-drill.md)

### 变更

- **备份与搬迁页把两张动作卡收成标题行里的一排按钮（FR-40 追加五）**：用户复核追加四后的界面，提出「顶上两块太突兀了 需要收纳成按钮放到 备份与导入事件」——那两张常驻卡（立即备份 / 搬迁到新机器）合计约 173px，而它们承载的只是几个按钮加三行说明文字。现整体收成**挂在「备份与导入事件」标题行右侧的一排按钮**：`生成备份包` / `冻结写入`（冻结态转为橙色 `解冻写入`） / `从 URL 导入` / `上传本机文件` / `自动备份设置` / `说明`。页面因此由三段收敛为**两段**（内联导入区 → 关键事实 → 事件表+动作条），省下的高度全部给事件表：1440 宽下事件表区块实测 687px（原 502px）。**说明文字没有被删，只是换了载体**：卡里那三行迁进 `OpsHelpButton` 的「说明」气泡（该组件本就为此而生，其注释写明"说明内容对操作不是必需的，常驻卡片会一直占用纵向空间"）；实现上**刻意复用原有文案键**，只新增气泡标题——若另写一套新文案，旧键会变零引用，等于把说明悄悄删掉（本次先写成新键，被自己的守卫抓出，已改回复用）。**冻结状态不再单列一只常驻 `StatusPill`**：它与按钮一一对应（可写 ↔「冻结写入」、已冻结 ↔「解冻写入」），由按钮自身的橙色变体与文案体现，自动解冻时间挂在该按钮 `title` 上（`formatStamp` 过的可读时刻，不是裸 ISO）；**解冻入口必须常驻可达**这条不变——冻结期间若没有它，就会形成「已冻结 → 按钮禁用 → 打不开弹窗 → 永远解不了冻」的死锁。一卡一个主按钮的层级差异改由 `variant` 保留（主路径 `filled`，其余 `default`），否则一排同款式按钮又看不出该先按哪个。**窄视口靠 `Group wrap="wrap"` 折行而不横向溢出**：实测 1440 宽动作条 678/678 不溢出、标题行 55px；1100 宽折两排 85px；880×622 折三排 125px、事件表仍有 264px（高于 150px 地板值）。测试同步：原「三段式顺序」守卫改为断言**动作条位于事件表区块之内**（挪回页面流即转红），另外新增「说明气泡仍能读到那三段文字」与「初始可写 / 冻结后转解冻且按钮无 `title`」两组守卫，均逐条变异验证转红后还原（动作条挪出标题行、删掉解冻入口两个变异分别转红）。规格见 [`docs/specs/0.12.0-auto-backup-and-drill.md`](docs/specs/0.12.0-auto-backup-and-drill.md) §7.4。

- **仓库删除权从「仅全局管理员」放开为可授予的仓库级动作**：此前协议层删除（Raw `DELETE`、批量制品删除）由 `IsAdmin()` 硬守卫，普通账号要能删某仓就必须被加为全局管理员（等于把实例级权限当仓库级权限发）。现在删除统一判定 `delete` 动作，可由管理员以**用户或组主体**按仓库授予；全局管理员分支原样保留。**`write` 不蕴含 `delete`**——因为此前 `write` 事实上从未蕴含删除（旧 `HasPermission` 只认三档、其余动作一律映射到 `admin`），故本次不会让任何既有账号失去删除能力；`delete` 的净变化是**放开**而非收紧。需要仓库级删除能力的账号请显式补授 `delete`（或继续依赖全局管理员），不要以为升级会自动获得。

- **`acl` 表重建与数据语义变化（升级注意事项）**：迁移 `0046_user_groups.sql` 按仓库既有范式（建新表 → 复制 → 删旧 → 改名 → 重建索引）重建 `acl`，新增 `subject_type`（`user`/`group`，恰有其一）与 `subject_group_id`，`action` 的 CHECK 放宽到六值；唯一性由原 `UNIQUE(repository_id, subject_id, action)` 改为**两条 partial unique index**（`idx_acl_user_grant` / `idx_acl_group_grant`），因为 SQLite 唯一索引把 NULL 视作互不相同、引入可空列后原约束会失效。既有行全部按**用户主体**原样复制（`subject_type='user'`），`(repository_id, subject_id, action)` 三元组与 `id` 逐条不变，故既有三档授权升级后行为一致。**这是一次性全表复制，不提供 down migration**：升级前请备份数据库，降级（回退版本）时旧版本代码不认识新列，必须从备份恢复，不能只回滚代码。详见 `docs/OPERATIONS.md` §5.1

### 修复

- **补齐「用本机备份包恢复」入口，并修掉演练永久卡在「进行中」（FR-40 追加六）**：用户复核时提出四个问题，逐条落到改动。① **「为什么只能备份无法恢复」**——恢复功能一直存在（URL 拉取 / 上传 / CLI 三条通道都会替换本机数据并需重启），但**没有以「恢复」的名义出现**：「导入 / 上传」在用户心里只是「把东西弄进来」，且这三条都要求包来自**外部**，于是页面上明明列着可用的包却没有任何按钮能用它恢复。现新增 `POST /api/v1/backups/{id}/restore`与事件表「⋯」菜单里的**「用这个包恢复」**（含确认弹窗：讲清会替换本机数据、需重启、留有回滚备份，并把覆盖开关**前置**而不是等失败再让人猜）；同时把动作条两个按钮改成能表达恢复的措辞（「从 URL 恢复」/「上传包恢复」）。新端点**刻意复用**既有编排（`StartLocalImport` → `Stage` → `restore.pending`），不另写一条恢复路径——否则会出现「两条路径行为不一致」这个更糟的后果；`origin` 记为 `local` 以便在事件表里与另外三条区分。② **「校验和深度校验有什么用、为什么不写提示」**——两者此前只有一个 toast，没有任何解释。现给三项动作各加悬停说明，并在「说明」气泡里集中讲清递进关系：校验只验归档结构（快）⊂ 深度校验逐 blob 比对内容摘要（慢）⊂ 演练在隔离目录真解包真恢复（只读）。③ **「演练又是干什么的」**——同一处说明气泡里给出定义与「回答什么问题」。④ **「点进去一直卡在演练进行中」**——这是**真 bug**，已复现并修复：测试站配置为间隔 30s + `KEEP_LAST=2`，一个 `auto` 包约 60–90 秒就被保留策略淘汰；而弹窗的停轮询条件只有「出现一条 finishedAt ≥ 受理时刻的报告」，于是包在演练途中被淘汰后 `verifications` 恒 404、轮询**永不停止**，用户看到的就是永久「演练进行中…」。更糟的是那个 404 落在 `refreshError` 上（旧数据在场），而判据只看 `state.error`，连「包已被淘汰」的提示都不显示。现三处齐修：404 终止轮询（两个 error 都看）、加 10 分钟超时兜底（报告永远不会到时如实说明并可刷新）、并点明「本次演练已被中断」（否则用户以为是自己点错）。三条守卫均**变异验证转红后还原**（只看 state.error / 轮询不因 404 停止 / 超时不生效），其中「轮询是否真的停下」这条直接观察请求是否继续发出——只断言文案抓不到它。

- **CLI 不再静默回落到另一个数据目录（FR-40 追加六）**：CLI 是独立进程，读不到 systemd 单元的 `EnvironmentFile`，因此 `JIAN_DATA_DIR` 缺失时会静默回落 `./data`——它操作的是**另一个库**（看不到服务侧真实的包，报"归档不存在"，看着像功能缺陷），且 `Load()` 会 `MkdirAll` 数据目录并生成 `jwt.secret` / `jianartifact.db`，于是一次误跑就**在原地建出一个完整的空实例**。这正是用户反馈的"莫名其妙的数据目录"。现在数据目录按三级解析并把结果与依据打印到 stderr：① `JIAN_DATA_DIR` 显式设置 → 以其为准（唯一权威）；② 未设置时自动发现正在运行的实例（读 `/opt/jianartifact/jianartifact.env` 等常见部署环境文件）——**唯一候选**就用并打印说明，**多个不同候选**则列出候选并报错退出（不猜，猜错就是操作到另一个库）；③ 探测不到才回落 `./data`，并打印警告说明"若本意是操作已有实例该怎么办"。核心原则是**自动发现可以是自动的，但不能是静默的**：任何一次解析都要能回答"这次用的是哪个目录、依据是什么"。**验收时又发现并补上一处不完整**：第一版只在"自动发现"分支采纳实例配置，显式 `JIAN_DATA_DIR=…` 时既不发现也不采纳，于是 CLI 仍以为密钥"未设置"，**在服务的数据目录里现场生成 `jwt.secret` 与 `migration-credential.key`**（只读命令也写文件）并打印误导性警告；现改为"显式指定该实例目录时同样采纳其 env"（且**只在目录匹配时**采纳，避免拿到别的实例的配置）。同时修复另一处会把服务打死的缺陷：`restore.pending` 读不了（典型成因是用 `sudo` 跑 CLI 导入、标记属主成了 root）时，服务改为**跳过该恢复、打印含 `chown` 修复命令的告警，但正常起服**；标记与暂存目录原样保留。此前把读取失败当致命错误，后果是启动失败 → systemd 重启 → 仍读不了 → 再失败的**重启循环，全站 502**（实测重启计数冲到 86），现场完全看不出"改属主"才是解法。取舍是有意的：待生效恢复是"稍后要应用的维护动作"，不该用可用性去换一个可延后动作。此外 CLI 导入成功后若检测到标记属主与当前进程不同，会**当场提示**去改属主，让问题在产生的当口暴露而不是等下次重启。新增 `GET /api/v1/backups/restore-status`（仅管理员）并让界面在有待生效恢复时把两个导入按钮转橙、点开可看原因与可复制的重启命令——该状态此前只体现在导入记录行的 `pending_restart` 徽章上，徽章被更近的事件挤出视野后就再也看不出"为什么导入一直被拒"（用弹窗而非常驻条：状态是临时的，常驻会白占高度，用户先前正是以"这个汇总提示不要"为由删掉了待处理条）。测试：`classifyMarkerReadError` 两个分支各有断言（权限错误可跳过且给出可照做原因／其他错误仍致命）、数据目录解析五条分支（显式优先／发现唯一／歧义不猜／同值不算歧义／回落告警）、env 文件解析边界（注释、export、引号、值内含 `=`），逐条变异验证转红后还原。

- **自动备份配置改为可在管理端直接修改（FR-40 追加）**：三项配置（间隔 / 保留份数 / 保留天数）原先只能经环境变量配置、且**改动需重启进程**，界面上的卡片刻意做成只读说明并如实标注该限制。现纳入既有 settings 通道：可在「迁移与备份 → 备份与搬迁 → 自动备份与保留策略」卡上直接编辑，**保存即生效、无需重启**。实现上不改造调度器（其间隔在注册时固定），而是让 `backup-auto` 作业以固定 30 秒**心跳**常驻、每轮重读配置自行判断是否到点——这正是热生效的全部依赖。三个环境变量的角色随之降级为**首次部署的初值**（仅在该组配置尚不存在时写入一次，此后以数据库为准），避免"界面改完重启又被 env 覆盖回去"的困惑。三处的 `0` 均为合法取值（关闭 / 不限制），前后端都不得按"空值"处理；写入时校验拒绝负数与超限值（30 天 / 1000 份 / 3650 天）。因作业改为常驻，「维护作业」页**始终**能看到 `backup-auto`，**看到它不再等于自动备份已启用**——是否备份取决于间隔是否为 0，界面与 `docs/OPERATIONS.md` 已如实说明这一语义变化。
- **备份列表操作列的按钮被压成残片（FR-40 追加）**：表格为 `layout=fixed` 且操作列写死 144px，而「获取链接 / 校验 / ⋯」三个按钮的自然宽度合计约 175px；flex 容器把按钮压缩后，Mantine 的 `Button-label`（`overflow:hidden`）**静默**把文字压到几个像素——实测「校验」可见宽仅 3px（需要 24px），肉眼即"按钮被裁切"。现改为：按钮禁止压缩（`flexShrink:0`，宁可整列溢出并由外层既有横向滚动承载）、操作列按实测需求给足 176px 并重新分配其余列宽、窄视口下行内按钮折叠为图标（配 `title`/`aria-label`）。
- **备份包与导入记录聚合到同一张表：删掉待处理条与「导入与传输」弹窗，导入改为就地展开（FR-40 追加四）**：用户复核追加三后的界面，指出三件事——① 「有 1 项需要你确认 / 1 次导入失败」这个汇总提示不要；② 备份包和导入记录应合到一个界面里，不必拆两处；③ 整个「导入与传输」模态框删掉，按钮、记录、包都聚在这一个界面内，**按事件顺序更好分清**。据此：**页面由四段收敛为三段**——① 动作（两张卡；搬迁卡的两个按钮改为**就地展开**表单，点已展开的那个收起、点另一个切过去，展开中的按钮转 `light`）→ ①附 内联导入区（收起时 `display:none`）→ ② 关键事实 → ③ **统一事件表**（本机备份包与导入记录按 `createdAt` 倒序混排，`flex:1` 独占剩余高度）。**为什么合并不会出现「同一件事两行」**：两类对象**不相交**——本机包登记在 `backup_package` 表；导入记录是把别处的包收进来（走 `RestoreService.Stage` 暂存、写 `restore.pending`），记录里的 `packageId` 是包内携带的**源机器**标识，不进本机备份包列表。两张表后端都按 `ORDER BY created_at DESC, id DESC` 返回，前端 `mergeBackupEvents` 用同一口径重排（平局按标识降序），**不依赖两个异步列表的到达顺序**——否则同一份数据可能两次渲染出不同次序。行的类型由**类型徽章**（「本机包」/「导入记录」，outline 变体，`data-row-kind` 便于测试定位）显式标注，不靠 `bk-` / `imp-` 前缀让人猜；来源列（手动/自动 vs URL/上传/CLI）单独也能区分两类。**删待处理条为什么不丢信息**：它当初存在是因为记录藏在弹窗里看不见，必须有汇总把用户引过去；记录常驻后，`pending_restart` 行直接在状态单元格写明「需重启服务后生效」、`failed` 行就地显示错误摘要，冻结则在「立即备份」卡上有常驻 `StatusPill` 与解冻入口。**角标一并删除**（它依赖的"需注意计数"现在等于"表里可以自己数的行"）。**列结构**：原导入记录表的独立「进度」列取消（进度条与"需重启"提示挪进状态单元格，它们本就是状态的补充说明），新增「错误」列、宽度从「包含」列让出（"包含多少"并入标识单元格第二行，信息一条没丢）。**内联区必须常驻挂载**（隐藏只置字面 `display:none`）：分片上传面板要在挂载时探测未完成的上传会话（`localStorage` 的 `uploadId` → `GET` 拉回 `uploadedChunks`），条件卸载会让续传提示与"只补缺失片"全部失效——与它在 `Collapse`（首版）、Modal（追加二）里踩过的坑同源；也不能用 Mantine 的 `activity` 隐藏模式（会停掉子树副作用）。上传进行中禁止收起面板（进度条与取消入口都在里面），按钮置 `disabled` 并说明原因。**顺带修掉一处我在本次改动中引入的回归**：`UrlImportPanel` 原本靠 `onImported` 回调刷新，改为内联后回调被去掉却没接上替代通道，会导致"提示已受理、事件表里却看不到新行"（事件表只在其内已出现非终态记录时才开始轮询）；现改为与上传通道**同一个** `REFRESH_EVENT`，两处只留一条刷新通道。测试重写并逐条变异验证（混排顺序、类型徽章、弹窗已消失、上传面板常驻挂载、导入记录行有状态无动作）。**实机验收又发现并修掉一处我在追加三里引入的缺陷**：关键事实条的「下次自动备份」把 `factNextPending`（「等待首次执行」）同时用在了两个语义相反的场合——「确实还没有自动包」与「算出的下次时刻已过、等下一轮心跳」。后者场景下实例上**已经有自动包**，说"等待首次执行"是假话，而 30 秒这类短间隔下几乎总走这一支，用户会看到与表格自相矛盾的结论（表格里明明列着 `origin=auto` 的包）；该分支的注释本已写明"如实显示'即将执行'"，实现却没照做。现拆出 `factNextDue`（「即将执行」/「Due shortly」）专用于已过点分支，并新增守卫（变异验证：改回 `factNextPending` 即转红）。规格见 [`docs/specs/0.12.0-auto-backup-and-drill.md`](docs/specs/0.12.0-auto-backup-and-drill.md) §7.4。
- **备份与搬迁页改为「任务台」信息架构：先问你要做什么，再摊开历史（FR-40 追加三）**：经三份交互方案比选后采用「任务台」，页面由「状态条 + 列表」改为**四段式**——① **待处理**（有问题才占据视觉重量：待重启生效 / 导入失败 / 写入仍冻结，一句话汇总 + 一个入口；全清时只留一行极轻的「一切正常」，避免用户分不清"没待办"与"没加载出来"）② **动作**（把两条主路径前置，每张卡**恰好一个主按钮**：备份卡=生成备份包、搬迁卡=从 URL 导入，否则用户看不出该先按哪个）③ **关键事实**（最近成功备份 / 下次自动备份 / 最近校验 / 占用 / 保留策略）④ **备份包列表**（`flex:1` 独占剩余）。**为什么宁可让列表让出 259px**（688→429px，实测仍有约 6 行）：用户来这一页几乎总是为了"打一份包"或"把包搬到新机器"，此前列表最大、两个主动作却藏在弹窗里，等于让主路径比次要路径更难走。**四项事实各有真实来源、且刻意排除了会算错的口径**：最近成功备份取列表里 `done` 且 `createdAt` 最大的一条（不依赖后端返回顺序）；**下次自动备份用「配置间隔 + 最近一个 `origin=auto` 包的生成时刻」，刻意不用 `maintenance/jobs` 的 `intervalSeconds`**——`backup-auto` 注册的是固定心跳、每轮重读配置判断是否到点，那是心跳粒度而非备份间隔，拿它算会算出一个错的时刻，也不能混入手动包（否则手动备份一次就把"下次"推后一个间隔）；最近校验读**落库**的演练报告，不借 `/verify` 的即时结论冒充历史，读不到就如实显示「尚未校验」；占用**不写成"磁盘占用"**——没有全局用量端点，它只是这一页能看到的那些包的求和，故同时给出包数。间隔 0 如实显示「已关闭」（0 是"关闭"的语义，不是"每 0 秒"）。⑤ 顺带收敛：`ImportEntry` 由自渲染入口改为**受控弹窗**（`opened`/`initialTab`/`onClose`），点哪个入口直接落到对应标签；`BackupStatusStrip` 组件随之下线（其「保留策略摘要」迁为事实条第 5 项，信息一条未丢）。⑥ **修掉两处实测发现的问题**（比对过程稿按 `doc-sync` §5 不入库）：搬迁卡的「从 URL 导入」首版照抄设计稿渲染成了默认样式，与备份卡的主按钮不对称、丢掉了"该先按哪个"的指引，已恢复主色；**矮视口下列表被 ①②③ 压成 2px**（880×622 实测，设计稿未画该形态），已给列表 `minHeight:150`（表头+一行半）并让页根容器 `overflowY:auto` 兜底。⑦ 测试 20→35 条并逐条变异验证（含 16 个变异全部转红后还原），新增守卫覆盖四段结构与顺序、待处理条有/无/计数随数据变化、**「自动备份设置」入口必须可达**（该守卫复现了本次真实发生的一次回归：重做布局时把已交付功能的入口整个弄丢，而原有用例只测弹窗内容、抓不到）、事实条五项 + 间隔 0 显示「已关闭」、上传/记录入口各自落到正确标签。⑧ **非本功能引入的一处测试竞态顺手修复**：`TestBackupDrillWritesAudit` 在全量质量门并发负载下偶发 `TempDir RemoveAll cleanup: directory is not empty`——`drill` 端点按设计 202 + 后台 goroutine 执行，而后台演练持有 fixture 的 `t.TempDir()`，该用例不等收尾就返回，清理与演练自身的 `RemoveAll` 赛跑；改用同文件既有助手 `waitForDrillSettled`（其注释本就写明"拿到报告不等于演练已彻底收尾"）后 `-count=10` 稳定。**注意：本条的待处理条与导入弹窗已被上一条（追加四）删除**，此处保留以记录演进过程。规格见 [`docs/specs/0.12.0-auto-backup-and-drill.md`](docs/specs/0.12.0-auto-backup-and-drill.md) §7.4。
- **备份与搬迁页收敛为两段式，导入与传输整体进弹窗；上传通道补齐「覆盖已有数据」（FR-40 追加二）**：① **页面只剩两段**——状态与操作条（68px 常驻）+ 备份包区块（`flex:1`，独占全部剩余高度）。上一条交付的三段式里，导入与传输区块仍要分走列表一块高度，且区块自身的"收起/展开"又是一套需要维护的状态机（自动展开判据、手动覆盖、`display:none` 常驻挂载）。现把三条通道（导入记录 / 从 URL 导入 / 上传本机文件）全部收进一个弹窗、以标签页切换，那套状态机整个消失。② **入口按钮带角标**：导入记录的「待重启服务后生效」原先是页面上唯一提醒用户去重启的地方，收进弹窗后就看不见了；角标 = 需注意的导入记录数（进行中 / 失败 / 待重启）+ 未完成的上传会话数，**刻意不计 `done`**（历史成功记录再多也不该常驻提示）。③ **弹窗与标签页都必须 `keepMounted` + `keepMountedMode="display-none"`**：上传面板要在挂载时探测"上次是否有未完成的上传会话"（`localStorage` 的 `uploadId` → `GET` 拉回 `uploadedChunks`），弹窗关闭即卸载面板的话探测永远跑不到，续传提示与"只补缺失片"全部失效——与首版踩过的 `Collapse` 收起不挂载是同一个坑；且默认的 `activity` 模式会在隐藏时停掉子树副作用、同样停摆探测，故必须显式指定 `display-none`。④ **修掉"往非空实例传包必然失败"**：实测把下载来的包传到已有数据的实例上，界面报"上传成功"、随后记录变 `failed`，错误是"目标实例非空，导入前请开启「覆盖已有数据」"——而**界面上根本没有那个开关**。根因不在后端：契约的 `CompleteBackupUploadRequest` 一直带 `sha256`/`overwrite`/`deep`，服务端也早已透传，**缺陷纯在前端**（URL 导入通道有这两个开关，上传通道从未暴露，两条形式不一致）。因 `target_not_empty` 在**异步 Stage 内**判定（HTTP 早已返回 202，失败只体现在记录终态），开关必须**前置**在提交按钮旁而不是等失败后再让用户猜；已在"选择文件"上方补上覆盖与深校验两项。⑤ 顺带收敛：`ImportSection` 拆分/重写为 `ImportEntry`（入口 + 弹窗）+ `UrlImportPanel`（表单本体），删除零引用的 `importRecordsModalTitle` / `uploadModalTitle` / `uploadOptionsHint` 三个 i18n 键；上传完成后的刷新统一走既有 `REFRESH_EVENT`，不另设回调（避免同一件事两个刷新通道）。测试新增 5 条守卫并**逐条变异验证转红**：覆盖开关透传、关弹窗重开仍能发现续传会话（去掉 `keepMounted` 即转红）、角标按需注意计数（恒为 0 或把 `done` 计入均转红）、页面流内不再有导入区块（塞回页面流即转红）。规格见 [`docs/specs/0.12.0-auto-backup-and-drill.md`](docs/specs/0.12.0-auto-backup-and-drill.md) §7.4。
- **备份与搬迁页整页重排：不再有「可展开的常驻卡」（FR-40 追加）**：上一条把配置卡改成默认折叠只治了「常驻表单」这一种发作，**展开态仍会挤压**——中文 1280×900 下列表卡 185→76px、表格可视区仅 **19px**；英文 1100×900 下列表卡与另两张卡各只剩 **33px**（只看得到标题栏）。根因是「可展开的常驻卡」与「多个 `flex:1` 区块」共存于视口锁定容器：任何区块扩张都必须从别的区块身上抢，堵一次冒一次。现改为**整页信息架构重排**，三条原则：① 会改变自身高度的东西（自动备份配置、冻结表单、URL 导入表单、生成向导）一律进 **Modal**——Modal 默认 portal 到 `document.body`，**根本不参与页面 flex 布局**，这是从结构上消灭"展开即挤压"；② 每个操作跟它的对象放在一起；③ **状态常驻、设置收起**——只读状态留在常驻状态条上随时可见，写操作藏进 Modal。页面因此收敛为三段：**状态与操作条（68px 常驻）+ 备份包区块（`flex:1`，永远拿最大份额）+ 导入与传输区块（收起时仅标题行 57px）**。顺带把「从 URL 导入」与「分片上传」合并为一个区块——二者本就是同一条流水线（都产出 `BackupImport`，只差 `origin`），合并前上传完成要 `dispatchEvent(REFRESH_EVENT)` 去让**另一张卡**刷新，这个跨卡耦合本身就是"它们该是一张卡"的证据。导入区的展开规则：默认收起（无记录或都已终态），仅在**需要用户注意**时自动展开（进行中 / 失败 / 待重启 / 上传中 / 待续传），手动切换优先；判据**刻意不含 `done`**，否则历史成功记录会让区块永久展开，等于回到"多张表抢高度"。实测（1280×900，测试站隔离实例）：导入区收起时列表 **619px**、导入区 57px；**打开任一 Modal 时列表高度零变化（619 → 619）**——这是本次的核心验收点；导入区展开时两区块各 338px；1100×900 下列表仍有 521px。同一轮还按英文文案放宽了三处列宽（来源 72→88、方式 80→96、操作 56→92），宽度从「包标识」30%→28% 与「包含」104px 固定→12% 弹性让出，实测英文下 `Automatic` / `Hot backup` / `Actions` **均不再截断**。**过程中发现并修掉三处**：① **解冻死锁**——冻结按钮在已冻结时必须禁用，若解冻也只放在冻结 Modal 里，就会形成「已冻结 → 按钮禁用 → 打不开弹窗 → 永远解不了冻」；解冻没有表单（只有一次二次确认），弹窗化的理由对它本不成立，故移到状态条上与状态徽章同行。② 上传区 idle（尚未选文件）时显示的文案是「已开始上传，进度见下方」，与状态正好相反，已改为引导文案并删除该零引用键。③ 导入区空状态复用了备份包的「暂无备份包」，合并区块后两种空状态同屏相邻、用户分不清说的是哪个对象，已改为导入记录专属文案。规格见 [`docs/specs/0.12.0-auto-backup-and-drill.md`](docs/specs/0.12.0-auto-backup-and-drill.md) §7.4（该节同时标注 §7.3 的折叠方案已被其取代）。**该三段式随后被上一条的两段式取代**，此处保留以记录演进过程。
- **演练弹窗对已被淘汰的包给出可操作提示（FR-40 追加）**：界面复核时打开某行演练弹窗看到「备份包不存在」。核实后**不是缺陷**——测试站是 30 秒间隔 + `KEEP_LAST=2`，该 `auto` 包在审查期间被保留策略淘汰，弹窗如实报告了后端 404。但「陈旧行 → 裸后端文案」确实不够清楚：用户看到的是一句无从下手的错误。现改为在该 404 场景给出可操作提示（「该备份包可能已被保留策略删除……刷新列表即可看到最新状态」）并附「刷新」按钮，它刷新外层备份列表并关闭弹窗（该行已不存在）。其余错误（500 / 网络失败）仍走 `AsyncBoundary` 的通用错误渲染与自带重试，不混为一谈。
- **自动备份配置卡挤占备份列表高度（FR-40 追加）**：该卡从只读说明改成可编辑表单后从约 60px 涨到 365px，且带 `flexShrink:0`。本页可用高度 768px 减去冻结卡与间距后，三个 `flex:1` 区块（备份列表 / 从 URL 导入 / 分片上传）各只剩约 80px——备份列表作为本页主体只能显示约一行，用户实测反馈「高度压缩的看不清了」。现改为**默认折叠**：摘要放进标题行（`OpsSection` 的 `meta`）、展开开关放进标题行右侧（`actions`）、折叠时 `body` 内边距置 0，因此折叠态只剩标题行，展开后才渲染表单。实测配置卡 365px → **49px**、备份列表容器 80px → **185px**（对照：main 上根本没有这张卡时为 206px）。**注意「折叠」本身还不够**：折叠态若仍渲染 `body`，光是它的上下内边距就要 32px，实测那样只能到 103px / 列表 167px，故摘要必须进标题行。摘要在间隔为 0 时显示「自动备份已关闭」而非「每 0 秒备份一次」，避免把"没在跑"误读成"在跑"；折叠是纯展示态，不改变保存即生效的热生效路径。测试在 jsdom 里量不出高度，故改为守卫根因本身——断言配置卡默认折叠、以及摘要（含 0 值）渲染正确（变异验证：`expanded` 默认值改为 `true` 后三条用例转红）。同一轮还发现 devmock 的 `GET/PUT /settings` 完全不认识这三项配置（该端点非契约、手写实现，需人工同步），Mock 下摘要因此显示前端归一化兜底的默认值、保存只弹成功提示而值原样退回——已补齐 store 与 msw 的 PUT 分支（三项一律按 `typeof === "number"` 判断，避免 `||` 把合法的 0 吞掉），并新增「保存后回读必须是新值」的断言（变异验证：让 mock 丢弃该字段即转红，报 `expected '0' to be '60'`）。**该折叠方案随后已被上一条的整页重排取代**（展开态仍会挤压），此处保留是为记录演进过程。

- **退避窗口到期后业务流量无闸门地全量打向上游（FR-43）**：auto-block 窗口一到，`shouldBlock` 即不再拦截，而真正判定上游是否恢复要等后台 HEAD 探测返回——这段窗口里**全部真实业务流量**都会不受节流地打向可能仍未恢复的上游，等于每一档退避都白白放过一轮全量回源（上游持续故障时会在每个窗口边界反复制造一次压力尖峰）。现改为显式半开闸门：窗口到期先进入 `HALF_OPEN`，**每轮半开只放行一个探测请求**，其余请求在探测结果返回前等效封锁（零连接快速失败）；探测成功回到 `AVAILABLE` 并重置退避，失败则回 `AUTO_BLOCKED` 并推进一档窗口。闸门与状态判定在同一把锁内完成，避免并发竞态；手动重测语义不变（同步探测，不等窗口、不占用该名额）。同时修正对外状态：半开期不再显示为 `AUTO_BLOCKED`，而是新增的 `HALF_OPEN`。
- **备份包删除遗漏侧车索引，淘汰后留下只增不减的孤儿文件（FR-40 实机验收发现）**：`BackupService.Delete` 只删包体 `.tar.gz` 与登记行，**未删同目录的侧车索引 `<packageId>.index`**（该文件由 FR-134 引入，供后续差包求差用）。按需备份时这只是偶发残留，本批把它变成了**周期性行为**：`backup-auto` 作业每轮淘汰超限旧包，只删包体而索引永久留存——而 `storage-cleanup` 作业按设计不覆盖 `backups/` 目录，于是 `${JIAN_DATA_DIR}/backups/` 里的 `.index` 只增不减，与「保留策略兜住磁盘占用」（规格 §1.1）的设计目的直接冲突。测试站隔离实例以 30 秒间隔跑了 30 余分钟即实测到 **64 个索引文件 vs 6 个包体**。现改为 `Delete` 一并删除侧车索引（缺失不算错——早期版本删除未清理侧车，现存实例本就可能没有），并补两条用例守护：`TestBackupDeleteLifecycle` 断言删除后侧车消失、新增 `TestBackupRetentionLeavesNoOrphanSidecar` 走真实生成路径断言淘汰后不留孤儿索引且保留包的包体与侧车完好（变异验证：移除该行删除后两条用例均转红）。
- **升级 Go 工具链与 x/net 以清除标准库安全漏洞**：Go `1.26.6` 与 `golang.org/x/net` `v0.59.0` 被 2026-10-09 新披露的 12 个漏洞覆盖（含 `net/http`、`net/textproto`、`crypto/tls`、`html/template` 及 x/net 自身），`govulncheck` 因此由绿转红、质量门随之失败，与业务改动无关。`toolchain` 升至 `go1.26.9`、`x/net` 升至 `v0.60.0` 后 `govulncheck` 恢复「No vulnerabilities found」（仍余 1 条模块级漏洞，代码未调用、不阻断）。`release.yml` 的 Go 版本同步更新；**CI 的 Go 质量工具缓存 key 补上 Go 版本**——该缓存此前只钉工具版本，升级工具链后会命中旧工具链编译的 `govulncheck` 二进制，导致它继续按旧版本报告标准库漏洞（实测踩到），故必须把 Go 版本纳入 key。补记：本次升级**漏改了 `go.work`**（仍是 `toolchain go1.26.6`），而工作区模式下 `go.work` 的 toolchain 优先于 `go.mod`，导致本地质量门始终用 1.26.6 扫描、按旧版本报告标准库漏洞，CI 因直接安装 1.26.9 而看不出差异——`go.work` 已同步升至 `go1.26.9`，本地 `govulncheck` 随之恢复零漏洞。

## 0.11.0（2026-10-02）

### 新增

- **存储治理：仓库配额、清理作业、代理缓存保留与运维作业面（FR-41）**：① 仓库级存储配额 `quotaBytes` / `quotaAssets`（0 / 缺省 = 不限；计量口径为**逻辑字节** `SUM(asset.size)` 与制品计数，**与去重后的物理占用不等价**）按「已知长度预检（不读流）→ 流式限额读取器早拒 → **提交点事务内权威复检**」判定，越限统一 **429 `quota_exceeded`**（消息含占用与上限、不含文件系统路径），拒绝后暂存 blob 交既有失败路径回收；**不引入第二套预留**——提交点由单写者串行化保证「已提交状态永不越界」，明确接受「最多一个在途上传在提交点被拒」；仓库被调低到超限后连「缩小体积」的覆盖写也会被拒（有意语义），删除项不受配额阻断；迁移导入与备份恢复显式豁免（只记日志）；配额只对 `hosted` 强制，非 `hosted`（`group` / `proxy`）携带非 0 配额在配置层即 400。② 新增 `storage-cleanup` 周期作业（`JIAN_STORAGE_CLEANUP_INTERVAL`，默认 86400 秒，0 禁用）承担四类清理：终态操作与隔离元数据按 `JIAN_STORAGE_METADATA_RETENTION_DAYS`（默认 7 天，0 禁用）裁剪——只裁终态、且该操作下不再挂任何非终态隔离记录（绝不碰在途），空隔离目录（固定 1 小时宽限、**不设开关**）、过期 OCI 上传临时文件（`JIAN_STORAGE_TEMP_MAX_AGE_HOURS`，默认 24 小时，0 禁用，只碰 `tmp/oci-upload`）、代理缓存保留。③ 代理缓存保留 `cacheRetentionDays`（**仅 proxy、默认关闭**）按 `asset.updated_at` 判定且**不追踪命中**，删除走既有资产删除通道（引用归零→同步物理回收），每轮删除上限 200，系统发起的删除只记日志、不写审计。④ `pending_gc` 滞留件回收**本批不做**（操作引擎没有「在写门下判定是否在途」的入口，硬做会破坏回滚原子性），滞留件仍由启动恢复处理。⑤ 新增迁移 `0045`（`asset_mutation(status, updated_at)` 与 `asset(repository_id, updated_at, path)` 两个索引），把上述两条清理查询从每轮全表扫收敛为按序取够即停。⑥ 新增运维作业面 `GET /api/v1/maintenance/jobs` 与 `POST /api/v1/maintenance/jobs/{name}/run`（仅管理员；202 只表示已受理，运行中 409、未注册 404；触发写审计 `maintenance.job_run`；**本批无配套管理页面**）。⑦ 新增指标 `jianartifact_publish_rejections_total{reason}`（`reason` 为**闭集枚举**，v1 只有 `quota`）。⑧ 既有 Maven 空目录清理端点补 `repo.cleanup` 审计（成功与失败都记），响应结构保持兼容。规格见 [`docs/specs/0.11.0-storage-governance.md`](docs/specs/0.11.0-storage-governance.md)
- **LDAP 登录**：登录支持回退企业目录（服务账号检索→用户本人 bind，或 DN 模板直接 bind）；空口令在连接目录前即拒绝、用户名进过滤器前转义、证书严格校验且无跳过开关；目录身份沿用与 OIDC 相同的绑定/建号口径（角色固定 user、不设本地口令、管理员不参与自动绑定）；未配置目录时登录行为与现状一致
- **OIDC 登录**：新增 OIDC 授权码登录（PKCE + ID Token 按 JWKS 验签 + nonce / 邮箱验证校验），外部身份经绑定或建号后签发与本地登录同构的会话；登录弹窗按实例启用状态展示入口（顶层跳转，非 fetch），回调经 URL 片段自动建立会话并以新增的 `/api/v1/auth/me` 取回身份快照；本地管理员永不锁死、管理员账号不参与自动绑定、凭据只从环境变量解析；未配置 issuer 时端点返回 404
- **Prometheus 指标导出**：新增 `GET /metrics`（文本暴露格式，零新增依赖），输出制品协议请求计数（方法 / 状态码 / 缓存结果）、定时任务作业状态（执行与失败次数、运行中、最近执行时间与最近是否失败）与进程运行时（协程 / 堆 / 累计 GC）；抓取只读进程内数据、不查库，标签基数受控；匿名可见且不含版本等指纹信息（与探针端点同口径）
- **定时任务调度器**：新增进程内周期作业调度器 `internal/scheduler`，统一固化触发语义——启动不立即执行、间隔 ≤ 0 即禁用、单作业失败 / panic 隔离、同一作业不自我重叠——并提供作业运行状态快照（次数 / 失败 / 最近执行与错误，供后续 Prometheus 指标导出）；孤立 blob 定时清理（`JIAN_BLOB_GC_INTERVAL`）改由它驱动，行为不变

### 修复

### 变更

## 0.10.3（2026-09-30）

### 修复

- **操作聚合只有一大坨「其他」**：类型判定过窄且大小写敏感——除上传/删除/移动外的一切管理动作（令牌、设置、仓库、用户等）全落进兜底「其他」，一屏看不出操作类型、也无法按类型筛选；实测库中还存在 `SETTING.SET` 这类大写写法，精确小写匹配连白名单都命中不了，进一步加剧该现象。现按动作前缀细分 token / setting / repo / user 四类，判定前先做 `trim + 小写` 归一，「其他」只留真正无法分类的杂项。同时补「本页聚合」口径提示，显式说明聚合只覆盖当前页结果集、随翻页与筛选变化。

## 0.10.2（2026-09-30）

### 修复

- **制品下载忽略 Range 请求**：raw 与 maven 下载路径此前直接把内容流 `io.Copy` 回写，浏览器、`curl -C -` 与构建工具重试发出的 `Range` 被静默忽略并回 200 全量，断点续传退化为重下（每次重下还被计为一次完整下载，放大下载计次）。现把可寻址的本地 blob 交给 `http.ServeContent` 协商，返回 206 与 `Content-Range`，越界请求按 RFC 回 416；不可寻址的上游流维持原行为。
- **审计日志翻页失效**：服务端按契约对「未执行精确计数」返回 `totalCount=-1`（openapi 明示 -1 即未精确计数），而审计页仍以该值推算总页数，恒得 1 页并拒绝一切翻页。现改为：已知总数时用精确页数，未知时由服务端 `nextCursor` 判定「还能前进一页」。

## 0.10.1（2026-09-28）

### 修复

- **PyPI `simple` 索引不计缓存命中**：项目索引走 `PyPIFiles(repo, name)`（无 ctx 参数），即使补标记也传不出 hit/miss——pip 每次依赖解析都打 simple，导致 PyPI 仓的 `cacheHitRate` 系统性偏低；现给该方法补 ctx 并在「本地索引命中 / 本地为空回源」处记录来源（group 冻结为未知），协议层与 `packages` 路径同口径挂 `withCacheOutcome` + `markCacheResult`
- **置顶请求体缺 `repositoryIds` 时静默清空全部置顶**：契约里该字段是必填数组，生成类型为 `[]int64`，nil 与空数组不可区分——客户端漏传字段被当成「取消全部置顶」，返回 200 却清空了既有置顶；现改为可选指针并显式校验（缺省即 400，空数组仍表示取消全部），与 devmock 既有行为对齐
- **批量发布策略部分失败仍记 `result=ok`**：逐仓库结果可能部分失败（存储错误等），但审计固定写 `ok`，按 `result=ok` 过滤的审计视图会把「部分失败」计成成功；现全部成功才记 `ok`、否则记 `partial`，并在结果分类里把 `partial` 归入失败（沿用既有枚举，不改契约）
- **审计按仓库检索未按「主名 ∪ 别名」展开**：事件流的 `repository` 过滤按单名等值匹配，重命名后按当前主名筛会静默漏掉记在旧名（现为别名）下的历史事件（ADR-0028 明确要求按仓库维度的检索处理全集）；现把筛选名解析为「主名 ∪ 别名」集合并以 `IN` 过滤，未知名仍退回字面量（行为不变）。附：审计日志列表处理器 `GetAuditLogs` 当前未注册任何路由（也不在契约内），该路径的同类修正一并落地，待其接线后即可生效
- **主机监控网卡列表查询错误被静默吞掉**：`LatestHostNetworkInterfaces` 出错时只跳过赋值，而同函数的逐网卡样本与最新样本分支都会返回错误——DB 故障时前端只看到「没有网卡可选」而无任何信号；现统一为返回错误（无网卡数据时查询本身不报错、仍返回空集合，向后兼容不变）
- **发布策略多选只列前 100 个仓库**：用户页的发布策略弹窗用 `listRepositories({ page_size: 100 })` 只取第一页，仓库总数超过单页上限时第 2 页起的 hosted 仓库选不到；现改用 `listAllRepositories()` 按 `total` 翻完全部页（与本批其他页面同口径）
- **group 合并读取成员 metadata 无大小上限**：成员 `maven-metadata.xml` 用 `io.ReadAll` 整份读入内存，上游返回超大 body 时可把服务拖垮（与「大文件整体入内存」的架构红线相抵触）；现按 8 MiB 上限读取，超限成员视为不可用直接跳过（不截断，避免得到非法 XML），其余成员照常合并

### 变更

- **删除前端死代码 `buildUploadTree`**：它是「审计聚合」之外的第二套建树实现，生产代码零引用（只被自身 286 行单测保护），已随 `UploadTreeNode` / `MutableNode` / `upsert` / `finalize` / `compareNodes` 一并删除；模块收窄为「坐标反解 / 目标解析 / 搜索关键字」三个被复用的导出，`artifactSearchText` 的用例移到独立 describe 保留

## 0.10.0（2026-09-28）

### 新增

- **仓库别名与重命名（FR-146）**：仓库可配置多个别名，别名与主名**共享命名空间、全局唯一**，可等价访问（协议路由与管理端 API 全域经 `GetByName` 单点按名解析到主名仓库）；`POST /api/v1/repositories/{name}/rename` 重命名后**旧名自动转为别名**，旧链接与既有客户端坐标仍可解析。新增迁移 `0041_repository_alias.sql`（`repository_alias` 表，随仓库级联删除）；契约新增 `Repository.aliases`、`CreateRepositoryRequest`/`UpdateRepositoryRequest` 的 `aliases` 与 rename 端点；配置页签可编辑别名与重命名，创建表单可填别名。下载统计按「主名 ∪ 别名」聚合，`audit_log` / `asset_download_minutes` 保留旧名不回填。语义取舍见 [`docs/adr/0028`](docs/adr/0028-repository-alias-and-rename.md)
- **主机监控网络总量与网卡维度（FR-147）**：网络指标在既有速率之外新增「总发送 / 总接收」（**自网卡启动以来的累计字节**）与**逐网卡维度**。平台层由「聚合全网卡」改为逐网卡采集（Linux 读 `/proc/net/dev`、Windows 用 `GetIfTable2Ex`，均跳过回环），解析 / 过滤 / 求和下沉为无平台标签纯函数以便跨平台单测；服务层按**同名网卡**与上一份样本配对算逐网卡速率——网卡新增或计数回退时速率留空、不伪造 0。新增迁移 `0042_host_network_interface.sql`：`host_metric_minute` 增可空列 `network_receive_bytes_total` / `network_transmit_bytes_total`（该时刻全部非回环网卡的聚合累计，历史行 NULL 不回填），新表 `host_network_interface_minute`（逐网卡每分钟一行，主键 `(bucket_start, interface)` + `(interface, bucket_start)` 索引）。契约 `HostMetricPoint` 增 `networkReceiveBytesTotal` / `networkTransmitBytesTotal`，新增 schema `HostNetworkInterface`，主机监控响应增 `networkInterfaces`，`/api/v1/observability/host` 增可选 `interface` 参数（省略 = 全网卡聚合）。前端网络卡显示「总发送 / 总接收」+ 网卡选择器（首项「全部网卡」，选定后速率图与总量都按该网卡）
- **仓库使用说明扩充更多工具与构建系统**：在既有分组内补充常见客户端片段——Maven 增 sbt（`~/.sbt/repositories`）、Gradle 凭据（`~/.gradle/gradle.properties`）与一行式 `mvn dependency:get` / `mvn deploy -DaltDeploymentRepository`；npm 增 `.npmrc`（含 `//<host><path>:_authToken` 认证行）、pnpm 与 Yarn（1.x / 2+）等价配置、scoped 包 `publishConfig` 发布；raw 增带 `Authorization: Bearer` 的 curl 下载 / 上传与 wget 下载。分组枚举不变、只是同组内扩展（Maven hosted 5→14、npm hosted 3→8、raw hosted 2→5），只读（proxy）仓库仍不出现发布组，标题 / 描述继续按 `Accept-Language` 中英成对
- **审计工作台「上传聚合」视图**：在既有事件流之外新增按上传 / 发布事件聚合的树视图 `仓库 → groupId → artifactId → 版本 → 文件`（复用既有 GAV 坐标解析），节点显示该节点上传次数与时间范围，展开到叶子可见文件并可跳转到对应审计事件；非 Maven 路径（如 npm 的 `@scope/pkg/-/pkg-1.0.0.tgz`、raw 任意路径）与解析不出 GAV 的事件**降级归入「其他」**（按目录路径聚合，不丢事件）；只聚合当前查询结果集内的事件，空态给出明确文案。纯前端聚合，不新增接口
- **置顶仓库改数据库存储（FR-148）**：置顶此前是纯前端 `localStorage` 偏好（键 `jianartifact.pinnedRepos`），换设备 / 清缓存即丢失。现新增迁移 `0043_pinned_repository.sql`：`pinned_repository` 表（`user_id` NULL = **全局置顶**、非 NULL = 用户私有置顶，主键 `(user_id, repository_id)`，随 `user` / `repository` 级联删除）与 partial unique index `idx_pinned_repo_global`（`repository_id WHERE user_id IS NULL`，兜住 SQLite 主键唯一性不覆盖 NULL 的缺口）。契约新增 `GET/PUT /api/v1/me/pinned-repositories`（用户级，匿名 `GET` 回退全局、匿名 `PUT` 401）与 `GET/PUT /api/v1/settings/pinned-repositories`（仅管理员），写入为覆盖式（按 repositoryId 整体替换）；`/api/v1/public/repositories` 响应增 `pinnedNames`（全局置顶），使**公开页也按置顶排序并显示图钉**。前端 `usePinnedRepos` 改为读服务端数据 + 乐观更新失败回滚，`localStorage` 仅作只读降级；既有浏览器内置顶无法读进服务端，需重新置顶（不做自动导入）
- **发布策略支持多仓库批量保存**：发布策略原为 `publish_policy` 单仓库一条（主键 `(user_id, repository_id)`），前端表单只能选一个仓库。现契约新增批量端点 `PUT /api/v1/users/{id}/publish-policies`（body `{repositories[], ...}`），把同一份策略一次应用到多个 Hosted 仓库；服务层 `SaveMany` 先对全部仓库统一预校验（须存在且为 hosted，任一不合法即整体拒绝），再逐个落库，响应按仓库返回 `results[]`（部分失败可见）；单仓库端点保留。前端发布策略弹窗改多选仓库（Mantine `MultiSelect`）、展示逐仓库结果，并补一句策略用途说明（它约束发布账号的路径前缀与配额）

### 修复

- **仓库列表可见性过滤退化为逐行查库**：非管理员列表与公开列表对每个仓库调 `CanAccess`（内部再按名查一次库），N 个仓库产生约 2N~3N 次 SQLite 往返，公开列表端点无需认证、是可控的放大路径；现改用按对象鉴权 `CanAccessResolved`（行已在手上），并补两者判定一致的用例
- **审计归并把跨 15 分钟边界的同一次操作拆成两个**：归并键含绝对时间桶（`floor(时间/15min)`），一次 deploy 的各文件若跨分钟边界就会得到两个键、被拆成两行；现键内不含时间桶，归并阶段按锚点（与本段首个事件的时间差 < 窗口）切段，跨边界不再拆分，同时仍不并「一段时间内的巧合写入」
- **置顶保存后图钉闪回**：写入成功后重拉会先回放 60s 内的旧缓存快照，把刚保存的置顶顶回写入前的集合（真实网络下表现为图钉闪一下又消失）；重拉失败时界面还会无声停留在旧集合上。现写入成功后先失效置顶缓存、乐观层保留到新快照落地再丢弃，并把重拉失败暴露为提示
- **并发创建撞名会留下「已建但报失败」的仓库**：别名与主名的跨表唯一性没有 DB 兜底，只有服务层先查后写；并发窗口内后到的一方插进重复别名而失败，但仓库行已提交且不回收，调用方收到 `ErrConflict` 却有一个按主名可访问的仓库（重试只会得到「名称已被占用」）。现把收尾步骤（描述、别名）抽出，失败时补偿删除该仓库行，并补并发复现用例
- **批量发布策略对非法字段返回 200 且全仓失败**：批量端点的统一预校验只查「仓库存在且为 hosted」，补丁自身的字段（前缀含 `..`、配额为负）要到逐仓 `Save` 才报错，于是同一份非法输入在单仓库端点是 400、在批量端点变成 200 + 逐仓库失败；现把字段校验抽成共享函数并在批量级提前校验，非法即整体 400，与单仓库端点同口径
- **分组下载趋势的仓库过滤 404 未入契约**：未知仓库名（含已删除 / 未跟随重命名的旧名）返回 404，但契约只声明 200/400/401/403/409，前端 client 由契约生成；现补 404 响应与参数说明
- **置顶端点缺少入参数量上限**：任一登录用户可用超大 `repositoryIds` 在单个写事务内触发逐 ID 查询 + 插入，把一次请求放大成数万次查询并长时间持写锁；现按上限在入口拒绝
- **审计聚合树折叠状态跨分区串扰**：折叠键不含分区，同名仓库 / 坐标在「上传」分区被折叠后，「删除」分区的同名节点会一起折叠；现键带分区前缀
- **主机监控默认 24h 视图加载数秒**：服务端桶粒度阈值（≤24h → 分钟桶）与前端 `fallbackBucket`（≤6h → 分钟桶）不一致，默认 24h 视图返回约 1440 个分钟点（约 1MB），接口耗时 2.5~9.5s；现对齐为 ≤6h 分钟桶、≤7d 小时桶、其余天桶，24h 视图降为 24 个点（约 13KB、0.2s）
- **devmock 发布策略保存必然失败**：devmock 的 PUT 单仓库与批量发布策略端点把已废弃的 `immutableRelease` 当必填字段校验，而契约与真实后端都按「只读兼容字段」处理（不要求出现、出现即 400 `immutable_release_moved`），前端按契约发来的请求被误判为缺字段；现改为与真实后端同口径，并补一条直打 mock handler 的用例锁住该行为
- **上传制品在审计中不可追踪**：前端审计列表对制品事件优先显示 `repo/path` 副文本，此前该副文本被协议路由模板（`/repository/:repo/*artifactPath`）遮盖，看不到「传了什么」；同时后端补齐 Maven 网页表单上传的逐文件 `asset.put` 审计（该路径此前不触发审计回调），与协议 PUT 上传同口径（`EntityKey=repo/path`、`Detail=size=N`）
- **主机监控「节点状态」右栏被强行拉伸等高**：主 Grid `align="stretch"` 把右栏拉到与左列等高，中间留出大片空白；改为按内容取高（`align="start"`、移除 `h="100%"` 与末行 `mt="auto"`、压缩 Divider 间距），消除空占
- **主机监控缺失的 i18n 键**：`hostMonitoring.processCpu` 与 `hostMonitoring.metricDetails`（节点状态栏 Divider 标签）此前为裸键，补齐中英译文；顺带清理未被引用的孤儿键 `metricNetworkRx` / `metricNetworkTx`
- **审计中心切 30 天卡死**：统一事件流 CTE 在 `UNION ALL` 后全量物化 + 临时 B 树排序（时间索引只帮范围过滤、不帮合并排序），叠加事件页每次翻页的全量 `COUNT`、聚合探针同样物化、前端并发多请求，服务被拖死。修复：① 无过滤/白名单过滤（action↔op、actor↔source_actor）时把「每源按时间倒序取前 offset+limit 条」下推子查询（走索引取满即停），外层只归并 ≤2K 行——25 万行实测 **95ms → 0.2ms**；② 取消 `COUNT`（首屏亦不计，统一 `total=-1`），前端显示「已加载 N 条」并以多取 1 条精确判断有无更多
- **KPI/趋势/关注批次只统计审计事件**：30 天内 replication 同步记录达 114 万条（审计事件仅 1.1 万），既拖垮聚合又淹没「审计事件」语义；聚合路径改 `AuditOnly`，同步记录保留在事件流列表
- **replication 同步记录默认折叠**：events 端点增 `includeReplication`（默认 false）；前端筛选条增「含同步记录」开关（按需展开）
- **仪表盘「需要处理 / 最近活跃」显示裸 action 键**：面板未走 `actionLabel` 且映射表缺新增动作（`management.write_rejected`/`repo.recheck` 等 17 条）；补齐映射与中英译文并加守卫测试
- **匿名浏览公开仓库详情「制品数/体积」恒为 0**：匿名分支走 usage 端点（无统计字段）；统一走公开列表 API（匿名可读且带统计）
- **非 UTF-8 请求体在审计中呈乱码**：`SanitizeBody` 对 GBK 客户端经 `json.Unmarshal` 宽容路径静默替换为 U+FFFD 或字节直转入库；入口改校验 `utf8.Valid`，非 UTF-8 明确标注而非「损坏字节伪装成数据」
- **页眉 768–992px 区间按钮被裁切**：新增 `compactHeader`（62em）判定，该区间右侧按钮带退化为图标 + Tooltip（不动 `isMobile`，不连带导航与面包屑行为）
- **审计记录区滚动条遮挡内容**：新增 `.ja-hide-scrollbar` 工具类（隐藏滚动条、保留滚动）
- **矮视口下整页双重滚动**：`PageShell` 的 `min-height` 写死 480 在矮视口撑出视口；改为 `min(480px, 可视高度)`
- **置顶行高亮刺眼**（Mantine 9 light 变体不再透明）：`color-mix` 兑回半透明
- **仓库内搜索只搜到前 20 条**：后端 `page_size` 超限静默回落默认 20 且前端无翻页；改为超限夹取到上限 + 前端「加载更多」
- **审计表格最右列被裁（英文）**：「操作」列定宽 76px 装不下 "Details" 按钮（需 93px）与表头 "Operation"（需 87px），行内容溢出 17px；末列加宽至 104px，并从状态码（76→60）、耗时（88→80）列回收同量宽度
- **审计时间列被英文长日期撑破定宽列**：`dateStyle: medium` 在英文下产出约 190px 文案（如 "Sep 22, 2026, 1:17:44 AM"）；表格主时间列改为固定 `YYYY-MM-DD HH:mm:ss`（本地时区、跨语言等宽约 150px），列宽收敛至 168px
- **搜索无结果提示泄漏内部计数哨兵**：提示文案曾插入 `totalCount=-1`（界面显示「前 -1 条」）；去掉数字，改为「没有匹配「{{query}}」的事件」
- **「含同步记录」开关被整段误删**：自定义时间段由原生日期输入换成组件库选择器时，同段落内的 replication 折叠开关一并被替换掉；恢复开关并补渲染守卫测试
- **页眉右侧按钮内文案被硬裁**（刷新/切换语言/账户）：按钮带 `flex: 1` 与其 `flexShrink: 0` 组合展开后含 `flex-basis: 0%`，宽度被 grow 比例锁成容器 1/4（1280 下约 240px），文案超出即被 `overflow: hidden` 硬裁且无省略号；改为 `flex: 0 0 auto` 按内容取宽，剩余空间由面包屑与搜索按 1:2 分配
- **仪表盘仓库状态信息不足**：仓库明细补充制品数量与体积，并对缺失值显示占位符
- **主机监控资源口径不完整**：补充内存与磁盘总量、已用量、可用量；合并当前进程内存趋势与进程指标，并显示进程运行时长
- **审计详情缺少请求上下文**：记录脱敏后的常用请求头与服务端处理耗时，并按请求、性能、上下文分组展示；Referer 仅保留 scheme/host/path，剔除 userinfo、query 与 fragment
- **下载趋势多算右边界桶、分组轴与服务端桶起点错位**：统一按 `[from,to)` 过滤分钟源桶，前端轴按有效桶粒度对齐；`to` 落桶边界时不再额外生成终点桶
- **主机监控磁盘趋势「已用≈总量」错误读数**：磁盘图此前把「已用 / 可用」画在左轴（贴合数据的放大域）、「总量」画在量纲不同的右轴，结果已用贴近左轴顶、总量贴近右轴顶，视觉上「已用≈总量」，与 74% 的实际占比矛盾。现改为**单一左轴**：`primary=diskUsedBytes`（面积）+ `yDomain=[0, diskTotalBytes]`，已用面积之上到轴顶的空白即**可用**（与内存图同构）；`diskTotalBytes` 缺失（历史样本）时回退 `["auto","auto"]`。卡内「总量 / 已用 / 可用 / 占用率」数字保留，可用量仍以精确值可见
- **缓存命中率只对 raw 生效、其余格式恒为空**：`jianartifact.protocol.cache_result` 此前仅在 raw 协议的 Get 中设置（且只服务 raw 格式的 proxy 仓库），maven / npm / nuget / go / pypi / cargo / oci 的读路径都不写该键，于是业务仪表盘的缓存命中率长期显示 `—`（判定链路其余部分完好，只是没有数据来源）。现新增领域枚举 `CacheOutcome`（`hit` / `miss` / 未知）并在各 proxy 解析路径「先查本地、未命中再回源」的分叉处记录（`AssetService.proxyGet` / `ResolveOCIProxy`、`CargoService.proxyDownload`、`FormatMetadataService` 的 PyPI / NuGet 缓存路径）；协议层新增统一 `markCacheResult` 助手替换 raw 里原来的裸 `c.Set`，并删除旧的 raw-only `cacheCandidate` 判定，避免两套口径。hosted/group 一律保持未知——group 的首个命中成员无法在解析层可靠判定，宁可未知也不猜
- **发布策略保存此前从未成功**：旧前端保存时会发送 `immutableRelease` 字段，而后端对发布策略请求里的该字段一律返回 400 `immutable_release_moved`（不可变 Release 已改为仓库级配置）→ 保存请求必然失败（与线上 `publish_policy` 表 0 行吻合）。现前端改为只读展示、不再发送该字段，发布策略保存恢复正常
- **主机监控切换时间档位 / 网卡时整块被骨架替换**：`HostMonitoringLive` 的 `useAsync` 未传 `keepPreviousData`，跨键（时间档位或网卡变化）时会先 `setData(null) + setLoading(true)`，导致整块图表被骨架替换、布局重排。现补 `keepPreviousData: true`（与搜索页同款语义）：切换时保留旧数据、仅置后台刷新态，新数据到达就地替换，首载仍显示加载态

- **可见仓库超过一页时列表只加载第一页**：非管理员与匿名请求的仓库列表把 `total` 写成「当前页可见条数」，而前端正是按 `total` 判断是否继续翻页，于是公开 / 授权可见仓库超过单页上限（100 条）时永远只拉第一页；`/api/v1/public/repositories` 也固定在默认 20 条。现改为先按可见性过滤再分页、`total` 统计全部可见仓库，公开列表一次读全，且都不泄漏私有仓库

### 变更

- **页面权限与仓库浏览布局口径收敛**：路由层统一区分公开浏览、登录后页面与管理员页面；仓库列表、仓库详情、ACL、搜索、设置、迁移与备份统一使用单一 `PageShell`，仓库列表按「置顶 → 筛选 → 分组/排序 → 分页」处理，窄屏收起低价值列，详情文件树与文件详情上下堆叠，宽屏保留分割条
- **Maven 请求级仓库快照与快速 404 语义明确化**：协议请求按主名/别名解析一次并在请求内复用仓库快照；SNAPSHOT 先按时间戳元数据解析、失败后按字面路径回退；未知仓库、禁用格式和明确不存在的制品快速 404，确认不存在时写入默认 60 秒负缓存，上游故障或不可判定状态不缓存，成功写入/回源成功/删除后失效
- **主机监控内存/磁盘趋势纵轴贴合数据**：内存趋势纵轴改为**系统内存总量**、只画「已用」（已用之上的空白即表示空闲，不再单画可用量折线）；磁盘趋势与仪表盘「容量增长趋势」的 Y 轴统一为 `[0, 总量]`（磁盘图与内存图同构，容量增长图见下）。图表能力上 `TrendChart` 新增 `yDomain` 入参（不传时保持原「从 0 起」行为，零回归）
- **仓库详情下载趋势归位**：从页面顶层整行移入右侧详情面板顶部，不再占用页头与浏览面板之间的空间
- **审计时间范围档位改为 24h / 3d / 7d / 30d（默认 24h）**，并支持**自定义日期区间**（URL 深链 `range=custom&from=&to=`）；日期选择器文案与日历 locale 随界面中英文切换；「共 N 条」与时间切换器并入筛选条，记录区标题移除、左右留白且不再横向滚动
- **自定义日期区间改用 Mantine `DatePickerInput type="range"`**：替换原生 `<input type="date">`，与组件库保持一致
- **审计事件 `totalCount` 契约显式文档化**：openapi 明确 `-1` 表示服务端未执行精确 COUNT（去掉 `minimum: 0`），客户端据此展示「已加载 N 条」
- **仪表盘趋势图增强**：请求、下载与失败系列图例可点击隐藏/恢复；新增按客户端族/IP 分组的下载趋势、请求总量对照和下载占比饼图，图表项可跳转到带时间/IP/客户端筛选的审计页；移除重复的独立下载趋势图（FR-143、FR-144）
- **仓库详情补充下载数据**：展示累计下载次数与近 24 小时下载趋势；下载趋势按仓库过滤，复用分钟级下载计量数据（FR-142）
- **DevMock 补齐下载趋势端点**：新增分组下载趋势与仓库下载趋势模拟响应；桶粒度与服务端一致，按分钟源桶遵循 `[from,to)` 并对齐 minute/hour/day 起点，分组总数与时序点保持一致
- **趋势图移除「区间剖析」统计条**：删除图下方那条均值 / 峰值 / 谷值 / 区间变动的统计（及随之无用的中英 i18n 键 `trendChart.analysis*`）；**横向拖选聚焦放大仍保留**（拖选与还原、键盘可达性、悬停读数均不受影响）
- **仓库详情「下载趋势」常驻详情面板**：趋势图常驻右侧详情面板顶部（不折叠、不占页头高度），加载中骨架 / 失败降级文案 / 就绪复用 `TrendChart` 三态不变；总次数取自页面已有的下载趋势响应，**不新增请求**
- **仓库详情「使用说明」按用途分组 + 工具切换**：契约 `UsageSnippet` 新增必填 `group`（enum `auth` / `resolve` / `publish` / `other`，**结构化分组标记、不参与本地化**，与随 `Accept-Language` 变化的 `title` / `description` 不同）与 `tool`（构建工具标识）；前端按「认证 / 解析依赖 / 发布制品 / 其他」分组、组间以 `Divider` 分隔，**全部展开不折叠**；工具多于一种时顶部给一个全局工具 `Select`，切换后各组只显示该工具的片段（空组整组隐藏），每段代码块右上角复用 `CopyTextButton`
- **仪表盘「容量增长趋势」改为增量曲线**：此前以绝对逻辑体积作纵轴，容量基数在 GB 量级而 `formatBytes` 只到 GB 一位小数，坐标轴刻度与悬停读数被取整到同一个 GB 读数，期间的真实增长被基数掩盖，视觉上就像「卡住」。现把序列映射为相对**区间起点**的增量（首点恒为 0），纵轴在增量非负时自 0 起（只有在区间内出现负增量时才贴合数据两端，避免截断），卡片右上大数字改为当前增量而非总量；总量仍在 KPI 指标带的 `logicalBytes` 中可见。标题 / 摘要 / 主标签同步改为「容量增长（相对区间起点）」语义（`dashboard.trendCapacity*` 中英同步），空数据与单点数据安全（单点增量为 0，不除零、不产生空轴）

## 0.9.0（2026-09-21）

### 新增

- **英文界面资源补齐 + 页眉语言切换（FR-139）**：此前只有 `migrations` 一个命名空间是英文，其余 23 个命名空间整体继承中文——**即使把语言切成英文也只会看到中文**。现 `en.ts` 补齐全部 24 个命名空间、中英各 1093 个一级键（另各 1100 个叶子键，双向 1:1；键数以 `src/i18n/I18nKeys.test.ts` 的统计口径为准）。守卫是双层的：`en.ts` 用 `satisfies Resources` 在**编译期**拦住缺键与多键（`Resources` 是把 `zh` 的 `as const` 字面量类型递归放宽为 `string` 的形状类型），`src/i18n/I18nKeys.test.ts` 再复核值层面的形态——逐键比对插值占位符集合、拦空值与中文残留，并替换掉此前只比 `migrations` 的单命名空间断言。语言解析优先级为**用户显式选择 > 浏览器语言 > 路由默认**：公开页（`/repositories`、`/repositories/:name`、`/search`、`/p/:name`）按浏览器语言，管理页与 `/setup` 默认中文（运维不该被浏览器语言悄悄换掉界面），用户在页眉切换后该选择在任意路由上都优先并被记住。页眉新增语言切换入口（沿用「图标 + 文字」口径，窄屏走既有 `HeaderIconAction`），切换即时生效并同步 `<html lang>`。
- **36 处硬编码 `zh-CN` 收敛到统一 locale 入口（FR-139）**：新增 `src/i18n/current.ts` 的 `currentLocaleTag()` / `localizedFormatter()` 作为 `toLocaleString`、`Intl.*` 与 Mantine 日期组件 locale 的**唯一取值来源**。其中 5 处是 `components/audit/labels.ts` 的**模块级** `Intl.DateTimeFormat` 常量——模块级常量在切语言后不会重建，改语言时时间格式会停在旧语言，是本次最隐蔽的一处；现按「语言|选项」惰性构造并缓存。另 10 处无 locale 参数的 `toLocaleString()` 同样收敛，避免跟随浏览器 locale 而与界面语言分叉。
- DevMock 控制台悬浮球（仅开发态）：数据量（少量 / 中量 / 大量 = ×1 / ×4 / ×16）× 网速（快 / 中 / 慢 = 0 / 400 / 1500 ms）3×3 矩阵，切换即清页面缓存并触发全站重拉。延迟挂在全局前置守卫上，因此对全部端点生效；数据量会把仓库**物化**成更多真实仓库（`<名称>-v<轮次>`，列表与详情看到同一份数据），并放大主机采样点（48→768）与仪表盘趋势桶（12→192）。
- **DevMock 观测数据每次刷新都会变化**：仪表盘的 KPI 与趋势曲线、主机监控的 CPU / 内存 / 网络 / RSS / goroutine 采样，都由「快照序号」驱动一个确定性抖动，因此点页眉刷新能立刻看到数据变动（此前数值恒定，无法判断刷新是否生效）。序号 0 为**确定性基线**——契约测试与前端断言都基于它，`resetStore` 会把所有通道归零；各端点**独立计数**，避免同一页面内先发的其他请求吃掉基线序号。

### 变更

- **控制台布局口径收敛**：新增 `PageShell` 作为固定视口高度的唯一真源（`100dvh − header-offset − 2×padding`，含 `minHeight`），仓库列表 / 仓库详情 / 审计工作台 / 备份 Tab 统一改用，消除 vh 与 dvh、`header-height` 与 `header-offset`、有无 `minHeight` 三类写法分叉；内容区启用 `density.contentMaxWidth`（1280）居中，页面不再自造最大宽度。
- **KPI 收敛为 `OpsKpiBand` 一套**（新增 `variant="strip"` 紧凑横带），删除 `OpsKpi` / `OpsBar` / `OpsPageHeader` 及仅被它们引用的色表与状态映射。
- **DevMock 控制台可用性三项**：① **矩阵自解释**——行标带倍数（少量 ×1 / 中量 ×4 / 大量 ×16）、列表头带延迟（快 0ms / 中 400ms / 慢 1.5s），数字来自 devmock 新导出的 `MOCK_VOLUME_FACTORS` / `MOCK_SPEED_DELAYS`（档位含义的单一真源，UI 不再另抄一份），每个格子也带上含倍数与延迟的完整 `aria-label`；② **面板可拖拽**——默认停在右下角会盖住内容，现在按住标题即可挪开，位置记忆在 `localStorage`、视口变化时自动夹回可视区、双击标题归位（拖拽位移超过 4px 才判定为拖动，避免误吞「点击打开」）；③ **新增「重置」**——恢复初始种子数据与默认档位（复用 devmock 既有的 `resetStore()`），改坏 mock 数据后不必刷新页面。入口球在收起态显示当前倍数（如 `×4`）。
- **访问令牌页 KPI 与列表修复 + 三处触控一致性**（按用户反馈）：
  - **KPI 卡片**：窄屏 `cols` 原本是 `base:1` → 三个指标（令牌数 / 最近签发 / 最早签发）**竖着堆三行**、占掉 220px。改窄屏 2 列 → **169px**。这里没用 3 列：`2026-01-22` 这类完整日期在 3 列（每格约 115px）下会被裁（实测 `scrollWidth 107 > clientWidth 90`），宁少一行也不裁数据。
  - **列表布局真 bug**：窄屏「吊销」按钮直接放在 `Stack`（Mantine 默认 `align="stretch"`）里 → **被拉满整行、文字居中**，看着像一条空按钮。改为包一层 `Group`（现 73×30 贴合内容）。同时统一到仓库页口径：窄屏去斑马纹 + 行边框 + `verticalSpacing="md"`、行内间距 4 → 12px、「吊销」按钮 22 → 30px。
  - **一致性扫尾**：用户页同一口径（窄屏去斑马纹、行内间距 6 → 12px、发布策略/重置口令/删除三个按钮 22 → 30px）；审计行内「详情」按钮 22 → 30px。
- **三处移动端观感修复（按用户截图反馈）**：
  - **仓库列表行仍显挤**：行内间距 8 → **12px**，窄屏**去掉斑马纹**（相邻行底色相接会连成一片）改用行边框 + `verticalSpacing="md"` 分隔，「复制」推到名称行最右、名称改为省略号截断 → 行高 111 → **127px**，行与行之间清晰可辨。桌面（斑马纹 / `xs` 行距 / `gap 2`）保持不变。
  - **仓库详情文件树行太密**：树节点上下内边距窄屏 4px → **9px**，行高 ~26 → **38px**（达到触控目标），桌面仍 4px 以保留一屏可见节点数。
  - **审计中心窄屏点开行内详情后布局坏掉**：窄屏表格只有 4 列，而详情行按 `colSpan={7}` 渲染 → 与表头列完全不齐，sticky 表头还会压在详情上，且详情（客户端 IP / User-Agent / 请求体 JSON）长到在锁死视口里根本滚不到底。改为**窄屏用底部抽屉**承载同一份 `EventDetail`（桌面保持内联展开），抽屉在锁死视口外渲染，布局零抖动。
  - **审计中心窄屏 KPI 带过高**：12 个指标原本占 4 行（近半屏）。窄屏默认只渲染 6 个主指标（总数 / 失败 / 高危 / 成功率 / 平均耗时 / 待确认批次），其余由「更多指标」开关展开——记录区高度 316 → **386px**。
- **仓库列表窄屏松开 + 仓库详情页头填空**（按用户反馈）：
  - **窄屏仓库列表「挤」**：行内三行（名称 / 属性 / 操作）原本只隔 2px，操作按钮宽 `compact-xs`（22px）——手机上又挤又难点。现在行间距窄屏 8px（桌面仍 2px），操作按钮窄屏放大到 `xs`（22 → 30px）、按钮间距 4 → 8px，表格 `verticalSpacing` 窄屏上调一档；实测行高 81 → **111px**、三行间距 2 → 8px、按钮 30px。桌面行高 44px、8 列、按钮 22px 均保持原样。
  - **桌面仓库详情页头「中间太空」**：页签与右侧徽章之间实测有 **905px 空白带**（1440）。现在中间补一条仓库概览：制品数 / 体积 / 创建于（`≥992px` 显示，窄屏自动隐藏以留空间给页签与徽章；1024 实测不换行）。上游连接状态**故意不重复**——它属「配置」页签的正交信息，同文案两处出现既冗余又会让人按文本断言时产生歧义（本次就因此让两条既有用例失败，已回退该列）。
- **补齐 37 处缺失的 i18n 键（含 2 处会直接显示键名的可见缺陷）**：`t(key, { defaultValue })` 在键缺失时会静默回退兜底文案，肉眼与 `tsc` 都发现不了。独立审计（Node 实际求值 zh.ts 得到真值键集 + 全仓扫 `t("...")` 调用点）查出 37 个键从未录入 zh.ts，其中 **2 处连 defaultValue 都没写，界面会把 `common.retryLater`、`migrations.saved` 原样显示**（仪表盘接口失败占位、迁移保存成功提示）。现按调用点既定中文文案全部补录，横跨 9 个命名空间（nav / common / hostMonitoring / repoDetail / acl / migrations / repositories / settings / tokens），键总数 1039 → 1075，复核 0 缺失、0 裸键；同步补 en.ts 的 `migrations.saved`——`en` 只翻译 `migrations` 域，且 `MigrationDetailPage.test.tsx` 有「en 与 zh 的 migrations 键集一致」断言会拦住缺键。另删掉误放进 `dashboard` 命名空间的死键 `range1h`（1 小时档的标签是硬编码，无任何引用）。
- **窄屏全站验收与修复（11 路由实测）**：
  - **仓库列表工具条**：窄屏「排序 120px + 筛选 190px」硬挤一行，右侧留一条 44px 空白带、`新建仓库` 被挤到自己一行只有半个宽度 → 两个控件等分整行、主操作占整行。
  - **审计中心**：①窄屏 KPI 从 2 列改 3 列（12 个指标 6 行 → 4 行）；②高级筛选由内联展开改**抽屉**——内联展开会把记录表顶到视口外（y=864 > 844），而外壳 `overflow:hidden` 锁死视口，顶出去就再也够不到；③记录表窄屏裁列：只留 时间（仅时刻）+ 动作 + 结果 + 操作，操作者 / 耗时 / 客户端 IP 并入动作列副文本（原表 726px 在 390px 下被裁掉后 4 列）。新增 `formatClock` 只取时刻。
  - **开源协议页**：4 列表格 589px 塞在 `overflowX: hidden` 的卡片里，版本 / 协议 / 作者三列被裁且滚不到 → 窄屏只留「软件包 + 协议」，版本·作者并入副文本。
  - 验收口径：11 条路由在 390×844 下零元素越界（`maxRight ≤ 378`）、零控制台报错、零裸 i18n 键、零按钮文字截断；1440 桌面端列数与筛选控件保持原样。
  - 另：报表「仪表盘加载失败」排查为 dev server 被会话回收期间的假象（重启即恢复；生产近 3 小时日志零 5xx、零错误），非代码缺陷。
- **仓库详情页头压缩**：页签与「format / type / visibility 徽章 + 关闭」并排——宽屏一行放下（省掉整整一行），窄屏自动折成「页签 / 徽章 + 关闭」两行；徽章降为 `size="xs"`、关闭按钮改 `compact-xs` 并补上图标、页签内边距收到 6px、面板上边距窄屏收一档，描述独立成一行（`lineClamp={1}`）。窄屏「内容以上」占位 167 → **121px**（省 46px），桌面内容区 687 → **737px**。
- **窄屏仓库详情：文件树按内容自适应 + 选中文件后自动让位**：窄屏下树与详情此前各占 `flex: 1`（各 299px）——2 行的树白占半屏空白，详情被折叠线切断还得在卡内再滚一次。现在树卡按内容高度自适应（上限 45vh，超出时树内滚动），**选中文件后收成一行「路径 · 换文件」（43px）**把屏幕让给详情；点「换文件」展开且**保留树的展开状态**（收起只是隐藏、不卸载组件）；展开目录不收起，便于连续展开。实测 390×844：树 299→142px、详情 299→457px（选中文件后 555px），详情内容不再需要内部滚动；桌面端（1440）布局与拖拽分割条不受影响。
- **管理页顶部改为「概览带」**：用户 / 访问令牌 / 迁移与备份 / 设置四页的页内大标题此前已按「页标题交页眉面包屑」的约定移除，于是面包屑下方只剩一个右对齐的操作按钮，视觉上很空。现改为一条**概览带**——左侧是 3~5 个真实口径的摘要（用户：账号构成；令牌：令牌数与签发区间；迁移：任务状态分布；设置：已保存并生效的配置值），右侧是该页主操作，由 `OpsKpiBand` 紧凑带承载并新增 `actions` 插槽复用（避免每页各自包一层）。短列表页还在表格下方补了一张说明卡（用户=角色与权限、令牌=使用与安全、设置=生效与回滚），短列表时填补下半屏空白、长列表时随内容滚走，不长期占位。
- **全站操作按钮统一「图标 + 文字」**：把 25 个纯图标按钮改为「图标 + 文字」——表格行内的 置顶 / 清理 / 删除 / 发布策略 / 重置口令 / 吊销 / 下载、备份包的 获取链接 / 复制命令 / 校验 / 深度校验 / 删除、页眉的 刷新 / 搜索 / 登录，以及设置页回源 Token 的「重新生成 / 复制」（原先挤在输入框右侧只能放图标，现移到输入框下方成对显示）。仓库列表名称旁的「复制」也从裸图标改为文字按钮——单独一个复制图标看不出复制的是协议地址。随之删除 `CopyTextButton` 的纯图标分支（已零引用）。同时给**原本只有文字的动作类按钮**补上左侧图标（新建 / 保存 / 删除 / 下载 / 上传 / 导入 / 重试 / 搜索 / 生成 / 校验 / 查看详情 / 上一步 / 下一步 / 开始 / 停止 / 提交 / 复制 / 冻结 / 解冻…，共 40 余处，涉及 20 个文件）；弹窗里的「取消 / 关闭 / 返回」以及纯区间预设、无动词语义的按钮（筛选切换、全选/全不选、移动/重命名）保持纯文字，避免图标噪声。
- **说明类内容改为工具栏「说明」按钮 + 气泡**：新增 `OpsHelpButton`（图标 + 文字按钮，点开在 Popover 里展示 label/value 明细）。用户页「角色与权限」、令牌页「使用与安全」、设置页「生效与回滚」从表格下方的常驻卡片搬到概览带右侧的「说明」按钮里——**完全不占页面布局**，需要时才展开（窄屏同样适用）。
- **列表页统一「固定视口内滚动 + 表头吸顶」**（用户、令牌、开源协议、搜索），搜索页去掉嵌套滚动容器（嵌套会使吸顶失效）。
- **公开界面导航修复**：面包屑非末级改为可点击链接（此前为纯文本，从列表进详情后无路可返）；匿名侧栏补「全部仓库」固定入口与空态提示；匿名视图收起可见性与连接状态列。
- **移动端响应式**：面包屑窄屏只保留末两级；仓库详情的「文件树 + 详情」双栏在窄屏改上下堆叠（此前 390px 下详情栏被压到文字竖排）；仓库列表与用户管理的多列表格在窄屏只留「主标识 + 操作」，被裁列信息落到主标识下方副文本；审计中心 12 项 KPI 窄屏切紧凑横带。
- **刷新入口收敛到页眉**：删除仪表盘、迁移列表、备份列表页内重复的刷新按钮——这些页面的数据都由全局刷新事件驱动，页眉一处即可更新当前页全部内容（已实测：迁移页一次刷新触发列表与活跃任务两处重拉，仓库页触发列表与格式两处重拉）。
- **切页面先出框架、再填数据**：关闭路由的 `startTransition`（开启时 React 会**保留旧页面内容**直到新 chunk 就绪，期间点导航毫无反馈，慢网速下就是"切不过去"）；`RouteFallback` 与 `AsyncBoundary` 的首载占位从居中转圈改为**结构化骨架**；导航项在 hover / 聚焦时预取目标页 chunk，把骨架窗口压到最短。

### 修复

- **仓库「使用说明」不随界面语言切换**（真机 E2E 发现）：切到英文后，仓库详情页的使用片段仍是中文——「下载制品（curl）」「以 API Token 作口令，用户名任意（公开仓库可匿名读）」等。根因是这些文案由**后端** `usage.go` 硬编码（它知道协议路径与对外基址，只能由后端组装），此前不感知请求语言。现按 `Accept-Language` 返回对应语言：新增 `api/language.go` 的 `requestLang`（逐个标签取首个可识别项，`zh*`→中文、`en*`→英文、其余与缺失头回退中文，与前端语言策略一致），`usage.go` 按语言组织全部 11 段文案（raw/maven/npm），**命令与配置片段本身与语言无关、不参与本地化**；契约与前端零变更。
- **英文界面残留中文全角括号**：审计中心的操作者展示是 `admin（Console session）`——括号硬编码在模板里，与语言无关。现将「操作者 + 认证来源」整体交给 i18n 文案（中文用全角、英文用半角），并加回归守卫（断言英文界面不含全角括号）。

- **补上脱敏提交漏掉的一处真实域名大小写变体**：`b640c33` 已把该域名纳入替换集合，但 `settings_test.go` 里的**输入端大写变体**（用于验证白名单归一化转小写）被漏掉，经归一化回读后真实域名残留在测试代码里。现输入端改 `REPO.EXAMPLE.ORG`、期望端改 `repo.example.org`，**大小写归一化的测试语义不变**（具体域名不在本文重复，以免在公开仓库里二次暴露）。git 历史中的 9 个旧地址已评估为低风险并在 `docs/OPERATIONS.md` §1.3 登记为「已知、接受、不重写历史」。
- **`main.go` 的版本默认值与 `VERSION` 漂移**：裸构建（未注入 `-ldflags`）时版本显示 `0.7.1`，与 `VERSION`（`0.8.1-dev`）不一致——而该变量上一行的注释正是「默认与 VERSION 文件一致」。现对齐为 `0.8.1-dev`，裸构建 / `go run` 下 `status` 显示正确版本。
- **HTML 目录索引页在目录较大时列表既不完整也不正确**：页面按「前缀下的**前 1000 条制品**」推导直接子项，而页脚显示的「共 N 项」其实是**前缀下递归制品总数**（仓库根时即整个仓库的制品数）——两个口径都不是「当前目录有多少项」。由于制品按全局 `path` 升序取数，某个子树会吃满 1000 条配额，同层的其它子目录整片消失（例如根目录只列出 `a/`、`z/` 完全不见），且页脚数字对不上。现改为在数据层按层级聚合：子目录由 SQL 侧 `DISTINCT` 出层级段名并**全量展示**（目录项规模只与同层目录数相关，不参与分页），只有直接文件分页——默认 500/页，`?per_page=` 可在 50–5000 间调整、`?page=` 翻页，配「首页 / 上一页 / 下一页 / 末页」与每页条数切换（纯链接、无 JS，翻页保留其它查询参数）；页脚给出精确的「目录 N 个 · 文件 M 项 · 当前显示第 i–j 项」，越界页码收敛到末页而不是显示空页。单次响应规模由此与子树规模解耦：根目录这类「本身项不多、递归制品极多」的目录一屏看全，真正几万个直接文件的目录也能翻到末页。顺带修掉同一根因的另一处：目录列举（前端目录浏览走的 `/repository/{name}/tree` 路径）此前会把前缀下**整棵子树**的制品行读进内存再去重目录，现只回传同层项，内存占用不再随子树规模增长。
- **HTML 目录索引页补齐单个文件的时间与校验和，并给出目录体积合计**：文件行此前只有名称与大小，无法核对「这份产物是什么时候传的、和本地那份是不是同一份」。现每行新增**创建时间（UTC）**、**修改时间（UTC）**两列，以及**校验和**列——摘要位显示短 SHA-256（前 12 位，悬停可看完整值），点开用原生 `<details>` 展开完整 **SHA-256 / SHA-1 / MD5**（纯文本、可选中复制）；历史数据里未落 SHA-1 / MD5 的行显示 `—` 而不是空白。时间以 `<time datetime>`（RFC3339、恒为 UTC）承载机器可读值，展示文案由页内脚本按访问者时区改写——**禁用脚本时回退为服务端渲染的 UTC 文本并把表头标回 `(UTC)`**，信息不丢失，机器可读值与 API 返回、ETag 校验始终同口径。目录统计行补上**合计体积**（同层直接文件的 `SUM(size)`，由数据层一次聚合，不额外扫表），子目录行不可用的列一律 `-` 以区别「无此字段」与「字段为空」。**目录行同步补齐**：大小列给出子树内制品总数、修改时间列给出子树内最近一次更新时间（目录没有自身的创建时间，该列保持 `-`），两处都用 `title` 标注口径以免与文件行的同名两列混淆；目录统计与段名聚合为**同一次 SQL**（`GROUP BY seg` 取代原单独 `DISTINCT`），浏览热路径的查询数不增。为避免窄屏被挤爆，表格外层加横向滚动容器并给最小宽度，窄屏整表可横向滚动查看。

- **懒加载失败会整页白屏**：此前应用没有任何错误边界，`React.lazy` 的 chunk 加载失败会让整棵组件树崩掉、只剩白屏——触发条件并不罕见：发版后停留在旧页面的用户点导航请求已被删除的旧 chunk、网络抖动导致脚本下载中断、dev server 重新预构建依赖期间的 `504 Outdated Optimize Dep`。现新增 `RouteErrorBoundary`，按路由路径强制重建（避免一次失败污染后续所有页面），给出明确提示与「重新加载」入口。
- **导航链接没有 `href`**：侧栏导航只有 `onClick` 导航，渲染出的 `<a>` 没有 `href`，于是中键新开标签、右键复制链接地址、爬虫跟进全部失效。现渲染为真正的 `<Link to>`，保留预取与路由高亮。
- **迁移详情页违反「刷新入口唯一 / 首载占位唯一」**：删除页内重复的刷新按钮（数据仍由页眉刷新一并更新），首载从居中转圈改为结构化骨架；搜索页与仓库文件树的首载同步改为骨架。
- **搜索页结果行的下载按钮缺少可读名称**：只有 Tooltip 提示，读屏与键盘用户拿不到按钮名称，补齐 `aria-label`。
- **仓库列表的置顶只在当前页内生效**：置顶是本地偏好、名称筛选是客户端条件，服务端分页都无从参与，于是一页之外就看不到置顶效果。改为一次拉全量（超过契约单页上限 100 时按页拼接）+ 客户端「置顶前置 → 名称筛选 → 分页」，置顶项现在恒在列表最前，不受分页影响。
- **仓库列表的筛选框与同行控件不对齐**：它只有 placeholder、没有 label，控件顶边比同行的排序 / 方向 / 分组矮一截，整排看起来是歪的；补齐 label 并对齐基线。
- **仓库列表每页条数写死 10 行**：表格区高度已经撑满，但每页恒渲染 10 行，高屏上数据只占顶部一小截、下方一大片空白（看起来像"数据是固定的"）。现每页条数随滚动区高度自适应（量出可用高度与真实行高换算，下限 10 行、上限 50 行作渲染护栏）。
- **置顶的仓库行没有视觉高亮**：根因是 Mantine 默认色板里**没有 `amber`**——`var(--mantine-color-amber-6)` 与 `color="amber"` 都会静默失效（图钉继承 `currentColor` 变成黑色）。现统一改用色板中真实存在的 `yellow`，并给置顶行整行高亮（半透明琥珀底 + 左侧色条）；底色用半透明渐变层而非 `background-color`，以免顶掉 Mantine 表格用类选择器写的斑马纹与 `highlightOnHover`（否则置顶行会失去悬停反馈）。
- **仪表盘的仓库数与仓库列表不同源**：观测端点的仓库数写死 9、仓库状态面板又写死按 50 条拉取，档位放大后同一屏里出现「列表 144 / 仪表盘 9 / 面板共 50 个仓库」。现三处同源：观测端点从数据层取实际总数，面板改为拉全量仓库（新增 `listAllRepositories` 作为"拉全量 + 按契约单页上限拼接"的唯一入口，仓库列表页也改用它，去掉重复的分页拼接实现）。
- **窄屏（<48em）下操作按钮竖排把行撑高**：按钮带上文字后「操作」列变宽，390px 下三个按钮只能竖着排、可见性表头也被压成三行。现窄屏把行内操作从「操作」列移到主标识（仓库名 / 用户名 / 令牌名）下方——那里有整行宽度，一行放得下；同时把可见性 / ID / 创建时间等列并入下方副文本（仓库的可见性仍是可点击徽章），令牌页补齐窄屏收敛（此前 4 列挤在 390px）。
- **DevMock 控制台自身的三处问题**：①「数据量」列头标签宽 34px 装不下 3 个汉字，被挤成竖排的「数/据/量」→ 加宽到 44px 并 `flexShrink: 0`；②面板内的「重载本页数据 / 收起控制台」与右下角入口球都还是纯图标（看不出是重载还是关闭）→ 统一为「图标 + 文字」（重载 / 收起），入口从 48px 齿轮球改为带文字的胶囊按钮；③窄屏面板 236px 装不下「标题 + 重载 + 收起」，标题折成两行 → 窄屏改 262px。
- **窄屏选中文件后的两处漏判**：①只有非管理员点文件会触发「树让位」——管理员点文件走 `onNodeInteraction`（多选路径）直接 `setSelected`，绕过了 `onSelectFile`；②即使用 `useEffect([selected])` 派生也会漏掉「换文件后又点回同一个文件」（同一个对象 → React 跳过更新 → 副作用不触发）。现两条选中入口统一收敛到 `selectFileAt()`，在该函数里显式收起。
- **搜索页最后一处裸图标按钮**：输入框右侧的「语法帮助」问号图标看不出点开是语法说明还是别的帮助 → 移到筛选 / 搜索同一排做成「图标 + 文字」按钮（全仓纯图标按钮至此清零）；该工具栏同时改为可换行并给输入框 `minWidth: 200`——否则窄屏下三个按钮会把输入框挤到 44px，完全没法输入。
- **页眉按钮文字被挤到截断**：页眉右侧操作组加 `flexShrink: 0`，让面包屑去占剩余空间（面包屑已按窄屏只留末两级）。
- **`backups.copyCommand` 文案键缺失**：该键从未在 `zh.ts` 里定义，此前只被 Tooltip 引用（显示成字面量 key 也不易察觉），按钮化后直接暴露在按钮上；补上「复制命令」。
- **概览带在窄屏被操作按钮挤到只剩一条缝**：390px 下状态徽章 + 保存按钮与摘要区同排，摘要区被压到几十像素宽，标签与数值全被 ellipsis 截成「匿…」『未…』；令牌页的完整签发时间戳同样被截成「2026-0…」。现给摘要区设 `flex-basis: 320px`，放不下时由 Group 的 wrap 把操作整行换到下一行、摘要区独占整行；令牌页概览带窄屏改单列并只取日期（完整时刻仍保留在表格里）。
- **管理端三处时间显示与浏览器时区不一致**：后端一律存 UTC，界面应跟随访问者时区，但有三处漏网——①迁移详情的「生命周期」展示计划创建时间的**裸串**（同组件的其它两处已走统一入口）；②仓库详情页头概览把创建时间**按 UTC 直接截前 10 位**取日期，东八区下 00:00–08:00 创建的仓库会显示成前一天；③可观测的审计聚合按 `toISOString()` 的 **UTC 天 / 小时**分桶，标签也是 UTC 串，于是本地 09:30 的记录被标成「01:00–01:59」，跨日时还会整批归错桶（UTC 20:00 在东八区已是次日 04:00）。现三处统一走 `lib/timeFormat` 的入口（新增日期粒度 `formatUtcToLocalDate`），分桶键与标签都按浏览器本地时区计算，非法输入退化为原串而不是抛错。
- **匿名访问仓库页会打管理端点留下 401**：启用格式是 admin 端点，而非管理员既不能也不该建仓库，此前每次进入仓库页都会请求它并在网络面板留下一条无意义的 401。现按管理权限收口（非管理员直接跳过）。
- **列表页内容区未撑满可用高度**：`AsyncBoundary` 的内层容器不是 flex 容器，而 `flex: 1` 只在 flex 父容器里生效——列表自己写的 `flex: 1` 全部失效，表格下方留一大片空白、分页也无法贴底。该层已改为 flex column。
- **DevMock 档位放大产出"假条目"（仓库详情打不开）**：早先档位放大只是在**响应里**把种子条目重复改名（`scaleList`：仓库名加 `-v{r}` 后缀、数值主键加数量级偏移），store 中并不存在这些仓库——列表里点进去，所有按名查询（详情 / 文件树 / ACL / 用量）一律 404「仓库不存在」；同一函数还会让字符串业务主键（如审计 `eventId`）在克隆之间重复，被消费方当作标识时会落到错误对象上。现改为在数据层**物化**：档位变化（或模块加载）时把种子仓库展开成真实的克隆仓库，连带复制制品树 / 连接状态 / ACL，列表、深链、文件树、ACL、用量与写操作看到的是同一份数据；`scaleList` 与被用来放大的审计列表端点一并清理（该端点本就无前端消费者，审计中心走观测端点）。
- **接口缓慢或挂起时整页卡死**：趋势图按上限降采样（240 点，三条序列共用同一组索引）；大序列悬停读数按帧合并；`ResponsiveContainer` 加 debounce；观测页轮询在上一轮未落地时跳过；同缓存键在途请求去重；页眉刷新按钮加 10s 兜底（此前只要有一个无关请求挂起，刷新按钮就被永久锁死）。
- **60s 静默刷新从未生效**：`useVisibleRefresh` 的 reload 改为 ref 持有，此前调用方传内联函数会让定时器每次渲染重建，同时监听器反复增删。
- **同键在途去重被跨语义共用击穿**：公开态 `RepoBrowser` 与仓库详情页共用缓存键但取数语义不同（前者返回 `null` 占位），去重把占位结果复用给页面，导致页头仓库信息永远停在骨架。现公开态不参与去重与缓存。
- 发布说明的「变更摘要」为空：`release.yml` 摘录 CHANGELOG 时只识别 `## [X.Y.Z]` 形式的标题，而 0.7.0 起的标题写作 `## X.Y.Z（日期）`，导致 0.7.0 / 0.7.1 / 0.8.0 三版 GitHub Release 的摘要段为空。现同时兼容 `## [X.Y.Z] - 日期`、`## X.Y.Z（日期）`、`## X.Y.Z (日期)` 与 `## X.Y.Z` 四种写法。
- **语言策略三优先级未真正落地（FR-139 收尾）**：此前英文资源与页眉切换「能用」，但按「用户显式选择 > 浏览器语言 > 路由默认」的解析优先级只实现了一半——公开页能跟随浏览器语言、页眉切换能立即生效，但「管理页默认中文、不被浏览器语言悄悄替换」与「显式偏好在任意路由上都优先」两点的真机表现未被守护。现补齐并固化：公开路由（`/repositories`、`/repositories/:name`、`/search`、`/p/:name`）按 `navigator.language`，管理页与 `/setup` 默认中文，用户切换后偏好写入 `localStorage` 并在任意路由优先。新增 `src/i18n/language.ts`（解析优先级与偏好读写）、`src/i18n/useLanguage.ts`（切换 hook）、`src/i18n/current.ts` 的 `currentLocaleTag()` 改为由语言状态派生；`src/i18n/language.test.ts` 与 `test/LanguageSwitch.test.tsx` 以真机三优先级为回归守卫（含「格式化输出跟随语言」与「偏好被记住」）。顺带修掉一处隐蔽硬编码：`AppLayout` 的面包屑末级文案字典把 `/dashboard` 写死「业务仪表盘」、不经 i18n，切英文时仍残留中文——现新增 `nav.dashboardPage` 键、仅该路径走 i18n。
- **两个历史不稳定用例的等待上限（更正此前的无依据放宽）**：①`test/ViteMockIsolation.test.ts` 生产构建用例：此前据"实测 56–75s"把预算从 `60s` 抬到 `120s`，**该依据不成立**——重新实测（本机空闲、连续 3 次）`vite build` 常态仅 **21–23s**，`60s` 相对常态有约 2.6 倍余量，原先并不偏短；已**回退为 `60s`**（实测通过）。②`test/AppRoutes.test.tsx`「登录访问 /host-monitoring」用例：该页含 recharts 实时图表，串行实测仅 1.1–1.4s，原 `5s` 上限本有约 3.5 倍余量，偶发超时成因是**并行争抢**而非常态过慢；等待上限取 `8s`、用例级超时 `12s`（均低于全局 `testTimeout: 15s`，仍有 5 倍以上余量）。两处真回归仍会稳定断言失败，不掩盖信号；若仍偶发，根因应查 CI 并发度而非继续抬上限。
- **i18n 收口遗漏补齐（FR-139 收尾）**：此前把「无 locale 参数的 `toLocaleString()`」收敛到 `currentLocaleTag()` 只覆盖了审计中心、仪表盘、仓库列表等页面，迁移与搜索相关组件漏了 11 处裸调用（迁移进度/计划仓库表/结果四宫格/向导统计、搜索聚合计数、文件详情字节数），数字仍按浏览器默认 locale 渲染，与中/英界面语言分叉。现统一改为 `toLocaleString(currentLocaleTag())`，全仓前端零处裸调用残留。搜索页「全部」文案也不再冗余带 `defaultValue`（键已录入）。
- **HTML 目录页 `<time datetime>` 显式按 UTC 解析（行为等价，非缺陷修复）**：`browse.go` 的 `toISOUTC` 改用 `time.ParseInLocation(assetTimeLayout, s, time.UTC)`。**更正此前的错误归因**：Go 的 `time.Parse` 在 layout 与值都不含时区指示时本就返回 **UTC**（实测：`TZ=Asia/Shanghai` 下 `Parse` 与 `ParseInLocation(..., time.UTC)` 结果完全相同），因此原实现并不存在"按服务器进程时区解析、在 UTC 机器上巧合正确"的问题——本次改动是**显式化时区意图**，行为与原先等价，未改变任何输出。
- **公开路径判定收紧，修掉一处隐性误判**：`isPublicPath` 原先用 `pathname.startsWith(prefix)` 宽松匹配，`/repositoriesXYZ` 这类不成路径也会被判为公开浏览面（落到「跟随浏览器语言」分支）。改为「等于前缀，或前缀后紧跟斜杠」的精确匹配。注意 `PUBLIC_PREFIXES` 原先写成 `/p/`（自带尾斜杠），若直接照搬 `prefix + "/"` 会拼出 `/p//maven-public` 双斜杠、把真实公开 ` /p/:name` 路由判成管理面——故前缀统一改为不带尾斜杠（`/p`）存储、比对时再补回。既有 `/p/maven-public` 用例因此修正而过。
- **主机监控采样范围档位改走 i18n**：`HostMonitoringLive` 的 5 个档位（近 1 小时/6 小时/24 小时/7 天/30 天）此前是硬编码中文，与同页 `DashboardRangePicker` 走 `labelKey` 的口径不一致；现新增 `hostMonitoring.range1h..range30d` 键、渲染时经 `t()` 取文案，并补一条「切英文后档位显示英文、无中文残留」的回归守卫（该用例原本没有，属于「改了但无守护」的缺口）。
- **审计目标 `kind` 语义漂移修复**：上一处「不再用仓库名覆盖 `label`」的改动连带删掉了 `Repository != ""` 分支里的 `Kind = repository`，导致 `EntityType` 不在映射表内但关联了仓库的事件（如 `publish_policy.update`）`kind` 由 `repository` 静默变 `other`。现补回回落逻辑（未匹配且仓库非空 → `repository`），并新增 `toAPIAuditTarget` 表驱动单测覆盖 asset / repository / publish_policy / 无仓库四类。
- **审计目标 `label` 截断到契约上限**：契约给 `label` 定 `maxLength: 512`，而制品路径在契约里没有长度上限——长路径会把 `label` 顶穿上限（`oapi-codegen` 不校验响应，CI 也发现不了）。现服务端按 512 **字符**（rune）截断并留省略号，附回归用例。
- **审计列表主时间列跟随语言**：`formatFullTime`（宽屏审计表的时间列）此前是手写 `getFullYear/getMonth` 拼接，完全绕过 locale，是上一轮 locale 收敛唯一的漏网路径——英文界面下与中文逐字符相同。现改走 `localizedFormatter`，并补一条"切语言后该列输出必须变化"的守卫（原守卫只断言另一条 `formatTime`，属假绿）。
- **审计预览 result 枚举澄清（消歧义）**：固定预览夹具的 `成功/失败/已应用` 值域原被注释为"数据契约的单一真源"，但其类型名 `AuditResult` 与 `api/types.ts` 里由契约生成的同名类型撞车、值域完全不同。现改名为 `AuditPreviewResult`，注释明确"这是预览夹具自己的值域，非 API 契约"。
- **`en.ts` 英文注释译为中文**：新增英文资源时带入了 32 行英文注释，违反「注释必须使用简体中文」的硬规则（且同位置 `zh.ts` 为中文，两资源文件注释语言分叉）。现全部译为中文。
- **慢用例观测脚本修正四处误导**：①环境变量未校验数值，非法值（如 `abc`）会让 `NaN` 使所有判据静默失效却仍报绿、且历史窗口失效导致无界增长——现非法即报错退出；②样本不足时终局仍打印"全部在预算内"（把"没证据"粉饰成"正常"）——现终局三态，显式报出"因样本不足未给判据"的项数；③只观测了 `/host-monitoring` 一处，而 `ViteMockIsolation` 的放宽同样无观测——现两者都纳入；④`--print-history` 与 P95 判据分组字段不一致（改名后打印的 n 会偏大）——现统一按 key。
- **CI 慢用例历史改用 `cache/restore` + `cache/save`**：组合式 `actions/cache` 的保存半程受 `post-if: success()` 门控，写在步骤上的 `if: always()` 只对 restore 半程生效，质量门失败时历史不会被保存；且同一 run 重跑时 `run_id` 不变会精确命中上次条目并跳过保存。现改为两个子 action（save 带 `if: always()`），key 带 `run_attempt`。

- **目录页 `formatSize` 加单位上限保护**：字节格式化在 `exp ≥ 6` 时 `"KMGTPE"[exp]` 会索引越界。需说明：`n` 为 `int64`（上限约 8 EiB），`exp` 最多到 5，越界需 `n ≥ 1024^7`（超过 int64 上限），故该 panic **对合法输入实际不可达**——本次是纯防御性护栏，非修复已发生的崩溃。
- **审计目标不再被仓库名覆盖（先看清单需求的第一步）**：`toAPIAuditTarget` 原先在有仓库时把 `target.Label` 覆盖成仓库名，而 asset 操作的 `entity_key` 本身是 `repo/path`——于是**具体被操作的制品路径在服务端就被丢掉**，审计里只看得到仓库名、看不出动了哪个文件。现不再覆盖（保留 `EntityKey`），`Kind` 仍由 `EntityType` 的 switch 判定。这是「展示具体文件清单」的前置修复。

  **遗留（未做，需新增契约字段）**：完整的文件清单仍未实现。缺在三层——Go 侧批量删除是「每文件一条独立审计行」、靠 `correlation_id` 关联，无字段存全量清单（但 `asset_mutation_item` 表已存完整路径、`AssetOperationResult.Paths` 已算出清单，只是没进审计、没出 API）；契约 `AuditEventSafeDetails` 只有 `resultSummary/errorClass/affectedCount`；前端列表副文本优先显示 `http.path`（路由模板）、详情无清单区。另：上传（`asset.put`）目前是单条审计且无 `correlation_id`，批量上传天然无法归组。

- **切换英文后残留的硬编码中文清理（全站扫描）**：`en.ts` 本身是干净的（0 中文残留、中英各 1093 个一级键双向零缺失、`satisfies Resources` 通过），问题全部出在**源码绕过 i18n 的硬编码**。本次修掉会渲染给用户的 7 处：迁移向导冲突策略下拉 3 项（`skip/overwrite/fail`，同组件 label 已用 `t()` 而选项却是硬编码）、迁移详情「暂无迁移候选」Alert 2 处、路由错误边界 3 处（class 组件取不到 hook，直接调 i18next 的 `t`）、路由骨架 `aria-label`、仓库列表「N 个制品」量词（复用既有 `repositories.assetCount`）、趋势图「持平」（`computeStats` 改为接收已翻译文案）。另删掉 `PreviewRangeControls` 里 4 处**永不生效的死代码**（内置 `DEFAULT_RANGES` 与默认 `ariaLabel` 带硬编码中文，但唯一调用点始终自行传入翻译后的档位；已改 `options`/`ariaLabel` 为必填）。**未改**：devmock 的 `cutover.checklist` 等 mock 业务数据（模拟后端返回的中文数据，真实后端同样返回中文；前端 i18n 只翻译界面文案，不应伪造翻译业务数据）；106 处 `t(key,{defaultValue:"中文"})` 是键都存在时的死兜底，当前不触发，仅作未来隐患记录。
- **审计事件 result 枚举收口为单一真源**：`auditGrouping` 的成功/失败计数此前直接拿中文字面量 `"失败"` 与事件 `result` 做等号比较，而该字面量与 devmock 数据契约（`AuditPreviewEvent.result`）各写一份。这里**刻意不改成读 i18n 键**——`result` 是内部判定用的数据标识而非展示文案，一旦翻译文案改动（如「失败」改「未成功」），判定会静默把所有失败事件计成成功。现抽出 `AUDIT_RESULT` 常量（`observabilityPreview.ts`），类型定义、种子数据与聚合判定三处统一引用它；展示文案仍走独立 i18n 键，两者解耦。
- **为放宽过等待上限的负载敏感用例补观测（`scripts/watch-slow-tests.mjs`）**：`test/AppRoutes.test.tsx` 的「登录访问 /host-monitoring」用例等待上限已放宽到 12s（用例级 20s），此后它**对性能退化失去敏感度**——首屏从 2s 退化到 11s 测试仍然绿。新增该脚本：单独跑被观测用例、从 vitest JSON 报告取耗时，超预算（默认 10s，可用 `SLOW_TEST_BUDGET_MS` 覆盖）时发 `::warning::` 告警；已接入 `scripts/check.sh`（本地与 CI 共用），**仅告警不阻断**质量门。
- **慢用例观测升级为跨运行真 P95**：仅有单次阈值时，「单次偶发慢」与「持续退化」无法区分（前者是噪音、后者是真问题）。现脚本把每次采样追加写入历史文件（`.tmp/slow-history.json`，每用例保留最近 200 条），并计算该用例的 **P95**：P95 超 `SLOW_TEST_P95_BUDGET_MS`（默认取单次预算）即告警——本次单跑哪怕完全正常，只要历史 P95 顶穿预算就会告警，这才是退化逃逸的兜网。历史跨运行累积由 `.github/workflows/ci.yml` 与 `release.yml` 用 `actions/cache` 在质量门前 restore、后 save 实现（cache key 带 `run_id` 以便每次写入新条目，`restore-keys` 回落读最近一次）。诚实口径：GitHub cache 是**尽力而为**，可能 miss 或被清理；样本不足（n<5）时脚本明确打印「不足以给出可信 P95，跳过该项判据」，不会把「没有历史」粉饰成「性能正常」。

### 工程

- **部署脚本对齐现网形态**：`remote-ssh.sh` 原本自行 `kill + nohup + pid 文件` 启动服务，与现网的**用户级 systemd 托管**冲突（双进程抢端口），且只写 3 个环境变量（丢掉格式配置、把 JWT 换成脚本内置默认值——会导致所有登录态失效）。现改为：`remote-ssh.sh` 只负责密钥、构建与登录入口，**部署一律委托 `deploy.sh`**（`releases/<stamp>` + 切 `current` + `systemctl --user restart` + 探活失败自动回滚）。同时对齐配置键名：支持 `DEPLOY_DIR`（现网键名，`RELEASE_DIR` 兼容）、`DEPLOY_SERVICE`（不再硬编码服务名）、`DEPLOY_USER`/`DEPLOY_PORT`/`DEPLOY_SSH_KEY`（完整连接参数）、`DEPLOY_MODE=systemd-user`（现网写法），并新增 `DEPLOY_ENV=prod` 选择环境文件（`deploy/.env.prod`）。两处安全加固：部署前**检查远端 EnvironmentFile 是否存在**（缺失只给指引、不代写，避免覆盖密钥与格式配置）；回滚改为记录 `current` 原指向而非「版本列表第二个」，避免并发部署时回滚到错误版本。

- **慢用例观测改为复用构建验证的报告**：`ViteMockIsolation` 此前由观测脚本再跑一次真实 vite build，与 `check.sh` 的构建验证步重复执行（多耗 30–60s），且两次构建抢 CPU 会偶发失败并误报「未能通过」。现构建验证步输出 JSON 报告（`--reporter=json`），观测脚本直接读取，不再重复执行。
- **`.gitignore` 补齐 `deploy/.env.*`**：此前只有 `.env`、`.env.local`、`.env.*.local` 三条规则，而 `deploy/.env.prod`（含部署目标主机与密钥路径）**不在任何规则内**，随时可能被 `git add .` 误提交（经核实历史中从未提交过）。现按目录整体忽略 `deploy/.env.*`、仅保留模板 `.env.example`。

- **前端依赖大版本升级（Dependabot PR #9#10#11#13 的等价落地）**：
  - **Mantine 7.17 → 8.3.18 全家桶**（core/hooks/form/dates/modals/notifications/charts，web + wiki + ui 三处必须同版本，否则 Provider 混用会崩）。两处适配：① `DatePicker` 在 8 起改用日期字符串（不再接受 Date 对象），`DashboardRangePicker` 的状态改为本地日期串并保持原有本地零点语义；② Mantine 8 新增 `env` 机制——默认环境下浮层（Popover/Select）依赖真实浏览器的异步定位，在 jsdom 里会停在 `display:none`（表现为"下拉打不开"），`AppProvider` 现仅在测试环境声明 `env="test"`，生产不变。另适配一处罚查询：Mantine 8 的 Select 会把 label 同时关联到输入框与选项容器（标准 ARIA combobox 模式），`getByLabelText` 会命中多个元素，测试改按 `role=textbox` 精确定位。
  - **i18next 23 → 26 + react-i18next 15 → 17**（跨 3 个大版本）：类型检查与 i18n 专项测试（键集守卫、三优先级、格式化跟随、偏好记忆）全部通过，无需改代码。
  - **jsdom 25 → 29.1.1**：跨 4 个大版本，测试环境行为无回归。未采用 Dependabot 提的 30（其要求 Node ≥24.15，当前工具链为 24.11）。
- **`go.work` 的 go 指令对齐 `go.mod`**：此前 go.work 写 `go 1.25.0` 而 go.mod 已是 `1.26.0`（漂移），`go get` 时由工具链自动同步为 `1.26.0`。
- **测试并发与构建验证的隔离（消除 Mantine 8 引入的负载敏感抖动）**：Mantine 8 后每个测试 fork 的启动成本显著上升（jsdom + Mantine 8 + 中英各 1093 键的资源），默认按 CPU 数开 31 个 fork 时内存/CPU 峰值过高，会把需要渲染与请求的用例拖过等待上限（表现为「找不到文本」「构建 120s 超时」这类**非确定性失败**，实测同一提交有时全绿、有时 3 个失败）。两项处理：① `maxWorkers: 16` 限制并发（弱机器如 CI 的 4 核自然低于该上限，不受影响；实测 `isolate: false` 会引入测试间状态泄漏，不可用）；② 把 `ViteMockIsolation`（spawn 一次真实 vite build，60–120s）**移出并行单测套件**，改用独立的 `vitest.build.config.ts`（node 环境，比 jsdom 更贴合）由 `check.sh` 在单测之后串行执行——单测主套件因此回到 58–84s 且连跑稳定，构建验证在 CPU 空闲时 33–55s 完成。慢用例观测脚本同步支持 per-target 配置（否则主配置的 exclude 会让它误报「未能通过」）。
- **已知代价**：Mantine 8 使测试总耗时上升（前端主套件 + 独立构建验证合计约 2 分钟，此前约 50s）；功能零回归（4 包 364 测试全过、完整质量门全绿）。

- **依赖升级：vitest 2→4 全链（含 vite 5→7、plugin-react 5、coverage-v8 4）**：Dependabot security updates 开启后自动为 vitest 的 critical alert 开出跨主版本升级 PR（2.1.9→4.1.11），本地落地验证后合入。vitest 4 硬性要求 vite ≥6，故连带升级：vite 5.4→7.3.6、@vitejs/plugin-react 4→5.1.2、@vitest/coverage-v8 2→4.1.11；devmock/ui 包此前靠提升隐式获得 vite，vitest 4 的 peer 检查后显式声明 vite 7。**配置零改动**（coverage/超时/装置全兼容）；唯一代码适配是 `BrandLogo` 测试断言——vite 7 的 vitest 环境把小资源内联成 data URI（vite 5 返回 `/src/assets/` 原路径），放宽为两种形态皆可，仍拦截 public/ 直接引用。全量回归：4 包 364 测试全过、vite 7 生产构建 19.2s 正常、产物隔离（worker 不入包）仍有效、完整质量门全绿。react-router-dom 6.30.4→6.30.6（另一个 Dependabot PR，补丁级）一并本地落地。

- **仓库安全设置开启与安全扫描触发器修正**：①Dependabot alerts 与 Dependabot security updates 由 API 开启（此前 alerts 未开，一键查得 30 条存量 alert——集中在 vitest/vite 等开发工具链，登记于 `docs/OPERATIONS.md` §1.3，主版本升级另行安排）；②osv-scanner 与 CodeQL 的 push 触发器补上 `dev` 分支——GitHub 只按默认分支上的 workflow 注册触发器，此前推送 dev 不触发扫描，开发窗口处于盲区；③CodeQL action 升级 v4（官方公告 v3 于 2026-12 弃用）。三 workflow（CI/CodeQL/依赖漏洞扫描）已实测随 dev 推送全绿。
- **新增 Dependabot**：每周（周二）为前端 pnpm workspace、后端 Go 模块与 CI 的 `actions/*` 分别开更新 PR，由 `ci.yml` 的质量门验证后人工合并。本仓库契约由 `api/openapi.yaml` 生成（`schema.gen.ts` / `api.gen.go` 均已入库），Dependabot 只改依赖版本、不触碰契约源文件，因此不会触发重新生成。
- **新增依赖漏洞扫描（osv-scanner）**：`pnpm audit` 在本仓库**不可用**——`.npmrc` 固定 `registry=https://registry.npmmirror.com/`，而镜像源没有 audit 端点（实测 `ERR_PNPM_AUDIT_ENDPOINT_NOT_EXISTS`）。改用 `osv-scanner`：它读 lockfile 后查 OSV 数据库，与 npm registry 解耦，因此不受镜像限制，同时覆盖前端 `pnpm-lock.yaml` 与后端 Go 模块。作为**独立 workflow、软失败**（`fail-on-vuln: false`）——漏洞信息走 SARIF 进 Security 面板供人工跟进，不阻断发版；Go 侧的 `govulncheck` 仍保留在质量门内作硬门禁。
- **新增 CodeQL 语义扫描（Go + TS/JS）**：`govulncheck` 只报已知 CVE、`eslint` 偏风格，CodeQL 补语义级分析（注入 / 路径遍历 / SSRF / 凭据泄露），对本项目手写的安全敏感代码（`internal/upstream` 的 SSRF 校验与重定向凭据剥离）有增量价值。独立 workflow、**软失败**（需构建、耗时长、偶发误报）。
- **新增 `apps/server/.golangci.yml`**：此前没有配置，linter 集合是工具的**隐式默认值**（会随版本漂移、可复现性差）。现把实际生效的六个（errcheck / gosimple / govet / ineffassign / staticcheck / unused）显式钉死，并增补 `gosec`。**未开启** `bodyclose` / `sqlclosecheck` / `rowserrcheck`——实测这三项在本仓库生产代码命中为 0（HTTP body 关闭已统一封装在 `internal/upstream/session.go` 的 `readResponse`；SQL rows 的关闭与 `rows.Err()` 检查在 `persistence/readonly.go` 已写好），为零收益引入只会增加噪声。`gosec` 的排除项均写明理由（协议强制的 MD5/SHA-1、测试夹具、环境变量名误报等），真实关注点（备份解压大小、`VACUUM INTO` 拼接）以 `//nolint` + 理由注释就地标注。
- **后端覆盖率产出**：`check.sh` 的 `go test` 加 `-coverprofile`，覆盖率落到 `.tmp/go-coverage.out` 并打印总计。**只产出、不设阈值**——存量覆盖水平未知，一上来设阈值会卡死质量门。前端同样产出到 `.tmp/web-coverage/`（新增 devDependency `@vitest/coverage-v8@2.1.9`，与 vitest 2.1.9 版本对齐；实测开启后测试耗时由 47.6s 增至 51.9s，约 +9%，可接受）。当前基线：前端语句覆盖 **86.53%**、分支 78.03%、函数 64.62%。

### 文档

- 0.8.1 开发线文档补齐（无代码行为变更）：
  - **版本口径**：`ARCHITECTURE.md` 与 `PRD.md` §1 的开发窗口仍写 `0.9.0`（M3）、`PRD.md` §4 引言仍写 `VERSION=0.8.0` 已发布、`ROADMAP.md` §3 仍指向 0.9.0 窗口——四处统一为「`0.8.0` 已发布，当前开发窗口 `0.8.1`」。`ROADMAP.md` §5 新增 0.8.1 窗口（功能范围 §5.1 + 门禁 §5.2），与 §1 版本表、§5 自洽。
  - **PRD 新增 §4.4.1「开发中：0.8.1 · 控制台体验收敛」**：本窗口此前**没有登记节**（§4.4 直接跳 §4.5 的 0.9.0），已提交的 9 个提交按增强既有 FR 登记（FR-117/118/120/122/53/25c），净新的三条开为 FR-139（语言策略）、FR-140（时区统一）、FR-141（目录页制品信息补全），窗口内状态一律 `开发中`。
  - **ARCHITECTURE 新增前端范式章节**（§3.1）与**目录索引页时区例外**（§3.2）：把 `PageShell` / `OpsKpiBand` / 骨架与错误边界 / 单一刷新入口 / 时间口径 / 语言与格式入口 / DevMock 契约，以及「服务端只认 UTC、时区改写靠页内内联脚本」这一唯一零依赖例外写进架构真源，避免后续页面各自另起一套。
  - **specs/README 新增 0.8.1 规格节**并登记 FR-139 规格 [`0.8.1-i18n-language-policy.md`](docs/specs/0.8.1-i18n-language-policy.md)（原 §2/§3/§4 顺延为 §3/§4/§5）。
- 清除 0.8.0 发布后残留的三类文档漂移（无代码行为变更）：
  - **版本口径**：`docs/README.md`、`docs/OPERATIONS.md`、`docs/ARCHITECTURE.md`、`docs/API.md`、`docs/ROADMAP.md`、`docs/specs/README.md`、`docs/adr/README.md` 与 PRD §1/§4 引言仍写「当前发布版本 0.7.1、0.8.0 尚未发布」，现统一为「`VERSION=0.8.0` 已发布，当前开发窗口 0.9.0」；40 份规格文件的状态行按 PRD 真源订正为 `已交付@vX.Y.Z` / `已退役@v0.8.0`。
  - **级联复制退役口径**：README、OPERATIONS（§1.1 环境变量表、§1.2–1.4、CLI 清单、备份章节、排障项）、ARCHITECTURE（§1–§9）、API（§3/§4）与 `specs/README.md` 仍把主备级联复制描述为当前能力，现改为 FR-138 退役后的一致性备份包搬迁口径；已废弃的 `JIAN_REPLICATION_*` 改为显式「不再解析」说明，`jianartifact` 帮助文本同步去掉三个已失效环境变量并补齐真实变量清单。ADR-0023/0026 标记为已被 ADR-0027 取代，ADR-0027 补入 ADR 索引。
  - **PRD FR-40 口径**：`备份与恢复` 与已在 v0.8.0 交付的 FR-132–FR-137 重叠，现收窄为「周期化自动备份 + 恢复演练/校验报告」并注明不重复登记；FR-38 中引用已退役 `repl_change` 的表述同步更正。

## 0.8.0（2026-09-14）

### 新增

- 节点备份包与搬迁（FR-132～FR-136，见 `docs/specs/0.8.0-node-backup-and-relocation.md` 与 ADR-0027）：搬迁的传输单位从实时复制流改为**一致性备份包**（tar.gz 内封 `manifest.json` + `VACUUM INTO` 快照 + `blobs.index` + 内容寻址 blob），流程退化为「生成包 → 传包 → 导入包 → 校验」，不再需要节点角色、水位、代次或拓扑。包内**仅含数据，不含任何密钥或节点本地配置**（排除复制凭据密钥、JWT 密钥、迁移凭据密钥、`-wal`/`-shm` 与 blob 临时/隔离目录），`manifest.kind` 固定为 `jianartifact-node-backup` 以与 Nexus 迁移 bundle 区分。
- 备份生成（FR-133）：支持**热备份**（不停服，独立连接的 `VACUUM INTO` 取一致性快照，不占用业务主连接池）与**冻结窗口**两种模式；blob 集合以快照库 `asset` 表为准而非扫描目录，因此未被引用的残留 blob 不会进包；同一时刻仅允许一个生成任务，失败会清理半成品并把登记置为 failed，服务重启会清理遗留的「生成中」包。
- 备份列表与下载导出（FR-136）：新增 `/api/v1/backups` 系列端点与「迁移与搬迁」页面（与既有 Nexus 迁移合并为双 Tab，`?tab=backups` 可深链）。列表用 Mantine Table 呈现状态 / 方式 / 大小 / 规模 / 创建时间，状态走 OpsKit `StatusPill`；生成中在页内轮询进度。**下载提供带时效的签名链接**（HMAC 令牌绑定单个包与截止时间，签名密钥由启动密钥派生），使普通 `<a href>` 与新机器无需登录即可拉取，并支持 Range 断点续传。支持完整性校验（浅校验查 db 与索引摘要，深度校验逐 blob 比对内容摘要）与被增量包引用的基线不可删除。
- 搬迁备份 CLI（FR-132）：`jianartifact backup create [--mode hot|frozen] [--label …]`、`backup list [--json]`、`backup verify <包标识|归档路径> [--deep]`、`backup link <包标识> [--ttl 30m] [--base https://…]`、`backup delete <包标识>`。
- 写入冻结窗口（FR-135）：写栅栏从「静态角色只读」扩展为「运行时可冻结」。`POST /api/v1/maintenance/freeze`（body `{until?, ttlSeconds?, reason?}`）冻结节点写入，`GET` 查询状态、`DELETE` 解冻（幂等，未冻结也返 200）。**窗口必须有界**：给 `until` 须晚于当前且不超过 `now+24h`（否则 400），否则用 `ttlSeconds`（缺省 7200，钳到 [60, 86400]）；**故意不提供无限期冻结**——忘记解冻会让服务退化成假死。冻结期间拒绝本地业务与管理写入（503 + `write_frozen`），但放行：读方法、`POST /api/v1/auth/login`、维护命名空间 `/api/v1/maintenance/`、以及备份导入/上传路径（`/api/v1/backups/import{s}`、`/api/v1/backups/uploads`）——它们只写 `restore-staging/` 与 `restore.pending`、重启才生效。响应 `WriteFreezeState{frozen, until?, frozenAt?, reason?}`，未冻结时省略 `until`/`frozenAt`。
- URL 拉取导入与导入记录（FR-137，分片上传未开始）：`POST /api/v1/backups/import` 由服务端从 `sourceUrl`（http/https，必填）拉取并异步导入（202 + 记录），`GET /api/v1/backups/imports`（分页，最近优先）与 `GET /api/v1/backups/imports/{id}` 查进度与详情；另支持 CLI `jianartifact backup import <归档路径> [--overwrite] [--deep] [--yes]`。状态机 `queued → fetching → staging → pending_restart → done`，失败转 `failed`。**SSRF 防护**：拒绝回环/私网/链路本地/云元数据地址，每次拨号重新解析（防 DNS 重绑定），重定向重新校验。**导入规模护栏**（防解压炸弹，写盘前校验）：快照 8 GiB、blob 总量 50 GiB、blob 条目 500 万。错误码：`fetch_failed` / `package_oversize` / `sha256_mismatch` / `manifest_invalid` / `target_not_empty`（409）/ `incompatible` / `restore_pending`（409）/ `internal`；增量差包导入当前不支持（见 FR-134）。目标「非空」判据排除内置 `anonymous` 主体（迁移 0007 无条件植入，否则任何全新实例都会被误判非空）。
- 分片上传导入（FR-137 第三通道）：Web 把 GB 级备份包按服务端约定的 **8 MiB** 分片顺序上传，服务端落盘后组装并交本地导入状态机（仍需重启生效）。`POST /api/v1/backups/uploads`（init，201，返回 `uploadId`/`chunkSize`/`uploadedChunks`/`status`/`expiresAt`）、`PUT /api/v1/backups/uploads/{id}/chunks/{index}`（`application/octet-stream` 二进制体，200 返回当前会话）、`GET /api/v1/backups/uploads/{id}`（查询，供续传）、`POST /api/v1/backups/uploads/{id}/complete`（202，body `{sha256?, overwrite?, deep?}`）、`POST /api/v1/backups/uploads/{id}/abort`（204）。状态机 `initialized → receiving → completed`，随时可 `aborted`；会话有效期 24 小时，单包上限 50 GiB（与 URL 拉取同一护栏）。**续传按磁盘推导**：`uploadedChunks` 以磁盘上真实存在的分片为准，缺哪片补哪片、重复片幂等覆盖；`complete` 缺片会带缺失序号、不符声明 sha256 删除半成品；`abort` 仅删磁盘目录、记录保留 `aborted`（故 `GET` 返回 200 而非 404）。客户端输入类错误（片号越界 / 单片超额 / 缺失 / 空体）统一 400；`complete` 的 `overwrite`/`deep` 已支持。
- 增量差包与短停机切换（FR-134）：`jianartifact backup create --base <packageId>` 以某基线包生成差包，只携带新增 blob + 新 db，把停机窗口从"传整个包"缩到"只传新增 blob + 新 db"，可压到分钟级。每个包生成时额外写侧车索引 `${JIAN_DATA_DIR}/backups/<packageId>.index`（与包内 `blobs.index` 同格式）：全量包侧车 = 完整集合，差包侧车 = 应用后的完整并集（差包可再派生差包）。差包 manifest 新增可选 `expected`（`{count,totalBytes,indexSha256}`，全量包不写、`omitempty` 向后兼容），导入端算本地集合 ∪ 包内差集与其比对——一致才继续，不一致报"缺少基线包，请先导入基线包 <id>"且不留任何文件/标记；`basePackageId` 非空但 `expected` 为空（畸形手工包）仍报"增量包导入尚未支持"。被差包引用的基线不可删。
- 首个二进制契约响应：`GET /api/v1/backups/{id}/download` 以 `application/gzip` 直接流式返回归档，为后续分片上传 / 大文件导出确立二进制端点范式。

- 允许访问域名白名单（FR-129，见 `docs/specs/0.8.0-*` 与运维文档）：管理端设置页配置逗号分隔域名，配置后仅白名单 Host 可访问、IP:端口 直连返回 404；协议凭据按白名单放行 CDN 回源；节点本地即时生效。
- 回源 Token 校验（FR-130，见 `docs/specs/0.8.0-origin-token-guard.md`）：后台「安全防护」配置请求头名与随机 Token（默认关闭），开启后非回环请求必须携带 CDN 回源注入的 Token，直连源站一律 404；与 Host 白名单并存，节点本地。
- 服务内置 TLS（FR-131，见 `docs/specs/0.8.0-server-tls.md`）：`JIAN_TLS_ADDR` / `JIAN_TLS_CERT` / `JIAN_TLS_KEY` 环境变量配置后 HTTP 与 HTTPS 双监听（共用同一 handler），自签证书即可，供 CDN 回源 HTTPS 加密。
- 设置页（FR-117 / FR-129 / FR-130）改为双栏分区卡：左「服务设置」（匿名访问 / 对外基础 URL / 回源超时），右「安全防护」（回源 Token 校验）+「允许访问的域名」；顶部常驻保存与「有未保存的改动」提示，域名白名单改为可逐条增删、可粘贴完整 URL 的标签列表。
- 管理控制台页面数据缓存（FR-128）：仓库、ACL、用户、令牌、迁移、设置、搜索、仓库详情/浏览与仪表盘等页面在会话内缓存最近一次成功数据，路由切换即时回放并后台静默刷新，不再每次切换整页转圈；登录/登出/切换账号即清空缓存。
- 管理控制台完整开发态 Mock 验收矩阵（FR-122）：18 条页面路由均可按 URL 复现正常、空态、加载和失败；同步事件/同步历史、公开仓库列表与管理审计日志补齐 empty 场景空数据；仓库、账户、设置与迁移的关键写操作提供成功和失败反馈，加载请求与内存状态在测试后显式复位。
- 管理控制台信息架构与账户菜单（FR-117）：侧栏按概览、运维、管理分区，开源协议固定置底；页眉将身份与退出收敛为账户菜单，并为全局管理员提供当前节点最近 24 小时风险通知中心及 `attentionId` 审计跳转。
- 当前节点审计中心（FR-118）：迁移 `0026` 为审计增加关联标识，并新增追加式复制应用事件与确认身份快照；提供脱敏的概览、趋势、完整分页记录、风险批次、原子确认和至多 20 条通知预览。聚合读取限制为 5,000 条源事件，完整记录分页限制为每页 1–100 条。
- v0.8.0 制品格式与迁移扩展（FR-25 / FR-26 / FR-27 / FR-28 / FR-29 / FR-31 / FR-32）：Docker/OCI、Cargo sparse、PyPI Simple、Go modules proxy、NuGet，以及声明式格式启停（未启用格式零路由零后台任务）和 Nexus 扩展迁移来源。
- 当前主机监控（FR-120）：每分钟采样本实例所在主机的 CPU、内存、磁盘、网络、进程与 `/readyz`，保留 30 天；不跨节点读取、不参与复制。
- 统一制品操作与生命周期协调器（FR-104 / FR-105）：Raw hosted 支持目录递归删除、多选移动、单项重命名；Maven/npm 执行格式感知删除；引用归零后在成功响应前完成物理回收，任一步失败整批恢复资产引用（原子 operation envelope 保留复用）。
- 发布账号防护与审计身份（FR-109）：普通账号可配置 Web 登录开关、多个路径前缀、并发安全的制品数/字节额度和 Hosted Release 不可变策略；协议审计记录用户、认证来源、Token、IP 与凭据引用。
- Linux arm64 静态构建资产（FR-33）与 Windows 发布检查入口（scripts/check.ps1）。
- 管理员可直接填写外部 Nexus URL 创建在线迁移（FR-116），支持匿名、Basic（含 User Token）和 Bearer 认证；任务凭据使用 AES-256-GCM 加密保存以支持断点续传。
- 凭据引用审计补齐（FR-109）：迁移 discover/远程仓库索引读取实际使用凭据引用时写入 `migration.credential_ref` 审计（仅逻辑名称与认证类型）；`migration.start` 审计附带任务凭据引用；proxy 仓库创建/更新审计附带 `credentialRef` 配置与清除记录。全程不记录明文、摘要或环境变量全名。

### 变更

- **实时复制通道整体退役（FR-138）**：移除复制通道的页面、CLI 与诊断读模型（集群页面/集群监控/同步记录、逐跳凭据 CLI、主备同步诊断接口），`JIAN_SYNC_*` 遗留配置不再参与调度；集群数据表默认保留不 DROP，原子 operation envelope 与写栅栏按 FR-104 / FR-135 继续复用。节点搬迁统一改走一致性备份包（见新增段与 [`adr/0027`](docs/adr/0027-package-based-node-backup-and-relocation.md)）。
- 当前节点审计页改为「概览 / 记录」双 Tab 信息架构：指标卡与事件趋势收纳进概览 Tab，筛选与记录列表首屏直达，不再纵向长页堆叠；`attentionId` 跳转自动落在记录 Tab，数据口径、快照与确认行为不变。
- 旧批量删除接口保留兼容路径并转发到统一事务引擎，不再返回部分成功语义；管理操作仅全局管理员可执行。
- 孤立 blob 不再在服务启动时扫描删除；仅 primary 在 `JIAN_BLOB_GC_INTERVAL` 指定的周期执行清理（默认 24 小时，`0` 禁用），避免重启过程误影响待同步数据。
- 管理端趋势图改为 `@mantine/charts`（recharts）实现：Y 轴刻度与数值、X 轴标签自动抽稀、网格与平滑曲线由图表库接管；对外渲染契约（`role="img"`、aria-label、悬停汇总文本与常驻占位）保持不变，仪表盘 / 主机监控 / 审计概览趋势一次升级。
- 仪表盘与主机监控加载态从纯文字升级为骨架屏（指标卡 + 图表占位）；主机监控页全部文案接入 i18n（含不可用回退与 `/s` 单位），不再硬编码中文。

### 修复

- 开发态 Mock 版本与真实版本脱节：`/api/v1/status` 长期返回 `0.2.0-mock`、迁移版本停在 `0001_init`、备份包元数据为 `0.7.1` / schema 34，均与当前 0.8.0 开发版（迁移 `0036_audit_http_context`）不符。现由 `packages/devmock/src/version.ts` 统一提供版本锚点；「开源协议」页原先手工维护的 4 条样例（`@mantine/core` 版本也已过期）改为构建期从后端内嵌清单生成，与真实的 44 条 Go + 240 条 npm 依赖同源，不再随发版漂移。
- 开发态「迁移与搬迁」页在浏览器中恒为空态：夹具注入的判定直接读取请求头，而浏览器仅在 URL 携带 `?__mock=` 时才发送场景头，正常访问被误判为未知场景而跳过注入。现复用统一的场景解析（未声明即 `normal`），并同步为迁移向导测试显式声明前置条件——存在 running 任务时向导会提示并跳转到该任务（不允许并发迁移，属正确产品行为），此前测试依赖了种子的偶然状态。
- 开发态仓库列表的「制品数 / 总大小」恒为 0：种子未填写 `artifactCount` / `totalSize`，且 9 个仓库中只有 1 个有可浏览制品，导致列表、目录树与全局搜索都接近空白。现补全仓库规模（合计 28,416 制品 / 86.4 GB，与仪表盘 KPI 严格同源）与各仓库的可浏览制品样本，并修正 `seq.repo` 落后于实际仓库数、新建仓库可能拿到重复 id 的问题；用户与令牌种子补充停用 / 禁 Web 登录 / 多令牌等状态多样性。
- 业务仪表盘容量口径（FR-53）：仓库总数按资产行放大统计（LEFT JOIN 资产后未去重，测试站实机 3 仓库被计成 7）；现按 DISTINCT 仓库去重，资产数与字节数口径不变。
- npm 协议经「用户配置的 registry 基址」（`…/repository/<repo>/`）发布时，仓库被分派到 Raw 处理器：packument 被当普通文件覆盖写入、`_attachments` 内的 tarball 静默丢失（真实 npm 客户端全新安装 404 tarball）。现 Dispatcher 按 format 分派 npm 仓库到 npm 处理器（两条前缀语义一致），并对齐 scoped 包在子路径 registry 下「附件键带 scope 前缀、dist.tarball basename 为裸名」的存储名差异；新增真实 npm 客户端 publish→install 端到端测试与 `/repository/` 前缀回归测试。packument 中 `dist.tarball` 现按请求基址重写，不再指回硬编码主机。
- 各主列表页在后台刷新失败时保留已加载数据并展示非阻断警告（可重试），不再因一次网络抖动或场景模拟失败把整页打成死胡同；无任何可用数据时仍显式报错。
- 修复真实控制台中用户列表「允许 Web 登录」开关的无障碍名称与开关状态语义相反、移动端导航按钮缺少名称、仪表盘关注项无法定位风险批次或混入已确认批次、资产树与仓库详情页的多处可访问性问题。
- 原生协议仅接受实际 TLS、实际回环或显式 Host 白名单的 CDN HTTP 回源兼容链路中的凭据，拒绝伪造的 `X-Forwarded-Proto` 绕过；兼容链路须叠加回源 Token 或 HTTPS 回源。
- 回环来源只按服务端看到的 TCP 对端地址判定，外部连接伪造 `Host: localhost/127.0.0.1/::1` 不再能绕过 Host 白名单、回源 Token 或原生协议明文凭据保护。
- 修复 OCI 分块上传的最终块计费与限额；Cargo、PyPI、NuGet 在写入临时文件前预留发布额度，拒绝超限内容不再占满临时磁盘。
- Go modules proxy 对上游 410 下架响应保持逐请求的 410 语义，不再把永久下架误判为仓库级故障触发 auto-block，保证真实 Go 客户端可以按 GOPROXY 逗号链路继续回退。
- 修复运行期孤立 Blob GC 与发布交错时删除尚未建立引用内容的竞态。
- 修复 NuGet 不存在包精确搜索返回 500，并以压缩包、条目数、总解压与 NuSpec 预算替代对普通大二进制条目的错误 1 MiB 限制。
- 设置页在旧后端 / 旧 Mock 缺少 `allowedHosts` 或 `originToken*` 字段时渲染崩溃（对 undefined 调 `join`）；表单初始化与设置读取均做字段归一化兜底。
- 备份包被还原后自身登记被误标「failed」（FR-132 / FR-134）：热备份的内嵌快照取自生成过程中，包内登记行仍是生成中；导入并重启后启动清理把它标为失败（`服务重启，备份生成被中断`），导致**目标节点无法基于该包继续生成增量差包**（报「基线包不可用，状态为 failed（需 done）」），而归档本身完好（深度校验通过、签名下载一致、还原成功）。现启动收口按「归档是否完整可读」判定：manifest 身份正确即收口为 `done` 并回填大小与计数，归档缺失才判失败。
- 分片上传重传同一分片被误拒（FR-137）：累计字节校验把磁盘上已存在的同序号分片计入，网络抖动后重传会被 400 `累计分片 … 超过声明总字节` 拒绝，与「续传按磁盘推导、重复片幂等覆盖」的契约不符；现先扣除本次序号在磁盘上的既有字节再校验。
- 设置页保存任意字段时报「请求头名不能为空」（FR-130 回归）：回源 Token 未开启时，请求头名 / Token 值以空串提交仍被强制做格式校验；现按“请求应用后的生效状态”判定——开启时头名与值必填且格式合法，关闭时允许留空清除。
- 允许访问域名白名单粘贴完整 URL 被误判「含非法字符」（FR-129）：`https://repo.example.com/path` 这类输入直接按字符校验必然报错；现先归一化再校验（剥离协议 / `user@` / 路径 / 端口 / IPv6 方括号并统一小写），且端口不参与匹配——此前存了端口的条目因请求侧按去端口 Host 比较而永远命中不了。
- 修复全量并行跑 web 测试偶发超时：懒加载路由 + 图表重页面超过 `findBy*` 默认 1s 等待；统一放宽 testing-library 异步等待并给 jsdom 的 ResizeObserver 固定尺寸 stub，避免 recharts 容器 0×0 空跑。

## 0.7.1（2026-08-21）

### 新增

- group 聚合读性能（FR-110，见 `docs/specs/0.7.1-group-read-latency.md`）：group 读路径改为「本地快查命中优先 + 未命中并行回源」（首个成功即取消其余），成员超时缩短（10s→3s），proxy 回源失败进入短窗熔断（30s 内跳过不可达上游）；回源客户端 IPv4 优先（tcp4 Dialer），规避无 IPv6 出口宿主被 DNS AAAA 优先拖慢。修复 maven group 聚合大量 proxy 成员时缺失制品 20+ 秒/超时才返回的问题（现稳定快速 404，Gradle 可回退下一仓库）。
- proxy/group 404 负缓存（FR-111，见 `docs/specs/0.7.1-404-negative-cache.md`）：对明确 404 做短窗缓存（TTL 60s），同路径缺失制品后续请求秒级 404；上游故障（非 404）不写负缓存、有成员被 auto-block/offline 跳过时不缓存（不可判定）、复制请求跳过、成功/上传后失效。
- proxy 上游 auto-block 主动探测恢复（FR-112，见 `docs/specs/0.7.1-proxy-auto-block.md`）：上游失败进入 AUTO_BLOCKED（翻倍退避 40s→80s→…），阻止窗口内零连接快速失败；后台 HEAD 探测成功自动恢复可用。
- 仓库级 online/offline 开关（FR-113，见 `docs/specs/0.7.1-repository-online-offline.md`）：proxy/group 仓库可手动置 offline，group 读跳过 offline 成员、offline proxy 单独读不回源；online 持久化且不参与复制（守护测试保障）。
- 仓库列表上游连接状态展示（FR-114，见 `docs/specs/0.7.1-repository-connection-status-ui.md`）：proxy 仓库列表显示连接状态徽章（可用/自动阻止/离线/错误），详情页 online/offline 开关 + 手动重新探测按钮；offline 优先覆盖展示。

## 0.7.0（2026-08-20）

### 验收

- 发布门通过：Windows 原生 `make check` 全绿；GitHub Actions `Release` 的质量门、版本解析、多平台资产、容器镜像和 GitHub Release 预览链路 5/5 全绿。
- 三节点实机通过：`maven.example.com`、`repo1.example.com`、`t2.example.com` 全互联，双向上传/删除与断网恢复一致；100 MiB+1 blob 三端哈希一致；复制端点全程 GET-only；CDN/Tunnel 对外 URL 不泄露源站 IP。

### 新增

- 复制变更日志与冲突解决（FR-83，见 `docs/specs/0.7.0-replication-core.md` 与 `docs/adr/0013-multi-node-replication.md`）：
  - 迁移 `0010`：`repl_change` 变更日志表（seq / node_id / op / entity_type / entity_key / data / ts）。
  - 全部写路径（制品 / 仓库 / ACL / 用户 / 令牌 / 配置）业务写成功后各记一条变更日志，经 `ChangeRecorder` 接口注入（记录失败不阻断业务，靠对账兜底）。
  - `ReplicationService`：`Record`（落日志）、`Apply`（按 `(ts, node_id)` last-writer-wins 后写覆盖 + 删除 tombstone）、`NodeID`（env `JIAN_NODE_ID` 优先，否则生成持久化）、`LatestSeq` / `ListSince`（seq 断点续传水位）。
  - 令牌仅同步 sha256 摘要、口令同步 argon2id 哈希（明文不出现）；复制通道全走 GET 拉取、禁止 PUT 推送（规避 Cloudflare Tunnel / CDN 上传体积限制）。
- 节点间复制协议（FR-84，见 `docs/specs/0.7.0-replication-protocol.md`）：
  - `GET /api/v1/cluster/sync/pull`（按 seq 增量拉取，`since=0` 即全量初始化）+ `GET /api/v1/cluster/sync/blob/{hash}`（blob 流式，只传缺失）。
  - 专用同步令牌 `JIAN_SYNC_TOKEN`（Bearer 鉴权、常量时间比较）；未配置令牌时复制端点不注册（404）。
  - `ReplicationClient`（domain 层）：`Pull` / `FetchBlob` / `Sync` 一站式同步（拉变更 → Apply → 缺失 blob 补拉），返回推进后水位供调度器（FR-85）使用。
  - 全程 GET、零 PUT 推送；端点为非契约，经 `WithProtocolRoutes` 注册。
- 同步调度（FR-85，见 `docs/specs/0.7.0-replication-scheduler.md`）：
  - `ReplicationScheduler` 后台循环按 `JIAN_SYNC_INTERVAL`（默认 5s）从对端 Sync 一次——拉取模型下轮询既是近实时同步也是定期对账兜底。
  - 配置 `JIAN_SYNC_PEER_URL` 即启用调度；首次（本地无对端水位）自动全量初始化（`since=0`）。
  - 同步水位持久化于 `setting`（键 `repl:watermark:<peerURL>`），重启续拉不重拉全量。
  - 双向：两端各自配置对方基址 + 相同 `JIAN_SYNC_TOKEN`，互相同步最终一致。
- 集群配置与管理面（FR-86，见 `docs/specs/0.7.0-replication-console.md`）：
  - `ReplicationScheduler` 支持持久化启停开关（`repl:enabled`，缺省 true）+ 最近同步状态（`repl:last_sync_at` / `repl:last_error`）。
  - CLI `jianartifact replication status/start/stop`：查看对端配置 / 同步水位 / 最近同步与错误，启停调度。
  - 管理端点 `GET/PUT /api/v1/cluster`（仅 admin）+ web「集群」页（侧边栏管理段，仅管理员）：展示节点 ID / 对端 / 令牌配置态 / 水位 / 最近同步与错误 + 启停开关。
  - 对端 URL 与令牌初始经环境变量（`JIAN_SYNC_PEER_URL` / `JIAN_SYNC_TOKEN`）注入；FR-88 起可在运行时经 web「集群」页配置并入库（令牌不入日志、不回显）。
- 对外基础 URL 配置（FR-87，见 `docs/specs/0.7.0-public-url.md`）：
  - 新增 `JIAN_PUBLIC_URL` 环境变量（对外 CDN 域名）；所有对外 URL（npm `dist.tarball`、usage 复制片段）优先使用它。
  - 适配 CDN 回源：源站收到回源 Host 而非客户端域名时 URL 依然正确，且隐藏源站 IP。
  - 未配置时回退 `X-Forwarded-Proto` + 请求 Host 推断（兼容非 CDN 部署）。
- 对端配置 web 可视化（FR-88，见 `docs/specs/0.7.0-peer-web-config.md`）：
  - 对端基址/令牌入库（`setting` 键 `repl:peer_url` / `repl:peer_token`），web「集群」页可配置；**配置对端 ≠ 开始同步**——保存仅入库，轮询由「自动同步」开关（`repl:enabled`，默认开）控制。
  - 新增 `POST /api/v1/cluster/sync-now`「立即同步」：手动触发一次不受开关限制（web 按钮 / API 均可，最长等待 30s）。
  - `PUT /api/v1/cluster` 支持可选字段（`peerUrl` / `peerToken` / `enabled`，传哪个改哪个）；令牌不回显。
  - 调度器常驻（不再依赖 `JIAN_SYNC_PEER_URL` 是否设置），每轮从 setting 读对端；环境变量仅作首启初始默认写入（web 可覆盖）。
- 历史数据全量回填（复制对齐存量数据）：
  - 启动时 `ReplicationService.BackfillHistory` 一次性为存量实体（用户 / 令牌 / 仓库 / ACL / 制品）生成 put 变更日志，对端 `since=0` 全量拉取即可同步集群启用前写入的历史数据。
  - 回填顺序满足 Apply 依赖（用户 → 令牌 → 仓库 → ACL → 制品）；`repl:backfill_done` 幂等标记，失败不阻塞启动、下次重启重试（对端重复应用由 LWW 兜底）。
  - 排除内置 anonymous 用户与已吊销令牌；不回填 setting（避免覆盖对端显式配置）。
- 同步历史可视化（复制记录与进度）：
  - 迁移 `0011`：`repl_sync_log` 表——每次同步一条（时间 / 对端 / 进行中-成功-失败 / 起始-结束水位 / 变更·应用·失败条数 / 补拉 blob 数 / 变更实体构成 `entity_counts` JSON / 错误摘要），开始即写进行中记录，结束回写结果。
  - `ReplicationClient.Sync` 返回 `SyncStats` 统计（拉取 / 应用 / 失败 / blob / 按实体类型计数）；`ReplicationScheduler.doSync` 每轮落日志。
  - 新增 `GET /api/v1/cluster/sync-logs`（仅 admin，分页，按开始时间倒序）；web「集群」页「同步历史」卡片表格展示：时间 / 对端 / 状态徽章 / 变更构成（如「用户2 · 仓库1 · 制品5」）/ 变更·应用·失败 / Blob / 水位 / 错误详情，带分页。
- 设置 API 与基础配置动态生效（FR-89，见 `docs/specs/0.7.0-settings-api.md`）：
  - 基础配置四项入库 `setting`（`public_url` / `upstream_timeout` / `repl:sync_interval`，复用 `anonymous_access_enabled`）；env 值仅首启兜底写入，web 可运行时覆盖。
  - 新增 `GET/PUT /api/v1/settings`（仅 admin）：GET 返回生效值，PUT 字段可选、传哪个改哪个；校验 `publicUrl` 为 http/https 绝对 URL 或空、秒级取值 1–3600，非法 400。
  - **运行时生效（不重启）**：同步间隔由 `ReplicationScheduler` 每轮读 setting（变化则重置 ticker，缺省回退 env/默认 5s）；对外 URL 由 usage 与 npm `dist.tarball` 生成处动态读；回源超时经 `OnUpstreamTimeoutChange` 回调即时同步到 `upstream.Client.SetTimeout`（RWMutex 保护，Fetch 与 SetTimeout 并发安全）。
  - 多节点部署时 `public_url` 保持节点本地，不写入复制变更日志、不应用对端值，避免 CDN/Tunnel 域名相互覆盖。
- 设置页统一化（FR-90，见 `docs/specs/0.7.0-settings-page.md`）：
  - 侧边栏「管理」新增「设置」入口（仅管理员），一级 tab：基础设置（匿名开关 / 对外 URL / 回源超时 / 同步间隔，接 FR-89 设置端点）与集群（对端 URL / 令牌 / 自动同步开关，接既有集群端点）。
  - 配置迁移：匿名访问开关自用户页、对端配置自集群页迁入设置页；集群页保留同步状态、同步历史与「立即同步」按钮。
- 制品创建/更新时间与源 Nexus 对齐（FR-91，见 `docs/specs/0.7.0-asset-time-sync.md`）：
  - `admin backfill-times`：从源 Nexus assets API 分页拉取 `blobCreated`/`lastModified`（RFC3339Nano→UTC `YYYY-MM-DD HH:MM:SS`），按 仓库+路径 回填本地 `asset.created_at/updated_at`；多仓库并行拉取，幂等可重复。
  - 复制变更携带时间：`AssetChangeData` 增加 `CreatedAt`；`AssetService.Put` 变更数据携带资产实际时间；`ReplicationService.applyAsset` 经 `AssetRepo.UpsertWithTime` 回填 created/updated；`backfillAssets` 同样携带时间——复制两侧资产时间一致。
  - `admin emit-asset-times`：为全部 hosted 仓库资产重新登记带时间的 put 变更，对端复制应用后自动同步时间（无需在对端单独回填）。
  - `ReplicationClient.Sync` 水位推进修复：推进到本批最后一条 seq（而非对端最新），避免对端一次性积压大量变更时跳过未拉取部分。
- Maven SNAPSHOT 字面路径回退（FR-92）：GET `artifact-version-SNAPSHOT.ext` 时间戳解析失败时回退按字面路径解析，支持 Gradle 快照发布（metadata 无顶层 `<snapshot>` 标签）场景。
- 多对端全互连集群（FR-93，代码代号 FR-D）：对端配置支持多对端列表（`setting` 键 `repl:peers`，JSON 数组 `[{url,token}]`，优先于单值 `repl:peer_url`/`repl:peer_token`）；`PUT /api/v1/cluster` 传 `peers` 则全量替换；调度器对每个对端各同步一轮，节点间可全互连双向复制。
  - 复制客户端对 CDN/Tunnel 对端固定使用 HTTP/1.1，规避部分代理对 HTTP/2 复制流返回 `INTERNAL_ERROR`。
- 复制性能增强（FR-94，代码代号 FR-A/B）：`ReplicationClient.Sync` 资产元数据先落库（不阻塞），缺失 blob 用工作协程池（`blobFetchWorkers`）并发补拉，避免逐个串行拉 blob（单个可能 30s 超时）导致批量同步卡死；blob 拉取用更宽松超时。
- 同步进度/构成摘要（FR-95，代码代号 FR-C）：`GET /api/v1/cluster` 返回最近一次同步摘要（`LastSync`：起始/结束水位、变更·应用·失败、blob、按实体类型计数 `entityCounts`）；同步历史 `entity_counts` 落 `repl_sync_log`，集群页展示变更构成（如「用户2 · 仓库1 · 制品5」）。
- 集群配置键隔离（FR-96）：`repl:*` 集群配置键写路径不记录变更、应用路径拒绝应用（`SettingService`/复制应用侧过滤），避免对端配置互相覆盖。
- 复制调度失败日志收缩（FR-97）：`ReplicationScheduler` 对连续相同错误合并记录（起始→终止 + 次数），每 `errProgressEvery` 次打一条进度，错误变化/恢复时输出汇总，避免持续失败刷屏。
- 审计日志（FR-38）：新增 `audit_log` 表（迁移 0012）+ 全部管理写操作记录（制品上传/删除、仓库/ACL、用户、令牌、设置），含操作者/时间/对象/仓库/结果/IP；`GET /api/v1/audit-logs`（分页 + actor/action/repo/时间筛选）+ 管理端「审计日志」页（侧边栏入口）。
- 集群同步历史二级页（FR-98）：`GET /api/v1/cluster/sync-logs/:id/changes` 按 fromSeq→toSeq 从 `repl_change` 反推某次同步的具体变更；集群页同步历史行可点击进入详情页（变更列表 + 操作/实体徽章 + 分页）。
- 仓库详情左右宽度拖拽（FR-99）：浏览页左树/右详情加拖拽分割条，宽度自由调整（min 280/max 720）并本地持久化。
- 管理端布局与图标：设置页/集群页移除页面内 maw=900 二次限宽（内容区由全局 contentMaxWidth 控制）；favicon 与品牌 logo 统一为单一文件 `public/favicon.svg`（index.html 与 BrandLogo 均引用文件，去除内联 SVG/data URI）。
- 制品单个删除（FR-102）：管理端文件详情面板新增「删除」按钮（仅管理员，危险操作确认），删除后刷新文件树；复用协议 `DELETE /repository/{repo}/{path}`（元数据删 + blob 保留 + `asset.delete` 审计 + 复制 tombstone 联动）。
- 制品批量删除（FR-103，见 `docs/specs/0.7.0-batch-delete-assets.md`）：文件树新增复选框多选（限定当前目录内文件行，仅管理员）+ 工具栏「删除所选」批量删除；新增管理端点 `POST /api/v1/repositories/{name}/assets/batch-delete`（paths ≤500、响应 `deleted/failed`、部分失败不整体回滚），逐条复用 `AssetService.Delete` 并写 `asset.delete` 审计与复制 tombstone。

### 修复

- 制品删除权限与范围：协议单删在服务端强制全局管理员，拥有仓库 write ACL 的普通用户不能绕过管理端限制；批删在切换目录或搜索上下文时清空勾选，避免跨目录提交删除。
- 开发态审计日志：devmock 补齐 `GET /api/v1/audit-logs` 的管理员鉴权、筛选与分页响应，审计页不再因未处理请求落到 Vite HTML 而加载失败。
- 后端质量门：Go toolchain 升至 1.26.6，规避已报告的标准库可达漏洞；修正 npm 协议分流的静态检查问题。
- Windows 质量门：新增 `scripts/check.ps1`，使用原生 PowerShell 执行前端、Go、漏洞与构建检查，避免依赖 WSL 质量工具安装。
- 静态资源缓存头（修复"前端发版后浏览器仍显示旧版"）：
  - `index.html`（含 SPA 回退）响应加 `Cache-Control: no-cache`，每次请求回源验证，发版后刷新立即拿到引用最新 content-hash 资源的入口页。
  - `/assets/*`（构建产物带 content-hash，内容变则文件名变）加 `Cache-Control: public, max-age=31536000, immutable` 长缓存；其他无 hash 静态文件（如 favicon）保持 `no-cache`，避免误缓存导致更新不生效。
- 同步历史空数据崩溃（`GET /api/v1/cluster/sync-logs` 无记录时 `items` 返回 `null` 而非 `[]`，web 集群页同步历史渲染 `list.items.length` 崩溃）：
  - 后端空数据时返回空数组 `[]`（复现测试 `TestClusterSyncLogsEmpty`）；前端 `list.items` 加空值防御（`?? []`）。
- group 仓库无法编辑成员：仓库详情配置 tab 增加 group 类型分支（members MultiSelect，选同格式仓库），修复 group 成员无法编辑（组件测试 `Test` group 配置可编辑成员仓库）。
- 双斜杠路径下载 404：`cleanArtifactPath` 改用 `TrimLeft` 去全部前导斜杠并折叠连续斜杠（`//→/`），兼容客户端拼 `baseUrl + "/" + path` 产生的双斜杠 URL（复现测试 `TestDoubleSlashPathCompat`）。
- Maven SNAPSHOT 字面路径回退：`MavenHandler.Get` 对 `-SNAPSHOT` 文件时间戳解析失败后回退按字面路径解析，支持 Gradle 快照发布（metadata 无顶层 `<snapshot>` 标签）场景。
- 复制同步失败项水位语义：Apply / blob 下载 / 哈希不匹配失败不再越过失败变更推进水位，后续轮询可重试补齐（回归测试覆盖 Apply 失败、blob 404、传输中断、哈希错误、批次续拉）。
- 多对端 LWW 版本状态：新增已应用版本表（迁移 0013），本地与远端成功写入均更新，避免旧远端变更覆盖新写入（回归测试覆盖新→旧远端覆盖场景）。
- 集群配置保存令牌保留：更新 peers / URL / enabled 时，token 缺失或留空保留已有值，不再被覆盖清空（`sync-now` 不受自动同步开关限制并有 handler 测试）。
- 设置动态生效补齐：启动时应用持久化回源超时；public URL 清空正确回退请求 Host；`PUT /settings` 先整体校验再写入，避免部分写。
- 失败日志收缩按对端隔离：连续相同错误按 peer 分组合并，避免多对端互相串扰。
- 同步历史详情改为模态框：PRD FR-98 与 spec 同步为「详情模态框」交互，支持分类、正向分页与失败摘要；详情端点补正向区间、404、403 测试。
- 审计日志补齐：仓库更新与集群配置变更写入审计，审计页新增起止时间筛选（组件测试覆盖）。
- 小屏页眉响应式登录/退出：按移动/桌面只渲染一组控件，消除重复登录入口（回归测试覆盖 480px 与桌面象限）。
- 审计日志页分页条被裁切（FR-101）：管理/复制两 tab 的滚动区容器改为纵向 flex，分页条固定在表格下方、不再被固定高度容器 `overflow:hidden` 裁切（布局回归测试覆盖两 tab）。
- 批量删除不存在仓库的 404 语义（FR-103 收尾）：`POST /api/v1/repositories/{name}/assets/batch-delete` 循环前预检仓库存在性，不存在返回 404（与契约及 devmock 行为一致），避免 200+全 failed 假象；补 404 与 blob 保留断言测试。

## [0.6.0] - 2026-07-29

### 新增

- npm 标准 registry 端点补齐（FR-82，见 `docs/specs/npm-registry-endpoints.md`）：`npm login`（legacy adduser 流，验证账号口令后签发 jat\_ API Token，后台可见可吊销）、`whoami`、`ping`、`dist-tag ls/add/rm`（latest 拒删）、`unpublish` 单版本与整包（write 权限即可，hosted 限定）、`deprecate`（复用 publish 合并路径）、`search`（`/-/v1/search`，group 按成员合并）、audit 兜底（空报告）与 abbreviated packument（`Accept: application/vnd.npm.install-v1+json` 白名单裁剪）。
- 内置 anonymous ACL 主体 + 实例级「允许匿名访问」全局开关（FR-66，见 `docs/specs/0.6.0-anonymous-access.md`）：
  - 迁移 `0007`：`setting` 表 + 内置 `anonymous` 用户（不可登录/删除/改密/停用）；ACL 页可为其授 `read`（含 private 仓库）。
  - 开关默认开；关闭后一切匿名请求 401（public 仓库也不例外）；用户管理页顶部提供开关卡片（仅管理员）。
  - `GET /api/v1/repositories` 改可选鉴权：匿名返回"匿名可读"仓库集合；新增 `GET/PUT /api/v1/settings/anonymous-access`（仅管理员，非契约端点）。
- 登录模态框化（FR-67）：删除 `/login` 整页，Header「登录」与受保护页均弹模态框（取消回仓库列表）；`/` 与任意未知路径统一落 `/repositories`，不再强制跳转登录页。
- 异步加载体验重构（FR-69）：`AsyncBoundary` 与仓库文件树在刷新/翻页/上传后保留旧数据并叠加局部 LoadingOverlay，仅首载显示整块骨架，消灭整页重刷。
- 开源协议页（FR-72，见 `docs/specs/0.6.0-license-page.md`）：`/licenses` 展示 Go/npm 依赖协议清单（搜索过滤 + 协议徽章）；清单由 `scripts/generate-licenses.mjs` 构建时生成并内嵌到后端二进制，经 admin 专属端点 `GET /api/v1/licenses` 返回（不打进前端 bundle）；入口位于侧边栏「管理」段（仅管理员可见），匿名/普通用户不展示。
- Maven 网页上传（FR-73，见 `docs/specs/0.6.0-maven-web-upload.md`）：Maven hosted 仓库管理端 GAV 表单上传，服务端自动生成 pom.xml、各文件 `.md5`/`.sha1` 并读改写 `maven-metadata.xml`（含 checksum）；仅限 release 版本，SNAPSHOT 提示走 `mvn deploy`；新增非契约端点 `POST /api/v1/repositories/{name}/maven-upload`。
- 首屏加载性能（FR-70）：全部页面组件路由级 `React.lazy` 代码分割，主 chunk 683.98 kB → 474.59 kB（gzip 206.40 → 152.98 kB，约 -26%）；懒加载在布局内挂 Suspense 占位，路由切换侧栏/页眉不闪、不白屏。
- 页眉打磨（FR-71）：刷新按钮点击后旋转动画并禁用，随全局网络活动计数归零（含最短旋转时长防闪烁）恢复；登出按钮登出期间呈 loading；`useAsync` 统一响应页眉刷新事件，刷新对所有数据页面生效。
- 仓库描述可配置（FR-81）：`repository` 表新增 `description` 列（迁移 `0009`），建仓表单与详情页配置 Tab 可编辑描述；详情页页头副标题改为展示描述（原固定说明文案移除），匿名详情页经 `usage` 端点带出描述；浏览面板去掉与页头重复的 format/type/visibility 徽章层；文件详情改结构化元数据（大小含精确字节、类型、创建/更新时间），树 API 补 `sha1`/`md5`/`createdAt` 透传。
- 仓库详情页打磨（FR-74）：整页固定高度布局（对齐 FR-68，树/详情面板内滚不再整页滚动）；未登录登录入口收敛到页眉一处；客户端发布提示由大块 Alert 收纳为紧凑小字 + 跳使用说明链接。
- 仓库列表页优化（FR-68）：页头/筛选/分页固定、表格区内滚 + sticky 表头（body 不滚）；操作列删「公开页」留「浏览」；匿名视图隐藏新建/删除/清理等管理操作。
- 协议端点 Basic Auth 支持用户名 + 口令认证（此前仅 API Token 作 password），兼容 Maven/Gradle 账号密码推送。
- group 仓库 assets 列表聚合成员制品（管理端浏览 group 仓库可见聚合内容）。
- Gradle usage 片段 + 清理空制品目录端点。
- Maven SNAPSHOT 时间戳版本解析 + group `maven-metadata.xml` 并行合并。
- 前端 UI 现代化优化（Mantine 视觉与交互打磨）。
- 文件树懒加载（tree API，FR-54）、全局跨仓库搜索页（FR-30）、仓库列表排序/分组（FR-56）、浏览页内制品搜索（FR-57）、Header 搜索栏与刷新（FR-59）。
- 全局搜索增强（FR-30 增强）：高级搜索表达式（多词 AND、`-` 负筛选、双引号短语、`repo:`/`format:`/`ext:` 限定及对应排除形式，解析后下推 SQL）；结果按仓库聚合 facets（点击即过滤）；结果表格表头排序（名称/仓库/路径/大小/更新时间，存储层排序 + 分页总数）。
- 浏览页与仓库列表交互打磨：浏览页搜索结果树形化展示（FR-57 增强，命中路径按目录聚合）+ 上传表单折叠收纳；仓库列表匿名视图标题文案独立、仓库名称点击直达详情、复制按钮 Tooltip 去重（消除嵌套提示重叠）。

### 变更

- 公开页 `/p/:name` 与整页登录 `/login` 移除：公开浏览合并入主布局（匿名直接进入 `/repositories` 与仓库浏览页）。

### 修复

- 协议端点（`/repository/*`）匿名 401 响应补充 `WWW-Authenticate: Basic realm="JianArtifact"` 质询头：修复 Maven/Gradle 等非抢占式认证客户端对私有仓库拉取 / 发布时无法带凭据重试的问题（`/api/*` 不受影响，不会触发浏览器原生登录框）。
- proxy 仓库不再把上游 HTML 目录索引页缓存为制品：目录形路径（空串或以 `/` 结尾）直接按不存在处理，并经迁移 0008 清理历史误缓存（修复文件树中空白名、无图标的假文件）。
- group 仓库 `maven-metadata.xml` 并行合并恢复按 members 配置顺序合并：此前按成员响应到达顺序合并，`latest`/`release` 随调度顺序漂移（有序合并为 0.3.0 起的既定契约）。

### 移除

- `apps/web` 删除 `LoginPage` 与 `PublicRepoPage`（能力分别由登录模态框与主布局匿名视图承接）。

## [0.5.0] - 2026-07-25

### 新增

- M1 整期验收通过，首个稳定 MVP 发布。
- Nexus 迁移实机验收（FR-20~23）：离线目录直读 55,325 资产从 5 hosted 仓零丢失迁入；17 proxy + 1 group 重建并公开匿名读可用。
- AC-01 原生客户端验收：Drop-in URL `/repository/{name}/{path}` 经 HTTPS 全部 HTTP 200，group 聚合读 + proxy pass-through 正常。
- AC-02 零丢失对账：迁移报告 55,325/0/0，逐仓计数精确匹配，抽样 sha256 字节一致。
- AC-08 部署演练：二进制 symlink 升级/回滚 ~1s 探活通过；Docker/Compose 可用。
- NFR 基线冻结：大文件流式 cold 2.7ms；并发 10x max 17.5ms；P95 0.97ms；RSS 19.3MB。55k 资产稳态。

## [0.4.0] - 2026-07-24

### 新增

- 仓库浏览增强（FR-16a）：管理端文件树左右分栏、点文件看详情/usage/下载；Raw hosted 单文件上传；未登录公开页 `/p/:name`（仅 public 仓）。
- Nexus 迁移域地基（FR-21/22/23 部分，见 `docs/specs/0.4.0-migration-foundation.md` 与 `docs/adr/0012-nexus-migration-state-machine.md`）：
  - `0003_migration.sql` 新增 `migration_task` 表；`MigrationTaskRepo` + `domain.MigrationService` 状态机骨架（创建 **planned**、显式 **start**、cancel/resume 守卫、启动时 **running→failed**）。
  - OpenAPI 管理面：`/api/v1/migrations` 列表/创建、`/{id}`、`/start`、`/resume`、`/cancel`、`/report`、`/discover`；仅 admin；凭据仅 `credentialRef`。
  - devmock 内存任务状态机与契约测试对齐；OPERATIONS 补充迁移凭据引用约定。
- Nexus 三来源发现（FR-20 / FR-21 计划预览，见 `docs/specs/0.4.0-migration-discover.md`）：
  - `internal/migration/discover`：`OnlineREST`（Nexus REST 仓库列表 + 有限页资产估算）、`OfflineDir`（夹具布局）、`OfflineBundle`（manifest + content）。
  - `POST /migrations/discover` 同步发现，成功落库 **planned** 返回 `taskId`+`plan`；失败不落库；不自动 start。
- Nexus 迁移异步执行（FR-21/22，见 `docs/specs/0.4.0-migration-execute.md`）：
  - `internal/migration/runner`：planned→start 后台搬运、冲突 skip/overwrite/fail、checkpoint 断点、协作 cancel、启动 running→failed。
  - offline_bundle/offline_dir 流式 Put；wiring 注入 Runner。
- Nexus 迁移报告与切换增量（FR-23，见 `docs/specs/0.4.0-migration-report-cutover.md`）：
  - `GET .../report` 组装 totals/failures/cutover checklist；`POST .../finalize` 仅 completed，差量复制并写 `report.delta`。
- 管理端迁移向导（见 `docs/specs/0.4.0-migration-web-wizard.md`）：
  - 侧栏「迁移」入口（admin）；列表 / 向导（discover→planned→显式 start）/ 详情（轮询、取消、续传、finalize、报告与 cutover 清单）。
- Nexus 迁移真机可用性增强：
  - `online_rest` 执行：按 plan 枚举 Nexus assets 并流式下载 `downloadUrl` 写入 hosted。
  - `sourceConfig.includeRepositories` + `POST .../start` body `includeRepositories`：发现前/启动前均可多选仓库（在线+离线）。
  - 向导预览页 Checkbox 多选 + TagsInput 预过滤；离线枚举严格按 plan 白名单。
  - 仓库删除级联清理 asset 元数据（复测前可删仓重建）；补充删除单测。
  - `deploy/remote-ssh.sh`：SSH 密钥生成、远程二进制部署与探活（密钥目录 `deploy/ssh/` 不入库）。
- Nexus drop-in URL 兼容与公开读入口（FR-24a，见 `docs/specs/0.4.0-nexus-dropin.md`）：
  - 路径与 Nexus 3 同形（Raw/Maven `/repository/{repo}/…`），迁移后客户端只改 host；npm 走 `/npm/{repo}/`（差异已文档标注）。
  - `visibility=public` 仓库协议层匿名 GET/HEAD 可读；浏览器公开预览 `/p/{name}`。
  - group 跨成员有序聚合读，Maven `maven-metadata.xml` 合并 versions。
  - 管理端仓库列表新增「访问 URL」（一键复制）、「成员/上游」列、type 着色 Badge。
  - OPERATIONS §1.1.6 Nexus drop-in 替换步骤与同名 group 冲突处理方案。
- 仓库页面与权限完善（FR-25a~e，见 `docs/specs/0.4.0-repo-pages-acl-enhance.md`）：
  - FR-25a：ACL 页面用户下拉框（可搜索，显示用户名，替代手填 ID）
  - FR-25b：asset 表扩展 sha1/md5 列（0005 迁移），上传时登记；详情面板展示多校验和 + Maven 8 种依赖坐标 + HTML View 外链
  - FR-25c：后端 HTML 目录索引页（`/repository/{repo}/{dir}/` 尾斜杠渲染目录列表，public 匿名可读）
  - FR-25d：仓库列表新增「制品数」「总大小」列（后端 GROUP BY 聚合，避免 N+1）
  - FR-25e：仓库详情页 Tab 布局（浏览/配置/ACL，管理员可见配置与 ACL）

### 变更

- 部署：`deploy/remote-ssh.sh` 构建时从仓库根 `VERSION` 注入 `-X main.version`，管理端/status 展示与文件一致。
- 运维：补充历史资产 `sha1/md5` 回填命令说明（`admin backfill-checksums`），见 `docs/OPERATIONS.md` §1.1.4c。

### 修复

- 管理端复制：HTTP 非安全上下文下降级 `execCommand`，避免 `navigator.clipboard` 不可用导致复制失败（`CopyTextButton` / `copyToClipboard`）。
- 历史制品：提供 `jianartifact admin backfill-checksums`，从 blob 流式补算并写回空的 sha1/md5。

### 移除

- 暂无。

## [0.3.0] - 2026-07-23

### 新增

- Raw hosted 协议纵切贯通（FR-13 部分 / FR-18，见 `docs/specs/0.3.0-raw-hosted.md` 与 `docs/adr/0009-protocol-auth-and-blob-layout.md`）：
  - `internal/blobstore` 文件系统内容寻址存储：sha256 两级分片布局 `<root>/ab/cd/<hash>`，临时文件 + 边写边算哈希 + 原子 rename 落盘，相同内容天然去重（`config` 补建 `blob/tmp` 目录）。
  - `0002_asset.sql` 迁移新增 `asset` 表（`UNIQUE(repository_id, path)` + 外键级联）与 `AssetRepo`（`Upsert`/`GetByPath`/`DeleteByPath`）；`domain.AssetService` 编排「校验 raw-hosted → 流式入 blob → upsert 元数据」的发布 / 拉取 / 删除。
  - `internal/protocol` Raw handler + router：`GET|HEAD|PUT|DELETE /repository/{repo}/{path...}`，GET 回写 `Content-Type`/`Content-Length`/`ETag`(=blob sha256)；错误映射 401/403/404/409；经 `httpserver.WithProtocolRoutes` 注册于契约路由之后、SPA 回退之前。
  - 鉴权：auth 中间件补 `Authorization: Basic` 解析（token 作 password，为空则 username），仅接受 `jat_` API Token（不启用口令登录），Bearer 行为不变；协议端点复用 `CanAccess`（public read 匿名放行）。
- 仓库配置基座：proxy/group 契约扩展与校验（FR-13 proxy/group 前置，见 `docs/specs/0.3.0-repository-config.md`）：
  - proxy/group 配置复用 `repository` 表既有 `config` JSON 列（`{"remoteUrl":...,"members":[...]}`），不新增迁移；`repository` 新增 `RepositoryConfig` 结构与 `DecodeConfig`/`EncodeRepositoryConfig` 编解码，`RepoRepo.Create` 增 config 入参、`GetByName`/`List` 读回 config、新增 `UpdateConfig`。
  - `domain.RepositoryService.Create`/`Update` 改为结构化配置签名并新增 `validateConfig`：hosted 两字段须空、proxy 必填合法 http/https `remoteUrl`、group 必填 `members`（成员均存在、同 format、禁自引用），违规返回新增语义 `ErrValidation`（经 `writeDomainErr` 映射 400）。
  - `api/openapi.yaml` 的 `Repository`/`CreateRepositoryRequest`/`UpdateRepositoryRequest` 增可选 `remoteUrl`/`members`，`oapi-codegen` 与 `openapi-typescript` 重生成；devmock store/handlers 透传字段并补 npm-proxy 种子 `remoteUrl`；web 仓库创建表单据 `type` 条件渲染 remoteUrl（proxy）与 members 多选（group）+ i18n 文案。
- Raw proxy/group 回源与聚合读（FR-13 proxy/group 全 / FR-17 / FR-18 复验，见 `docs/specs/0.3.0-raw-proxy-group.md` 与 `docs/adr/0010-proxy-cache-singleflight-group-routing.md`）：
  - 新增 `internal/upstream` 回源客户端：`Client.Fetch(ctx, baseURL, path)` 流式返回上游内容 + 响应头，上游 404→`ErrNotFound`、非 2xx→`StatusError`、支持 `IsTimeout` 超时判定；超时可配（`JIAN_UPSTREAM_TIMEOUT` 秒，默认 30s），`config`/wiring 装配注入 `AssetService`。
  - `domain.AssetService` 新增 `Resolve(ctx, repoName, path)` 按 `repo.Type` 分派读路径：hosted 读本地；proxy 命中即返回、未命中经 upstream 回源**流式**落 blob（不整体入内存）并 upsert 缓存后返回；group 按 `members` 有序递归解析、首个命中即返回、全未命中→404（深度上限遏制环引用）。以 `golang.org/x/sync/singleflight`（键 `repoID\x00path`）收敛并发首次回源，同一未命中路径只回源一次。
  - 新增领域错误 `ErrUpstream`（→502）、`ErrUpstreamTimeout`（→504）；`protocol/raw.go` 的 `GET/HEAD` 改用 `Resolve`（支持 hosted/proxy/group 读），`writeAssetErr` 补 `upstream_error`/`upstream_timeout` 映射；`PUT`/`DELETE` 仍仅 hosted（proxy/group 写→409）。proxy 暂不做缓存失效/TTL、GC 仍不即时。
- Maven 全类型 hosted/proxy/group（FR-14，见 `docs/specs/0.3.0-maven.md`）：
  - `/repository/:repo/*artifactPath` 端点改由单一路由 + `protocol.Dispatcher` 按仓库 `format` 分派（`maven`→`MavenHandler`，其余→`RawHandler`）；`RegisterRoutes` 签名泛化为 `artifactHandler` 接口。制品字节的发布/拉取/删除与鉴权复用既有内容寻址存储与 `AssetService.Resolve`（hosted 本地 / proxy 回源缓存 / group 有序聚合），`MavenHandler` 经 Go 嵌入 `RawHandler` 仅覆盖 GET。
  - Maven 语义补两点：校验和文件 `.md5/.sha1/.sha256` 缺失时据去后缀的底层制品字节现算摘要以 `text/plain` 返回（已部署则原样优先命中）；group 的 `maven-metadata.xml` 按 `members` 有序合并各成员 `versions`（去重）并重算 `latest`/`release`/`lastUpdated`，全成员皆无→404。
  - `domain.AssetService.Put` 校验由 raw-hosted 放宽为 **hosted-only**（格式路由上移至协议层）；无新增表/迁移。SNAPSHOT 唯一时间戳版本按路径原样存取，group metadata 版本顺序采成员出现顺序而非语义化比较（边界见 spec）。
- npm registry 全类型 hosted/proxy/group（FR-15，见 `docs/specs/0.3.0-npm.md` 与 `docs/adr/0011-npm-registry-layout-and-tarball-rewrite.md`）：
  - 新增 `internal/protocol/npm.go` + `RegisterNpmRoutes`：registry 基址 `<server>/npm/:repo/`，单 catch-all `/:repo/*rest` 在 handler 内解析 packument（`GET <pkg>`）/tarball（`GET <pkg>/-/<file>`）/publish（`PUT <pkg>`），支持 scoped `@scope/name`；与 `/api/v1`、`/repository`、SPA 回退互不冲突。`NpmHandler` 经 Go 嵌入 `RawHandler` 复用鉴权与制品存取。
  - 复用内容寻址 blob + `asset` 表（无新增表）：packument 整份文档存于路径 `<pkg>`、tarball 存于 `<pkg>/-/<file>`。publish 解码 `_attachments` base64 落各 tarball、与已存 packument 合并（versions/dist-tags/time 逐键 last-writer-wins）后覆盖写；install 经 `AssetService.Resolve` 拉取。
  - 服务端统一把 packument 各版本 `dist.tarball` 重写为 `<请求基址>/npm/<本仓>/<pkg>/-/<原文件名>`（依 `X-Forwarded-Proto`/`Host`）：proxy 回源上游 packument/tarball 并缓存、group 合并成员 packument（versions 并集、dist-tags 首成员优先）并经有序命中回落 tarball，客户端始终经本仓拉取。publish 仍仅 hosted（proxy/group 写→409）。
- 制品浏览与使用说明（FR-16，见 `docs/specs/0.3.0-browse-usage.md`）：
  - 新增只读管理面端点 `GET /api/v1/repositories/{name}/assets`（分页 + 可选 `prefix` 前缀过滤，返回 `AssetList{items:[AssetSummary{path,size,hash,contentType?,updatedAt}],total}`，按 path 升序）与 `GET /api/v1/repositories/{name}/usage`（据 format/type 返回 `UsageInfo{format,type,snippets:[UsageSnippet{title,description?,code}]}`）；均经 `requireRepoRead` 授权（admin/`CanAccess(read)`，public 匿名可读），未认证 401、越权 403、仓库不存在 404。
  - `AssetRepo` 增 `ListByRepo`/`CountByRepo`（SQLite `LIKE ... ESCAPE '\'` 前缀过滤，转义 `%`/`_`/`\`）；`RepositoryService` 增 `ListAssets`/`Usage`，`buildUsage` 据 format/type 组装 maven/npm/raw 客户端接入片段（writable=hosted 才含写入片段），对外基址由 `X-Forwarded-Proto`/`Host` 推断（maven/raw→`<base>/repository/<name>`、npm→`<base>/npm/<name>/`）。
  - `api/openapi.yaml` 增两端点与 `AssetSummary`/`AssetList`/`UsageInfo`/`UsageSnippet` schema 及 `prefix` 参数，`oapi-codegen` 与 `openapi-typescript` 重生成；devmock store 补 `assets` 种子与 `listAssets`/`usage`（`buildUsage` 与后端一致）、msw 增两 handler、契约一致性测试覆盖新 schema。
  - `apps/web` 新增仓库详情页 `RepositoryDetailPage`（路由 `/repositories/:name`）：制品浏览表（路径/大小/哈希/更新时间 + 前缀过滤）与使用说明卡片（`CopyButton` 可复制），仓库列表页增「浏览」入口 + i18n `repoDetail` 文案。
- 原生客户端真机验收脚手架（FR-19，见 `scripts/e2e-smoke.mjs`）：跨平台 Node 冒烟脚本（`node scripts/e2e-smoke.mjs`，仅依赖 Node 内置 fetch）扩 0.3.0 全链路 roundtrip——Raw hosted 发布/拉取、制品浏览 + 使用片段（含 prefix 过滤与权限）、Maven hosted deploy/resolve（含缺失校验和现算）、npm publish/install（含 packument tarball 重写与字节一致）；原生 `mvn`/`npm` 客户端按可用性自动探测提示，proxy/group 外网回源经 `--include-proxy` 可选开启。发版前已完成 curl/mvn/npm 原生客户端真机互通验收。

## [0.2.0] - 2026-07-22

### 新增

- 0.2.0 认证授权与管理端骨架（后端核心，见 `docs/specs/0.2.0-auth-core.md`、`0.2.0-user-management.md`、`0.2.0-repository-acl.md` 与 `docs/adr/0008-auth-session-model.md`）：
  - 持久化底座：`internal/config` 配置加载 + `internal/persistence` sqlx 连接、内置迁移器与 `0001_init.sql`（user/api_token/revoked_token/repository/acl 建表）；`/readyz` 注入 SQLite ping 与 blob 目录可写自检。
  - 认证授权（FR-06/07/08）：argon2id 口令哈希、无状态 JWT HS256 会话（登出经 `revoked_token` 短期黑名单）、API Token（仅存 sha256 摘要，明文仅签发时返回一次）；`Bearer` 统一鉴权中间件（先 JWT 后 Token 摘要）。
  - 管理员自举改为网页端点 `POST /api/v1/auth/bootstrap`（仅 `user` 表为空时开放，去除环境变量令牌，已初始化恒 409）。
  - 用户管理（FR-09）：用户 CRUD 与口令修改 / 重置（管理员改任意、普通用户仅改自己）。
  - 仓库与 ACL（FR-10）：仓库 CRUD、可见性、`/repositories/{name}/acl` 读写与 ACL 授权判定接入鉴权（越权 403、未认证 401）。
  - 新增 `/api/v1/status` 运行时状态端点；CLI 扩为子命令分发器，新增 `admin reset`（离线重置 / 创建管理员）与 `status`（在线探测 / 离线静态输出）。
  - `api/openapi.yaml` 扩展至 `/api/v1` 管理面全端点与 schema（含嵌套 `Error` 信封），`oapi-codegen` 重生成接口；devmock 覆盖新端点，`openapi-typescript` 重生 `schema.gen.ts`，契约一致性测试保持绿。
- 0.2.0 管理端 Web（FR-11，对 devmock 完成，见 `docs/specs/0.2.0-web-admin.md` 与 `docs/adr/0003-frontend-stack.md`）：
  - `apps/web` 全页面：登录/自举（据 `/api/v1/status` 自动切换）、仪表盘、用户、访问令牌、仓库、仓库 ACL；React 18 + Mantine 7 + i18next（中文）+ react-router v6。
  - typed API client（`Bearer` 注入 + `ApiError` 归一化）+ 鉴权上下文（登录/自举/登出、路由守卫、会话与用户快照持久化）+ `useAsync`/`AsyncBoundary` 统一加载/错误/越权态。
  - `packages/ui` 迁入 Mantine 7：`AppProvider`（MantineProvider + 主题）、`PageHeader`、加载/空/错误/越权四类状态态组件与设计令牌拆分。
  - `packages/devmock` 新增 MSW 双端拦截：内存态 CRUD `store` + `msw.ts` handlers（`*/` 前缀通配 origin）+ 浏览器 `worker`（仅 dev 启动）与 Node `server`（集成测试），并导出 `./schema` 子路径供 web 类型级复用；MSW 钉 `2.7.6`。
  - 测试：devmock 契约/MSW 12 + `packages/ui` 组件 6 + `apps/web` 集成 9（testing-library + MSW Node server）全绿；`vite build` 产 dist 供单二进制内嵌，生产构建不含 MSW/devmock。
- 0.2.0 组件 / 业务模式验收站（FR-12，见 `docs/specs/0.2.0-wiki.md`）：
  - `apps/wiki` 重建为 React 18 + Mantine 7 独立静态验收站：`AppShell` 画廊 + 展台注册表，四类展台——设计令牌、页头 `PageHeader`、四类状态态（加载/空/错误/越权）、关键交互（`useForm` 表单校验 + `notifications` 全局通知 + `@mantine/modals` 危险操作确认弹窗，与管理端删除流程同源）。
  - 复用 `packages/ui` 的 `AppProvider` 与组件、并与管理端一致挂载 `ModalsProvider`，核验共享主题/组件/确认弹窗在验收站与管理端同源无重复实现；脱离后端运行，不内嵌进后端二进制。
  - 测试：`apps/wiki` 组件/交互 7（Gallery 3 + sections 4）全绿；`vite build` 产独立 dist。
- 0.2.0 管理端 Web 视觉系统对齐旧项目控制台外壳（FR-11 细化，对齐 `docs/adr/0003-frontend-stack.md`）：
  - `apps/web` 外壳重写为 `AppShell layout="alt"` 可折叠分段侧栏（品牌 logo + 版本号置顶、浏览/管理分段、收起态仅图标 + Tooltip、footer 折叠按钮），导航仅接线 0.2.0 已有页面。
  - 新增密度令牌 `theme/density.ts` 与 `global.css`（`scrollbar-gutter: stable`）；登录卡与仪表盘 KPI 卡样式对齐旧项目。
  - 配色对齐旧项目：`packages/ui` 主题主色改为 Mantine 原生蓝、品牌蓝 `#228be6` 仅用于 logo、`AppProvider` 色彩模式改为 `auto`（跟随系统深浅色）。
  - 新增依赖 `@tabler/icons-react`（外壳图标）。

### 变更

- `apps/wiki`（组件 / 业务模式验收站）随 0.2.0 引入 Mantine 7 组件后重新纳入 pnpm 工作区。
- 依赖新增：`modernc.org/sqlite`、`github.com/jmoiron/sqlx`、`github.com/golang-jwt/jwt/v5`、`golang.org/x/term`；`golang.org/x/crypto`（argon2）转 direct。
- 统一错误响应为嵌套信封 `{"error":{"code","message"}}`；`/readyz` 503 体与 OpenAPI `Error` schema 同步调整。

## [0.1.0] - 2026-07-20

### 新增

- 初始化 SDD 项目脚手架：PRD / ARCHITECTURE / API / ROADMAP / OPERATIONS / SECURITY / 决策记录（ADR）。
- 防漂移治理规则（`.claude/rules/`）、演进与维护指南、版本与许可（MIT）、工程化配置。
- monorepo 工程骨架（pnpm workspace + Turborepo + Makefile + Go Task + go.work）与部署编排骨架（`deploy/`）。
- 0.1.0 工程基座落地（见 `docs/specs/0.1.0-foundation.md`）：
  - 后端单二进制：`api/openapi.yaml` 设计优先 + oapi-codegen 生成接口/类型；Gin `/healthz`、`/readyz` 返回 `HealthStatus`；`//go:embed` 内嵌前端产物 + SPA 回退；`CGO_ENABLED=0` 静态编译；`healthcheck` 子命令供容器探活。
  - 前端可构建工作区：React + Vite + TS strict 的 `apps/web`；`packages/ui` 设计令牌；`packages/devmock` 以 openapi-typescript 生成类型 + ajv 运行时校验实现 devmock↔OpenAPI 契约一致性（mock 漂移即测试失败）。
  - 统一质量门 `scripts/check.sh`：前端 format/lint/typecheck/test/build + 后端 gofmt/vet/golangci-lint/`go test -race`/govulncheck/build + 契约一致性。
  - 部署骨架：多阶段 `Dockerfile`（基础镜像经 `ARG` 参数化，默认 distroless nonroot）可离线构建；容器 `/readyz` 探活通过。

### 变更

- Go 工具链升级至 `go1.26.5`；`apps/wiki` 暂移出 pnpm 工作区（延后至 0.2.0）。

### 修复

- 修复依赖与标准库漏洞（升级 `quic-go`、`golang.org/x/net` 及 Go 工具链），`govulncheck` 零告警。
- 修复 `.gitignore`：全局 `dist/` 规则会连带排除后端 embed 占位目录，导致 `//go:embed all:dist` 在纯净检出时无法编译；改为先重新纳入目录再放行占位 `index.html`。

### 移除

- 暂无。

> 发版时把"未发布版本"段切成 `## [X.Y.Z] - YYYY-MM-DD`，再新建空的"未发布版本"段。
