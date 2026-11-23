# ADR-002: 認証・認可境界と Grafana OSS 単一 Organization MVP 範囲

- ステータス: 採用（2026-11-22）
- 対象バージョン: `Grafana OSS v12.4.15`

## コンテキスト
Grafana App Plugin の Backend (`CallResource`) にはブラウザの Session Cookie が転送されない。また Grafana OSS では Enterprise/Cloud のデータソース RBAC / LBAC が利用できない。

## 決定
1. **データ取得・Dry-Run・プレビュー・ダッシュボード保存**は、すべて Frontend からログインユーザーの Grafana セッションを用いて実行する。
2. **Go Backend の呼び出し元識別**は、Grafana が付与する信頼できる `backend.PluginConfigFromContext(ctx)` (`OrgID`, `User.Login`) のみに依拠し、リクエスト本文の `orgId`/`userId` は一切信用しない（`DisallowUnknownFields` で拒否）。すべてのジョブ操作で `(OrgID, UserLogin)` の所有者一致を強制する。
3. **`llmclient` 用サービスアカウントの分離**:
   - `plugin.json` の `iam.permissions` は `{"action":"plugins.app:access","scope":"plugins:grafana-llm-app"}` に限定する。
   - Grafana OSS で手動作成するサービスアカウントには最小基本ロール `Viewer` を割り当てる（`pkg/services/pluginsintegration/pluginaccesscontrol/accesscontrol.go` 確認済）。
   - OSS の `Viewer` は同一 Org の読み取り権限も含むため、コード上このトークンを `llmclient` 以外へ渡さないことを厳守する。
4. **メタデータとドラフトの隔離**:
   - メトリクス名・HELP・ラベルは保護対象とし、初期 MVP ではブラウザメモリおよび単一リクエスト内のみで扱い、共有永続インデックスへ登録しない。
   - Organization 変更・データソース変更・アンマウント時にブラウザ上のドラフトとメタデータを即座に破棄する。
5. **単一 Organization 前提の明示**:
   - 異なる閲覧権限を持つ利用者へのマルチテナント提供は本 MVP では非対応であり、本番対応には Organization 分離＋上流データソース側の資格情報/テナント/認可プロキシによる実データ隔離が必須である。
