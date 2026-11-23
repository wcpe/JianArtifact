package main

// query 子命令：面向 AI agent 的**远程查询客户端**。
//
// 与其它子命令的差别：它不加载本地配置以外的任何东西、不打开 SQLite、不启动服务，
// 只把请求发到远端实例并打印结果。它的核心价值不是"多一个工具"，而是
// **让错误请求快速失败**：所有响应都要校验 Content-Type，收到前端 HTML 立即非零退出，
// 绝不把页面当数据输出——实测把 tree 端点的 prefix 写成 path 时，服务端会静默忽略参数
// 并返回根目录，agent 无从察觉。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/itchyny/gojq"
)

const (
	// envQueryServer 是服务端基址的环境变量名。
	envQueryServer = "JIAN_SERVER"
	// envQueryToken 是访问令牌的环境变量名。
	envQueryToken = "JIAN_TOKEN"
	// queryDefaultTimeout 是单次请求的默认整体超时。
	queryDefaultTimeout = 30 * time.Second
	// queryMaxReplyBytes 限制单个响应体的读取上限，避免异常响应撑爆内存。
	queryMaxReplyBytes = 16 << 20
	// querySearchPageSizeMax 与服务端 page_size 上限一致，超出时直接报错而非静默夹取。
	querySearchPageSizeMax = 100
)

// queryHTMLHint 是「收到前端页面」时的固定提示语。
// 这条文案要能让 agent 立刻定位到"路径写错了"，因此逐字保留既定表述。
const queryHTMLHint = "服务端返回了前端页面而非数据，通常是路径拼写错误；制品路径形如 /repository/{repo}/{path}"

// queryUsageError 表示用法错误（参数缺失、取值非法、开关不认识），进程退出码为 2。
type queryUsageError struct{ msg string }

// Error 实现 error 接口。
func (e *queryUsageError) Error() string { return e.msg }

// usageErr 构造一个用法错误。
func usageErr(msg string) error { return &queryUsageError{msg: msg} }

// usageErrf 构造一个带格式化的用法错误。
func usageErrf(format string, args ...any) error {
	return &queryUsageError{msg: fmt.Sprintf(format, args...)}
}

// queryExitCode 把 query 子命令的错误映射成进程退出码：用法错误 2，其余 1。
func queryExitCode(err error) int {
	var usage *queryUsageError
	if errors.As(err, &usage) {
		return 2
	}
	return 1
}

// queryEnv 是 query 子命令读取外部环境的通道，测试可注入替身。
type queryEnv struct {
	// getenv 读取环境变量（测试注入假实现，避免依赖真实环境）。
	getenv func(string) string
	// configPath 覆盖配置文件路径；空表示按默认位置查找。
	configPath string
}

// queryCmd 是 `jianartifact query` 的入口。
func queryCmd(args []string) error {
	return runQuery(args, os.Stdout, queryEnv{getenv: os.Getenv})
}

// runQuery 执行 query 子命令；out 便于测试捕获输出。
func runQuery(args []string, out io.Writer, env queryEnv) error {
	if len(args) == 0 {
		return usageErr("用法：jianartifact query <repos|ls|find|get|usage|api> [参数]（详见 jianartifact query help）")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "-h", "--help":
		queryUsage(out)
		return nil
	case "repos":
		return queryRepos(rest, out, env)
	case "ls":
		return queryLs(rest, out, env)
	case "find":
		return queryFind(rest, out, env)
	case "get":
		return queryGet(rest, out, env)
	case "usage":
		return queryUsageInfo(rest, out, env)
	case "api":
		return queryAPI(rest, out, env)
	default:
		return usageErrf("未知 query 子命令：%s（支持 repos / ls / find / get / usage / api / help）", sub)
	}
}

// queryUsage 打印 query 子命令的完整用法。
func queryUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `jianartifact query — 远程查询远端实例（结果直接打印，不落本地数据目录）

用法：
  jianartifact query repos [--json [字段,…]] [--jq EXPR]
  jianartifact query ls <repo>[/<prefix>] [--json [字段,…]] [--jq EXPR]
  jianartifact query find <关键词> [--repo <仓库>] [--limit <N>] [--json [字段,…]] [--jq EXPR]
  jianartifact query get <repo>/<path> [--json [字段,…]]
  jianartifact query usage <repo> [--json]
  jianartifact query api <path> [--json [字段,…]] [--jq EXPR] [-X 方法] [-d 请求体]

连接开关（所有子命令通用）：
  --server <URL>     服务端基址，形如 https://jian.example.com（也可用 JIAN_SERVER）
  --token <令牌>     访问令牌（也可用 JIAN_TOKEN；公开仓库可省略）
  --timeout <时长>   单次请求整体超时，默认 30s（纯数字按秒解释）
                     也可写在 ~/.config/jianartifact/config.json 的 server/token/timeout 字段

输出开关：
  --json             输出完整 JSON 对象
  --json 字段,字段   只输出这些字段（对数组逐条筛选，缺失字段写 null）
  --jq EXPR          用 jq 表达式过滤（先做 --json 字段筛选，再执行 jq）

子命令说明：
  repos              列出匿名可见仓库（GET /api/v1/public/repositories）
  ls                 列出仓库某前缀下的目录与文件（tree?prefix=，目录显示名裁成末段）
  find               全文搜索（q/repository/page_size 参数名由 CLI 生成）
  get                查单件元数据，并拼出下载地址 /repository/{repo}/{path}
  usage              打印仓库的依赖解析与发布片段
  api                万能兜底：直连任意端点（path 必须以 / 开头）

示例：
  jianartifact query repos
  jianartifact query repos --json name,format,artifactCount
  jianartifact query ls maven-public/org/springframework/spring-core
  jianartifact query find "spring-core" --repo maven-public --limit 20 --jq '.items[].path'
  jianartifact query get maven-public/org/springframework/spring-core/6.1.0/spring-core-6.1.0.jar
  jianartifact query usage maven-public
  jianartifact query api /api/v1/status --jq '.version'
  jianartifact query api /api/v1/repositories/maven-public/tree --jq .directories
`)
}

// queryCommonValueFlags 是所有子命令都接受的取值开关。
var queryCommonValueFlags = []string{"server", "token", "timeout", "jq"}

// queryArgs 是解析后的子命令参数：位置参数 + 开关取值。
type queryArgs struct {
	positional []string
	values     map[string]string
	switches   map[string]bool
}

// str 返回开关取值（未出现时为空串）。
func (a *queryArgs) str(name string) string { return a.values[name] }

// hasSwitch 判断不带取值的开关是否出现。
func (a *queryArgs) hasSwitch(name string) bool { return a.switches[name] }

// parseQueryArgs 解析子命令参数。
//
// 位置参数按出现顺序收集；开关支持 --name value、--name=value、-X value 三种写法。
// extraFlags 是子命令专属的取值开关（如 find 的 --repo/--limit）。
// json 的取值是可选的（字段列表），因此分两步定案：第一遍一律按布尔开关处理，
// 第二遍再看紧跟其后的那个令牌能否安全地改判为字段列表——判据是「摘掉它之后
// 位置参数仍然够数」。这样 `query api --json /api/v1/status` 里的路径不会被误吃掉
// （摘掉它就没有位置参数了），而 `query repos --json name,format` 能正常取到字段。
func parseQueryArgs(args []string, requiredPos int, usageLine string, extraFlags ...string) (*queryArgs, error) {
	known := map[string]bool{"json": true}
	for _, name := range queryCommonValueFlags {
		known[name] = true
	}
	for _, name := range extraFlags {
		known[name] = true
	}
	parsed := &queryArgs{values: map[string]string{}, switches: map[string]bool{}}
	// positionalIndex 记录每个位置参数在原参数表中的下标，供 --json 改判时定位。
	var positionalIndex []int
	jsonIndex := -1
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
			parsed.positional = append(parsed.positional, arg)
			positionalIndex = append(positionalIndex, i)
			continue
		}
		name, inline, hasInline := splitQueryFlag(arg)
		if !known[name] {
			return nil, usageErrf("未知参数 %s\n用法：%s", arg, usageLine)
		}
		if name == "json" {
			parsed.switches["json"] = true
			if hasInline {
				parsed.values["json"] = inline
				continue
			}
			if jsonIndex < 0 {
				jsonIndex = i
			}
			continue
		}
		if hasInline {
			parsed.values[name] = inline
			continue
		}
		if i+1 >= len(args) {
			return nil, usageErrf("参数 %s 需要一个取值\n用法：%s", arg, usageLine)
		}
		i++
		parsed.values[name] = args[i]
	}
	if jsonIndex >= 0 && parsed.values["json"] == "" {
		for pos, index := range positionalIndex {
			if index != jsonIndex+1 {
				continue
			}
			// 摘掉这个位置参数后仍满足必需个数，才认定它是 --json 的字段列表。
			if len(parsed.positional)-1 >= requiredPos {
				parsed.values["json"] = parsed.positional[pos]
				parsed.positional = append(parsed.positional[:pos], parsed.positional[pos+1:]...)
			}
			break
		}
	}
	if len(parsed.positional) < requiredPos {
		return nil, usageErrf("参数不足\n用法：%s", usageLine)
	}
	if len(parsed.positional) > requiredPos {
		return nil, usageErrf("多余的参数 %s\n用法：%s", strings.Join(parsed.positional[requiredPos:], " "), usageLine)
	}
	return parsed, nil
}

// splitQueryFlag 把 --name=value / -X 这类开关拆成名字与内联取值。
func splitQueryFlag(arg string) (name, inline string, hasInline bool) {
	trimmed := strings.TrimLeft(arg, "-")
	if index := strings.Index(trimmed, "="); index >= 0 {
		return trimmed[:index], trimmed[index+1:], true
	}
	return trimmed, "", false
}

// queryOutputOptions 是输出侧的开关：--json（可选字段列表）与 --jq。
type queryOutputOptions struct {
	json   bool
	fields []string
	jq     string
}

// parseQueryOutputOptions 把解析结果转换为输出开关。
func parseQueryOutputOptions(parsed *queryArgs) (queryOutputOptions, error) {
	opts := queryOutputOptions{jq: strings.TrimSpace(parsed.str("jq"))}
	if !parsed.hasSwitch("json") {
		return opts, nil
	}
	opts.json = true
	raw := strings.TrimSpace(parsed.str("json"))
	if raw == "" {
		return opts, nil
	}
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			return opts, usageErrf("--json 字段列表含空字段：%q", raw)
		}
		opts.fields = append(opts.fields, field)
	}
	return opts, nil
}

// queryConfigFile 是 ~/.config/jianartifact/config.json 的可选内容。
type queryConfigFile struct {
	Server  string `json:"server"`
	Token   string `json:"token"`
	Timeout string `json:"timeout"`
}

// loadQueryConfig 读取可选配置文件；文件不存在视为空配置，文件损坏则报错（不能静默忽略）。
func loadQueryConfig(env queryEnv) (queryConfigFile, error) {
	path := env.configPath
	if path == "" {
		path = defaultQueryConfigPath()
	}
	if path == "" {
		return queryConfigFile{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return queryConfigFile{}, nil
		}
		return queryConfigFile{}, fmt.Errorf("读取配置文件 %s：%w", path, err)
	}
	var cfg queryConfigFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return queryConfigFile{}, fmt.Errorf("配置文件 %s 不是合法 JSON：%w", path, err)
	}
	return cfg, nil
}

// defaultQueryConfigPath 返回默认配置文件路径（用户配置目录下的 jianartifact/config.json）。
func defaultQueryConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "jianartifact", "config.json")
}

// querySettings 是从命令行、环境变量与配置文件合并出的连接设置。
type querySettings struct {
	base    *url.URL
	token   string
	timeout time.Duration
}

// resolveQuerySettings 按「命令行 > 环境变量 > 配置文件 > 默认值」合并连接设置。
func resolveQuerySettings(parsed *queryArgs, env queryEnv) (querySettings, error) {
	serverRaw := parsed.str("server")
	if serverRaw == "" && env.getenv != nil {
		serverRaw = env.getenv(envQueryServer)
	}
	token := parsed.str("token")
	if token == "" && env.getenv != nil {
		token = env.getenv(envQueryToken)
	}
	timeoutRaw := parsed.str("timeout")
	// 仅在还有未定项时才读文件，避免每次查询都碰磁盘。
	if serverRaw == "" || token == "" || timeoutRaw == "" {
		cfg, err := loadQueryConfig(env)
		if err != nil {
			return querySettings{}, err
		}
		if serverRaw == "" {
			serverRaw = cfg.Server
		}
		if token == "" {
			token = cfg.Token
		}
		if timeoutRaw == "" {
			timeoutRaw = cfg.Timeout
		}
	}
	base, err := normalizeQueryServer(serverRaw)
	if err != nil {
		return querySettings{}, err
	}
	timeout, err := parseQueryTimeout(timeoutRaw)
	if err != nil {
		return querySettings{}, err
	}
	return querySettings{base: base, token: strings.TrimSpace(token), timeout: timeout}, nil
}

// normalizeQueryServer 校验并规范化服务端基址。
func normalizeQueryServer(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, usageErr("缺少服务端地址：请用 --server 或环境变量 " + envQueryServer + " 指定（形如 https://jian.example.com）")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, usageErrf("--server 不是合法 URL：%v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, usageErrf("--server 必须以 http:// 或 https:// 开头，实际 %q", raw)
	}
	if parsed.Host == "" {
		return nil, usageErrf("--server 缺少主机名：%q", raw)
	}
	trimmedPath := strings.TrimRight(parsed.Path, "/")
	// 基址里带 /api/v1 或 /repository 是最常见的拼接错误：再拼接口路径必然 404 或拿到前端页面。
	for _, wrong := range []string{"/api/v1", "/repository"} {
		if strings.HasSuffix(trimmedPath, wrong) {
			return nil, usageErrf("--server 基址不应包含 %s：它只是实例根地址（形如 https://host:port），实际 %q", wrong, raw)
		}
	}
	parsed.Path = trimmedPath
	parsed.Fragment = ""
	return parsed, nil
}

// parseQueryTimeout 解析 --timeout：纯数字按秒解释，其余按 Go 时长写法（如 500ms、2m）。
func parseQueryTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return queryDefaultTimeout, nil
	}
	if _, err := strconv.Atoi(raw); err == nil {
		raw += "s"
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return 0, usageErrf("--timeout 格式不正确（示例：30s、2m、500ms）：%q", raw)
	}
	if duration <= 0 {
		return 0, usageErrf("--timeout 必须为正数，实际 %q", raw)
	}
	return duration, nil
}

// queryClient 是 query 子命令使用的远程 HTTP 客户端；连接复用由共享的 http.Client 保证。
type queryClient struct {
	base  *url.URL
	token string
	http  *http.Client
}

// newQueryClient 依据连接设置构造客户端。
func newQueryClient(settings querySettings) *queryClient {
	return &queryClient{
		base:  settings.base,
		token: settings.token,
		// 复用默认 Transport：保留连接池与长连接复用，不另建连接器。
		http: &http.Client{Timeout: settings.timeout},
	}
}

// endpoint 组装完整请求地址：基址 + 路径 + 由 CLI 生成的查询串（全部经 net/url 转义）。
func (c *queryClient) endpoint(endpointPath string, params url.Values) string {
	target := *c.base
	target.Path = strings.TrimRight(c.base.Path, "/") + endpointPath
	target.RawQuery = params.Encode()
	return target.String()
}

// downloadURL 拼出制品的下载地址 /repository/{repo}/{path}。
func (c *queryClient) downloadURL(repo, path string) string {
	target := *c.base
	segments := []string{strings.TrimRight(c.base.Path, "/"), "repository", url.PathEscape(repo)}
	for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
		if segment == "" {
			continue
		}
		segments = append(segments, url.PathEscape(segment))
	}
	target.Path = strings.Join(segments, "/")
	target.RawQuery = ""
	return target.String()
}

// queryReply 是一次请求校验后的结果。
type queryReply struct {
	status      int
	contentType string
	body        []byte
	target      string
}

// do 发起请求并完成状态码与 Content-Type 校验。
func (c *queryClient) do(ctx context.Context, method, endpointPath string, params url.Values, body []byte) (*queryReply, error) {
	target := c.endpoint(endpointPath, params)
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	// 目标地址来自用户显式指定的 --server，属于本命令的既定用途。
	req, err := http.NewRequestWithContext(ctx, method, target, reader) //nolint:gosec // G107：地址由用户显式指定，命令本身就是远程客户端
	if err != nil {
		return nil, fmt.Errorf("构造请求 %s %s：%w", method, target, err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败：%w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, queryMaxReplyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取响应体（%s）：%w", target, err)
	}
	reply := &queryReply{
		status:      resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		body:        raw,
		target:      target,
	}
	if err := checkQueryReply(reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// checkQueryReply 校验响应：先看 Content-Type（收到前端页面立即失败），再看状态码。
func checkQueryReply(reply *queryReply) error {
	if strings.Contains(strings.ToLower(reply.contentType), "text/html") {
		return fmt.Errorf("请求 %s 返回了前端页面（Content-Type: %s）\n%s\n提示：用 --server 检查基址是否多了或少了路径——它应只是实例根地址（形如 https://host:port），不要带 /api/v1 或 /repository；再核对接口路径与查询参数名",
			reply.target, reply.contentType, queryHTMLHint)
	}
	if reply.status < http.StatusOK || reply.status >= http.StatusMultipleChoices {
		return translateQueryError(reply)
	}
	return nil
}

// serverErrorPayload 是服务端标准错误响应：{"error":{"code","message"}}。
type serverErrorPayload struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// translateQueryError 把非 2xx 响应翻译成可操作提示，并附上服务端原始错误 JSON。
func translateQueryError(reply *queryReply) error {
	var payload serverErrorPayload
	_ = json.Unmarshal(reply.body, &payload)
	head := fmt.Sprintf("请求 %s 返回 %d", reply.target, reply.status)
	if payload.Error.Code != "" {
		head += "（code=" + payload.Error.Code + "）"
	}
	if payload.Error.Message != "" {
		head += "：" + payload.Error.Message
	}
	body := strings.TrimSpace(string(reply.body))
	if body != "" {
		head += "\n服务端响应：" + truncateQueryText(body, 600)
	}
	switch reply.status {
	case http.StatusNotFound:
		return errors.New(head + "\n提示：核对仓库名与路径是否拼写正确；`jianartifact query repos` 可列出可见仓库，`jianartifact query ls <repo>` 可确认存在的目录；若该地址属于前端页面路由，说明接口路径写错了")
	case http.StatusUnauthorized:
		return errors.New(head + "\n提示：该端点需要认证，请补充 --token <令牌> 或设置环境变量 " + envQueryToken)
	case http.StatusForbidden:
		return errors.New(head + "\n提示：当前身份无权访问，请确认令牌具备该仓库读权限，或用 --token / " + envQueryToken + " 换一个令牌")
	default:
		return errors.New(head)
	}
}

// truncateQueryText 截断过长的响应文本，避免把整页 HTML 灌进终端。
func truncateQueryText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…（已截断）"
}

// buildQueryClient 完成「解析输出开关 → 合并连接设置 → 构造客户端」。
func buildQueryClient(parsed *queryArgs, env queryEnv) (*queryClient, queryOutputOptions, error) {
	opts, err := parseQueryOutputOptions(parsed)
	if err != nil {
		return nil, opts, err
	}
	settings, err := resolveQuerySettings(parsed, env)
	if err != nil {
		return nil, opts, err
	}
	return newQueryClient(settings), opts, nil
}

// queryRepositoryList 对应 GET /api/v1/public/repositories 的响应。
type queryRepositoryList struct {
	Items []queryRepository `json:"items"`
	Total int               `json:"total"`
}

// queryRepository 是仓库列表里的单个仓库（只取展示所需的稳定字段）。
type queryRepository struct {
	Name          string   `json:"name"`
	Format        string   `json:"format"`
	Type          string   `json:"type"`
	Visibility    string   `json:"visibility"`
	Aliases       []string `json:"aliases"`
	ArtifactCount *int     `json:"artifactCount"`
	TotalSize     *int64   `json:"totalSize"`
	Online        *bool    `json:"online"`
}

// queryTreeResponse 对应 GET /api/v1/repositories/{name}/tree?prefix= 的响应。
// directories 返回的是**自仓库根起算的完整路径**，不是末段名字。
type queryTreeResponse struct {
	Directories []string        `json:"directories"`
	Files       []queryTreeFile `json:"files"`
}

// queryTreeFile 是 tree 端点返回的单个文件条目。
type queryTreeFile struct {
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	Hash          string `json:"hash"`
	Sha1          string `json:"sha1"`
	Md5           string `json:"md5"`
	ContentType   string `json:"contentType"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
	DownloadCount int64  `json:"downloadCount"`
}

// querySearchResponse 对应 GET /api/v1/search 的响应。
type querySearchResponse struct {
	Items []querySearchItem `json:"items"`
	Total int               `json:"total"`
}

// querySearchItem 是搜索结果里的单条制品。
type querySearchItem struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Hash       string `json:"hash"`
	Sha1       string `json:"sha1"`
	UpdatedAt  string `json:"updatedAt"`
}

// queryUsageResponse 对应 GET /api/v1/repositories/{name}/usage 的响应。
type queryUsageResponse struct {
	Format   string              `json:"format"`
	Type     string              `json:"type"`
	Snippets []queryUsageSnippet `json:"snippets"`
}

// queryUsageSnippet 是一条使用片段（解析依赖 / 发布制品等）。
type queryUsageSnippet struct {
	Group       string `json:"group"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Tool        string `json:"tool"`
	Code        string `json:"code"`
}

// queryRepos 列出匿名可见仓库。
func queryRepos(args []string, out io.Writer, env queryEnv) error {
	usageLine := "jianartifact query repos [--json [字段,…]] [--jq EXPR] [--server URL] [--token T] [--timeout 30s]"
	parsed, err := parseQueryArgs(args, 0, usageLine)
	if err != nil {
		return err
	}
	client, opts, err := buildQueryClient(parsed, env)
	if err != nil {
		return err
	}
	reply, err := client.do(context.Background(), http.MethodGet, "/api/v1/public/repositories", nil, nil)
	if err != nil {
		return err
	}
	var typed queryRepositoryList
	if err := json.Unmarshal(reply.body, &typed); err != nil {
		return fmt.Errorf("解析仓库列表响应：%w", err)
	}
	return emitQueryResult(out, reply.body, opts, func(w io.Writer) error {
		return printQueryRepos(w, typed)
	})
}

// printQueryRepos 以对齐表格打印仓库列表。
func printQueryRepos(w io.Writer, list queryRepositoryList) error {
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "名称\t格式\t类型\t可见性\t制品数\t总大小\t在线\t别名")
	for _, repo := range list.Items {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			repo.Name, repo.Format, repo.Type, repo.Visibility,
			formatQueryCount(repo.ArtifactCount), formatQuerySize(repo.TotalSize),
			formatQueryOnline(repo.Online), strings.Join(repo.Aliases, ","))
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "共 %d 个仓库\n", list.Total)
	return err
}

// queryLs 列出仓库某前缀下的目录与文件。入参形如 <repo>[/<prefix>]。
func queryLs(args []string, out io.Writer, env queryEnv) error {
	usageLine := "jianartifact query ls <repo>[/<prefix>] [--json [字段,…]] [--jq EXPR] [--server URL] [--token T]"
	parsed, err := parseQueryArgs(args, 1, usageLine)
	if err != nil {
		return err
	}
	repo, prefix := splitQueryRepoSpec(parsed.positional[0])
	if repo == "" {
		return usageErrf("缺少仓库名\n用法：%s", usageLine)
	}
	client, opts, err := buildQueryClient(parsed, env)
	if err != nil {
		return err
	}
	// 参数名必须是 prefix（不是 path）：写成 path 时服务端会静默忽略并返回根目录。
	// 且必须以 "/" 结尾才表示「列出该目录内容」，故统一走 queryTreePrefix 归一化。
	params := url.Values{}
	params.Set("prefix", queryTreePrefix(prefix))
	reply, err := client.do(context.Background(), http.MethodGet,
		"/api/v1/repositories/"+url.PathEscape(repo)+"/tree", params, nil)
	if err != nil {
		return err
	}
	var typed queryTreeResponse
	if err := json.Unmarshal(reply.body, &typed); err != nil {
		return fmt.Errorf("解析目录树响应：%w", err)
	}
	return emitQueryResult(out, reply.body, opts, func(w io.Writer) error {
		return printQueryTree(w, repo, prefix, typed)
	})
}

// printQueryTree 以对齐表格打印目录树：显示名裁成末段，完整路径单列保留。
func printQueryTree(w io.Writer, repo, prefix string, tree queryTreeResponse) error {
	where := repo
	if prefix != "" {
		where += "/" + prefix
	}
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "类型\t名称\t大小\t更新时间\t完整路径")
	for _, dir := range tree.Directories {
		_, _ = fmt.Fprintf(table, "目录\t%s\t-\t-\t%s\n", queryDisplayName(dir), dir)
	}
	for _, file := range tree.Files {
		_, _ = fmt.Fprintf(table, "文件\t%s\t%s\t%s\t%s\n",
			queryDisplayName(file.Path), formatQueryBytes(file.Size), file.UpdatedAt, file.Path)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s：%d 个目录、%d 个文件\n", where, len(tree.Directories), len(tree.Files))
	return err
}

// queryFind 走全文搜索端点。
func queryFind(args []string, out io.Writer, env queryEnv) error {
	usageLine := "jianartifact query find <关键词> [--repo <仓库>] [--limit <N>] [--json [字段,…]] [--jq EXPR]"
	parsed, err := parseQueryArgs(args, 1, usageLine, "repo", "limit")
	if err != nil {
		return err
	}
	keyword := strings.TrimSpace(parsed.positional[0])
	if keyword == "" {
		return usageErrf("缺少搜索关键词\n用法：%s", usageLine)
	}
	params := url.Values{}
	params.Set("q", keyword)
	if repo := strings.TrimSpace(parsed.str("repo")); repo != "" {
		params.Set("repository", repo)
	}
	if raw := strings.TrimSpace(parsed.str("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			return usageErrf("--limit 必须是正整数，实际 %q", raw)
		}
		if limit > querySearchPageSizeMax {
			return usageErrf("--limit 不能超过 %d（服务端 page_size 上限）", querySearchPageSizeMax)
		}
		params.Set("page_size", strconv.Itoa(limit))
	}
	client, opts, err := buildQueryClient(parsed, env)
	if err != nil {
		return err
	}
	reply, err := client.do(context.Background(), http.MethodGet, "/api/v1/search", params, nil)
	if err != nil {
		return err
	}
	var typed querySearchResponse
	if err := json.Unmarshal(reply.body, &typed); err != nil {
		return fmt.Errorf("解析搜索结果：%w", err)
	}
	return emitQueryResult(out, reply.body, opts, func(w io.Writer) error {
		return printQuerySearch(w, typed)
	})
}

// printQuerySearch 以对齐表格打印搜索结果。
func printQuerySearch(w io.Writer, result querySearchResponse) error {
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "仓库\t大小\t更新时间\t路径")
	for _, item := range result.Items {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\n",
			item.Repository, formatQueryBytes(item.Size), item.UpdatedAt, item.Path)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "匹配 %d 条，本次返回 %d 条\n", result.Total, len(result.Items))
	return err
}

// queryGet 查单件元数据：向父目录要 tree，再在 files 里精确匹配路径。
func queryGet(args []string, out io.Writer, env queryEnv) error {
	usageLine := "jianartifact query get <repo>/<path> [--json [字段,…]] [--server URL] [--token T]"
	parsed, err := parseQueryArgs(args, 1, usageLine)
	if err != nil {
		return err
	}
	repo, path := splitQueryRepoSpec(parsed.positional[0])
	if repo == "" || path == "" {
		return usageErrf("需要 <repo>/<path> 两段\n用法：%s", usageLine)
	}
	client, opts, err := buildQueryClient(parsed, env)
	if err != nil {
		return err
	}
	parent := queryParentPrefix(path)
	params := url.Values{}
	params.Set("prefix", queryTreePrefix(parent))
	reply, err := client.do(context.Background(), http.MethodGet,
		"/api/v1/repositories/"+url.PathEscape(repo)+"/tree", params, nil)
	if err != nil {
		return err
	}
	var tree queryTreeResponse
	if err := json.Unmarshal(reply.body, &tree); err != nil {
		return fmt.Errorf("解析目录树响应：%w", err)
	}
	var found *queryTreeFile
	for i := range tree.Files {
		if strings.Trim(tree.Files[i].Path, "/") == path {
			found = &tree.Files[i]
			break
		}
	}
	if found == nil {
		hint := "jianartifact query ls " + repo
		if parent != "" {
			hint += "/" + parent
		}
		return fmt.Errorf("仓库 %s 的目录 %q 下没有找到 %q\n提示：先执行 `%s` 确认该目录下的实际文件名（大小写与后缀必须完全一致）",
			repo, parent, path, hint)
	}
	download := client.downloadURL(repo, path)
	// --json / --jq 输出单条制品（而不是整份目录树），并补上拼好的下载地址。
	payload := reply.body
	if opts.json || opts.jq != "" {
		payload, err = querySingleFilePayload(reply.body, path, download)
		if err != nil {
			return err
		}
	}
	return emitQueryResult(out, payload, opts, func(w io.Writer) error {
		return printQueryFile(w, repo, *found, download)
	})
}

// querySingleFilePayload 从 tree 响应里挑出匹配的那条文件，并附加 downloadUrl。
// 走通用解码而非结构体回填，是为了保留服务端实际返回的字段集合（omitempty 的键不该凭空出现）。
func querySingleFilePayload(raw []byte, path, download string) ([]byte, error) {
	value, err := decodeQueryJSON(raw)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return raw, nil
	}
	files, ok := object["files"].([]any)
	if !ok {
		return raw, nil
	}
	for _, item := range files {
		file, isObject := item.(map[string]any)
		if !isObject {
			continue
		}
		itemPath, _ := file["path"].(string)
		if strings.Trim(itemPath, "/") != path {
			continue
		}
		file["downloadUrl"] = download
		return json.Marshal(file)
	}
	return raw, nil
}

// printQueryFile 打印单件制品的元数据与下载地址。
func printQueryFile(w io.Writer, repo string, file queryTreeFile, download string) error {
	_, err := fmt.Fprintf(w, `仓库：%s
路径：%s
大小：%s（%d 字节）
内容哈希：%s
SHA-1：%s
MD5：%s
内容类型：%s
创建时间：%s
更新时间：%s
下载次数：%d
下载地址：%s
`, repo, file.Path, formatQueryBytes(file.Size), file.Size,
		queryDash(file.Hash), queryDash(file.Sha1), queryDash(file.Md5), queryDash(file.ContentType),
		queryDash(file.CreatedAt), queryDash(file.UpdatedAt), file.DownloadCount, download)
	return err
}

// queryUsageInfo 打印仓库的使用片段。
func queryUsageInfo(args []string, out io.Writer, env queryEnv) error {
	usageLine := "jianartifact query usage <repo> [--json [字段,…]] [--server URL] [--token T]"
	parsed, err := parseQueryArgs(args, 1, usageLine)
	if err != nil {
		return err
	}
	repo := strings.Trim(strings.TrimSpace(parsed.positional[0]), "/")
	if repo == "" {
		return usageErrf("缺少仓库名\n用法：%s", usageLine)
	}
	client, opts, err := buildQueryClient(parsed, env)
	if err != nil {
		return err
	}
	reply, err := client.do(context.Background(), http.MethodGet,
		"/api/v1/repositories/"+url.PathEscape(repo)+"/usage", nil, nil)
	if err != nil {
		return err
	}
	var typed queryUsageResponse
	if err := json.Unmarshal(reply.body, &typed); err != nil {
		return fmt.Errorf("解析 usage 响应：%w", err)
	}
	return emitQueryResult(out, reply.body, opts, func(w io.Writer) error {
		return printQueryUsage(w, repo, typed)
	})
}

// printQueryUsage 打印仓库信息与按用途分组的使用片段。
func printQueryUsage(w io.Writer, repo string, usage queryUsageResponse) error {
	if _, err := fmt.Fprintf(w, "仓库：%s\n格式：%s\n类型：%s\n", repo, usage.Format, usage.Type); err != nil {
		return err
	}
	for _, snippet := range usage.Snippets {
		title := snippet.Title
		if snippet.Tool != "" {
			title += "（" + snippet.Tool + "）"
		}
		if _, err := fmt.Fprintf(w, "[%s] %s\n", snippet.Group, title); err != nil {
			return err
		}
		if snippet.Description != "" {
			if _, err := fmt.Fprintf(w, "  %s\n", snippet.Description); err != nil {
				return err
			}
		}
		for _, line := range strings.Split(snippet.Code, "\n") {
			if _, err := fmt.Fprintf(w, "  %s\n", line); err != nil {
				return err
			}
		}
	}
	return nil
}

// queryAPI 是万能兜底：按用户给定路径与方法直连任意端点。
func queryAPI(args []string, out io.Writer, env queryEnv) error {
	usageLine := "jianartifact query api <path> [--json [字段,…]] [--jq EXPR] [-X 方法] [-d 请求体] [--server URL] [--token T]"
	parsed, err := parseQueryArgs(args, 1, usageLine, "X", "d")
	if err != nil {
		return err
	}
	rawPath := strings.TrimSpace(parsed.positional[0])
	if !strings.HasPrefix(rawPath, "/") {
		return usageErrf("path 必须以 / 开头（相对实例根地址），实际 %q\n用法：%s", rawPath, usageLine)
	}
	endpointPath, params, err := splitQueryEndpoint(rawPath)
	if err != nil {
		return err
	}
	method := strings.ToUpper(strings.TrimSpace(parsed.str("X")))
	if method == "" {
		method = http.MethodGet
	}
	var body []byte
	if raw := parsed.str("d"); raw != "" {
		body = []byte(raw)
	}
	client, opts, err := buildQueryClient(parsed, env)
	if err != nil {
		return err
	}
	reply, err := client.do(context.Background(), method, endpointPath, params, body)
	if err != nil {
		return err
	}
	// 兜底端点的形状未知，人类可读输出直接给格式化 JSON。
	return emitQueryResult(out, reply.body, opts, func(w io.Writer) error {
		value, err := decodeQueryJSON(reply.body)
		if err != nil {
			return err
		}
		return writeQueryJSON(w, value)
	})
}

// splitQueryEndpoint 把 `<path>?<query>` 拆成路径与结构化参数，并重新编码，
// 使 api 兜底也走 net/url 转义而不是手工拼串。
func splitQueryEndpoint(raw string) (string, url.Values, error) {
	index := strings.Index(raw, "?")
	if index < 0 {
		return raw, url.Values{}, nil
	}
	params, err := url.ParseQuery(raw[index+1:])
	if err != nil {
		return "", nil, usageErrf("查询串不合法（%s）：%v", raw[index+1:], err)
	}
	return raw[:index], params, nil
}

// emitQueryResult 按 --jq / --json / 人类可读三种模式输出。
// --json 字段筛选先于 --jq 生效，便于"先选字段再过滤"。
func emitQueryResult(out io.Writer, raw []byte, opts queryOutputOptions, human func(io.Writer) error) error {
	if !opts.json && opts.jq == "" {
		return human(out)
	}
	value, err := decodeQueryJSON(raw)
	if err != nil {
		return err
	}
	if opts.json && len(opts.fields) > 0 {
		value = selectQueryFields(value, opts.fields)
	}
	if opts.jq != "" {
		return runQueryJQ(out, opts.jq, value)
	}
	return writeQueryJSON(out, value)
}

// decodeQueryJSON 把响应体解码成通用结构；用 UseNumber 保住整数精度（哈希长度、字节数）。
func decodeQueryJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("解析响应 JSON：%w", err)
	}
	return value, nil
}

// writeQueryJSON 输出缩进 JSON。
func writeQueryJSON(w io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 JSON：%w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", encoded)
	return err
}

// selectQueryFields 按字段名裁剪：对数组逐条筛选，缺失字段写 null。
// 若顶层是带 items 数组的对象、且请求字段没有全部出现在顶层，则自动下钻到 items——
// 否则 `query repos --json name` 只会得到整片 null。
func selectQueryFields(value any, fields []string) any {
	switch typed := value.(type) {
	case []any:
		return selectQueryFieldItems(typed, fields)
	case map[string]any:
		items, isArray := typed["items"].([]any)
		if isArray && !queryFieldsAllPresent(typed, fields) {
			return selectQueryFieldItems(items, fields)
		}
		return pickQueryFields(typed, fields)
	default:
		return value
	}
}

// selectQueryFieldItems 对数组逐条做字段筛选。
func selectQueryFieldItems(items []any, fields []string) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		out = append(out, pickQueryFields(object, fields))
	}
	return out
}

// pickQueryFields 从对象里取指定字段；支持 a.b 形式的嵌套取值，缺失一律填 null。
func pickQueryFields(object map[string]any, fields []string) map[string]any {
	picked := make(map[string]any, len(fields))
	for _, field := range fields {
		picked[field] = lookupQueryField(object, field)
	}
	return picked
}

// lookupQueryField 按（可含 . 的）字段名取值，任一层缺失返回 nil。
func lookupQueryField(object map[string]any, field string) any {
	segments := strings.Split(field, ".")
	var current any = object
	for _, segment := range segments {
		next, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = next[segment]
		if !ok {
			return nil
		}
	}
	return current
}

// queryFieldsAllPresent 判断所有请求字段是否都能在顶层命中（用于决定是否下钻 items）。
func queryFieldsAllPresent(object map[string]any, fields []string) bool {
	for _, field := range fields {
		if lookupQueryField(object, field) == nil {
			return false
		}
	}
	return true
}

// runQueryJQ 用 gojq 执行过滤表达式，每个结果输出一段缩进 JSON。
func runQueryJQ(out io.Writer, expr string, value any) error {
	query, err := gojq.Parse(expr)
	if err != nil {
		return usageErrf("--jq 表达式解析失败：%v", err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return usageErrf("--jq 表达式编译失败：%v", err)
	}
	iter := code.Run(value)
	for {
		item, ok := iter.Next()
		if !ok {
			return nil
		}
		if halted, isHalt := item.(*gojq.HaltError); isHalt {
			// halt 且无值：正常停止迭代。
			if halted.Value() == nil {
				return nil
			}
			return fmt.Errorf("--jq 执行中止：%v", halted)
		}
		if runErr, isErr := item.(error); isErr {
			return fmt.Errorf("--jq 执行失败：%v", runErr)
		}
		encoded, err := gojq.Marshal(item)
		if err != nil {
			return fmt.Errorf("--jq 输出序列化失败：%v", err)
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, encoded, "", "  "); err != nil {
			return fmt.Errorf("--jq 输出格式化失败：%v", err)
		}
		if _, err := fmt.Fprintf(out, "%s\n", indented.Bytes()); err != nil {
			return err
		}
	}
}

// splitQueryRepoSpec 把 `<repo>[/<prefix>]` 拆成仓库名与前缀：第一个 / 之前是仓库。
func splitQueryRepoSpec(spec string) (repo, prefix string) {
	spec = strings.TrimSpace(spec)
	spec = strings.TrimPrefix(spec, "/")
	if index := strings.Index(spec, "/"); index >= 0 {
		return spec[:index], strings.Trim(spec[index+1:], "/")
	}
	return spec, ""
}

// queryParentPrefix 返回制品路径的父目录前缀（根层文件返回空串）。
func queryParentPrefix(path string) string {
	path = strings.Trim(path, "/")
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[:index]
	}
	return ""
}

// queryTreePrefix 把用户给的目录路径转成 tree 端点要求的 prefix 形态。
//
// 服务端 ListDirectChildren 用 `start = 字符数(prefix) + 1` 作为层级切分起点，
// 因此 prefix **必须以 "/" 结尾**才表示「列出该目录的内容」；不带斜杠时切分起点
// 落在目录名内部，instr 找不到 "/" 而返回空结果（不报错）。这个约定是隐式的，
// 拼错只会得到空列表而非错误，故由 CLI 统一归一化，调用方不必记住。
// 空串保持空串（表示仓库根）。
func queryTreePrefix(dir string) string {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return ""
	}
	return dir + "/"
}

// queryDisplayName 返回路径的末段名，用于人类可读输出（--json 仍保留完整路径）。
func queryDisplayName(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}

// formatQueryBytes 把字节数格式化成人可读字符串。
func formatQueryBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, name := range []string{"KB", "MB", "GB", "TB", "PB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, name)
		}
	}
	return fmt.Sprintf("%.1f EB", value)
}

// formatQuerySize 格式化为带单位的大小，缺省（nil）显示短横线。
func formatQuerySize(size *int64) string {
	if size == nil {
		return "-"
	}
	return formatQueryBytes(*size)
}

// formatQueryCount 格式化计数，缺省（nil）显示短横线。
func formatQueryCount(count *int) string {
	if count == nil {
		return "-"
	}
	return strconv.Itoa(*count)
}

// formatQueryOnline 格式化在线状态，缺省（nil）显示短横线。
func formatQueryOnline(online *bool) string {
	if online == nil {
		return "-"
	}
	if *online {
		return "在线"
	}
	return "离线"
}

// queryDash 把空字符串显示为短横线，避免表格出现空洞。
func queryDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
