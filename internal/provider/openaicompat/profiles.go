package openaicompat

// Profile is a preset for a known OpenAI-compatible hosted API: base URL,
// the conventional environment variable for its key, and quirks. Verified
// against provider docs 2026-07; recorded fixtures are the source of truth
// when the two disagree.
type Profile struct {
	BaseURL string
	EnvKey  string
	Quirks  Quirks
}

// Profiles maps profile name → preset. These are configuration, not code:
// adding a hosted OpenAI-compatible provider is one entry here.
var Profiles = map[string]Profile{
	"groq":       {BaseURL: "https://api.groq.com/openai/v1", EnvKey: "GROQ_API_KEY"},
	"together":   {BaseURL: "https://api.together.xyz/v1", EnvKey: "TOGETHER_API_KEY"},
	"fireworks":  {BaseURL: "https://api.fireworks.ai/inference/v1", EnvKey: "FIREWORKS_API_KEY"},
	"deepseek":   {BaseURL: "https://api.deepseek.com/v1", EnvKey: "DEEPSEEK_API_KEY"},
	"xai":        {BaseURL: "https://api.x.ai/v1", EnvKey: "XAI_API_KEY"},
	"mistral":    {BaseURL: "https://api.mistral.ai/v1", EnvKey: "MISTRAL_API_KEY"},
	"cerebras":   {BaseURL: "https://api.cerebras.ai/v1", EnvKey: "CEREBRAS_API_KEY"},
	"moonshot":   {BaseURL: "https://api.moonshot.ai/v1", EnvKey: "MOONSHOT_API_KEY"},
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", EnvKey: "OPENROUTER_API_KEY"},
	"qwen":       {BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", EnvKey: "DASHSCOPE_API_KEY"},
	"zhipu":      {BaseURL: "https://open.bigmodel.cn/api/paas/v4", EnvKey: "ZHIPU_API_KEY"},
	"minimax":    {BaseURL: "https://api.minimax.io/v1", EnvKey: "MINIMAX_API_KEY"},
}
