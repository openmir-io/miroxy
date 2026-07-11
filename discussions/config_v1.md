miroxy v3 is a unified AI Traffic Governance Control Plane, designed to manage multi-provider LLM traffic across OpenAI, Gemini, DeepSeek, Anthropic, Grok and others.

It is inspired by Envoy, Kubernetes, and Service Mesh architecture.

The system is NOT a simple proxy or model router.

It is a declarative + programmable decision-and-execution architecture for AI traffic routing.

2. Core Principle (Absolute Rule)

There is exactly ONE authority for routing decisions:

Router Engine is the single source of truth for all routing decisions

Everything else is execution, adaptation, or fallback.

3. System Architecture

The system is split into two fundamental planes:

Control Plane (Decision Layer)
Router Engine (core decision system)
Optional inputs:
Config rules
WASM policy plugins
Sidecar ML router
Data Plane (Execution Layer)
Pipeline Engine
KeyPool management
Provider adapters
Streaming / response handling
Execution Flow
Request
   ↓
Router Engine (ONLY decision authority)
   ↓
RouteDecision
   ↓
Pipeline Engine (execution only)
   ↓
Cluster → Target → Provider → KeyPool
   ↓
LLM Provider API
4. Routing Model
4.1 RouteDecision (Single Output Contract)

Router Engine MUST output exactly one structured decision:

{
  "cluster": "premium",
  "target": "gpt-4",
  "provider": "openai",
  "reason": "user_tier=premium + low_latency",
  "fallback_chain": ["gemini-pro", "deepseek-v4"]
}

This is the ONLY truth consumed by execution layer.

4.2 Router Inputs

Router Engine may use:

request metadata (model alias, user context)
cost budget constraints
latency history
provider health status
KeyPool availability
optional plugin signals (WASM / sidecar ML)
4.3 Router Execution Sources

Router Engine can combine multiple policy sources:

Static config rules (deterministic baseline)
WASM policy plugins (sandboxed logic extensions)
Sidecar ML router (optional inference system)

However:

ALL sources MUST converge into ONE final RouteDecision.

No partial or layered routing is allowed downstream.

5. Pipeline Engine (Execution Only)

Pipeline Engine MUST NOT perform routing decisions.

It is strictly responsible for execution:

authentication
request normalization / rectification
caching
compression
security filtering
retry logic
KeyPool selection
upstream LLM request execution
streaming response handling

Pipeline consumes RouteDecision and executes it deterministically.

6. Resource Model
6.1 Providers

Represents LLM vendors:

openai
anthropic
gemini
deepseek
grok

Responsibilities:

authentication
API request execution
rate limiting
circuit breaking
key management
6.2 Targets

A Target = Provider + Model binding

targets:
  gpt4:
    provider: openai
    model: gpt-4

  gemini_pro:
    provider: gemini
    model: gemini-2.5-pro
6.3 Clusters

A Cluster = load-balanced group of Targets

clusters:
  premium:
    strategy: weighted
    targets:
      - target: gpt4
        weight: 50
      - target: gemini_pro
        weight: 50

Supported strategies:

round_robin
weighted
least_latency
cost_optimized
failover
7. KeyPool System

Each Provider owns a KeyPool:

multiple API keys per provider
per-key rate limiting
circuit breaker per key
health tracking
automatic key rotation

Key selection strategies:

least_requests
round_robin
health_aware
8. Configuration Model (STRICT SEPARATION)
8.1 Resource Definition ONLY

Configuration MUST define only static resources:

providers:
targets:
clusters:

NO routing logic is allowed here.

8.2 Router Configuration

Routing behavior is defined separately:

router:
  mode: hybrid

  engines:
    - type: config
      weight: 50
    - type: wasm
      weight: 30
    - type: sidecar
      weight: 20

  fallback: config
8.3 Critical Rule

Config MUST NOT:

directly decide routing outcomes
override Router Engine output
embed business logic for model selection

Config is only INPUT to Router Engine.

9. WASM + Sidecar Extensions
WASM Plugins
stateless routing policies
sandboxed execution
low latency
safe extensions
Sidecar ML Router
heavy inference routing logic
LLM-based or ML-based decision systems
executed over gRPC / Unix socket
strict timeout enforcement

Fallback order:

Sidecar ML Router
WASM Router
Config Router
Default identity routing
10. Execution Guarantees

The system MUST guarantee:

Single routing authority (Router Engine only)
Deterministic fallback chain
Separation of decision and execution
No routing logic inside Pipeline
Provider abstraction fully isolated
Multi-provider load balancing supported
Safe extensibility via WASM and sidecar
Zero modification to Pipeline when adding new routing logic
11. Design Philosophy

miroxy v3 is not:

a proxy
a model switcher
a config-driven router

It is:

A programmable AI traffic control plane (Envoy-style for LLMs)

12. Evolution Path
V1: config-based routing (simple mapping)
V2: introduction of Router Engine
V3: single source of truth routing + execution separation (current)
V4 (future):
CEL expression routing
cost-aware optimization
latency prediction routing
per-user adaptive routing memory
A/B shadow routing + traffic learning
13. Final System Definition

miroxy v3 enforces a strict architecture:

Routing decisions are centralized in a single Router Engine,
while execution is fully delegated to a deterministic Pipeline Engine.

All extensions (WASM, sidecar, config) are inputs to the Router Engine only.