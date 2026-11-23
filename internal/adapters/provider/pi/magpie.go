package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

// MagpieSelection reads the user's actual Pi selection in Magpie. It is not
// Pi's whole authenticated-provider registry and never reads credentials.
type MagpieSelection struct{ keys []string }

func ReadMagpieSelection(settingsPath string) (MagpieSelection, error) {
	if settingsPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return MagpieSelection{}, err
		}
		settingsPath = filepath.Join(home, ".config", "magpie", "settings.json")
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return MagpieSelection{}, &api.Error{Code: api.NotReady, Message: "无法读取本机 Magpie 配置，请在 Magpie 中为 Pi 选择模型后重新载入"}
	}
	var settings struct {
		PickedModels map[string][]string `json:"pickedModels"`
	}
	if json.Unmarshal(raw, &settings) != nil {
		return MagpieSelection{}, &api.Error{Code: api.NotReady, Message: "Magpie 模型配置无法读取，请检查配置后重新载入"}
	}
	selection := MagpieSelection{}
	seen := map[string]bool{}
	for _, id := range settings.PickedModels["pi"] {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := "magpie/" + id
		if !seen[key] {
			seen[key] = true
			selection.keys = append(selection.keys, key)
		}
	}
	return selection, nil
}

// Base matches complete configured identities first, then an optional Pi
// thinking suffix. Model IDs themselves may contain slashes and colons.
func (s MagpieSelection) Base(model string) (string, bool) {
	for _, key := range s.keys {
		if key == model {
			return key, true
		}
	}
	base := ""
	for _, key := range s.keys {
		if strings.HasPrefix(model, key+":") && len(model) > len(key)+1 && len(key) > len(base) {
			base = key
		}
	}
	return base, base != ""
}

// MagpieCatalog intersects the authoritative Magpie Pi selection with what Pi
// can currently execute. Other authenticated providers never enter this list.
func MagpieCatalog(ctx context.Context, executable, directory, selected, settingsPath string) (api.ModelCatalog, error) {
	selection, err := ReadMagpieSelection(settingsPath)
	if err != nil {
		return api.ModelCatalog{}, err
	}
	if selected != "" {
		base, ok := selection.Base(selected)
		if !ok {
			return api.ModelCatalog{}, &api.Error{Code: api.InvalidCommand, Message: "所选模型不在本机 Magpie 的 Pi 模型配置中"}
		}
		selected = base
	}
	if len(selection.keys) == 0 {
		return api.ModelCatalog{Models: []api.ModelInfo{}, ConfiguredDefault: ConfiguredModel(), Warnings: []string{"尚未在 Magpie 中为 Pi 选择模型，请选择并应用配置后重新载入。"}}, nil
	}
	return queryCatalog(ctx, executable, directory, selected, &selection)
}

func ResolveMagpieModel(settingsPath, model string) (string, error) {
	selection, err := ReadMagpieSelection(settingsPath)
	if err != nil {
		return "", err
	}
	base, ok := selection.Base(model)
	if !ok {
		return "", &api.Error{Code: api.InvalidCommand, Message: "当前模型不在本机 Magpie 为 Pi 选择的模型中，请从 Magpie 模型列表重新选择"}
	}
	return base, nil
}

func ValidateMagpieModel(settingsPath, model string) error {
	_, err := ResolveMagpieModel(settingsPath, model)
	return err
}

func (s MagpieSelection) filter(models []api.ModelInfo) []api.ModelInfo {
	available := map[string]api.ModelInfo{}
	for _, model := range models {
		if model.Provider == "magpie" {
			available[model.Key()] = model
		}
	}
	result := make([]api.ModelInfo, 0, len(s.keys))
	for _, key := range s.keys {
		if model, ok := available[key]; ok {
			result = append(result, model)
		}
	}
	return result
}
