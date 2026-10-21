package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// DurationUnit 是自动备份间隔的显示单位。
//
// # 为什么需要
//
// 之前间隔只能用秒输入：想设"每天一次"要填 86400，"每月一次"要算 2592000，
// 既容易算错也无法一眼核对。运维实际关心的是"多久一次"这个语义，
// 而不是它的秒数表示。
//
// # 存储口径不变
//
// 权威值仍然是**秒**（setting 表的 backup_auto_interval，也是调度器唯一读的量）。
// 本类型只负责"把人的表达翻译成秒"，因此不引入任何存储迁移，也不改变既有配置的含义。
type DurationUnit string

const (
	DurationSeconds DurationUnit = "seconds"
	DurationMinutes DurationUnit = "minutes"
	DurationHours   DurationUnit = "hours"
	DurationDays    DurationUnit = "days"
	DurationWeeks   DurationUnit = "weeks"
	DurationMonths  DurationUnit = "months"
	DurationYears   DurationUnit = "years"
)

// unitSeconds 是各单位的秒数。
//
// 月与年是**保守近似**（30 天 / 365 天）而不是日历精确值：
// 自动备份是"至少隔这么久跑一次"的兜底动作，差一两天的语义影响远小于
// "日历月实际长度不同导致配置含义漂移"的困惑。这也是绝大多数调度器的通行做法。
var unitSeconds = map[DurationUnit]int64{
	DurationSeconds: 1,
	DurationMinutes: 60,
	DurationHours:   3600,
	DurationDays:    24 * 3600,
	DurationWeeks:   7 * 24 * 3600,
	DurationMonths:  30 * 24 * 3600,
	DurationYears:   365 * 24 * 3600,
}

// ParseDurationUnit 解析单位标识；非法值返回错误（由调用方决定是回落还是报错）。
func ParseDurationUnit(raw string) (DurationUnit, error) {
	u := DurationUnit(strings.ToLower(strings.TrimSpace(raw)))
	if _, ok := unitSeconds[u]; ok {
		return u, nil
	}
	return "", fmt.Errorf("未知的时间单位 %q", raw)
}

// Seconds 返回该单位的秒数。
func (u DurationUnit) Seconds() int64 {
	if s, ok := unitSeconds[u]; ok {
		return s
	}
	return 0
}

// Valid 报告单位是否受支持。
func (u DurationUnit) Valid() bool { _, ok := unitSeconds[u]; return ok }

// DurationToParts 把秒拆成"最适合显示"的数值与单位，用于界面回填。
//
// 选择规则：优先用**能整除且数值不小于 1** 的最大单位，这样 86400 显示为「1 天」
// 而不是「86400 秒」。而 90 会显示为「90 秒」而不是「1.5 分钟」——本函数只做
// 整数拆分，绝不产生小数（"1.5 天"这种表达对运维没有意义）。
func DurationToParts(seconds int64) (value int64, unit DurationUnit) {
	if seconds <= 0 {
		return 0, DurationSeconds
	}
	// 从大到小试；第一个能整除的就是最合适的表达。
	// 顺序即优先级。周刻意排在**天之后**：7 天一次是最常见的表达，
	// 若让周抢先，"7 天"会被显示成"1 周"，用户认不出这是自己填的值。
	//
	// 副作用：周在后意味着**回填时几乎不会出现"周"**（14 天显示为"14 天"）。
	// 这是有意的取舍——w 仍可作**输入**别名（"1w" → 604800 秒），
	// 只是不作为显示单位。让用户看到自己填的那个量级，比省几个字符重要。
	for _, cand := range []DurationUnit{
		DurationYears, DurationMonths, DurationDays, DurationWeeks,
		DurationHours, DurationMinutes,
	} {
		unitSecs := unitSeconds[cand]
		if seconds%unitSecs == 0 && seconds/unitSecs >= 1 {
			return seconds / unitSecs, cand
		}
	}
	return seconds, DurationSeconds
}

// ComposeDuration 把"数值 + 单位"合成为秒，并按上限钳制。
//
// 返回错误的情形只有一种：单位非法或数值为负。上限不报错而是钳制——
// 用户输入"100 年"的意图是"尽长时间"，回落到上限比拒绝保存更符合意图
// （与既有的 ttlSeconds 钳制口径一致）。
func ComposeDuration(value int64, unit DurationUnit, maxSeconds int64) (int64, error) {
	if value < 0 {
		return 0, fmt.Errorf("时间数值不能为负：%d", value)
	}
	if !unit.Valid() {
		return 0, fmt.Errorf("未知的时间单位 %q", unit)
	}
	if value == 0 {
		// 0 恒等于"关闭"，不受单位影响——否则"0 个月"会成为一个既非开也非关的怪状态。
		return 0, nil
	}
	unitSecs := unit.Seconds()
	// 先做溢出保护：value * unitSecs 在极端输入下会溢出 int64。
	if value > maxSeconds/unitSecs {
		return maxSeconds, nil
	}
	total := value * unitSecs
	if maxSeconds > 0 && total > maxSeconds {
		return maxSeconds, nil
	}
	return total, nil
}

// ParseDurationExpression 解析形如 "30"、"90s"、"5m"、"2h"、"1d"、"1mo"、"1y" 的表达式，
// 返回秒。不带后缀时按秒解释（与旧行为一致，避免既有配置的含义被改变）。
//
// 支持的后缀：s/sec/seconds、m/min/minutes、h/hour/hours、d/day/days、
// w/week/weeks、mo/month/months、y/year/years。
//
// 注意 m 是**分钟**而不是月：这是 time 包的通用约定，而月必须写 mo——
// 否则 "1m" 会被误读成一个月，是 43200 倍的偏差，属于必须避免的歧义。
func ParseDurationExpression(raw string, maxSeconds int64) (int64, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return 0, fmt.Errorf("间隔不能为空")
	}
	// 拆出数字前缀与后缀。
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("间隔必须以数字开头：%q", raw)
	}
	numPart, suffix := s[:i], strings.TrimSpace(s[i:])
	value, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("间隔数值非法：%q", raw)
	}
	if suffix == "" {
		return ComposeDuration(value, DurationSeconds, maxSeconds)
	}
	unit, err := unitFromSuffix(suffix)
	if err != nil {
		return 0, err
	}
	return ComposeDuration(value, unit, maxSeconds)
}

// unitFromSuffix 把后缀映射到单位；别名表集中在函数内，避免包级散落一堆常量。
func unitFromSuffix(suffix string) (DurationUnit, error) {
	switch suffix {
	case "s", "sec", "secs", "second", "seconds":
		return DurationSeconds, nil
	case "m", "min", "mins", "minute", "minutes":
		return DurationMinutes, nil
	case "h", "hr", "hour", "hours":
		return DurationHours, nil
	case "d", "day", "days":
		return DurationDays, nil
	case "w", "week", "weeks":
		return DurationWeeks, nil
	case "mo", "month", "months":
		return DurationMonths, nil
	case "y", "year", "years":
		return DurationYears, nil
	}
	return "", fmt.Errorf("未知的时间单位后缀 %q（支持 s/m/h/d/w/mo/y）", suffix)
}

// FormatDurationHuman 把秒渲染成可读的中文表达，供日志与摘要使用。
func FormatDurationHuman(seconds int64) string {
	if seconds <= 0 {
		return "关闭"
	}
	value, unit := DurationToParts(seconds)
	switch unit {
	case DurationYears:
		return fmt.Sprintf("%d 年", value)
	case DurationMonths:
		return fmt.Sprintf("%d 个月", value)
	case DurationWeeks:
		return fmt.Sprintf("%d 周", value)
	case DurationDays:
		return fmt.Sprintf("%d 天", value)
	case DurationHours:
		return fmt.Sprintf("%d 小时", value)
	case DurationMinutes:
		return fmt.Sprintf("%d 分钟", value)
	default:
		return fmt.Sprintf("%d 秒", value)
	}
}
