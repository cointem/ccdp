package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"ccdp/internal/config"
)

// IDs are editable. originalID identifies the source record whose additional
// fields and model references must follow a rename.
type modelForm struct {
	ID, ContextWindow, MaxOutputTokens string
	originalID                         string
}

type providerForm struct {
	ID, BaseURL, WireAPI, APIKeyEnv, APIKey string
	Models                                  []*modelForm
	originalID                              string
}

type configFormValues struct {
	Model, FallbackModel, PermissionMode string
	Providers                            []*providerForm
}

type configForm struct {
	configFormValues
	defaultTarget, fallbackTarget *modelForm
	initial                       configFormValues
	file                          configFileSnapshot
	resolved                      config.Config
}

func loadConfigForm(path string) (*configForm, error) {
	file, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}
	// Decode only file-backed values. Environment overrides, including resolved
	// credentials, must never be copied into a user's configuration file.
	var fields struct {
		Model          string                           `json:"model"`
		FallbackModel  string                           `json:"fallback_model"`
		PermissionMode string                           `json:"permission_mode"`
		Providers      map[string]config.ProviderConfig `json:"providers"`
	}
	if err := json.Unmarshal([]byte(file.Text), &fields); err != nil {
		return nil, err
	}
	if fields.PermissionMode == "" {
		fields.PermissionMode = "auto"
	}
	values := configFormValues{Model: fields.Model, FallbackModel: fields.FallbackModel, PermissionMode: fields.PermissionMode}
	for _, id := range slices.Sorted(maps.Keys(fields.Providers)) {
		stored := fields.Providers[id]
		provider := &providerForm{originalID: id, ID: id, BaseURL: stored.BaseURL,
			WireAPI: config.WireAPINormalized(stored.WireAPI), APIKeyEnv: stored.APIKeyEnv, APIKey: stored.APIKey}
		for _, name := range slices.Sorted(maps.Keys(stored.ModelConfigs)) {
			limits := stored.ModelConfigs[name]
			model := &modelForm{originalID: name, ID: name}
			if limits.ContextWindow != 0 {
				model.ContextWindow = strconv.Itoa(limits.ContextWindow)
			}
			if limits.MaxOutputTokens != 0 {
				model.MaxOutputTokens = strconv.Itoa(limits.MaxOutputTokens)
			}
			provider.Models = append(provider.Models, model)
		}
		values.Providers = append(values.Providers, provider)
	}
	form := &configForm{configFormValues: values, initial: values.clone(), file: file}
	form.selectDefaultModel(values.Model)
	form.selectFallbackModel(values.FallbackModel)
	return form, nil
}

func (v configFormValues) clone() configFormValues {
	v.Providers = slices.Clone(v.Providers)
	for i, provider := range v.Providers {
		p := *provider
		p.Models = slices.Clone(p.Models)
		for j, model := range p.Models {
			m := *model
			p.Models[j] = &m
		}
		v.Providers[i] = &p
	}
	return v
}

func (f *configForm) dirty() bool { return !reflect.DeepEqual(f.configFormValues, f.initial) }

func (f *configForm) namedModels(yield func(string, *modelForm) bool) {
	for _, p := range f.Providers {
		if strings.TrimSpace(p.ID) == "" {
			continue
		}
		for _, m := range p.Models {
			if strings.TrimSpace(m.ID) != "" && !yield(p.ID+"/"+m.ID, m) {
				return
			}
		}
	}
}

func (f *configForm) modelOptions() []string {
	var options []string
	for name := range f.namedModels {
		options = append(options, name)
	}
	return options
}

func (f *configForm) findModel(selected string) *modelForm {
	for name, model := range f.namedModels {
		if name == selected {
			return model
		}
	}
	return nil
}

func (f *configForm) selectDefaultModel(name string) {
	f.Model, f.defaultTarget = name, f.findModel(name)
}

func (f *configForm) selectFallbackModel(name string) {
	f.FallbackModel, f.fallbackTarget = name, f.findModel(name)
}

// Track the model row while either of its editable IDs changes. The first
// remaining model is also the default shown by the picker after a deletion.
func (f *configForm) resolveModel(target *modelForm, stored string, useFirst bool) string {
	if target == nil && len(f.Providers) == 0 && len(f.initial.Providers) == 0 {
		return stored // Configurations using the built-in endpoint may name a model directly.
	}
	first := ""
	for name, model := range f.namedModels {
		if model == target {
			return name
		}
		if useFirst && first == "" {
			first = name
		}
	}
	return first
}

func (f *configForm) defaultModel() string {
	return f.resolveModel(f.defaultTarget, f.Model, true)
}

func (f *configForm) fallbackModel() string {
	return f.resolveModel(f.fallbackTarget, f.FallbackModel, false)
}

// Patch the stored document, retaining advanced settings, optional/null fields
// and exact numbers (including values beyond the precision of float64).
func (f *configForm) save(path string) error {
	if !f.dirty() {
		return f.publish(path, f.file.Text, f.Model, f.FallbackModel)
	}
	var doc map[string]any
	decoder := json.NewDecoder(strings.NewReader(f.file.Text))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return err
	}
	if doc == nil {
		doc = make(map[string]any)
	}
	if !reflect.DeepEqual(f.Providers, f.initial.Providers) {
		storedProviders, _ := doc["providers"].(map[string]any)
		providers := make(map[string]any, len(f.Providers))
		for _, p := range f.Providers {
			if strings.TrimSpace(p.ID) == "" || strings.Contains(p.ID, "/") {
				return errors.New("供应商标识不能为空或含有斜杠")
			}
			if _, ok := providers[p.ID]; ok {
				return fmt.Errorf("供应商标识重复：%s", p.ID)
			}
			record, err := p.record(storedProviders[p.originalID])
			if err != nil {
				return err
			}
			providers[p.ID] = record
		}
		doc["providers"] = providers
	}
	model, fallback := f.defaultModel(), f.fallbackModel()
	if model != f.initial.Model {
		setOptionalText(doc, "model", model)
	}
	if fallback != f.initial.FallbackModel {
		setOptionalText(doc, "fallback_model", fallback)
	}
	if f.PermissionMode != f.initial.PermissionMode {
		setOptionalText(doc, "permission_mode", f.PermissionMode)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return f.publish(path, string(data)+"\n", model, fallback)
}

func (f *configForm) publish(path, text, model, fallback string) error {
	resolved, err := saveConfigFile(path, f.file, text)
	if err != nil {
		return err
	}
	file, err := readConfigFile(path)
	if err != nil {
		return err
	}
	if file.Text != text {
		return errors.New("配置保存后又被修改，请重新打开设置")
	}
	f.file, f.resolved = file, resolved
	f.selectDefaultModel(model)
	f.selectFallbackModel(fallback)
	for _, p := range f.Providers {
		p.originalID = p.ID
		for _, m := range p.Models {
			m.originalID = m.ID
		}
	}
	f.initial = f.configFormValues.clone()
	return nil
}

func setOptionalText(record map[string]any, key, value string) {
	if value == "" {
		delete(record, key)
	} else {
		record[key] = value
	}
}

func (p *providerForm) record(original any) (map[string]any, error) {
	record, _ := original.(map[string]any)
	if record == nil {
		record = make(map[string]any)
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return nil, fmt.Errorf("请填写供应商 %s 的 API 地址", p.ID)
	}
	if p.APIKey != "" && p.APIKeyEnv != "" {
		return nil, fmt.Errorf("供应商 %s 只能选择一种密钥来源", p.ID)
	}
	storedWire, _ := record["wire_api"].(string)
	if storedWire == "" || p.WireAPI != config.WireAPINormalized(storedWire) {
		record["wire_api"] = p.WireAPI
	}
	for key, value := range map[string]string{"base_url": p.BaseURL, "api_key_env": p.APIKeyEnv, "api_key": p.APIKey} {
		if old, _ := record[key].(string); old != value {
			setOptionalText(record, key, value)
		}
	}
	if len(p.Models) == 0 {
		return nil, fmt.Errorf("请为供应商 %s 添加至少一个模型", p.ID)
	}
	storedModels, _ := record["models"].(map[string]any)
	models := make(map[string]any, len(p.Models))
	for _, model := range p.Models {
		if strings.TrimSpace(model.ID) == "" {
			return nil, fmt.Errorf("请填写供应商 %s 的模型标识", p.ID)
		}
		if _, ok := models[model.ID]; ok {
			return nil, fmt.Errorf("供应商 %s 的模型标识重复：%s", p.ID, model.ID)
		}
		old, exists := storedModels[model.originalID]
		fields, _ := old.(map[string]any)
		if fields == nil {
			fields = make(map[string]any)
		}
		for _, limit := range []struct{ key, label, value string }{{"context_window", "上下文窗口", model.ContextWindow}, {"max_output_tokens", "最大输出", model.MaxOutputTokens}} {
			stored, _ := fields[limit.key].(json.Number)
			value := strings.TrimSpace(limit.value)
			if value == stored.String() || value == "" && stored.String() == "0" {
				continue
			}
			if value == "" {
				delete(fields, limit.key)
				continue
			}
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("模型 %s 的%s必须是非负整数", model.ID, limit.label)
			}
			fields[limit.key] = n
		}
		if exists && old == nil && len(fields) == 0 {
			models[model.ID] = nil
		} else {
			models[model.ID] = fields
		}
	}
	record["models"] = models
	return record, nil
}
