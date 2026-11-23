# ADR-004: 初期 MVP での非ストリーミング生成と SSE 延期の判断

- ステータス: 採用（2026-11-22）
- 対象バージョン: `Grafana OSS v12.4.15`, `grafana-plugin-sdk-go v0.278.0`

## コンテキスト
LLM 生成中の体験向上としてトークンストリーミングや SSE 進捗配信が検討されるが、`grafana-llm-app` のブラウザ向け配信、Go `llmclient` のストリーム、Custom Backend の `CallResource` 経由 SSE はそれぞれ別機構であり、途中の不完全な JSON を Scenes へ逐次適用すると破損や過剰クエリの原因になる。

## 決定
1. 初期 MVP では **非ストリーミング Structured Output 生成** を採用し、処理段階の状態管理（`generating` / `validating_ir` / `awaiting_preview` / `repairing` / `cancelled` / `failed`）と明示的な **キャンセル機能**（Frontend `unsubscribe` + Backend `POST /jobs/{id}/cancel` + `context.CancelFunc`）を優先実装する。
2. 生成完了・決定的検査合格後にのみドラフトを提示し、ユーザーが明示的に「検証・プレビューを実行」を選択してから Dry-Run と Scenes 描画を行う。
3. 将来 SSE による処理段階通知を導入する場合は、以下を実環境で事前確認する条件とする：
   - 採用版 Grafana + `grafana-plugin-sdk-go` の `CallResource` における `http.Flusher` / チャンク flush 挙動とクライアント切断検知
   - リバースプロキシ（Nginx / Envoy 等）の `proxy_buffering off`・HTTP/1.1・アイドルタイムアウト設定
   - トークンストリーミングとアプリ段階通知の明確な分離
