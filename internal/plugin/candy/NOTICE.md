# Third-party material in this directory

`codex_context.json` (Codex CLI tools, base instructions and turn context with
paths replaced by `/home/user/workspace`) and the candy puzzle prompt/answer
check in `../suite_candy.go` are taken from **cpa-codex-candy-eval v0.3.15**
(https://github.com/haowang02/cpa-plugin-codex-candy-eval), MIT License,
Copyright (c) 2026 Hao Wang. The full license text is in
`LICENSE-cpa-codex-candy-eval`.

The suite does not depend on cpa-codex-candy-eval at runtime: it never reads
that plugin's state file or calls it, and works when it is uninstalled.
