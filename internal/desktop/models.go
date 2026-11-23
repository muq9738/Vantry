package desktop

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

const modelOutsideCatalog = "当前模型不在所选提供方的可用目录中，请重新载入或选择其他模型。"

func (w *workbench) configuredDefaultModel() string {
	if catalog := w.features.catalog; catalog != nil && catalog.ConfiguredDefault != "" {
		return catalog.ConfiguredDefault
	}
	return w.defaultModel
}

func (w *workbench) modelDisplayName(model string) string {
	for _, item := range w.models {
		providerID, raw := splitExecutionModel(item)
		if (item.Key() == model || providerID == w.threadProvider(w.selected) && raw == model) && item.Name != "" {
			return item.Name
		}
	}
	return shortModel(model)
}

func (w *workbench) draftModel(threadID string) string {
	if comp := w.composers[threadID]; comp != nil && comp.model != nil {
		return *comp.model
	}
	if w.defaultModel != "" {
		return w.defaultModel
	}
	return w.defaults.Model
}

func (w *workbench) modelInCatalog(model string) bool {
	if model == "" || !w.modelsLoaded || w.modelsErr != "" || len(w.models) == 0 {
		return true
	}
	for _, candidate := range w.models {
		key := candidate.Key()
		if model == key || model == candidate.ID || strings.HasPrefix(model, key+":") {
			return true
		}
	}
	return false
}

func (w *workbench) modelReadyToSend(threadID string) bool {
	if !w.piThread(threadID) || w.providerModelInCatalog(w.threadProvider(threadID), w.currentModel()) {
		return true
	}
	w.composerFor(threadID).feedback = notice{Text: modelOutsideCatalog, Level: noticeWarn}
	w.modelErr = modelOutsideCatalog
	w.widgets.modelPickerOpen = true
	return false
}

func catalogFailure(action string, err error) string {
	var detail *api.Error
	if errors.As(err, &detail) {
		if detail.Code == api.VersionMismatch {
			text := describe(action, err)
			if detail.Message != "" && !strings.Contains(text, detail.Message) {
				text += "。" + detail.Message
			}
			return text
		}
		if detail.Code == api.NotReady || detail.Code == api.InvalidCommand {
			return action + "：" + detail.Message
		}
	}
	return describe(action, err)
}

// ensureModels uses the prompt-free query contract. In particular it does not
// need a ThreadID and must never create or inspect a local draft just to show
// the first-run model picker.
func (w *workbench) ensureModels(force bool) {
	if w.modelsLoading || w.modelsLoaded && !force {
		return
	}
	service, ok := w.service.(api.WorkbenchQueries)
	if !ok {
		w.modelsLoaded = true
		w.modelsErr = "本地服务版本与界面不兼容：缺少模型目录查询。"
		return
	}
	w.modelsLoading = true
	w.modelsErr = ""
	ctx := w.ctx
	w.start(func() {
		models, err := service.ListModels(ctx)
		w.post(func() {
			w.modelsLoading = false
			w.modelsLoaded = true
			if err != nil {
				w.modelsErr = catalogFailure("模型目录载入失败", err)
				return
			}
			w.models = append([]api.ModelInfo(nil), models...)
			sort.SliceStable(w.models, func(i, j int) bool {
				if w.models[i].Provider == w.models[j].Provider {
					return w.models[i].Key() < w.models[j].Key()
				}
				return w.models[i].Provider < w.models[j].Provider
			})
		})
	})
}

func (w *workbench) modelOptions(query string) []string {
	query = strings.ToLower(strings.TrimSpace(query))
	options := []string{""}
	seen := map[string]bool{"": true}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			options = append(options, id)
		}
	}
	add(w.currentModel())
	for _, model := range w.models {
		if query != "" {
			haystack := strings.ToLower(model.Key() + " " + model.Name + " " + model.Provider)
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		add(model.Key())
	}
	return options
}

func (w *workbench) modelWorkspace() string {
	var spec *api.WorkspaceSpec
	if thread, ok := w.selectedThread(); ok {
		spec = thread.Workspace
	} else {
		spec = w.defaults.Workspace
	}
	if spec == nil {
		return ""
	}
	if spec.Path != "" {
		return spec.Path
	}
	return spec.Repository
}

// Resolve only against actual catalog keys: IDs may contain '/' or ':'. A
// complete key wins before considering a thinking-level suffix.
func (w *workbench) modelSelection() (base, level string) {
	value := w.currentModel()
	catalog := w.features.catalog
	if catalog == nil {
		return value, ""
	}
	if value == "" {
		return catalog.Default, ""
	}
	for _, model := range catalog.Models {
		if value == model.Key() {
			return value, ""
		}
	}
	for _, model := range catalog.Models {
		if strings.HasPrefix(value, model.Key()+":") && len(model.Key()) > len(base) {
			base, level = model.Key(), strings.TrimPrefix(value, model.Key()+":")
		}
	}
	if base == "" {
		return value, ""
	}
	return base, level
}

// modelEffortLabel keeps the selected reasoning level visible without
// requiring the user to reopen the catalog. An empty model still resolves
// through the catalog default, so changing effort never invents a model key.
func (w *workbench) modelEffortLabel() string {
	base, level := w.modelSelection()
	if level != "" {
		return level
	}
	if base != "" && base == w.features.thinkingModel && len(w.features.thinkingLevels) > 0 {
		return "默认"
	}
	return ""
}

func (w *workbench) modelCatalogNotices() []notice {
	catalog := w.features.catalog
	if catalog == nil {
		return nil
	}
	notes := make([]notice, 0, len(catalog.Warnings)+1)
	for _, warning := range catalog.Warnings {
		if strings.TrimSpace(warning) != "" {
			notes = append(notes, notice{Text: warning, Level: noticeWarn})
		}
	}
	if catalog.ConfiguredDefault != "" && catalog.ConfiguredDefault != catalog.Default {
		actual := catalog.Default
		if actual == "" {
			actual = "未选择"
		}
		notes = append(notes, notice{
			Text:  "已配置的默认模型为 " + catalog.ConfiguredDefault + "；当前可用默认模型为 " + actual + "。请确认本次使用的模型。",
			Level: noticeWarn,
		})
	}
	return notes
}

func (w *workbench) invalidateModels() {
	w.features.catalogRequest++
	w.features.thinkingRequest++
	w.features.catalog, w.features.thinkingLevels = nil, nil
	w.features.catalogLoading, w.features.thinkingLoading = false, false
	w.features.thinkingModel = ""
	w.features.modelFeedback = notice{}
}

// loadModels obtains optional rich catalog metadata (warnings and thinking
// levels). The authoritative selectable list remains WorkbenchQueries.models.
func (w *workbench) loadModels() {
	if _, ok := w.service.(api.WorkbenchService); !ok {
		w.features.modelFeedback = notice{Text: "当前服务不提供模型详情；仍可从目录选择模型。", Level: noticeWarn}
		return
	}
	w.features.catalogRequest++
	request, path := w.features.catalogRequest, w.modelWorkspace()
	w.features.catalogLoading = true
	w.features.modelFeedback = notice{}
	w.workbenchCall(fmt.Sprintf("models:%d", request), api.WorkbenchRequest{Action: "models", Workspace: path}, func(response api.WorkbenchResponse, err error) {
		if request != w.features.catalogRequest || path != w.modelWorkspace() {
			return
		}
		w.features.catalogLoading = false
		if err != nil || response.Catalog == nil {
			w.features.modelFeedback = notice{Text: describe("读取模型详情失败，请重试", err), Level: levelFor(err)}
			return
		}
		w.features.catalog, w.features.catalogPath = response.Catalog, path
		base, _ := w.modelSelection()
		if base == response.Catalog.Default {
			w.features.thinkingRequest++
			w.features.thinkingLoading = false
			w.acceptThinking(base, response.Catalog.ThinkingLevels)
		} else {
			w.loadThinking()
		}
	})
}

// selectModel retains the older metadata-driven test/helper path. The native
// picker calls chooseModel, which also commits SetThreadModel for real threads.
func (w *workbench) selectModel(model api.ModelInfo) {
	w.chooseModel(model.Key())
}

func (w *workbench) loadThinking() {
	if _, ok := w.service.(api.WorkbenchService); !ok {
		return
	}
	w.features.thinkingRequest++
	request, path := w.features.thinkingRequest, w.modelWorkspace()
	base, _ := w.modelSelection()
	w.features.thinkingModel, w.features.thinkingLevels = "", nil
	w.features.modelFeedback = notice{}
	w.features.thinkingLoading = true
	w.workbenchCall(fmt.Sprintf("thinking:%d", request), api.WorkbenchRequest{Action: "models", Workspace: path, Model: base}, func(response api.WorkbenchResponse, err error) {
		current, _ := w.modelSelection()
		if request != w.features.thinkingRequest || current != base || path != w.modelWorkspace() {
			return
		}
		w.features.thinkingLoading = false
		if err != nil || response.Catalog == nil {
			w.features.modelFeedback = notice{Text: describe("读取所选模型的思考等级失败", err), Level: levelFor(err)}
			return
		}
		w.acceptThinking(base, response.Catalog.ThinkingLevels)
	})
}

func (w *workbench) acceptThinking(base string, levels []string) {
	w.features.thinkingModel = base
	w.features.thinkingLevels = append([]string(nil), levels...)
	_, level := w.modelSelection()
	w.features.modelFeedback = notice{}
	if level != "" && !slices.Contains(levels, level) {
		w.features.modelFeedback = notice{Text: "此模型不支持当前思考等级，请重新选择。", Level: noticeWarn}
	}
}

func (w *workbench) setThinking(level string) {
	base, _ := w.modelSelection()
	if base == "" || base != w.features.thinkingModel || w.features.thinkingLoading {
		return
	}
	if level != "" && !slices.Contains(w.features.thinkingLevels, level) {
		return
	}
	model := base
	if level != "" {
		model += ":" + level
	}
	if isLocalSession(w.selected) || !w.hasThread(w.selected) {
		w.defaults.Model = model
		w.composerFor(w.selected).model = &model
		w.features.modelFeedback = notice{}
		w.saveState()
		return
	}
	w.commitModel(model, false)
}
