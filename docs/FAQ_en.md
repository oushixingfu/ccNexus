# FAQ

## Installation and Startup

**Q: Does this branch support Windows/macOS GUI installer packages?**

The optimized branch is intended to run as server/Docker + Web UI. The Wails desktop source still exists under `cmd/desktop` for legacy development, but Windows/macOS GUI installer packages are not the recommended deployment path for this branch.

**Q: Port is in use?**

In server mode, set `CCNEXUS_PORT` or use the port flag. In Docker, change the host port mapping, for example `127.0.0.1:3022:3000`.

## Endpoint Configuration

**Q: How to choose a transformer?**

- Claude official or compatible services → `claude`
- OpenAI or compatible services → `openai`
- DeepSeek → `deepseek`
- Kimi/Moonshot → `kimi`
- Google Gemini → `gemini`

**Q: Why is the model field required for OpenAI/Gemini/DeepSeek/Kimi?**

Claude Code requests contain Claude model names. The proxy needs to know which target model to convert to.

**Q: Endpoint test succeeds but usage fails?**

Check: API key permissions, model name, API quota. View logs for detailed errors.

## Usage Issues

**Q: Is token statistics accurate?**

It's an estimate based on text length, may differ from actual billing.

**Q: How to backup configuration?**

1. Use WebDAV cloud sync
2. Manually copy `~/.ccNexus/ccnexus.db`

**Q: Endpoint rotation order?**

In list order, can be adjusted by drag and drop.

**Q: Is data secure?**

All data is stored locally in `~/.ccNexus/`, API keys are never sent to third parties.
