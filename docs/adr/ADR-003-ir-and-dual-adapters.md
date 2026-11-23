# ADR-003: 軽量 IR・共通正規化層・Create-Only 保存 API の採用

- ステータス: 採用（2026-11-22）
- 対象バージョン: `Grafana OSS v12.4.15 / v13.3.1`, `@grafana/scenes v9.3.0`, `@grafana/grafana-foundation-sdk v0.0.22`

## コンテキスト
LLM に生の Grafana Dashboard JSON を直接生成させると、スキーマ逸脱・任意データソース UID 指定・HTML/JS 混入・Scenes プレビューと保存結果の乖離が発生しやすい。また旧 `/api/dashboards/db` API は、`id: null` であっても `uid` が既存ダッシュボードと一致し `version` が偶発的に一致すると `overwrite: false` でも上書きが発生し得る（`grafana v12.4.15` `pkg/services/dashboards/service/dashboard_service.go` L839-862 確認済）。さらに Grafana 13 では旧 `/api` エンドポイントが非推奨化されている。

## 決定
1. **正本 JSON Schema (`schemas/dashboard-ir.schema.json`, `schemas/dashboard-patch.schema.json`)**:
   - LLM は軽量な IR / パッチのみを出力し、データソース参照は論理固定値 `"selected_prometheus"` のみ許可する。
   - Go (`pkg/ir`) と TypeScript (`src/ir`) の契約同期は `scripts/verify-schema-sync.mjs` で自動検証する。
2. **共通正規化層 (`NormalizedDashboardSpec`) + 薄い 2 アダプター**:
   - レイアウト (`gridPos`)・単位・閾値・データソース参照・query target (`refId: "A"`) を `src/spec/normalize.ts` で 1 回だけ計算する。
   - `src/spec/toScene.ts`（`@grafana/scenes` 公開 API のみ）と `src/spec/toFoundation.ts`（`@grafana/grafana-foundation-sdk`）は薄い変換器に徹し、`src/spec/parity.test.ts` で一致を検証する。
3. **Create-Only 保存 API (`v1beta1`) の採用**:
   - Grafana `v12.4.15` および `v13.3.1` の両方で実在確認済みの `POST /apis/dashboard.grafana.app/v1beta1/namespaces/{namespace}/dashboards` を使用する（`v13.3.1` の `apps/dashboard/pkg/apis/dashboard/v1beta1/register.go` では `v1beta1` が `v1` の完全同一スキーマエイリアスとして提供されている）。
   - フォルダ一覧取得は書き込み権限フィルタ (`permission=Edit`) のため `/api/folders` を優先しつつ、旧 `/api` が無効化された環境 (`404`/`410`) では `/apis/folder.grafana.app/v1beta1/namespaces/{namespace}/folders` へフォールバックする。
   - Dashboard UID はアプリ側で毎回新規生成し、LLM には決めさせない。`409 Conflict` 時に `PUT` や上書きへ自動切替せず、保存前に `spec.revision === previewedRevision` と二重クリック防止ロックを強制する。
   - 保存・エクスポート JSON に会話履歴・資格情報・不要ラベル・実データを一切埋め込まない。
