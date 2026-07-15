// Package openaiprov is the first-party OpenAI provider: the generic
// openai-compat adapter pinned to api.openai.com. Azure OpenAI joins here
// in a later phase (same wire format, different URL and auth shape).
package openaiprov

import (
	"net/http"

	"github.com/llmrelay/relay/internal/provider/openaicompat"
)

// DefaultBaseURL is the OpenAI API root.
const DefaultBaseURL = "https://api.openai.com/v1"

// New builds the OpenAI provider. baseURL may be empty for the default.
func New(name, baseURL, apiKey string, httpClient *http.Client) *openaicompat.Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return openaicompat.New(openaicompat.Config{
		Name:    name,
		BaseURL: baseURL,
		APIKey:  apiKey,
	}, httpClient)
}
