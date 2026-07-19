# TypeScript — OpenAI SDK through relay

```ts
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:4000/v1",
  apiKey: "unused-on-loopback", // or your RELAY_API_KEY if configured
});

const resp = await client.chat.completions.create({
  model: "anthropic/claude-haiku-4-5", // OpenAI SDK, Anthropic model — relay translates
  messages: [{ role: "user", content: "haiku about gateways" }],
});
console.log(resp.choices[0].message.content);

// streaming
const stream = await client.chat.completions.create({
  model: "bulk", // alias from relay.yaml
  messages: [{ role: "user", content: "count to 10" }],
  stream: true,
});
for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");
}
```
