package desktop

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func apiCommandProvider(threadID, providerID, model string) api.Command {
	return api.Command{Version: api.ProtocolVersion, CommandID: newCommandID("model"), Kind: api.SetThreadModel, ThreadID: threadID, ProviderID: providerID, Model: model}
}

type modelSection struct {
	id, title string
	models    []api.ModelInfo
}

func splitExecutionModel(model api.ModelInfo) (string, string) {
	return "pi", model.Key()
}

func (w *workbench) providerModelInCatalog(providerID, model string) bool {
	if model == "" || !w.modelsLoaded || w.modelsErr != "" {
		return true
	}
	if providerID != "" && providerID != "pi" {
		return false
	}
	for _, m := range w.models {
		value := m.Key()
		if value == model || strings.HasPrefix(model, value+":") {
			return true
		}
	}
	return false
}

func (w *workbench) piModelCount() int {
	return len(w.models)
}

func (w *workbench) modelSections(query string) []modelSection {
	query = strings.ToLower(strings.TrimSpace(query))
	var matched []api.ModelInfo
	for _, model := range w.models {
		if query != "" && !strings.Contains(strings.ToLower(model.Key()+" "+model.Name+" "+model.Provider), query) {
			continue
		}
		matched = append(matched, model)
	}
	if len(matched) == 0 {
		return nil
	}
	return []modelSection{{id: "pi", title: "Pi", models: matched}}
}

func (w *workbench) providerMark(c *ui.Context, color ui.Color, size float32, _ string) {
	piMark(c, color, size)
}

func (w *workbench) chooseProviderModel(providerID, model string) {
	if providerID == "" {
		providerID = "pi"
	}
	if providerID == "pi" && w.piThread(w.selected) {
		w.chooseModel(model)
		return
	}
	if w.selected == "" {
		w.newSession()
	}
	if w.modelSaving || !w.providerModelInCatalog(providerID, model) {
		if !w.modelSaving {
			w.modelErr = modelOutsideCatalog
			w.widgets.modelPickerOpen = true
		}
		return
	}
	providerIDCopy := providerID
	modelCopy := model
	comp := w.composerFor(w.selected)
	if isLocalSession(w.selected) || !w.hasThread(w.selected) {
		comp.provider = &providerIDCopy
		comp.model = &modelCopy
		w.widgets.modelPickerOpen = false
		w.modelErr = ""
		w.saveState()
		w.widgets.composerFocus.Focus()
		return
	}
	threadID := w.selected
	command := apiCommandProvider(threadID, providerID, model)
	if prior := w.modelCommand; prior != nil && prior.ThreadID == threadID && prior.Model == model && prior.ProviderID == providerID {
		command = *prior
	}
	w.modelCommand = &command
	w.modelSaving, w.modelErr = true, ""
	w.dispatch(command, func(receipt api.Receipt, err error) {
		w.post(func() {
			w.modelSaving = false
			if err != nil || !receipt.Accepted {
				if w.selected == threadID {
					w.modelErr = w.tr3("模型切换未确认，可以重试", "Model switch was not confirmed. Try again.", "モデル変更が確認されませんでした。再試行してください。")
				}
				return
			}
			w.modelCommand = nil
			comp.provider = nil
			comp.model = nil
			if w.selected == threadID {
				w.widgets.modelPickerOpen = false
				w.widgets.composerFocus.Focus()
			}
			w.saveState()
		})
	})
}
