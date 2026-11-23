package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

var validProviderIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var setupPresets = []api.PiSetupPreset{
	{
		ID:           "deepseek",
		Name:         "DeepSeek",
		ProviderID:   "deepseek",
		API:          "openai-completions",
		DefaultModel: "deepseek-flash",
		Description:  "官方 DeepSeek API Key，直接写入标准 ~/.pi/agent/auth.json",
	},
	{
		ID:           "openai",
		Name:         "OpenAI",
		ProviderID:   "openai",
		API:          "openai-responses",
		DefaultModel: "gpt-5.4",
		Description:  "OpenAI API Key（支持中转 Base URL 覆盖）",
	},
	{
		ID:           "anthropic",
		Name:         "Anthropic Claude",
		ProviderID:   "anthropic",
		API:          "anthropic-messages",
		DefaultModel: "claude-sonnet-4-6",
		Description:  "Anthropic API Key（支持中转 Base URL 覆盖）",
	},
	{
		ID:           "google",
		Name:         "Google Gemini",
		ProviderID:   "google",
		API:          "google-generative-ai",
		DefaultModel: "gemini-2.5-pro",
		Description:  "Gemini API Key（Google AI Studio）",
	},
	{
		ID:          "openrouter",
		Name:        "OpenRouter",
		ProviderID:  "openrouter",
		API:         "openai-completions",
		Description: "OpenRouter API Key，自动拉取可用模型列表",
	},
	{
		ID:           "moonshotai-cn",
		Name:         "Kimi (Moonshot CN)",
		ProviderID:   "moonshotai-cn",
		API:          "openai-completions",
		DefaultModel: "kimi-k3",
		Description:  "Moonshot 国内站 API Key",
	},
	{
		ID:              "custom",
		Name:            "自定义兼容接口",
		ProviderID:      "custom",
		API:             "openai-completions",
		RequiresBaseURL: true,
		Description:     "OpenAI / Anthropic 兼容网关（写入 ~/.pi/agent/models.json）",
	},
}

// SetupPresets returns built-in quick-start provider templates for first-run setup.
func SetupPresets() []api.PiSetupPreset {
	out := make([]api.PiSetupPreset, len(setupPresets))
	copy(out, setupPresets)
	return out
}

func findSetupPreset(id string) (api.PiSetupPreset, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range setupPresets {
		if p.ID == id {
			return p, true
		}
	}
	return api.PiSetupPreset{}, false
}

// HasPiConfiguration reports whether ~/.pi/agent (or env vars) already has
// usable provider credentials or custom models configured.
func HasPiConfiguration(agentDir string) bool {
	if strings.TrimSpace(agentDir) == "" {
		agentDir = piAgentDirectory()
	}
	if agentDir != "" {
		if raw, err := os.ReadFile(filepath.Join(agentDir, "auth.json")); err == nil {
			var auth map[string]any
			if json.Unmarshal(raw, &auth) == nil && len(auth) > 0 {
				return true
			}
		}
		if raw, err := os.ReadFile(filepath.Join(agentDir, "models.json")); err == nil {
			var doc struct {
				Providers map[string]struct {
					Models []any `json:"models"`
				} `json:"providers"`
			}
			if json.Unmarshal(raw, &doc) == nil {
				for _, p := range doc.Providers {
					if len(p.Models) > 0 {
						return true
					}
				}
			}
		}
	}
	for _, key := range []string{
		"OPENAI_API_KEY",
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"DEEPSEEK_API_KEY",
		"GEMINI_API_KEY",
		"OPENROUTER_API_KEY",
		"MOONSHOT_API_KEY",
		"XAI_API_KEY",
		"GROQ_API_KEY",
		"MISTRAL_API_KEY",
		"ZAI_API_KEY",
		"ZAI_CODING_CN_API_KEY",
		"KIMI_API_KEY",
		"MINIMAX_API_KEY",
		"MINIMAX_CN_API_KEY",
	} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// SaveProviderSetup non-destructively writes provider credentials, optional
// custom models/endpoints, and default model selection directly into the
// standard Pi agent directory (~/.pi/agent) without creating symlinks, then
// verifies the resulting catalog via Pi RPC.
func SaveProviderSetup(ctx context.Context, executable, agentDir string, req api.PiSetupRequest) (api.ModelCatalog, error) {
	if strings.TrimSpace(agentDir) == "" {
		agentDir = piAgentDirectory()
	}
	if strings.TrimSpace(agentDir) == "" {
		return api.ModelCatalog{}, errors.New("无法确定 Pi 配置目录（~/.pi/agent）")
	}

	preset, hasPreset := findSetupPreset(req.Preset)
	providerID := strings.ToLower(strings.TrimSpace(req.ProviderID))
	if providerID == "" && hasPreset {
		providerID = preset.ProviderID
	}
	if !validProviderIDPattern.MatchString(providerID) {
		return api.ModelCatalog{}, errors.New("Provider ID 只能包含小写字母、数字、点、下划线和短横线")
	}

	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		return api.ModelCatalog{}, errors.New("请输入 API Key")
	}

	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL != "" {
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || strings.TrimSpace(u.Host) == "" {
			return api.ModelCatalog{}, errors.New("Base URL 必须是以 http:// 或 https:// 开头的有效地址")
		}
		baseURL = strings.TrimRight(baseURL, "/")
	}

	isCustom := strings.EqualFold(strings.TrimSpace(req.Preset), "custom") || (hasPreset && preset.RequiresBaseURL)
	if isCustom && baseURL == "" {
		return api.ModelCatalog{}, errors.New("自定义接口需要填写 Base URL")
	}

	cleanModels := make([]string, 0, len(req.Models))
	seenModels := map[string]bool{}
	for _, raw := range req.Models {
		for _, part := range strings.Split(raw, ",") {
			m := strings.TrimSpace(part)
			if m == "" || seenModels[m] {
				continue
			}
			seenModels[m] = true
			cleanModels = append(cleanModels, m)
		}
	}
	if isCustom && len(cleanModels) == 0 {
		return api.ModelCatalog{}, errors.New("自定义接口至少需要填写一个模型 ID")
	}

	apiType := strings.TrimSpace(req.API)
	if apiType == "" && hasPreset {
		apiType = preset.API
	}
	if apiType == "" {
		apiType = "openai-completions"
	}

	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return api.ModelCatalog{}, fmt.Errorf("创建 Pi 配置目录失败：%w", err)
	}

	// 1. Non-destructively merge auth.json.
	authPath := filepath.Join(agentDir, "auth.json")
	authMap, err := readJSONMap(authPath)
	if err != nil {
		return api.ModelCatalog{}, fmt.Errorf("读取 auth.json 失败：%w", err)
	}
	authMap[providerID] = map[string]any{
		"type": "api_key",
		"key":  apiKey,
	}
	if err := writeJSONFile(authPath, authMap); err != nil {
		return api.ModelCatalog{}, fmt.Errorf("写入 auth.json 失败：%w", err)
	}

	// 2. Write models.json when configuring a custom endpoint, overriding baseUrl,
	// or explicitly declaring custom models. Built-in presets with default URLs
	// stay out of models.json so Pi retains its native model metadata & compat.
	if isCustom || baseURL != "" || len(cleanModels) > 0 {
		modelsPath := filepath.Join(agentDir, "models.json")
		modelsDoc, err := readJSONMap(modelsPath)
		if err != nil {
			return api.ModelCatalog{}, fmt.Errorf("读取 models.json 失败：%w", err)
		}
		if _, ok := modelsDoc["$schema"]; !ok {
			modelsDoc["$schema"] = "https://pi.dev/schemas/models.schema.json"
		}
		providers, ok := modelsDoc["providers"].(map[string]any)
		if !ok || providers == nil {
			providers = map[string]any{}
		}
		entry, ok := providers[providerID].(map[string]any)
		if !ok || entry == nil {
			entry = map[string]any{}
		}
		if baseURL != "" {
			entry["baseUrl"] = baseURL
		}
		if isCustom || (baseURL != "" && entry["api"] == nil) || len(cleanModels) > 0 {
			entry["api"] = apiType
		}
		if len(cleanModels) > 0 {
			existingList, _ := entry["models"].([]any)
			merged := make([]any, 0, len(existingList)+len(cleanModels))
			existingByID := map[string]bool{}
			for _, item := range existingList {
				if m, ok := item.(map[string]any); ok {
					if id, _ := m["id"].(string); id != "" {
						existingByID[id] = true
					}
				}
				merged = append(merged, item)
			}
			for _, id := range cleanModels {
				if existingByID[id] {
					continue
				}
				existingByID[id] = true
				merged = append(merged, map[string]any{
					"id":   id,
					"name": id,
				})
			}
			entry["models"] = merged
		}
		providers[providerID] = entry
		modelsDoc["providers"] = providers
		if err := writeJSONFile(modelsPath, modelsDoc); err != nil {
			return api.ModelCatalog{}, fmt.Errorf("写入 models.json 失败：%w", err)
		}
	}

	// 3. Determine target default model & non-destructively update settings.json.
	chosenDefaultModel := ""
	if len(cleanModels) > 0 {
		chosenDefaultModel = cleanModels[0]
	} else if hasPreset {
		chosenDefaultModel = preset.DefaultModel
	}
	chosenDefaultModel = strings.TrimPrefix(chosenDefaultModel, providerID+"/")

	settingsPath := filepath.Join(agentDir, "settings.json")
	settingsMap, err := readJSONMap(settingsPath)
	if err != nil {
		return api.ModelCatalog{}, fmt.Errorf("读取 settings.json 失败：%w", err)
	}
	existingProvider, _ := settingsMap["defaultProvider"].(string)
	if req.SetDefault || strings.TrimSpace(existingProvider) == "" {
		settingsMap["defaultProvider"] = providerID
		if chosenDefaultModel != "" {
			settingsMap["defaultModel"] = chosenDefaultModel
		} else {
			delete(settingsMap, "defaultModel")
		}
		if err := writeJSONFile(settingsPath, settingsMap); err != nil {
			return api.ModelCatalog{}, fmt.Errorf("写入 settings.json 失败：%w", err)
		}
	}

	// 4. Verify via real Pi RPC catalog query (never leak any credentials).
	catalog, err := Catalog(ctx, executable, "", "")
	if err != nil {
		return catalog, err
	}

	if (req.SetDefault || strings.TrimSpace(existingProvider) == "") && chosenDefaultModel == "" {
		for _, m := range catalog.Models {
			if m.Provider == providerID && strings.TrimSpace(m.ID) != "" {
				chosenDefaultModel = strings.TrimPrefix(m.ID, providerID+"/")
				settingsMap["defaultProvider"] = providerID
				settingsMap["defaultModel"] = chosenDefaultModel
				_ = writeJSONFile(settingsPath, settingsMap)
				catalog.ConfiguredDefault = providerID + "/" + chosenDefaultModel
				break
			}
		}
	}
	return catalog, nil
}

func readJSONMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
