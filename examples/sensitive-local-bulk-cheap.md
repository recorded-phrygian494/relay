# The "sensitive-local / bulk-cheap" recipe

Two aliases: anything sensitive never leaves the machine; bulk traffic goes
to the cheapest capable hosted tier with failover. Clients just pick a model
name.

```yaml
providers:
  ollama:
    type: ollama
  gemini:
    api_key: ["${GEMINI_API_KEY}"]
  groq:
    profile: groq
    api_key: ["${GROQ_API_KEY}"]

routing:
  aliases:
    # tokens never leave localhost — point agents with secrets here
    private: [ollama/gpt-oss:20b, ollama/mistral-nemo]

    # cheapest capable hosted model wins; the other is the fallback
    bulk:
      policy: cheapest
      candidates: [gemini/gemini-3.1-flash-lite, groq/llama-3.3-70b]

logging:
  log_prompts: off      # metadata only, even locally
```

Use it from any OpenAI SDK:

```python
client.chat.completions.create(model="private", messages=[...])   # local only
client.chat.completions.create(model="bulk", messages=[...])      # cheap hosted
```

Every decision logs its reason (`cheapest: gemini/gemini-3.1-flash-lite
$0.25/$1.50 per Mtok beats ...`), and `/dashboard` shows what each alias
actually cost. Add `routing.default: smart` on top if you also want unaliased
names difficulty-routed — see the smart-routing section of the README.
