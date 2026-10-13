// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因（参考 issue #49）：一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与主程序缺省漂移。把 Schedule 段 + 默认值/归一化抽到本包，两个命令共用。
//
// RicheeAI 口径（见 docs/PORT_NOTES.md）：两类独立排程 —— 签到（checkin）/
// 礼包（gift）。参考的旅行/活跃上报/开学季/夜猫子无对应端点，不移植。
package config

import "fmt"

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 两类独立排程：签到 / 礼包。cmd/server 与 cmd/activity 共用本结构，
// 默认值由 DefaultSchedule 填充、缺省归一由 Normalize 完成。
type Schedule struct {
	CheckinHours []int `json:"checkin_hours"` // [9,21]
	GiftHours    []int `json:"gift_hours"`    // [10]
	// CheckinEnabled/GiftEnabled 显式禁用开关（缺省 true）。
	//
	// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
	//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
	//     所有老 config 的行为；bool 缺省 true 则对老配置零影响，向后完全兼容。
	//   - 开关与取值解耦：禁用时仍保留用户显式配的小时，重新启用无需补配。
	//   - 无需猜测哨兵（[-1] 之类），非法小时一律报错并提示改用本开关。
	CheckinEnabled bool `json:"checkin_enabled"` // 缺省 true；false = 关签到
	GiftEnabled    bool `json:"gift_enabled"`    // 缺省 true；false = 关礼包领取
}

// DefaultSchedule 返回排程段的默认值。
//
// 开关「缺省 true」靠这里实现：调用方先取 DefaultSchedule 再用 json.Unmarshal
// 覆盖，键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
func DefaultSchedule() Schedule {
	return Schedule{
		CheckinHours:   []int{9, 21},
		GiftHours:      []int{10},
		CheckinEnabled: true,
		GiftEnabled:    true,
	}
}

// Normalize 归一化排程段：空数组/null 回落默认小时，校验小时范围。
//
// 空数组与 null 反序列化后覆盖掉 DefaultSchedule 的排程值（键缺席才保留），
// 在此补齐。空 = 未配置 → 回落默认；「禁用」一律走 *_enabled=false。
func (s *Schedule) Normalize() error {
	if len(s.CheckinHours) == 0 {
		s.CheckinHours = []int{9, 21}
	}
	if len(s.GiftHours) == 0 {
		s.GiftHours = []int{10}
	}
	return s.validateHours()
}

// validateHours 校验排程小时落在 0-23。
//
// 为什么不用 `[-1]` 之类的哨兵值表意"禁用"：非法小时被静默吞掉时，用户以为
// 关掉了签到，实际可能被当成另一个整点照常执行；这里直接快速失败，并在错误
// 信息里指向正确的开关（checkin_enabled / gift_enabled）。
func (s *Schedule) validateHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", s.CheckinHours); err != nil {
		return err
	}
	return checkHourRange("schedule.gift_hours", "gift_enabled", s.GiftHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d 不是合法小时（0-23）；如要关闭该任务请设 schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
