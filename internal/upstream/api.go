// api.go RicheeAI 域方法：credit / userSign / gift / subscription / user /
// SMS 登录族 / inviteCode。全部走 doJSON（信封 + 分类 + 传输层重试）。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"richee2api/internal/auth"
)

// 路径常量（SPEC 事实，勿发明端点）。
const (
	creditInfoPath   = "/credit/info"
	userSignCreate   = "/userSign/create"
	userSignToday    = "/userSign/today"
	giftStatusPath   = "/gift/status"
	giftClaimPath    = "/gift/claim"
	subscriptionPath = "/subscription/my"
	userCurrentPath  = "/claw/user/current"
	userProfilePath  = "/claw/user/profile"
	sendCodePath     = "/claw/auth/verify-code/send"
	loginPath        = "/claw/auth/login"
	geetestInitPath  = "/claw/auth/geetest/init"
	inviteCheckPath  = "/claw/inviteCode/checkState"
	ssoPath          = "/sso"
)

// CreditInfo /credit/info 响应体。
// 字段是浮点（实测 totalCredits:3099.96——积分带小数），聚合口径取
// 向下取整（可用余额宁少勿多，防选号权重虚高）。
type CreditInfo struct {
	TotalCredits      float64 `json:"totalCredits"`
	WelfareCredits    float64 `json:"welfareCredits"`
	PlanCredits       float64 `json:"planCredits"`
	TopupCredits      float64 `json:"topupCredits"`
	PlanQuota         float64 `json:"planQuota"`
	PlanConsumed      float64 `json:"planConsumed"`
	TopupExpireTime   string  `json:"topupExpireTime"`
	SubscriptionState string  `json:"subscriptionStatus"`
	// Usable 实测是 bool（「当前可用」语义标记，非数值）——SPEC 描述口径修正。
	Usable bool `json:"usable"`
}

// CreditInfo 查询账号积分（totalCredits 聚合口径；usable 为上游声明可用额）。
func (c *Client) CreditInfo(a *auth.Auth) (*CreditInfo, error) {
	data, err := c.apiJSON(a, http.MethodGet, creditInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var ci CreditInfo
	if err := json.Unmarshal(data, &ci); err != nil {
		return nil, fmt.Errorf("credit parse: %w", err)
	}
	return &ci, nil
}

// UserResource 查询账号当前可用积分（credit/info 的 totalCredits 口径，
// 浮点向下取整——积分带小数，选号权重用整数即可）。pool 签到解冻用。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	ci, err := c.CreditInfo(a)
	if err != nil {
		return 0, err
	}
	return int64(ci.TotalCredits), nil
}

// ResourceSummary 查询账号积分的完整聚合口径（remain/used/size/packs），
// 供运维工具（cmd/credit）展示真实余额。RicheeAI 的 /credit/info 是单
// 套餐口径（非 CodeBuddy 的 Accounts 数组），packs 恒 1，used = planQuota -
// planConsumed 的下游语义按 remain/quota 折算：
//   - remain = totalCredits（可花总额）
//   - size = totalCredits + planConsumed（历史总量估算）
//   - used = planConsumed（已消耗的套餐配额）
func (c *Client) ResourceSummary(a *auth.Auth) (remain, used, size int64, packs int, err error) {
	ci, err := c.CreditInfo(a)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	remain = int64(ci.TotalCredits)
	used = int64(ci.PlanConsumed)
	size = int64(ci.TotalCredits + ci.PlanConsumed)
	if size < remain {
		size = remain
	}
	return remain, used, size, 1, nil
}

// CreditBuckets 按到期紧迫度拆分的积分余额（供 pool 优先消耗快过期积分）。
// RicheeAI /credit/info 无到期时间字段：Expiring 恒 0（分桶禁用语义），
// 保留结构以兼容 pool 侧的调用形态。
type CreditBuckets struct {
	Expiring int64
	Stable   int64
}

// Total 两桶合计。
func (b CreditBuckets) Total() int64 { return b.Expiring + b.Stable }

// UserResourceDetailed 同 UserResource，但带分桶口径（RicheeAI 无到期字段，
// 全部归 Stable）。
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (int64, CreditBuckets, error) {
	remain, err := c.UserResource(a)
	if err != nil {
		return 0, CreditBuckets{}, err
	}
	return remain, CreditBuckets{Stable: remain}, nil
}

// apiJSON 发 API 域请求（GET/POST + richee-token）并解信封。
func (c *Client) apiJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	return c.doJSONAuth(c.baseURL()+path, method, body, func(req *http.Request) {
		c.CommonHeaders(req, a)
	})
}

// doJSONAuth 是 doJSON 的请求构造前置（带自定义头注入的版本）。
func (c *Client) doJSONAuth(url, method string, body any, inject func(*http.Request)) (json.RawMessage, error) {
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	if inject != nil {
		inject(req)
	}
	return c.doJSON(req)
}

// DailyCheckin 执行每日签到（POST /userSign/create → {data:true}）。
// 已签到（业务失败 + msg 已签到）也返回错误，调用方按 IsAlreadyCheckin 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.apiJSON(a, http.MethodPost, userSignCreate, map[string]any{})
	return err
}

// SigninToday 查询今日签到记录（GET /userSign/today → {id,userId,createTime,longTimes}）。
// 未签到时 data 为 null / 空 → (nil, nil)。
func (c *Client) SigninToday(a *auth.Auth) (*SignRecord, error) {
	data, err := c.apiJSON(a, http.MethodGet, userSignToday, nil)
	if err != nil {
		return nil, err
	}
	trimmed := string(bytes.TrimSpace(data))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var rec SignRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("signin parse: %w", err)
	}
	if rec.ID == "" && rec.UserID == "" {
		return nil, nil // 空对象按未签到处理
	}
	return &rec, nil
}

// SignRecord 今日签到记录。
type SignRecord struct {
	ID         string `json:"id"`
	UserID     string `json:"userId"`
	CreateTime string `json:"createTime"`
	LongTimes  int    `json:"longTimes"`
}

// GiftStatus 新手礼包状态（GET /gift/status）。
type GiftStatus struct {
	CanClaim    bool   `json:"canClaim"`
	ClaimStatus string `json:"claimStatus"`
	GiftType    string `json:"giftType"`
	ClaimTime   string `json:"claimTime"`
}

// GiftStatus 查询新手礼包状态。
func (c *Client) GiftStatus(a *auth.Auth) (*GiftStatus, error) {
	data, err := c.apiJSON(a, http.MethodGet, giftStatusPath, nil)
	if err != nil {
		return nil, err
	}
	var gs GiftStatus
	if err := json.Unmarshal(data, &gs); err != nil {
		return nil, fmt.Errorf("gift parse: %w", err)
	}
	return &gs, nil
}

// ClaimGift 领取新手礼包（POST /gift/claim）。已领过返回业务错误（*Error），
// 调用方静默跳过。返回是否成功领取。
func (c *Client) ClaimGift(a *auth.Auth) (bool, error) {
	_, err := c.apiJSON(a, http.MethodPost, giftClaimPath, map[string]any{})
	if err != nil {
		return false, err
	}
	return true, nil
}

// Subscription 订阅信息（GET /subscription/my）。
type Subscription struct {
	PlanName     string `json:"planName"`
	PlanType     string `json:"planType"`
	BillingCycle string `json:"billingCycle"`
	Status       string `json:"subscriptionStatus"`
	StartTime    string `json:"startTime"`
	EndTime      string `json:"endTime"`
	PlanQuota    int64  `json:"planQuota"`
	PlanCredits  int64  `json:"planCredits"`
	PlanConsumed int64  `json:"planConsumed"`
}

// SubscriptionMy 查询订阅信息。
func (c *Client) SubscriptionMy(a *auth.Auth) (*Subscription, error) {
	data, err := c.apiJSON(a, http.MethodGet, subscriptionPath, nil)
	if err != nil {
		return nil, err
	}
	var sub Subscription
	if err := json.Unmarshal(data, &sub); err != nil {
		return nil, fmt.Errorf("subscription parse: %w", err)
	}
	return &sub, nil
}

// UserProfile 当前用户信息（GET /claw/user/current）。
type UserProfile struct {
	UserID   string `json:"userId"`
	NickName string `json:"nickName"`
	Phone    string `json:"phone"`
}

// UserCurrent 查询当前用户（GET /claw/user/current）——token-only auth 文件
// 的 uid/nickName 补齐入口。
func (c *Client) UserCurrent(a *auth.Auth) (*UserProfile, error) {
	data, err := c.apiJSON(a, http.MethodGet, userCurrentPath, nil)
	if err != nil {
		return nil, err
	}
	var u UserProfile
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("user parse: %w", err)
	}
	return &u, nil
}

// UserProfileOf 查询用户档案（GET /claw/user/profile）。
func (c *Client) UserProfileOf(a *auth.Auth) (*UserProfile, error) {
	data, err := c.apiJSON(a, http.MethodGet, userProfilePath, nil)
	if err != nil {
		return nil, err
	}
	var u UserProfile
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("profile parse: %w", err)
	}
	return &u, nil
}

// SendVerifyCode 发送 SMS 验证码（POST /claw/auth/verify-code/send）。
// scene 固定 "0"；geetest 字段（challenge/validate/seccode）可选——
// 服务端对每手机号每天前 3 次发送跳过校验。
func (c *Client) SendVerifyCode(phone string, geetest GeetestFields) error {
	body := map[string]any{
		"userName": phone,
		"scene":    "0",
	}
	if geetest.Challenge != "" {
		body["challenge"] = geetest.Challenge
		body["validate"] = geetest.Validate
		body["seccode"] = geetest.Seccode
	}
	_, err := c.doJSONAuth(c.baseURL()+sendCodePath, http.MethodPost, body, func(req *http.Request) {
		c.AuthHeaders(req)
	})
	return err
}

// GeetestFields geetest 三件套（>3 次/日发送时的验证挑战；前 3 次可空）。
type GeetestFields struct {
	Challenge string
	Validate  string
	Seccode   string
}

// GeetestInit geetest init（POST /claw/auth/geetest/init →
// {new_captcha,success,challenge,gt}）——发送次数超限时的人机挑战数据源。
type GeetestInit struct {
	NewCaptcha bool   `json:"new_captcha"`
	// Success 字段容差：上游实测为数字 1（真）而非布尔——用 any 解码后归一。
	Success    any    `json:"success"`
	Challenge  string `json:"challenge"`
	GT         string `json:"gt"`
}

// SuccessOK 报告 success 字段的归一化真值（数字 1 或布尔 true）。
func (g *GeetestInit) SuccessOK() bool {
	switch v := g.Success.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v == "1" || strings.EqualFold(v, "true")
	}
	return false
}

// GeetestInit 拉取 geetest 挑战参数。
func (c *Client) GeetestInit() (*GeetestInit, error) {
	data, err := c.doJSONAuth(c.baseURL()+geetestInitPath, http.MethodPost, map[string]any{}, func(req *http.Request) {
		c.AuthHeaders(req)
	})
	if err != nil {
		return nil, err
	}
	var gi GeetestInit
	if err := json.Unmarshal(data, &gi); err != nil {
		return nil, fmt.Errorf("geetest parse: %w", err)
	}
	return &gi, nil
}

// LoginWithCode SMS 验证码登录（POST /claw/auth/login →
// {token,userId,nickName,...}）。成功返回完整凭证。
func (c *Client) LoginWithCode(phone, code string) (*auth.Auth, error) {
	body := map[string]any{
		"userName":   phone,
		"verifyCode": code,
		"scene":      "0",
	}
	data, err := c.doJSONAuth(c.baseURL()+loginPath, http.MethodPost, body, func(req *http.Request) {
		c.AuthHeaders(req)
	})
	if err != nil {
		return nil, err
	}
	var tok struct {
		Token    string `json:"token"`
		UserID   string `json:"userId"`
		NickName string `json:"nickName"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.Token == "" {
		return nil, fmt.Errorf("login_failed: no token in response — re-login required")
	}
	return &auth.Auth{
		Token:    tok.Token,
		UID:      tok.UserID,
		Nickname: tok.NickName,
		Phone:    phone,
	}, nil
}

// InviteState 邀请码状态（GET /claw/inviteCode/checkState）。
type InviteState struct {
	// 响应字段按 spec 探测口径：canClaim/claimed 泛型形态。
	CanCheck bool `json:"canCheck"`
}

// InviteCodeCheckState 探测邀请码状态（activity 域只读端点）。
func (c *Client) InviteCodeCheckState(a *auth.Auth) (json.RawMessage, error) {
	return c.apiJSON(a, http.MethodGet, inviteCheckPath, nil)
}

// SSOURL 生成 web 控制台 SSO 跳转链接（GET {base}/sso?token=...&redirect=...）。
// 只读构造，无上游调用。
func (c *Client) SSOURL(a *auth.Auth, redirect string) string {
	tok := ""
	if a != nil {
		tok = a.TokenValue()
	}
	return c.baseURL() + ssoPath + "?token=" + tok + "&redirect=" + redirect
}

// IsAlreadyCheckin 报告 err 是否表示「今天已签到」（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作
// 幂等成功，否则停机补签遇到抖动会误记为 already、账号当天实际未签到。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	return alreadyCheckinRule.hit(ue.Msg, strings.ToLower(ue.Msg))
}
