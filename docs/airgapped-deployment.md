# 閉域網（エアギャップ）搬入・配備・運用手順

本手順は、実行時にインターネット接続・外部 CDN・Grafana Cloud・外部 LLM API を一切使用せずに `yuuki-llmstudio-app` を閉域網へ配備するための標準手順である。

## 1. 事前準備（インターネット接続のあるステージング環境での収集）

### 1.1 コンテナイメージの固定・保存
タグではなく可能な限りダイジェスト (`@sha256:...`) を記録した上で `docker save` で tarball 化する。

```bash
# 例: バージョン固定イメージの取得とエクスポート
docker pull grafana/grafana-oss:12.4.15
docker pull prom/prometheus:v3.5.0
# vLLM イメージは選定モデル対応バージョンを明示固定する（例: vllm/vllm-openai:<pinned-tag>）

docker save \
  grafana/grafana-oss:12.4.15 \
  prom/prometheus:v3.5.0 \
  -o airgap-images.tar
sha256sum airgap-images.tar > airgap-images.tar.sha256
```

### 1.2 プラグイン資材と依存パッケージのオフラインバンドル
1. **Custom App Plugin (`yuuki-llmstudio-app`)**:
   ```bash
   # 依存のオフラインキャッシュ作成（閉域内で再ビルドする場合）
   go mod vendor
   npm ci --prefer-offline

   # ビルド済み成果物（dist/）の作成
   npm run verify:schemas
   npm run build
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -o dist/gpx_yuuki_llmstudio_app_linux_amd64 ./pkg
   tar -czf yuuki-llmstudio-app-0.1.0.tar.gz -C dist .
   sha256sum yuuki-llmstudio-app-0.1.0.tar.gz > yuuki-llmstudio-app-0.1.0.tar.gz.sha256
   ```
2. **公式 `grafana-llm-app` (`v1.0.8`)**:
   - Grafana Plugin Catalog または GitHub Release (`v1.0.8`) の署名済み zip アーカイブをダウンロードし、SHA-256 を検証・記録して搬入する。
   - **実行時の `GF_INSTALL_PLUGINS` による自動ダウンロードは閉域では使用しない。**

### 1.3 LLM モデルと Tokenizer の Revision 固定搬入
- Hugging Face の特定コミット SHA（`main` ブランチ名ではなく 40 桁コミットハッシュ）を指定して重み・`config.json`・`tokenizer.json`・`tokenizer_config.json`・chat template / encoding ファイルを一式ダウンロードする。
- 全 `.safetensors` および設定ファイルの SHA-256 チェックサムマニフェストを生成し、閉域側で照合してから vLLM の読み取り専用ボリュームへマウントする。
- vLLM 起動時はローカルディレクトリパスのみを指定し、`HF_HUB_OFFLINE=1` および `TRANSFORMERS_OFFLINE=1` を必ず設定する。

## 2. 閉域網での配備設定

### 2.1 未署名 Custom App Plugin の許可（対象 ID 明示）
`grafana.ini` または環境変数で、**対象プラグイン ID のみを明示許可**する（ワイルドカードは使わない）。

```ini
[plugins]
allow_loading_unsigned_plugins = yuuki-llmstudio-app
```

環境変数の場合:
```bash
GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS=yuuki-llmstudio-app
```

外部通信・テレメトリも無効化する:
```ini
[analytics]
reporting_enabled = false
check_for_updates = false
check_for_plugin_updates = false
```

### 2.2 `grafana-llm-app` のローカル vLLM 接続設定
- Provider: `Custom OpenAI-compatible` (`provider: "custom"`)
- URL: 閉域内 vLLM エンドポイント（例: `http://vllm.internal:8000`、API path `/v1`）
- Model Mapping:
  - `base` -> vLLM の `--served-model-name`（例: `glm-5.3-flash-local`）
  - `large` -> 高精度モデル名（単一モデル運用の場合は同一名）
- **モデル固有の vLLM 起動設定例**:
  - `HF_HUB_OFFLINE=1`, `TRANSFORMERS_OFFLINE=1`
  - `--served-model-name <fixed-name>`
  - `--max-num-seqs <env-limit>`（推論サーバー全体の同時実行保護）
  - Structured Outputs バックエンド明示（例: `--structured-outputs-config.backend auto`）
  - GLM-5.3 系のように chat template 既定値調整が必要な場合は、vLLM 側の `--default-chat-template-kwargs` 等で `clear_thinking` を配備設定として固定する（`grafana-llm-app` が `extra_body` を中継しないため）。

### 2.3 LLM 専用サービスアカウント権限（Grafana OSS 12.4.15）
1. Grafana OSS において `yuuki-llmstudio-app` の App Config に設定するサービスアカウントを作成し、基本ロールに **`Viewer`** を割り当てる（OSS で `plugins.app:access` を含む最小基本ロール。`Editor` / `Admin` は絶対に付与しない）。
2. トークンは `secureJsonData` (`llmServiceAccountToken`) に保存し、環境変数ファイルや Git リポジトリに平文でコミットしない。
3. **LLM 基盤全体の利用制限**: アプリ内予算制御（`maxConcurrentJobs`, `maxRepairAttempts`, `maxOutputTokens`）は本プラグイン経由の制御に留まる。同一 `grafana-llm-app` を他プラグインや別経路から利用する場合、リバースプロキシ（Nginx / Envoy）または vLLM 側で IP/Token ごとのレート制限・同時実行数制限・タイムアウトを併用する。

## 3. Prometheus / VictoriaMetrics 上流クエリ上限（DoS 防御の最終境界）

ブラウザ側の制限（直近 5m・`maxDataPoints=10`・並列 2・10s タイムアウト）は**操作ミス防止であり、DoS 防御の最終境界ではない**。必ずデータソースまたは既存の入口側で以下を設定する。

### Prometheus の起動フラグ例
```text
--query.timeout=15s
--query.max-concurrency=10
--query.max-samples=10000000
```

### VictoriaMetrics (`vmsingle` / `vmselect`) の起動フラグ例
```text
-search.maxQueryDuration=15s
-search.maxConcurrentRequests=10
-search.maxSeries=30000
-search.maxSamplesPerQuery=10000000
-search.maxUniqueTimeseries=30000
```

### リバースプロキシ（Grafana 前段・vLLM 前段）のタイムアウト設定
- 非ストリーミング生成では、LLM 生成完了まで最大 `llmTimeoutSeconds` (既定 45s) 程度待機するため、Grafana 前段リバースプロキシの `proxy_read_timeout` は `jobDeadlineSeconds`（既定 90s）以上に設定する。
- 将来 SSE ストリーミングを導入する場合は、`proxy_buffering off;` および `proxy_http_version 1.1;` を設定し、`CallResource` の flush・切断検知を実環境で事前検証する。
