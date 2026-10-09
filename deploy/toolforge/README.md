# ToolForge

给任意 LLM API 补齐 / 统一 **Function Calling** 的中间件。

```text
Client (OpenAI / Anthropic / Gemini SDK)
        │  tools / tool_calls / tool_use / functionCall
        ▼
   ToolForge  ──►  原生 FC 透传 或  XYML 提示词 + 解析
        │
        ▼
任意上游：OpenAI / Anthropic / Gemini / Grok / 本地兼容 / 你的网关
```

中间件**永不执行工具**。客户端拿到 `tool_calls` 后自行执行，再以 `tool` / `tool_result` 回传。

| 路径 | 条件 | 行为 |
|------|------|------|
| **A 原生透传** | 上游 `native_fc: true` | 原样转发 `tools` / `tool_calls` |
| **B 提示词 FC** | 上游 `native_fc: false` 或强制 prompt | 注入 XYML → 解析文本 → 标准 `tool_calls` |

---

## Docker

```bash
git clone https://github.com/YuJunZhiXue/toolforge.git
cd toolforge
docker compose up -d --build
curl http://127.0.0.1:8080/healthz
```

自定义配置：

```bash
cp config.example.yaml config.yaml
HOST_CONFIG=./config.yaml docker compose up -d --build
```

改端口：`HOST_PORT=9000`  
宿主机上游：`base_url: "http://host.docker.internal:7860/v1"`

---

## 本地

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
pip install -r requirements.txt
cp config.example.yaml config.yaml
uvicorn app.main:app --host 0.0.0.0 --port 8080
```

---

## 客户端

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8080/v1",
    api_key="sk-toolforge-demo",  # = config allowed_keys
)

r = client.chat.completions.create(
    model="your-model",
    messages=[{"role": "user", "content": "东京天气？"}],
    tools=[{
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "按城市查天气",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"],
            },
        },
    }],
)
print(r.choices[0].message.tool_calls)
```

强制提示词路径：Header `X-ToolForge-FC-Mode: force_prompt`

---

## 配置

| 字段 | 说明 |
|------|------|
| `client_authentication.allowed_keys` | 客户端访问本服务的 key |
| `upstreams[].type` | `openai_compat` / `anthropic` / `gemini` |
| `upstreams[].base_url` / `api_key` | 上游；支持 `${ENV}` |
| `upstreams[].native_fc` | `true` 透传；`false` 走 XYML |
| `upstreams[].models` | 模型名；`["*"]` 接所有 |
| `features.fc_mode` | `auto` / `prefer_native` / `force_prompt` |

---

## 端点

| 方法 | 路径 |
|------|------|
| GET | `/healthz` |
| GET | `/v1/models` |
| POST | `/v1/chat/completions` |
| POST | `/v1/responses` |
| POST | `/v1/messages` |
| POST | `/v1beta/models/{model}:generateContent` |
| POST | `/v1beta/models/{model}:streamGenerateContent` |

---

## 目录

```text
toolforge/
├── app/                  # 中间件代码
│   ├── main.py           # 入口
│   ├── config.py / auth.py
│   ├── engine/           # XYML + 编排
│   ├── adapters/         # 客户端协议
│   ├── upstream/         # 上游 HTTP
│   └── stream/           # SSE
├── config.example.yaml
├── requirements.txt
├── Dockerfile
├── docker-compose.yml
├── README.md
└── LICENSE
```

MIT
