# バージョン互換性表・推論モデル公式確認結果

確認日: **2026-11-22**

## 1. 採用スタック互換性表（固定バージョン）

本実装は **Grafana OSS `12.4.15`（React 18 系）** および **Grafana OSS `13.3.1`（React 19 系）** の 2 系統を検証・サポート対象バージョンとして固定する（`plugin.json` では `grafanaDependency: ">=12.4.0 <14.0.0"` を宣言し、v11 以前や未検証の v14 以降は対象外とする）。

| コンポーネント | 採用バージョン / タグ | 互換性・採用根拠 | 公式ソース URL | 確認状況 |
|---|---|---|---|---|
| **Grafana OSS** | `v13.3.1` (`grafana/grafana-oss:13.3.1`) / `v12.4.15` (`grafana/grafana-oss:12.4.15`) | 13.3 系最新パッチ (React 19, 2026-11-10) および 12.4 系最新パッチ (React 18, 2026-11-10)。両系統とも `sharedDependencies.ts` で `react/jsx-runtime`・`react/jsx-dev-runtime`・`@emotion/react`・`i18next`・`react-router` を公開し、`dashboard.grafana.app/v1beta1` を提供。 | https://github.com/grafana/grafana/releases/tag/v13.3.1 / https://github.com/grafana/grafana/releases/tag/v12.4.15 | 両タグのソースコード直接確認済 |
| **Node.js** | `22.23.5` (`>=22.0.0`) | Grafana plugin toolchain / npm 12 互換。 | https://grafana.com/developers/plugin-tools/ | ローカル実行・ビルド確認済 |
| **Go** | `1.24.3` | `grafana-plugin-sdk-go v0.278.0` (`go 1.24.1`), `llmclient v0.20.0` (`go 1.19`), `prometheus v0.305.0` (`go 1.23.0`) と完全整合。 | https://go.dev/ | ローカルビルド・`-race` テスト済 |
| **Grafana Plugin SDK (Go)** | `v0.278.0` | `backend/grpcplugin/handshake.go` の `ProtocolVersion = 2` は Grafana `v12.4.15` (`v0.287.0`)・`v13.3.1` (`v0.297.0`)・最新 `v0.302.0` と完全一致。`CallResource` / `CheckHealth` 互換。 | https://github.com/grafana/grafana-plugin-sdk-go/releases/tag/v0.278.0 | `v0.278.0` / `v0.297.0` ソース比較確認済 |
| **@grafana/runtime / data / ui / schema / i18n** | ビルド時 `12.4.15` (下限固定・実行時は Grafana 側共有インスタンスを使用) | Grafana 公式 `13.1.x -> 13.2.x` 移行ガイドに従い、12.4.x (React 18) と 13.2+ (React 19) の両対応を維持するため下限 API (`12.4.15`) に対してビルドし、`react` / `react-dom` / `react/jsx-runtime` / `react/jsx-dev-runtime` を外部化。使用中の `@grafana/ui` コンポーネント (`Alert`, `Spinner`, `SecretInput` 等) は `v13.3.1` ソースで互換確認済。 | https://grafana.com/developers/plugin-tools/migration-guides/update-from-grafana-versions/migrate-13_1_x-to-13_2_x | 型検査・Webpack・`@grafana/react-detect`・`v13.3.1` ソース確認済 |
| **@grafana/scenes** | `9.3.0` | `peerDependencies` が `@grafana/runtime >=12.4`, `react ^18.0.0 \|\| ^19.0.0`。同梱 `react-draggable@4.7.2` (`>=4.4.0`), `react-resizable@3.2.0` (`>=3.1.3`) は公式 React 19 ガイドの互換バージョン要件を満たす。公開 API のみ使用。 | https://github.com/grafana/scenes/releases/tag/v9.3.0 | インストール型定義・`@grafana/react-detect`・Jest 確認済 |
| **grafana-llm-app** | Grafana 13.x: **`v1.2.0`** / Grafana 12.4.x: **`v1.0.8`** | `v1.2.0` は `grafanaDependency: ">=13.0.0-0"`、`v1.0.8` は `">=9.5.2"`。両バージョンとも `/api/plugins/grafana-llm-app/resources/llm/v1/chat/completions`、2 分 HTTP タイムアウト、`go-openai` 経由中継を維持。 | https://github.com/grafana/grafana-llm-app/tree/v1.2.0 / https://github.com/grafana/grafana-llm-app/tree/v1.0.8 | 両タグのソースコード直接確認済 |
| **Go `llmclient`** | `llmclient/v0.20.0` (`go-openai v1.41.1`) | `llmclient.NewLLMProviderWithClient` で `/api/plugins/grafana-llm-app/resources/llm/v1` へ接続。`ResponseFormat (json_schema)`, `MaxCompletionTokens`, `ReasoningEffort` 対応。 | https://github.com/grafana/grafana-llm-app/tree/llmclient/v0.20.0/llmclient | ソース確認・契約テスト済 |
| **@grafana/llm** | **MVPでは非採用** | ブラウザ直接 LLM 呼び出し用パッケージ。本設計ではプロンプト構築・IR 検査・予算管理を Go Backend に集約するため使用しない。 | https://grafana.com/developers/plugin-tools/how-to-guides/app-plugins/use-llms-and-mcp | 役割分離確認済 |
| **Grafana Foundation SDK (TS)** | `@grafana/grafana-foundation-sdk@0.0.22` (`v0.0.22`) | Classic Dashboard JSON (`schemaVersion: 42`) を型安全に生成 (`DashboardBuilder`, `timeseries.PanelBuilder`, `stat.PanelBuilder`, `prometheus.DataqueryBuilder`)。 | https://github.com/grafana/grafana-foundation-sdk/tree/v0.0.22 | インストール型定義・パリティテスト済 |
| **Dashboard / Folder API** | `POST /apis/dashboard.grafana.app/v1beta1/namespaces/{namespace}/dashboards`、Folder 一覧は `/api/folders?permission=Edit`（404/410 時は `/apis/folder.grafana.app/v1beta1/namespaces/{namespace}/folders` へフォールバック） | Grafana `v13.3.1` では `dashboard.grafana.app/v1beta1` は `v1` と完全同一スキーマのエイリアスとして維持され、`folder.grafana.app/v1beta1` も提供される。データソース呼び出しはすべて UID 指定 (`/api/datasources/proxy/uid/{uid}/...`, `/api/ds/query`) のみ使用し、Grafana 13 で既定無効化された数値 ID API は使用しない。 | https://github.com/grafana/grafana/tree/v13.3.1/apps/dashboard/pkg/apis/dashboard/v1beta1 | `v12.4.15` / `v13.3.1` ソース・OpenAPI 確認済 |
| **Prometheus PromQL Parser** | `github.com/prometheus/prometheus v0.305.0` (`promql/parser`) | 公式 PromQL AST パーサによる構文・range/subquery/offset/join/regex 検査。 | https://prometheus.io/docs/prometheus/latest/querying/api/ | Go ビルド・単体テスト済 |
| **VictoriaMetrics API 差異** | Prometheus 互換 (`/api/v1/series`, `/labels`, `/label/<name>/values`, `/metadata`) | VM は `start..end` を性能のため UTC 日単位へ丸める挙動がある点と、`limit` が `-search.maxSeries` 以下の範囲で効く点を考慮。 | https://docs.victoriametrics.com/victoriametrics/url-examples/ | 公式ドキュメント確認済 |
| **vLLM** | `>= v0.12.0` (OpenAI 互換サーバー、例: `v0.32+` 系) | `guided_json` 等の旧フィールドは v0.12.0 で削除済。標準 OpenAI `response_format: {"type":"json_schema",...}` を XGrammar / Guidance バックエンドでサポート。 | https://docs.vllm.ai/en/latest/features/structured_outputs/ | 公式ドキュメント確認済（実 GPU 環境接続は未実行） |

> **未確認・実環境依存の明示**: 本開発環境には GPU ノードおよび稼働中の Grafana (`12.4.15` / `13.3.1`) + `grafana-llm-app` + vLLM クラスターが存在しないため、実 vLLM サーバーでの推論精度・レイテンシ・実 Grafana コンテナへの統合 E2E は未実行である。一方で、Grafana `v12.4.15` / `v13.3.1`・`grafana-llm-app v1.0.8` / `v1.2.0` の実ソースコード、公式 `@grafana/react-detect` スキャン、および契約テスト (`pkg/llm/contract_test.go`) により、ランタイム依存関係・React 19 互換・HTTP/JSON プロトコル制約を検証済みである。

---

## 2. 推論モデル候補の敵対的検証結果（2026-11-22 時点の公式情報）

当初想定されていた **「20B〜70B 級」という規模想定は、提示された 4 モデルすべてと大きく矛盾する**。4 モデルはいずれも超大規模 MoE（総パラメータ **320B 〜 2.8T**、必要重みメモリ **320 GB 〜 1,561 GB**）であり、単一〜2枚程度の GPU では動作しない（8×H100 / 8×H200 / 8×B300 等のマルチ GPU ノードが必要）。また、**オープンウェイト（Open-Weight）と完全 OSS（OSI 準拠・学習データ/レシピ公開）は同一ではない**。

| 候補モデル | 実在・公式リポジトリ | 総パラメータ / 活性パラメータ | 公開重み・ライセンス区分 | vLLM 対応・注意点 | 必要メモリ目安（KV キャッシュ除く重みのみ） | 20B〜70B想定との整合性 |
|---|---|---|---|---|---|---|
| **GLM-5.3** | 実在: `zai-org/GLM-5.3` | 約 **744B〜753B** 総パラメータ / **40B** 活性 (MoE) | **オープンウェイト（独自修正 MIT ライセンス）**: 年間売上 100 億 USD 超の MaaS 事業者の商用利用時に Z.AI 安全審査を要求する条項あり（純粋な OSI OSS ではない）。 | vLLM / SGLang 対応。`reasoning_effort` がデフォルト `max`、`clear_thinking` がデフォルト `false` のため、vLLM 起動設定およびリクエスト側での制御が必要。 | FP8 重み約 **744〜756 GB**（例: 8×H200 ノード級） | **不整合**（活性 40B だが総パラメータは 744B 級、VRAM は 750GB+ 必要） |
| **GLM-5.3-Flash** | 実在: `zai-org/GLM-5.3-Flash` | **320B** 総パラメータ / **18B** 活性 (ハイブリッド MoE) | **オープンウェイト（MIT License）**: 重み・コードは MIT（ただし学習データ非公開のため完全 OSS とは区別する）。 | vLLM 対応。`--chat-template-content-format string` 等の設定不備でマルチターン劣化のコミュニティ報告あり。H100 での FP8 KV cache 構成は事前検証必須。 | Native FP8 約 **320〜328 GB**（4-bit 量子化でも約 200 GB、8×H100/H200 級） | **不整合**（活性 18B だがロードには 320GB 級 VRAM 必要） |
| **Kimi-K 3.1 Pro** (公式表記: `Kimi-K3.1-Pro`) | 実在: `moonshotai/Kimi-K3.1-Pro` | 約 **2.8T** (3T 級) 総パラメータ（活性パラメータ数は公式カード未明示箇所あり・要個別確認） | **オープンウェイト（独自修正 MIT ライセンス）**: 商用大規模サービス（MAU 1 億超または月商 2,000 万 USD 超）に UI 表記義務・MaaS 条件あり（社内閉域利用は免除規定あり）。 | vLLM 対応（MXFP4 重み / MXFP8 活性）。`trust_remote_code=True`・専用 processor/encoding 依存に注意。 | 4-bit (MXFP4) でも約 **1,561 GB**（8×B300 / TP=8 で 1 ノード専有級） | **大幅に不整合**（2.8T 級、1.5TB 超の VRAM 必須） |
| **DeepSeek-V4.1-Flash-1017** | 実在: `deepseek-ai/DeepSeek-V4.1-Flash-1017` | 公式リポジトリで総/活性パラメータ詳細未確認（重みサイズ約 **510.3 GB** の超大規模 MoE + Engram 構成） | **オープンウェイト（MIT License）**: 重み・リポジトリは MIT（学習データは非公開）。 | **重要注意点**: Jinja 形式の `chat_template` を同梱せず、独自 `encoding.py` / `deepseek-recipe` を採用。採用する vLLM バージョンでのネイティブ対応と Structured Output (`response_format`) 動作の事前契約テストが必須。 | 約 **510.3 GB**（TP=4 で 4×B300 / 8×H100 等、Engram オフロード構成は要実測） | **不整合**（510GB 級 VRAM 必要） |

### 設計上の帰結
1. 閉域網で保有する GPU 資産が 20B〜70B 級（例: 1〜4×A100/H100 80GB）である場合、上記 4 モデルをそのまま配備することはできない。その場合は別途 20B〜70B 級の MIT/Apache-2.0 オープンウェイトモデルを選定するか、上記 4 モデルに合わせて 8×H200 / 8×B300 級の推論ノードを確保する必要がある。
2. 本アプリのコードは特定のモデル名・パラメータ規模に一切ハードコードせず、`grafana-llm-app` のモデル抽象化 (`base` / `large`) 経由で推論先を完全に差し替え可能としている。
3. 特に `GLM-5.3` / `GLM-5.3-Flash` の `reasoning_effort` 既定値 (`max`) は、思考トークン消費による `finish_reason="length"` 切れを招くリスクがあるため、App Config の `reasoningEffort`（既定 `low`）と `maxOutputTokens`（既定 `2048`）を設定可能とし、`finish_reason == "length"` を明示的エラー (`ErrOutputTruncated`) として検出する。

## 3. `grafana-llm-app` 契約テストで実証したプロトコル制約

`pkg/llm/contract_test.go` および `grafana-llm-app v1.0.8` ソース読解により以下を確認した：

1. **モデル指定**: `grafana-llm-app` はリクエストの `model` に `"base"` または `"large"` のみ受理し（任意モデル名は HTTP 400）、プラグイン設定の `ModelSettings.Mapping` で実 vLLM モデル名へ変換する。
2. **Structured Output (`response_format`) の透過性と制約**:
   - `response_format: { type: "json_schema", json_schema: { name, schema, strict: true } }` は `grafana-llm-app` を通過して vLLM へ到達する。
   - ただし `grafana-llm-app v1.0.8` は内部で `go-openai v1.41.2` の `jsonschema.Definition` へ一度 Unmarshal する。
   - この構造体は `Enum []string` のみ許容するため、**数値 `enum`（例: `[0, 1]`）を含めると HTTP 400 で拒否される**。本プロジェクトの正本スキーマはすべて文字列 `enum` のみを使用し、整数制約は `minimum`/`maximum` で記述した上で `scripts/verify-schema-sync.mjs` で自動検査する。
   - また `minimum`/`maximum`/`minLength`/`maxLength`/`pattern`/`minItems`/`maxItems` は `grafana-llm-app` 中継時に削ぎ落とされるため、**`type`/`properties`/`required`/`items`/`additionalProperties:false`/文字列 `enum` は vLLM 側制約生成で強制し、長さ・範囲・正規表現・PromQL 安全性は Go Backend の決定的バリデータで二重強制する**。
3. **`extra_body` / `chat_template_kwargs` の非透過性**:
   - `go-openai` 構造体に存在しない `structured_outputs` や `chat_template_kwargs` (`clear_thinking`) は `grafana-llm-app` により破棄される。`clear_thinking` 等の推論エンジン固有設定は vLLM サーバー起動オプション側（配備設定）で固定する。
4. **エラー・タイムアウト・キャンセル伝播**:
   - `grafana-llm-app v1.0.8` の `openai_provider.go` は固定 `2 * time.Minute` の HTTP クライアントタイムアウトを持つ。Go Backend 側でより短い `llmTimeoutSeconds` (既定 45s) と `jobDeadlineSeconds` (既定 90s) の `context.WithTimeout` を設定することで、リクエストキャンセルを Grafana -> `grafana-llm-app` -> vLLM へ伝播させる。
   - 通信障害リトライは一時的ネットワーク障害・HTTP 502/503/504 のみに限定し最大 1 回とし、意味的な自動構文修復（ジョブ全体で最大 2 回）とは完全に分離して入れ子増幅を防ぐ。
