# miroxy vs claude-code-router vs cc-switch vs LiteLLM
一份详尽对比，帮助判断miroxy是否值得继续开发。

## 一句话总结

这几个工具解决的是不同的问题。**miroxy是配额倍增器**（池化多个API key换取更高吞吐）。**claude-code-router是请求路由器**（把不同类型的请求发给不同模型）。**cc-switch是provider管理器**（用GUI切换工具底层用哪个backend）。**LiteLLM是通用LLM网关**（统一100+家provider的协议，自带多deployment负载均衡和企业级治理功能）。重叠是真实存在的，但miroxy的核心价值主张——**针对同一provider的多个key做配额感知的水平扩展**——四者里只有miroxy把这一点做到了"刁钻且正确"的程度；LiteLLM有功能上重叠的能力，但实现思路和定位不同（详见下文修正说明）。

## 四个工具概览

| | miroxy | claude-code-router | cc-switch | LiteLLM |
|---|---|---|---|---|
| 语言 | Go | TypeScript / Node.js | Rust + React（Tauri桌面应用） | Python |
| 接口形态 | Headless HTTP代理 | Headless HTTP代理 | 桌面GUI + 可选代理 | Headless HTTP代理 + 可选管理UI |
| 二进制/安装 | 单一静态二进制 | npm install -g | 平台安装包（.deb/.dmg/.msi） | pip install / Docker镜像 |
| 运行时依赖 | 无 | Node.js 20 + ~40个npm包 | 无（已打包） | Python运行时 + 依赖；分布式场景需Redis |
| 支持的Provider | Gemini（v1），可扩展 | 12+（任何OpenAI兼容的） | 50+预设，7个应用集成 | 100+（几乎所有主流provider） |
| Key池化/多Deployment | 是 —— 核心功能 | 否 | 否 | 是 —— 但是**多deployment负载均衡**，不是专门为"同provider多key配额倍增"设计的 |
| 请求路由 | 否 | 是 —— 按token数+场景分类 | 仅基础failover | 是 —— 按权重/延迟/用量等多种策略 |
| 配置 | YAML文件 | JSON5文件 | SQLite + GUI | YAML文件（+可选DB持久化） |
| 测试 | 是（单元+集成） | 无 | 是（vitest + cargo test） | 是，覆盖广泛 |

---

## 详细架构

### miroxy

```
Claude Code
    │  POST /v1/messages
    ▼
[认证校验器]
    │
[模型查找] ──► config.yaml
    │
[KeyPool.Acquire]
  round_robin | least_requests
  滑动窗口RPM检查
    │
[Translator.ToUpstream]
  Anthropic → Gemini 请求格式
    │
[Gemini API] ◄── 按key发起HTTP调用
    │
[Translator.FromUpstream]
  Gemini → Anthropic 响应/SSE格式
    │
[KeyPool.Release]
  递增式429退避（10→30→60→120→300秒）
  达到5xx阈值触发熔断
    │
Claude Code ◄── Anthropic格式的响应
```

KeyPool是核心抽象。每个模型条目下，多个API key共享一个池子。池子追踪：每个key的并发in-flight数、滑动窗口请求计数（在触顶前主动软性轮转）、每个key的429失败分级（基于Gemini 429响应体里权威的retryDelay字段递增退避）、以及5xx失败的熔断状态。流式与非流式走两条独立代码路径；流式请求在提交任何SSE header之前，最多用不同的key重试3次。

### claude-code-router (CCR)

```
Claude Code
    │  POST /v1/messages
    ▼
[认证] ── 可选的APIKEY校验
    │
[Router] ◄── tiktoken计算token数
  场景分类：background / think / longContext
            webSearch / image / default
    │
[Provider + Model] 从Router配置中选定
    │
[Agent拦截？] ── 图片agent，tool-call拦截
    │
[Transformer链 — 请求]
  GeminiTransformer, DeepseekTransformer,
  MaxTokenTransformer, ReasoningTransformer …
    │
[上游HTTP] ──► provider base_url
    │
[Transformer链 — 响应]
    │
Claude Code ◄── Anthropic格式的响应
```

CCR的核心思路是**语义路由**——统计进来请求的token数，把任务分类（快速后台操作、长上下文、推理、网络搜索、图片），然后分派给不同的模型。Router配置把场景名映射到"provider,model"字符串。这是纯粹的质量/成本优化：便宜快速的模型处理简单任务，强模型处理硬推理。它有20+个provider专属transformer，带市场分发的预设共享生态，服务管理CLI，以及Web UI。**没有key池化**——每个provider一个key，不做轮转。

### cc-switch

```
-- 主模式：配置文件管理 --

GUI操作（切换provider）
    │
[Provider服务] ── 从SQLite读取
    │
写入实时配置文件：
  ~/.claude/settings.json
  ~/.codex/config.toml + auth.json
  ~/.config/gemini/config.json
  ~/.config/opencode/opencode.json
  …（共7个应用目标）

-- 可选模式：本地代理 --

Claude Code
    │  HTTP → 127.0.0.1:15721
    ▼
[Handler上下文] ── 检测应用类型、认证策略
    │
[Provider路由] ── 每个provider独立熔断器
    │
[格式适配器]
  Anthropic ↔ OpenAI Chat ↔ OpenAI Responses ↔ Gemini原生
    │
[转发器] ── reqwest，3–6次重试，指数退避
    │
[上游API]
    │
[响应处理器] ── 解析用量，流式转换
    │
[用量记录器] ──► SQLite（成本、token、延迟）
    │
Claude Code ◄── 响应
```

cc-switch本质上是一个**配置管理器**，不只是代理。它的主要工作是同时写入`~/.claude/settings.json`及7个工具的等效文件。代理功能是可选的附加件。它有50+个provider预设、会话历史浏览、带成本图表的用量看板、MCP server同步、深链导入（`ccswitch://...`）、云备份（WebDAV / S3）。Provider管理是一对一的——没有key池化，只有顺序failover队列。

### LiteLLM

```
任意客户端（OpenAI SDK / Anthropic SDK / 原生HTTP）
    │  POST /v1/chat/completions 或 /v1/messages
    ▼
[认证 + 虚拟Key校验] ── 团队/预算/RBAC检查（企业版）
    │
[Router] ◄── routing_strategy选择
  simple-shuffle | least-busy |
  usage-based-routing | latency-based-routing
    │
[同一model_name下的多个Deployment]
  按order/weight/rpm/tpm过滤可用项
    │
[Translator] ── 统一转译为OpenAI格式作为中间表示
    │
[上游Provider HTTP调用]
    │
[失败处理]
  按错误类型差异化retry_policy
  429立即触发该deployment冷却（cooldown_time）
  耗尽该model_name下所有deployment后fallback到下一个model group
    │
[响应转译回客户端期望的格式]
    │
客户端 ◄── 响应
```

LiteLLM的核心思路是**统一抽象 + 可靠性兜底**。它把100+家provider都抽象成一份配置里的`model_list`条目，**同一个`model_name`可以挂多个`deployment`**（可以是同provider的多个key/region，也可以是完全不同的provider），路由策略决定每次请求选哪个。失败时按可配置的错误类型分别重试（`RateLimitErrorRetries`、`TimeoutErrorRetries`等），429会立即让该deployment进入冷却（`cooldown_time`），冷却期长度是**固定值**，不是递增退避。多实例部署场景下用Redis共享冷却状态和用量计数。

---

## 一处重要修正：LiteLLM并非"什么都没做"

原对比文档（只涵盖CCR和cc-switch）的结论是"两者都没做key池化，miroxy在这块是唯一选项"。**加入LiteLLM后，这句话需要修正**——LiteLLM确实有功能上重叠的能力，但实现的"刁钻程度"和设计目标跟miroxy不同：

| 维度 | miroxy | LiteLLM |
|---|---|---|
| 多key/多deployment负载均衡 | 是 | 是 |
| 设计目标 | 专门针对"同一provider的多个免费层key如何榨出最大吞吐" | 通用目的：跨provider/跨region的可靠性与负载均衡，企业多租户场景 |
| 429退避策略 | **递增式**（10→30→60→120→300秒），并解析Gemini 429响应体里**权威的retryDelay**字段 | **固定**冷却时长（`cooldown_time`），不解析provider返回的具体retryDelay数值，不区分"短暂限流"和"配额耗尽" |
| 配额耗尽检测 | 设计中明确区分"短期限流"vs"长期配额耗尽"（计划中：retryDelay>1小时则整段冷却到位） | 没有这种区分；长时间限流和短时限流都走同一套冷却机制，可能造成无意义的反复重试 |
| 运行依赖 | 单一静态二进制，无外部依赖 | Python运行时；多实例部署需要Redis共享状态 |
| 定位 | 轻量、单一用途的代理 | 全功能LLM网关（多租户、预算管理、SSO、审计日志等企业特性） |

**结论**：LiteLLM的Router**理论上能配置出类似miroxy的效果**（把同一个Gemini模型配置成多个deployment，每个用不同key，配合`usage-based-routing`策略），但它不是为"免费层key的配额精细化管理"这个场景精雕细琉出来的——没有递增退避、没有权威retryDelay解析、没有配额耗尽与短期限流的区分。**如果你的诉求只是"轮着用我的几个Gemini key"，LiteLLM能凑合用；如果你的诉求是"在免费层配额边界上尽可能不浪费一次请求"，miroxy的设计精细度更高**。但反过来说，这也意味着miroxy面对的真实竞争对手比原文档说的更强一些——LiteLLM团队随时可能把"识别长retryDelay并整段冷却"这种增强加进自己的Router里，毕竟这只是一个if判断的工程量，不是架构级的缺失。

---

## 逐项功能对比

### Key轮转与配额管理

| 功能 | miroxy | CCR | cc-switch | LiteLLM |
|---|---|---|---|---|
| 单provider多key | 是 | 否 | 否 | 是（建模为多个deployment） |
| Round-robin / 最少in-flight优先 | 是 | — | — | 是（`simple-shuffle` / `least-busy`） |
| 单key滑动窗口RPM上限 | 是 | — | — | 部分（`rpm`/`tpm`为硬限，非滑动窗口主动软轮转） |
| 触顶前主动软轮转 | 是 | — | — | 否 |
| 429递增退避 | 是（10→30→60→120→300秒） | — | 固定60秒 | 固定`cooldown_time`（不递增） |
| 权威retryDelay解析 | 是（解析Gemini响应体） | — | 否 | 否 |
| 熔断（5xx） | 是 | — | 是（按provider） | 是（`allowed_fails`阈值触发） |
| 流式请求的key轮转重试 | 是（提交header前最多3次） | 否 | 否 | 否（流式中途失败不重新选key） |

这是miroxy独特定位所在，但需要补充说明：LiteLLM在"多deployment负载均衡"这个大类上是有覆盖的，只是没有针对"免费层配额边界精细管理"这个细分场景做专门优化。如果你的目标是从多个免费层Gemini账号（各自约15 RPM上限）里榨出最大吞吐，miroxy目前仍是这四者里实现得最贴合这个场景的。

### Provider与格式支持

| 功能 | miroxy | CCR | cc-switch | LiteLLM |
|---|---|---|---|---|
| 当前支持的Provider数 | 仅Gemini | 12+（OpenAI兼容） | 50+预设 | 100+ |
| 新增一个Provider的成本 | 实现1个文件的Translator接口 | 配置项+transformer | GUI或深链 | 配置条目（绝大多数provider已内置支持） |
| Gemini原生格式 | 是（手写，正确） | 通过GeminiTransformer | 是 | 是（内置） |
| OpenAI Chat格式 | 否（计划中） | 是 | 是 | 是（原生） |
| Anthropic（passthrough） | — | 是 | 是 | 是 |
| VertexAI | 否 | 是（transformer） | 通过预设 | 是 |
| DeepSeek推理 | 否 | 是 | 通过预设 | 是 |
| Ollama/本地模型 | 否 | 是 | 是 | 是 |
| 工具调用翻译 | 部分（推迟实现） | 是 | 是 | 是 |

CCR、cc-switch和LiteLLM在广度上都赢miroxy。miroxy的translator窄（仅Gemini）但深——流式翻译、工具调用格式映射、系统提示词处理都是按规范严格实现的。广度是明确的v2议题。

### 请求路由智能

| 功能 | miroxy | CCR | cc-switch | LiteLLM |
|---|---|---|---|---|
| 按请求选模型 | 否（客户端自己选） | 是——按token数+场景 | 否 | 是——按权重/延迟/用量等策略 |
| 后台任务路由 | 否 | 是 | 否 | 否（无任务语义分类，只按可用性/权重选deployment） |
| 长上下文阈值路由 | 否 | 是（可配置token阈值） | 否 | 部分（`context_window_fallbacks`，按错误而非预判路由） |
| 推理任务路由 | 否 | 是 | 否 | 否 |
| 图片路由 | 否 | 是（图片agent） | 否 | 否 |
| 项目级配置覆盖 | 否 | 是（`~/.claude/projects/<id>`） | 部分 | 是（按key/team粒度） |
| Transformer/中间件管道 | 否 | 是（20+可链式transformer） | 否 | 是（callback系统，但偏日志/审计而非内容转换） |

CCR在这一项靠设计取胜——它的路由系统是核心差异化点：对客户端透明，自动给简单任务选便宜模型、给难任务选强模型。LiteLLM的"路由"更偏向**可靠性导向**（哪个deployment活着、哪个便宜、哪个延迟低），不是CCR那种**任务语义导向**（这是一个推理任务还是后台任务）。

### 流式处理

| 功能 | miroxy | CCR | cc-switch | LiteLLM |
|---|---|---|---|---|
| SSE流式 | 是 | 是 | 是 | 是 |
| 429时流式重试（提交header前） | 是 | 否 | 否 | 否 |
| 输出Anthropic SSE格式 | 是 | 是 | 是 | 是（若客户端走`/v1/messages`） |
| 流式中途格式转换 | 是 | 是（transformer） | 是 | 是 |
| 流式中途工具调用拦截 | 否 | 是（agent系统） | 否 | 否 |
| 首字节延迟追踪 | 否 | 否 | 是 | 是（Prometheus指标） |

### 可观测性与数据

| 功能 | miroxy | CCR | cc-switch | LiteLLM |
|---|---|---|---|---|
| 结构化请求日志 | 是（slog，JSON） | 是（pino，滚动日志） | 是（SQLite行记录） | 是（多种callback：OTEL/Langfuse等） |
| 用量/成本追踪 | 否 | 会话级LRU缓存 | 完整（看板+图表） | 完整（spend tracking，按key/team/tag查询） |
| 单key指标 | 否（Prometheus推迟实现） | 否 | 否 | 是（Prometheus原生支持） |
| 会话历史浏览器 | 否 | 否 | 是 | 否（有admin UI但非会话回放） |
| 云同步（WebDAV/S3） | 否 | 否 | 是 | 否 |

这一项LiteLLM的可观测性其实是四者里最成熟的——这也合理，毕竟它的商业模式部分建立在"企业要审计、要预算管控"这个需求上（参考之前讨论的LiteLLM Enterprise定价分层）。

### 开发与运维体验

| 功能 | miroxy | CCR | cc-switch | LiteLLM |
|---|---|---|---|---|
| 安装复杂度 | `go build` → 单一二进制 | `npm install -g` | 平台安装包 | `pip install` 或 Docker |
| Docker | 是（含Dockerfile） | 是（多阶段构建） | 否（桌面应用） | 是（官方镜像） |
| 配置变更方式 | 编辑YAML，重启 | 编辑JSON5，自动重载 | GUI，即时生效 | 编辑YAML，支持热加载部分配置 |
| 测试覆盖 | 是（单元+集成） | 无 | 是（vitest + cargo test） | 是，覆盖广泛 |
| Web UI | 否 | 是 | 是（原生应用） | 是（Admin UI，企业版更完整） |
| 预设/配置共享 | 否 | 是（市场） | 是（深链、预设） | 部分（社区配置示例，无市场机制） |
| MCP server管理 | 否 | 否 | 是 | 否 |

---

## 重叠关系图

```
          ┌─────────────────────────────────────────────────────┐
          │              坐在Claude Code与上游之间                  │
          │            暴露Anthropic兼容的HTTP端点                  │
          │                  处理SSE流式                          │
          │             让你能用非Anthropic的provider               │
          └───────────────────┬─────────────────────────────────┘
                              │
          ┌───────────────────┼───────────────────┬───────────────┐
          │                   │                   │               │
     ┌────▼────┐        ┌─────▼─────┐       ┌────▼────┐    ┌──────▼──────┐
     │ miroxy │        │    CCR    │       │cc-switch│    │  LiteLLM    │
     │         │        │           │       │         │    │             │
     │ Key池   │        │按token数  │       │桌面GUI  │    │多deployment │
     │ 配额    │        │路由       │       │7个应用  │    │负载均衡     │
     │ 倍增    │        │Transformer│       │会话历史 │    │企业治理     │
     │（针对   │        │管道       │       │用量看板 │    │（SSO/预算/  │
     │ 免费层  │        │预设市场   │       │         │    │ 审计）      │
     │ 精细化） │        │Agent系统  │       │         │    │100+provider │
     └─────────┘        └─────────┘        └─────────┘    └─────────────┘

  Headless，可服务器  ◄──────────────────────────►  桌面/GUI优先
  部署的轻量代理                                  Headless代理可选
                                                  （LiteLLM居中：
                                                   headless为主，
                                                   附带可选企业UI）
```

四者的功能重叠是真实存在的，但比看起来更窄。都站在Claude Code（或其他客户端）和上游之间，再往后目标分化得很明显——这一点加入LiteLLM后依然成立，只是LiteLLM的"分化方向"是**企业级通用网关**，跟CCR的"任务语义路由"、cc-switch的"桌面配置管理"、miroxy的"配额精细化"是四个完全不同的轴。

---

## 坦诚评估

### miroxy押的赌注

Gemini免费层给每个API key约15 RPM。Claude Code在密集会话时会突发5-10个请求/分钟。一个key意味着持续被限流。三个key配合智能轮转能给到约45 RPM的有效吞吐，实践中几乎不再被限流。这是miroxy存在的全部理由。最近几次会话里促使我们排查的那些429错误日志，就是这个问题真实存在的直接证据。

CCR和cc-switch都没解决这个问题。CCR假设一个key配额就够用。cc-switch的failover队列是顺序的——一个失败了切下一个provider，不是把同一个provider的多个key同时负载分流。

**LiteLLM确实有相近能力**（多deployment+多种路由策略），但没有针对"免费层配额边界"这个具体场景做精细化设计——它的冷却时长是固定值，不区分"过几秒就能恢复的限流"和"要等到明天配额重置的彻底耗尽"，也不解析provider返回的具体retryDelay数值。如果你愿意花时间把LiteLLM配置成"每个key一个deployment + usage-based-routing"，能凑合解决大部分问题，但锋利程度不及miroxy专门为这个场景打磨出的递增退避+权威retryDelay解析。

### 什么时候该用miroxy

- 你有2个以上Gemini API key（免费层或其他），想同时并发使用
- 你用Claude Code时经常碰到429
- 你想要一个能部署到任何地方、零运行时依赖的单一静态二进制
- 你需要正确、经过测试的Anthropic↔Gemini协议翻译
- 你是headless运行（VPS、容器、CI）

### 什么时候该用CCR

- 你只有一个provider的key，配额足够
- 你想让不同任务类型自动用不同模型，且不想改Claude Code配置
- 你今天就要OpenRouter、DeepSeek、Ollama或VertexAI支持
- 你想要预设市场和管理配置的Web UI
- 你的技术栈里已经在跑Node.js

### 什么时候该用cc-switch

- 你同时用多个工具（Codex、Gemini CLI、OpenCode等），想从一个地方统一管理
- 你想要GUI而不是编辑配置文件
- 你关心会话历史浏览、用量看板和成本图表
- 你想要跨工具的MCP server同步
- 你更喜欢原生桌面应用而不是后台HTTP守护进程

### 什么时候该用LiteLLM

- 你需要统一接入100+家provider，而不只是Gemini或几个OpenAI兼容provider
- 你是团队/企业场景，需要预算管控、SSO、RBAC、审计日志
- 你需要跨provider的容灾fallback（不只是同provider多key，是OpenAI挂了自动切Anthropic这种）
- 你愿意接受Python运行时和（分布式场景下）Redis这层额外依赖，换取生态成熟度和企业级功能
- 你不在乎"针对免费层配额的精细化退避"这个细分需求，只要"基本的负载均衡和failover"就够

---

## miroxy不该变成什么

看着CCR、cc-switch和LiteLLM，很容易动心想加：Web UI、20个provider支持、智能路由、会话历史、企业治理功能。**抵制所有这些冲动**。这三个替代品已经存在，而且做得很好。把这些功能加到miroxy上，只会做出一个用户基数更小、维护面更大的劣质第四克隆体。

miroxy能站得住的领地是**配额感知的key轮转**——这一点甚至连LiteLLM这种功能最全的网关都没有精细做到。CLAUDE.md里那些硬性约束（不做管理API、不上数据库、不引入第三方HTTP框架）之所以正确，正是因为它们防止miroxy滑向CCR、cc-switch或LiteLLM已经在做、而且做得更好的方向。

## miroxy值得填补的缺口

以下是CCR、cc-switch、LiteLLM都没有妥善做到，但又落在miroxy范围内的事情：

**1. 工具调用翻译（阻塞agentic用例）**
Anthropic的工具调用格式（`tool_use`内容块 → `tool_result`）需要完整的双向翻译到Gemini的function-calling格式。没有这个，Claude Code的agentic工作流（文件编辑、bash执行）跑不稳。目前标记为推迟，但这是主要的功能缺口。

**2. Prometheus指标**
按key维度的计数器：`requests_total`、`requests_in_flight`、`failures_total`、`rate_limit_cooldowns_total`、`cooldown_duration_seconds`。这是生产环境运维miroxy所需的可观测视图。CCR和cc-switch都没有暴露Prometheus兼容指标；LiteLLM有，但是是从"通用网关"视角设计的，不会单独给你"这个免费层key还剩多少配额窗口"这种针对性指标。

**3. `/v1/keys/status` 只读端点**
一个debug端点，展示每个key的当前状态：`{ id, state, cooldown_until, rl_failures, in_flight, requests_in_window }`。让运维人员不用翻结构化日志就能看到池子在干什么。

**4. 长冷却配额检测**
目前递增退避上限封顶在300秒。如果Gemini返回的`retryDelay`是`156h14m36s`（代表每日配额耗尽），这个key应该针对该模型整段熔断到配额窗口重置，而不是每5分钟徒劳重试一次。修复方案：解析出的`retryDelay`若大于1小时，就视为配额耗尽，套用`coolEnd = now + retryDelay`的整段冷却，而不是走递增退避的节奏。这一点**LiteLLM目前也没有**——它的冷却机制不解析具体的retryDelay数值，这是miroxy相对所有三个对比对象都独有的潜在优势点，值得作为差异化重点保留并优先完善。

---

## 总结表

| 问题 | 回答 |
|---|---|
| miroxy是CCR的重复品吗？ | 不是——CCR没有key池化；核心问题不同 |
| miroxy是cc-switch的重复品吗？ | 不是——cc-switch是GUI配置管理器；交付模式不同 |
| miroxy是LiteLLM的重复品吗？ | **部分重叠，但不算重复**——LiteLLM的多deployment负载均衡覆盖了"轮转多个key"这个大类，但没有针对"免费层配额精细化管理"做专门设计；这是两者真正的分界线 |
| 有重叠吗？ | 有——四者都在把Claude Code（或其他客户端）代理到非Anthropic的backend |
| 重叠是停止开发的理由吗？ | 不是——重叠是基线功能，miroxy的独特价值在基线之上 |
| miroxy该加CCR那样的路由吗？ | 不该——超出范围，CCR已经做了 |
| miroxy该加cc-switch那样的GUI吗？ | 不该——超出范围，cc-switch已经做了 |
| miroxy该加LiteLLM那样的多provider广度/企业治理吗？ | 不该——超出范围，且这是用完全不同的资源投入换取的护城河，跟miroxy"轻量单一二进制"的定位直接冲突 |
| miroxy该先完成什么？ | 工具调用翻译、Prometheus指标、配额耗尽检测（这一点尤其值得加速，因为它是miroxy相对LiteLLM唯一真正"独有"且别人没有动力去补的差异点） |

(EOF)
