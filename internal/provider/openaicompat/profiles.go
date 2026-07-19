package openaicompat

// Profile is a preset for a known OpenAI-compatible hosted API: base URL,
// the conventional environment variable for its key, onboarding pointers
// (where the key actually comes from, signup gotchas), and quirks.
// Verified against provider docs 2026-07; recorded fixtures are the
// source of truth when the two disagree.
type Profile struct {
	BaseURL string
	EnvKey  string
	// ConsoleURL is where a human gets an API key.
	ConsoleURL string
	// Notes are signup gotchas worth one line at init time.
	Notes  string
	Quirks Quirks
}

// Profiles maps profile name → preset. These are configuration, not code:
// adding a hosted OpenAI-compatible provider is one entry here — onboarding
// data lives here too, so the CLI never hardcodes it.
var Profiles = map[string]Profile{
	"groq": {BaseURL: "https://api.groq.com/openai/v1", EnvKey: "GROQ_API_KEY",
		ConsoleURL: "https://console.groq.com/keys",
		Notes:      "generous free tier; models are open-weight (Llama, etc.) served fast"},
	"together": {BaseURL: "https://api.together.xyz/v1", EnvKey: "TOGETHER_API_KEY",
		ConsoleURL: "https://api.together.ai/settings/api-keys"},
	"fireworks": {BaseURL: "https://api.fireworks.ai/inference/v1", EnvKey: "FIREWORKS_API_KEY",
		ConsoleURL: "https://app.fireworks.ai/settings/users/api-keys"},
	"deepseek": {BaseURL: "https://api.deepseek.com/v1", EnvKey: "DEEPSEEK_API_KEY",
		ConsoleURL: "https://platform.deepseek.com/api_keys",
		Notes:      "prepaid balance required before requests succeed"},
	"xai": {BaseURL: "https://api.x.ai/v1", EnvKey: "XAI_API_KEY",
		ConsoleURL: "https://console.x.ai"},
	"mistral": {BaseURL: "https://api.mistral.ai/v1", EnvKey: "MISTRAL_API_KEY",
		ConsoleURL: "https://console.mistral.ai/api-keys",
		Notes:      "free tier exists (La Plateforme 'experiment' plan) with phone verification"},
	"cerebras": {BaseURL: "https://api.cerebras.ai/v1", EnvKey: "CEREBRAS_API_KEY",
		ConsoleURL: "https://cloud.cerebras.ai"},
	"moonshot": {BaseURL: "https://api.moonshot.ai/v1", EnvKey: "MOONSHOT_API_KEY",
		ConsoleURL: "https://platform.moonshot.ai/console/api-keys",
		Notes:      "Kimi models; international console is moonshot.ai, mainland is moonshot.cn — keys are not interchangeable"},
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", EnvKey: "OPENROUTER_API_KEY",
		ConsoleURL: "https://openrouter.ai/settings/keys",
		Notes:      "an aggregator: one key fronts many providers, useful for comparing before committing"},
	"qwen": {BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", EnvKey: "DASHSCOPE_API_KEY",
		ConsoleURL: "https://dashscope.console.aliyun.com/apiKey",
		Notes:      "DashScope requires an Alibaba Cloud account; the international endpoint is dashscope-intl.aliyuncs.com (set base_url explicitly if your account is on it)"},
	"zhipu": {BaseURL: "https://open.bigmodel.cn/api/paas/v4", EnvKey: "ZHIPU_API_KEY",
		ConsoleURL: "https://open.bigmodel.cn/usercenter/apikeys",
		Notes:      "bigmodel.cn account flow is phone-number-first (mainland); GLM models"},
	"minimax": {BaseURL: "https://api.minimax.io/v1", EnvKey: "MINIMAX_API_KEY",
		ConsoleURL: "https://www.minimax.io/platform/user-center/basic-information",
		Notes:      "international platform is minimax.io, mainland is minimaxi.com — separate accounts"},
}
