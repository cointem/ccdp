package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"ccdp/internal/config"
	"ccdp/internal/fsops"
	"ccdp/internal/mcp"
)

type mcpEditor struct {
	name, transport, command, args, endpoint, extra, key string
	network, advanced, adding, preset, userHasExa        bool
	path, projectRoot, relative                          string
	file                                                 configFileSnapshot
	version                                              fsops.Version
}

func readMCPEditor(userPath, workspace string, entry projectExtension, restore bool) (*mcpEditor, error) {
	e := &mcpEditor{name: entry.name, path: userPath, adding: entry.name == "" || restore, preset: entry.source == config.SourceDefault || restore}
	var err error
	switch entry.source {
	case config.SourceProject, config.SourceProjectLocal:
		e.path, e.projectRoot = entry.path, workspace
		e.relative, err = filepath.Rel(workspace, entry.path)
		if err != nil || (filepath.ToSlash(e.relative) != ".ccdp/settings.json" && filepath.ToSlash(e.relative) != ".ccdp/settings.local.json") {
			return nil, errors.New("无法定位项目 MCP 配置")
		}
		root, err := os.OpenRoot(workspace)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		data, version, err := fsops.ReadAt(root, e.relative, 16<<20)
		if err != nil {
			return nil, err
		}
		e.file.Text, e.version = string(data), version
	case "", config.SourceDefault, config.SourceUserFile:
		e.file, err = readConfigFile(userPath)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("此配置来源不支持编辑")
	}
	if e.projectRoot == "" {
		cfg, err := config.ParseFile(userPath, []byte(e.file.Text))
		if err != nil {
			return nil, err
		}
		current, installed := cfg.MCPServers[mcp.ExaServer]
		e.userHasExa = installed
		if e.preset && (restore && installed || !restore && !current.DefaultInstall) {
			return nil, errors.New("服务配置已变化，请刷新 MCP 列表")
		}
	}
	var doc struct {
		Servers map[string]json.RawMessage `json:"mcp_servers"`
		Key     string                     `json:"exa_api_key"`
	}
	if err := json.Unmarshal([]byte(e.file.Text), &doc); err != nil {
		return nil, err
	}
	record := make(map[string]json.RawMessage)
	raw, explicit := doc.Servers[e.name]
	switch {
	case e.preset:
		cfg := mcp.DefaultExaServer()
		e.transport, e.endpoint, e.network = cfg.Transport, cfg.BaseURL, cfg.NetworkAuthorized
	case !e.adding:
		if !explicit || string(raw) == "null" {
			return nil, errors.New("该服务配置已移除，请刷新 MCP 列表")
		}
		var cfg mcp.ServerConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		e.transport, e.command, e.endpoint, e.network = cfg.Transport, cfg.Command, cfg.BaseURL, cfg.NetworkAuthorized
		if v := record["args"]; v != nil && string(v) != "null" {
			e.args = string(v)
		}
		for _, field := range []string{"transport", "command", "args", "base_url", "network_authorized"} {
			delete(record, field)
		}
	}
	if e.transport == "" {
		e.transport = "stdio"
	}
	if len(record) > 0 {
		b, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return nil, err
		}
		e.extra = string(b)
	}
	if e.projectRoot == "" && e.name == mcp.ExaServer {
		e.key = doc.Key
	}
	return e, nil
}

func (e *mcpEditor) hostedExa() bool {
	return e.name == mcp.ExaServer && (mcp.ServerConfig{Transport: e.transport, BaseURL: strings.TrimSpace(e.endpoint)}).IsHostedExa()
}

func (e *mcpEditor) record() (json.RawMessage, error) {
	fields := map[string]any{}
	if strings.TrimSpace(e.extra) != "" {
		decoder := json.NewDecoder(strings.NewReader(e.extra))
		decoder.UseNumber()
		if !json.Valid([]byte(e.extra)) || decoder.Decode(&fields) != nil || fields == nil {
			return nil, errors.New("高级配置必须是 JSON 对象")
		}
	}
	for _, field := range []string{"command", "args", "base_url", "transport", "network_authorized"} {
		if _, ok := fields[field]; ok {
			return nil, fmt.Errorf("请在连接表单中设置 %s", field)
		}
	}
	switch e.transport {
	case "stdio":
		if strings.TrimSpace(e.command) == "" && fields["disabled"] != true {
			return nil, errors.New("请填写启动命令")
		}
		fields["command"] = strings.TrimSpace(e.command)
		if strings.TrimSpace(e.args) != "" {
			var args []string
			if err := json.Unmarshal([]byte(e.args), &args); err != nil || args == nil {
				return nil, errors.New("启动参数必须是 JSON 字符串数组")
			}
			fields["args"] = args
		}
	case "http", "sse":
		endpoint := strings.TrimSpace(e.endpoint)
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("请填写有效的 HTTP 或 HTTPS 服务地址")
		}
		if !e.network && fields["disabled"] != true {
			return nil, errors.New("请勾选「允许连接此远程服务」后再启用 MCP")
		}
		fields["transport"], fields["base_url"], fields["network_authorized"] = e.transport, endpoint, e.network
	default:
		return nil, errors.New("未知的 MCP 连接方式")
	}
	if e.preset && e.hostedExa() {
		// Keep the offline tool manifest when customizing the shipped connection.
		if _, ok := fields["on_demand_tools"]; !ok {
			fields["on_demand_tools"] = mcp.DefaultExaServer().OnDemandTools
		}
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var cfg mcp.ServerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return b, nil
}

// Patch only the selected registration in its owning settings file.
func (e *mcpEditor) save(remove bool) (config.Config, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(e.file.Text), &doc); err != nil {
		return config.Config{}, err
	}
	if doc == nil {
		return config.Config{}, errors.New("配置必须是 JSON 对象")
	}
	servers := map[string]json.RawMessage{}
	table, hasTable := doc["mcp_servers"]
	if hasTable && string(table) != "null" {
		if err := json.Unmarshal(table, &servers); err != nil {
			return config.Config{}, err
		}
	}
	name := e.name
	if e.adding {
		name = strings.TrimSpace(name)
	}
	if e.adding && name == "" {
		return config.Config{}, errors.New("请填写 MCP 名称")
	}
	user := e.projectRoot == ""
	keepExa := e.userHasExa
	switch {
	case remove:
		if user && name == mcp.ExaServer {
			servers[name] = json.RawMessage("null")
			delete(doc, "exa_api_key")
		} else {
			delete(servers, name)
		}
	case e.preset && e.hostedExa() && e.network && strings.TrimSpace(e.extra) == "":
		delete(servers, name) // Reuse the shipped manifest, including on later upgrades.
		keepExa = true
	default:
		if existing := servers[name]; e.adding && (existing != nil && string(existing) != "null" || e.userHasExa && name == mcp.ExaServer) {
			return config.Config{}, errors.New("此名称的 MCP 已安装")
		}
		record, err := e.record()
		if err != nil {
			return config.Config{}, err
		}
		servers[name] = record
	}
	if user && name == mcp.ExaServer && e.hostedExa() && !remove {
		if key := strings.TrimSpace(e.key); key != "" {
			doc["exa_api_key"], _ = json.Marshal(key)
		} else {
			delete(doc, "exa_api_key")
		}
	}
	if _, hasExa := servers[mcp.ExaServer]; user && !hasExa && !keepExa {
		// Adding another service must not re-enable a removed default.
		servers[mcp.ExaServer] = json.RawMessage("null")
	}
	if user && len(servers) == 0 && keepExa {
		delete(doc, "mcp_servers")
	} else {
		doc["mcp_servers"], _ = json.Marshal(servers)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return config.Config{}, err
	}
	data = append(data, '\n')
	if user {
		return saveConfigFile(e.path, e.file, string(data))
	}
	root, err := os.OpenRoot(e.projectRoot)
	if err != nil {
		return config.Config{}, err
	}
	defer root.Close()
	lock, err := fsops.LockPath(e.path)
	if err != nil {
		return config.Config{}, err
	}
	defer lock.Close()
	info, err := root.Stat(e.relative)
	if err != nil {
		return config.Config{}, err
	}
	if info.Mode().Perm()&0222 == 0 {
		return config.Config{}, errors.New("项目配置文件为只读")
	}
	_, err = fsops.PublishAt(context.Background(), root, e.relative, data, info.Mode().Perm(), &e.version)
	return config.Config{}, err
}
