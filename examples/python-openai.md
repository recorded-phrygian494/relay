# Python — OpenAI SDK through relay

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:4000/v1",
    api_key="unused-on-loopback",  # or your RELAY_API_KEY if configured
)

# any provider/model relay knows, or an alias from your relay.yaml
resp = client.chat.completions.create(
    model="gemini/gemini-3.1-flash-lite",
    messages=[{"role": "user", "content": "three fun facts about otters"}],
)
print(resp.choices[0].message.content)

# streaming
for chunk in client.chat.completions.create(
    model="bulk",  # an alias — the SDK neither knows nor cares
    messages=[{"role": "user", "content": "count to 10"}],
    stream=True,
):
    print(chunk.choices[0].delta.content or "", end="")

# embeddings through the same gateway
vec = client.embeddings.create(model="ollama/nomic-embed-text", input="hello")
print(len(vec.data[0].embedding))
```
