package main

import (
	"os"

	"github.com/grafana/grafana-plugin-sdk-go/backend/app"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/yuuki/grafana-llm-studio-app/pkg/plugin"
)

func main() {
	if err := app.Manage("yuuki-llmstudio-app", plugin.NewApp, app.ManageOpts{}); err != nil {
		log.DefaultLogger.Error("failed to start yuuki-llmstudio-app backend", "err", err)
		os.Exit(1)
	}
}
