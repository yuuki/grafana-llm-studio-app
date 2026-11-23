# LLM Dashboard Studio (`yuuki-llmstudio-app`)

完全 OSS・閉域網（エアギャップ）環境で動作する Grafana App Plugin です。
Grafana 内で自然言語からメトリクス根拠付きのダッシュボード案（軽量 IR）を生成・部分修正し、`@grafana/scenes` でプレビューした上で、Grafana Foundation SDK 経由で新規ダッシュボード保存または JSON エクスポートできます。

## 設計原則と対象範囲（MVP）

- **対象バージョン固定**: **Grafana OSS `12.4.15`**（依存関係を含め固定。v11 以降の全バージョン動作は主張しません）
- **完全オフライン・閉域動作**: 実行時に Grafana Cloud、Grafana Assistant、外部 LLM API、外部 CDN を一切使用しません。
- **LLM 経路の一本化**: `Frontend -> Custom App Go Backend -> Grafana plugin resource API -> grafana-llm-app (v1.0.8) -> ローカル vLLM`
  - Custom App からローカル LLM への直結経路や汎用 LLM プロキシは持ちません。
  - Go Backend は公式 `github.com/grafana/grafana-llm-app/llmclient` (`v0.20.0`) を使用します。
  - 開発用 `llmMode=mock` は `allowMockInDev=true` の明示指定が必要であり、本番モード (`grafana-llm-app`) 失敗時に暗黙フォールバックしません。
- **権限・データ取得・保存の分離**:
  - メタデータ取得・Dry-Run 検証・Scenes プレビュー・新規保存はすべて Frontend からログインユーザーの Grafana セッションで実行します。
  - Go Backend は Grafana の信頼できる `PluginContext` (`OrgID`, `User.Login`) でジョブ所有者を隔離します。
  - `llmclient` 用サービスアカウントトークンは `secureJsonData` に保存し、LLM 中継以外のデータ取得・保存には流用しません。
- **MVP の制限とマルチテナント境界**:
  - 単一 Grafana Organization（同一データ閲覧権限の利用者）、管理者が許可した単一 Prometheus 互換データソース、`timeseries` / `stat` の 2 パネル種別、固定グリッド、自動 refresh 無効。
  - **重要**: Grafana OSS の同一 Organization 内では Folder 権限や UI 非表示だけでメトリクス閲覧制限はできません。異なるデータ閲覧範囲を持つ利用者へ本番提供する場合は、Organization 分離＋上流データソース側の資格情報/テナント/認可プロキシによる実データ隔離が必須です（詳細は [docs/architecture.md](docs/architecture.md)）。

## ドキュメント・ADR 一覧

- アーキテクチャ・認証経路・MVP 境界: [docs/architecture.md](docs/architecture.md)
- バージョン互換性表・推論モデル 4 候補の公式仕様検証: [docs/compatibility.md](docs/compatibility.md)
- 閉域網搬入・配備・Prometheus / VictoriaMetrics 上限設定手順: [docs/airgapped-deployment.md](docs/airgapped-deployment.md)
- ADR:
  - [ADR-001: LLM 接続一本化と Structured Output 制約](docs/adr/ADR-001-llm-routing-via-grafana-llm-app.md)
  - [ADR-002: 認証境界と Grafana OSS RBAC 範囲](docs/adr/ADR-002-auth-boundaries-and-oss-rbac.md)
  - [ADR-003: 軽量 IR・共通正規化層・Create-Only 保存 API](docs/adr/ADR-003-ir-and-dual-adapters.md)
  - [ADR-004: 非ストリーミング MVP と SSE 延期判断](docs/adr/ADR-004-non-streaming-mvp-and-sse-deferral.md)
- 正本 JSON Schema・サンプル:
  - [schemas/dashboard-ir.schema.json](schemas/dashboard-ir.schema.json)
  - [schemas/dashboard-patch.schema.json](schemas/dashboard-patch.schema.json)
  - [schemas/samples/valid-red-dashboard.json](schemas/samples/valid-red-dashboard.json)
  - [schemas/samples/invalid-dashboard.json](schemas/samples/invalid-dashboard.json)

## ローカルビルド・テスト

```bash
# 1. 依存インストール
npm ci
go mod download

# 2. スキーマ同期検査・型検査・単体/契約テスト・ビルド
make verify
```

個別実行:
```bash
npm run verify:schemas
npm run typecheck
npm run test:ci
npm run build
go test -race -v ./pkg/...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/gpx_yuuki_llmstudio_app_linux_amd64 ./pkg
```

## ローカル開発起動（Docker Compose + モック LLM + サンプルメトリクス）

```bash
make build
cp deploy/.env.example deploy/.env
docker compose -f deploy/docker-compose.yml up -d
```

1. Grafana (`http://localhost:3000`) にログイン（未署名プラグイン `yuuki-llmstudio-app` のみ明示許可済み）。
2. 左メニューから **LLM Dashboard Studio** (`/a/yuuki-llmstudio-app`) を開く。
3. データソース `Prometheus Main (prom-main)`、スコープ `service = checkout-api` でメトリクスを取得し、自然言語生成 → 検証・プレビュー → 部分修正 → 新規保存 / JSON エクスポートを確認する。

> 実 LLM (vLLM) へ接続する場合は、閉域搬入した `grafana-llm-app v1.0.8` をマウント・設定し、App Config で `llmMode` を `grafana-llm-app` へ切り替え、Viewer ロールのサービスアカウントトークンを `secureJsonData` に登録してください。

## 既知の制限

1. 単一 Backend プロセス内のメモリ上でジョブを管理するため、Grafana 再起動でドラフトジョブは失効します（マルチレプリカ間の継続処理は MVP 対象外）。
2. `grafana-llm-app v1.0.8` (`go-openai v1.41.2`) の仕様により、`response_format.json_schema.schema` の数値範囲・文字数制限・`pattern` や `extra_body` はプロキシ時に欠落します。構造・`additionalProperties:false`・文字列 `enum` は vLLM 制約生成で強制し、残余の境界・PromQL AST・カタログ照合は Go Backend の決定的検査で担保します。
3. 静的 PromQL 検査だけでは系列数やクエリ負荷を完全には予測できません。Prometheus / VictoriaMetrics 側のクエリタイムアウト・同時実行数・最大系列数上限の設定が必須です。
4. 実 GPU / vLLM ノードおよび実稼働 Grafana コンテナ上での統合 E2E は本開発環境では未実行です（Go/TS 単体テスト、Scenes/Foundation パリティテスト、`grafana-llm-app` 契約テストのみローカル検証済み）。
