# 常见问题

## 安装和启动

**Q: 当前分支支持 Windows/macOS GUI 安装包吗？**

当前维护分支推荐使用 server/Docker + Web UI。仓库里仍保留 `cmd/desktop` 的 Wails 桌面源码，但不把 Windows/macOS GUI 安装包作为当前分支的推荐部署方式。

**Q: 端口被占用？**

server 模式可以通过 `CCNEXUS_PORT` 或启动参数调整端口；Docker 部署可以修改宿主机端口映射，例如 `127.0.0.1:3022:3000`。

## 端点配置

**Q: 如何选择转换器？**

- Claude 官方或兼容服务 → `claude`
- OpenAI 或兼容服务 → `openai`
- DeepSeek → `deepseek`
- Kimi/Moonshot → `kimi`
- Google Gemini → `gemini`

**Q: 为什么 OpenAI/Gemini/DeepSeek/Kimi 必须填模型？**

Claude Code 请求中包含 Claude 模型名，代理需要知道转换为哪个目标模型。

**Q: 端点测试成功但使用失败？**

检查：API 密钥权限、模型名称、API 配额。查看日志获取详细错误。

## 使用问题

**Q: Token 统计准确吗？**

估算值，基于文本长度计算，与实际计费可能有差异。

**Q: 如何备份配置？**

1. 使用 WebDAV 云同步
2. 手动复制 `~/.ccNexus/ccnexus.db`

**Q: 端点轮换顺序？**

按列表顺序，可拖拽调整。

**Q: 数据安全吗？**

所有数据存储在本地 `~/.ccNexus/`，API 密钥不会发送给第三方。
