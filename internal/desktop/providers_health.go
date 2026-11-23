package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/pkg/api"
)

type piHealth struct {
	OK         bool
	Version    string
	Detail     string
	ModelCount int
}

func (w *workbench) applyPiRuntimeStatus(status *api.PiRuntimeStatus) {
	if status == nil {
		return
	}
	w.providers.status = status
	w.providers.loaded = true
	w.providers.customPiPath = status.CustomPath
	w.providers.customRegistry = status.NPMRegistry
	if status.DefaultModel != "" && w.defaultModel == "" {
		w.defaultModel = status.DefaultModel
	}
	ok := status.ActiveSource != piprovider.RuntimeSourceMissing && status.ActivePath != "" && status.ModelCount > 0
	detail := ""
	switch status.ActiveSource {
	case piprovider.RuntimeSourceMissing:
		if status.NPMAvailable {
			detail = w.tr3("未检测到外置 Pi CLI，可在检查兼容版本后通过 npm 一键安装", "External Pi CLI not found; install via npm after checking compatibility", "外部 Pi CLI が見つかりません。npm で導入できます")
		} else {
			detail = w.tr3("未检测到外置 Pi CLI，请先安装 Node.js (>=22) 与 @earendil-works/pi-coding-agent", "External Pi CLI not found; install Node.js (>=22) and @earendil-works/pi-coding-agent", "外部 Pi CLI が見つかりません")
		}
	default:
		ver := nonEmptyString(status.ActiveVersion, "CLI")
		if status.ModelCount > 0 {
			detail = fmt.Sprintf(
				w.tr3("外置 Pi %s · %d 个模型就绪", "External Pi %s · %d models ready", "外部 Pi %s · %d モデル準備完了"),
				ver,
				status.ModelCount,
			)
		} else {
			detail = fmt.Sprintf(
				w.tr3("外置 Pi %s 已就绪 · 尚未配置模型或 Key", "External Pi %s ready · No models or API keys configured yet", "外部 Pi %s 準備完了 · モデル未設定"),
				ver,
			)
		}
	}
	w.providers.health = &piHealth{
		OK:         ok,
		Version:    status.ActiveVersion,
		Detail:     detail,
		ModelCount: status.ModelCount,
	}
}

func (w *workbench) loadPiStatus() {
	if w.defaults.RemotePeer || w.providers.busy {
		w.providers.loaded = true
		return
	}
	if _, ok := w.service.(api.WorkbenchService); !ok {
		w.providers.loaded = true
		return
	}
	w.providers.busy = true
	w.workbenchCall("pi-status", api.WorkbenchRequest{Action: "pi.status"}, func(response api.WorkbenchResponse, err error) {
		w.providers.busy = false
		w.providers.loaded = true
		if err != nil {
			w.providers.err = describe(w.tr("读取 Pi 状态失败", "Failed to read Pi runtime status"), err)
			return
		}
		if response.PiRuntime != nil {
			w.providers.err = ""
			w.applyPiRuntimeStatus(response.PiRuntime)
		}
	})
}

func (w *workbench) checkPiUpdate() {
	if w.defaults.RemotePeer || w.providers.checking || w.providers.upgrading {
		return
	}
	w.providers.checking = true
	w.providers.err = ""
	w.providers.output = ""
	w.workbenchCall("pi-check-update", api.WorkbenchRequest{Action: "pi.check_update"}, func(response api.WorkbenchResponse, err error) {
		w.providers.checking = false
		if response.PiRuntime != nil {
			w.applyPiRuntimeStatus(response.PiRuntime)
		}
		if err != nil {
			w.providers.err = describeUpdateError(w.tr("检查 Pi 更新失败", "Checking Pi update failed"), err)
			return
		}
		if st := response.PiRuntime; st != nil && !st.UpdateAvailable && !st.CompatBlocked && !st.CanInstall {
			w.providers.output = w.tr("外置 Pi 已是最新兼容版本", "External Pi is up to date and verified")
		}
	})
}

func (w *workbench) upgradePi(install bool) {
	if w.defaults.RemotePeer || w.providers.upgrading {
		return
	}
	payload, _ := json.Marshal(api.PiUpgradeInput{Install: install})
	w.providers.upgrading = true
	w.providers.err = ""
	w.providers.output = ""
	w.workbenchCall("pi-upgrade", api.WorkbenchRequest{Action: "pi.upgrade", Input: payload}, func(response api.WorkbenchResponse, err error) {
		w.providers.upgrading = false
		if response.PiRuntime != nil {
			w.applyPiRuntimeStatus(response.PiRuntime)
		}
		if err != nil {
			w.providers.err = describeUpdateError(w.tr("更新外置 Pi 失败", "Updating external Pi failed"), err)
			return
		}
		w.providers.output = nonEmptyString(strings.TrimSpace(response.Output), w.tr("外置 Pi 已完成更新与兼容验证。", "External Pi updated and verified."))
		w.invalidateModels()
		w.ensureModels(true)
		if _, ok := w.service.(api.WorkbenchService); ok {
			w.loadModels()
		}
	})
}

func (w *workbench) savePiRuntimeSettings(input api.PiRuntimeSettingsInput) {
	if w.defaults.RemotePeer || w.providers.saving {
		return
	}
	payload, _ := json.Marshal(input)
	w.providers.saving = true
	w.providers.err = ""
	w.providers.output = ""
	w.workbenchCall("pi-runtime-settings", api.WorkbenchRequest{Action: "pi.settings.set", Input: payload}, func(response api.WorkbenchResponse, err error) {
		w.providers.saving = false
		if err != nil {
			w.providers.err = describeUpdateError(w.tr("保存 Pi 设置失败", "Saving Pi runtime settings failed"), err)
			return
		}
		if response.PiRuntime != nil {
			w.applyPiRuntimeStatus(response.PiRuntime)
		}
		w.providers.output = w.tr("外置 Pi 运行时与镜像设置已更新。", "External Pi runtime and mirror settings updated.")
		w.invalidateModels()
		w.ensureModels(true)
		if _, ok := w.service.(api.WorkbenchService); ok {
			w.loadModels()
		}
	})
}

func (w *workbench) probePi() {
	if w.providers.busy {
		return
	}
	if _, ok := w.service.(api.WorkbenchService); ok && !w.defaults.RemotePeer {
		w.providers.busy = true
		w.providers.err = ""
		w.workbenchCall("pi-probe", api.WorkbenchRequest{Action: "pi.status"}, func(response api.WorkbenchResponse, err error) {
			w.providers.busy = false
			w.providers.loaded = true
			if err != nil {
				w.providers.err = describe(w.tr("检测 Pi 失败", "Failed to probe Pi"), err)
				return
			}
			if response.PiRuntime != nil {
				w.applyPiRuntimeStatus(response.PiRuntime)
			}
		})
		return
	}
	w.providers.busy = true
	w.start(func() {
		ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
		defer cancel()
		health := &piHealth{}
		version, err := piprovider.Version(ctx, "")
		health.Version = version
		count := 0
		if err == nil {
			if service, ok := w.service.(api.WorkbenchQueries); ok {
				models, queryErr := service.ListModels(ctx)
				err = queryErr
				count = len(models)
			}
		}
		health.ModelCount = count
		health.OK = err == nil && count > 0
		if health.OK {
			health.Detail = fmt.Sprintf(
				w.tr3("外置 Pi %s · %d 个模型就绪", "External Pi %s · %d models ready", "外部 Pi %s · %d モデル準備完了"),
				nonEmptyString(health.Version, "CLI"),
				count,
			)
		} else {
			health.Detail = w.tr3("请检查外置 Pi 安装与模型配置", "Check external Pi installation and model configuration", "外部 Pi のインストールとモデル設定を確認してください")
		}
		w.post(func() {
			w.providers.health = health
			w.providers.busy = false
		})
	})
}
