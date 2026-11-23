# ADR-001: LLM 接続の `grafana-llm-app` 一本化と Structured Output 契約制約

- ステータス: 採用（2026-11-22）
- 対象バージョン: `grafana-llm-app v1.0.8`, `llmclient v0.20.0`, `go-openai v1.41.1 / v1.41.2`, `vLLM >= v0.12.0`

## コンテキスト
ローカル推論基盤 (vLLM) への接続先 URL、資格情報、モデル名マッピングをカスタム App 側へ分散させると、資格情報管理とガバナンスが二重化する。また、通信障害リトライと意味的な修復ループが入れ子になると推論負荷が増幅する。

## 決定
1. LLM 経路は **`Frontend -> Custom App Go Backend -> Grafana plugin resource API -> grafana-llm-app -> local vLLM`** に一本化し、App から vLLM への直結経路や汎用 LLM プロキシを追加しない。
2. Go Backend は公式 `github.com/grafana/grafana-llm-app/llmclient` (`v0.20.0`) を使用し、ブラウザ側の `@grafana/llm` は採用しない（プロンプト構築・IR 検査・予算管理を Backend 側で統制するため）。
3. 通信障害リトライ（一時的ネットワーク障害・HTTP 502/503/504 のみ最大 1 回）と、構文エラーの意味的修復（ジョブ全体で最大 2 回）を分離する。

## 実証された技術的制約と最小の対策（ソース確認・契約テストに基づく）
`grafana-llm-app v1.0.8` (`packages/grafana-llm-app/pkg/plugin/llm_provider.go`, `openai_provider.go`) のソースコードと `pkg/llm/contract_test.go` により、以下の具体的制約を確認した：

1. **数値 `enum` による HTTP 400**:
   - **再現条件**: `response_format.json_schema.schema` に数値型 `enum`（例: `"enum": [0, 1]`）を含めて `grafana-llm-app v1.0.8` へ送信すると、`go-openai/jsonschema.Definition.Enum` が `[]string` 型のため `json.Unmarshal` が失敗し HTTP 400 を返す。
   - **対策**: 正本 JSON Schema (`schemas/dashboard-ir.schema.json`, `schemas/dashboard-patch.schema.json`) で `enum` は文字列のみ使用し、整数境界は `minimum`/`maximum` で表現する。`scripts/verify-schema-sync.mjs` で継続検査する。
2. **中継時の制約キーワード欠落**:
   - **再現条件**: `go-openai/jsonschema.Definition` に定義されていない `minimum`/`maximum`/`minLength`/`maxLength`/`pattern`/`minItems`/`maxItems` や、OpenAI 非標準の `extra_body`/`chat_template_kwargs` は `grafana-llm-app` 中継時に破棄される。
   - **対策**: `type`/`properties`/`required`/`items`/`additionalProperties: false`/文字列 `enum`（`datasourceRef: ["selected_prometheus"]` を含む）は vLLM の Structured Output へそのまま届かせ、長さ・範囲・正規表現・PromQL AST・カタログ照合は Go Backend の決定的バリデータで必ず二重検証する。`clear_thinking` 等の推論エンジン固有設定は vLLM 配備設定側で固定する。
   - **将来の最小上流修正案**: `grafana-llm-app` のプロキシ層で `response_format.json_schema.schema` を `json.RawMessage` として透過転送するパッチを検討する（本制約のために vLLM 直結へ切り替えることはしない）。
3. **開発用モックの扱い**:
   - `llmMode=mock` は `allowMockInDev=true` を明示した開発・テスト環境専用とし、`llmMode=grafana-llm-app` の障害時に暗黙フォールバックしない。
