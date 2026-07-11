package config

// Default file paths and ports.
// Docker/k8s users can set log.file or dump.path to "" to disable file output.
const (
	DefaultLogFile   = "./log/miroxy.log"
	DefaultDumpPath  = "./log/dump.jsonl"
	DefaultProxyPort = 9000
	DefaultAdminAddr = "127.0.0.1:9001"
)

// Default connection parameters for built-in providers.
// These constants are the single source of truth — change them here when a
// provider updates its endpoint, protocol, or auth scheme. Config files always
// take precedence; these are only used to fill in fields the operator left blank.

const (
	GeminiDefaultBaseURL   = "https://generativelanguage.googleapis.com"
	GeminiDefaultProtocol  = "gemini"
	GeminiDefaultAuthStyle = "query_key"

	OpenAIDefaultBaseURL   = "https://api.openai.com/v1"
	OpenAIDefaultProtocol  = "openai"
	OpenAIDefaultAuthStyle = "bearer"

	AnthropicDefaultBaseURL   = "https://api.anthropic.com"
	AnthropicDefaultProtocol  = "anthropic"
	AnthropicDefaultAuthStyle = "api_key"

	DeepSeekDefaultBaseURL   = "https://api.deepseek.com/v1"
	DeepSeekDefaultProtocol  = "deepseek"
	DeepSeekDefaultAuthStyle = "bearer"

	GLMDefaultBaseURL   = "https://api.z.ai/api/paas/v4"
	GLMDefaultProtocol  = "glm"
	GLMDefaultAuthStyle = "bearer"

	GrokDefaultBaseURL   = "https://api.x.ai/v1"
	GrokDefaultProtocol  = "grok"
	GrokDefaultAuthStyle = "bearer"
)

// builtinProviderDefaults maps provider names to their default connection
// parameters. Config values always override these; missing fields are filled
// from here. Providers not listed have no built-in defaults.
var builtinProviderDefaults = map[string]ProviderDef{
	"gemini":    {BaseURL: GeminiDefaultBaseURL, Protocol: GeminiDefaultProtocol, AuthStyle: GeminiDefaultAuthStyle},
	"openai":    {BaseURL: OpenAIDefaultBaseURL, Protocol: OpenAIDefaultProtocol, AuthStyle: OpenAIDefaultAuthStyle},
	"anthropic": {BaseURL: AnthropicDefaultBaseURL, Protocol: AnthropicDefaultProtocol, AuthStyle: AnthropicDefaultAuthStyle},
	"deepseek":  {BaseURL: DeepSeekDefaultBaseURL, Protocol: DeepSeekDefaultProtocol, AuthStyle: DeepSeekDefaultAuthStyle},
	"glm":       {BaseURL: GLMDefaultBaseURL, Protocol: GLMDefaultProtocol, AuthStyle: GLMDefaultAuthStyle},
	"grok":      {BaseURL: GrokDefaultBaseURL, Protocol: GrokDefaultProtocol, AuthStyle: GrokDefaultAuthStyle},
}

// fillProviderDefaults fills any empty fields on pd from the built-in defaults
// for providerName. If providerName has no built-in defaults, pd is unchanged.
func fillProviderDefaults(pd *ProviderDef, providerName string) {
	def, ok := builtinProviderDefaults[providerName]
	if !ok {
		return
	}
	if pd.BaseURL == "" {
		pd.BaseURL = def.BaseURL
	}
	if pd.Protocol == "" {
		pd.Protocol = def.Protocol
	}
	if pd.AuthStyle == "" {
		pd.AuthStyle = def.AuthStyle
	}
}
