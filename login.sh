#!/usr/bin/env bash
# login.sh — RicheeAI SMS 登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh              # 交互式：提示手机号 → 发码 → 提示验证码 → 登录
#   ./login.sh <phone>      # 指定手机号（发码 + 登录一步完成）
#
# 流程:
#   1. 输入手机号
#   2. POST /claw/auth/verify-code/send 发送短信验证码（每号每天前 3 次免人机验证）
#   3. 输入收到的短信验证码
#   4. POST /claw/auth/login 拿 token+userId+nickName
#   5. 落盘 auths/richee-<uid>.json（uid 缺席时由 user/current 探测补齐）
#   6. 重启 richee2api 容器加载新账号（容器运行时）
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="richee2api"

mkdir -p "$AUTH_DIR" 2>/dev/null || true
# ─── auths/ 可写性预检：无写权限时 fail-fast，不白跑一次短信登录 ───
if ! [[ -w "$AUTH_DIR" ]]; then
    echo "❌ 无法写入 $AUTH_DIR（auths/ 已归属容器 uid 10001？）" >&2
    echo "    请在容器内登录（属主自动正确）：" >&2
    echo "    docker compose exec -it r2api bash -c './login.sh' && docker compose restart r2api" >&2
    exit 1
fi

# login 工具：缺失或构建输入比它新时重编。
LOGIN_BIN="./login_bin"
if [[ ! -x "$LOGIN_BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$LOGIN_BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 login（或镜像内置 /app/login_bin）" >&2
        exit 1
    fi
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  RicheeAI 短信登录"
echo "============================================================"
echo ""

PHONE=""
if [[ $# -gt 0 ]]; then
    PHONE="$1"
fi
if [[ -z "$PHONE" ]]; then
    if [[ -t 0 ]]; then
        read -rp "输入手机号: " PHONE
    else
        echo "非交互模式：请用 ./login.sh <phone>" >&2
        exit 1
    fi
fi
if [[ -z "$PHONE" ]]; then
    echo "手机号不能为空" >&2
    exit 1
fi

# 1) 发送验证码（go 侧 CLI：send 子命令）。
if ! "$LOGIN_BIN" send "$PHONE"; then
    echo ""
    echo "发送验证码失败。可能原因：" >&2
    echo "  - 手机号输错" >&2
    echo "  - 发送次数超限（每号每天前 3 次免人机验证，超限需 geetest）" >&2
    exit 1
fi

# 2) 输入验证码 + 登录（go 侧 CLI：login 子命令）。
read -rp "输入短信验证码: " CODE
RESULT=$("$LOGIN_BIN" login "$PHONE" "$CODE") || {
    echo ""
    echo "登录失败。可能原因：" >&2
    echo "  - 验证码输错（可重跑 ./login.sh 再试）" >&2
    exit 1
}

# 3) 解析凭证 JSON（token/userId/nickName/phone）。
TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('token',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('userId',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickName',''))")
if [[ -z "$TOKEN" ]]; then
    echo "无法获取 token，请检查验证码是否正确" >&2
    exit 1
fi

# 4) 落盘 auth 文件（与 internal/auth 读取格式一致）。
AUTH_FILE="$AUTH_DIR/richee-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（uid=${USER_ID}），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（uid=${USER_ID}），新增 auth 文件"
    ACTION="新增"
fi
R2A_LOGIN_TOKEN="$TOKEN" \
R2A_LOGIN_USER_ID="$USER_ID" \
R2A_LOGIN_NICKNAME="$NICKNAME" \
R2A_LOGIN_PHONE="$PHONE" \
R2A_LOGIN_AUTH_FILE="$AUTH_FILE" \
R2A_LOGIN_ACTION="$ACTION" \
python3 - <<'PYEOF'
import json, os, sys, tempfile

auth = {
    "token": os.environ["R2A_LOGIN_TOKEN"],
    "userId": os.environ.get("R2A_LOGIN_USER_ID", ""),
    "nickName": os.environ.get("R2A_LOGIN_NICKNAME", ""),
    "phone": os.environ.get("R2A_LOGIN_PHONE", ""),
}
auth_file = os.environ["R2A_LOGIN_AUTH_FILE"]
# 容器内 app 运行 uid（Dockerfile USER 10001）：属主不匹配会导致账号数为 0。
CONTAINER_UID = 10001
tmp_file = None
try:
    fd, tmp_file = tempfile.mkstemp(prefix=".richee-auth-", dir=os.path.dirname(auth_file) or ".")
    with os.fdopen(fd, "w") as f:
        json.dump(auth, f, indent=1, ensure_ascii=False)
    os.replace(tmp_file, auth_file)
    st = os.stat(auth_file)
    if st.st_uid != CONTAINER_UID:
        print(f"\n⚠️  权限警告：{auth_file} 属主 uid={st.st_uid}，容器内 app(uid={CONTAINER_UID}) 可能读不到")
        print(f"    请执行：chown -R {CONTAINER_UID}:{CONTAINER_UID} ./auths")
        print(f"    或在容器内登录（属主自动正确）：docker compose exec -it r2api bash -c './login.sh'\n")
except Exception:
    if tmp_file is not None:
        try:
            os.unlink(tmp_file)
        except FileNotFoundError:
            pass
    auth_dir = os.path.dirname(auth_file) or "."
    if not os.access(auth_dir, os.W_OK):
        print(f"\n❌ 写入失败：目录 {auth_dir} 不可写（权限不足）", file=sys.stderr)
        print(f"    请执行：chown -R {CONTAINER_UID}:{CONTAINER_UID} ./auths", file=sys.stderr)
    raise
print(f"已保存（{os.environ['R2A_LOGIN_ACTION']}）: {auth_file}")
PYEOF

# 5) 重启容器（运行时）。
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    API_KEY=$(python3 -c "import json; print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null)
    COUNT=$(curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer ${API_KEY:-test_key}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  UID: ${USER_ID:-（未获取到）}"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Phone: $PHONE"
echo "  Token: ${TOKEN:0:30}...（不过期，服务端撤销才失效）"
echo "============================================================"
