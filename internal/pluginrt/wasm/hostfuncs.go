package wasm

// hostfuncs.go defines the host import functions exposed to guest WASM modules.
//
// When wazero is wired (see runtime.go TODO), register host functions here via
// wazero's HostModuleBuilder. Only expose safe, bounded operations to guests.
//
// Core redlines — these MUST NOT be callable from guest WASM:
//   - SSE stream read/write
//   - KeyPool state locking and in-flight accounting
//   - 429-transparent-retry timing
//   - Core Anthropic↔provider protocol translation
//   - Secret decryption
//
// Safe host functions (add as needed when a real WASM plugin is authored):
//   - miroxy.log(level, msg)       — structured log forwarding
//   - miroxy.clock()               — monotonic clock (no wall-clock leak)
//   - miroxy.http_out(req) resp    — outbound HTTP (content-filtered, no upstream creds)
//
// TODO(pluginrt): implement with wazero HostModuleBuilder once a real WASM plugin exists.
