package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ReadSkillTool loads the full body of a skill by name (the Claude Code skill
// loading idea: descriptions ride in the system prompt, bodies load on demand).
type ReadSkillTool struct{}

// NewReadSkillTool creates the ReadSkill tool.
func NewReadSkillTool() *ReadSkillTool { return &ReadSkillTool{} }

func (t *ReadSkillTool) Name() string { return "ReadSkill" }

func (t *ReadSkillTool) Description() string {
	return `Load the full instructions of a skill by name. Skills are reusable
capability packs (workflows, style guides, domain playbooks) available in this
session. The system prompt lists their names and one-line descriptions; call
this tool with the exact name when you need the full skill. Returns the skill's
markdown body.`
}

func (t *ReadSkillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The exact skill name to load.",
			},
		},
		"required": []string{"name"},
	}
}

func (t *ReadSkillTool) Execute(ctx *Context, args map[string]any) (Result, error) {
	return executeText(ctx, args, func(ctx *Context) (string, error) {
		if err := ctx.checkResources(); err != nil {
			return "", err
		}
		name := StringArg(ctx.Args, "name", "")
		if name == "" {
			return "", fmt.Errorf("ReadSkill: name is required")
		}
		if ctx.Skills == nil {
			return "", fmt.Errorf("ReadSkill: skills are not available in this session")
		}
		sk, ok := ctx.Skills.Get(name)
		if !ok {
			all := ctx.Skills.All()
			names := make([]string, 0, len(all))
			for _, s := range all {
				names = append(names, s.Name)
			}
			return "", fmt.Errorf("ReadSkill: no skill %q (available: %s)", name, strings.Join(names, ", "))
		}
		path := filepath.Join(sk.Path, "SKILL.md")
		if _, err := ctx.ResolveRead(path); err != nil {
			return "", fmt.Errorf("ReadSkill: %w", err)
		}
		// A link may have been retargeted since loading. The cached body still
		// belongs to the file that was opened, so authorize that source as well.
		if sk.CanonicalFile == "" {
			return "", fmt.Errorf("ReadSkill: skill %q has no cached source", name)
		}
		if _, err := ctx.ResolveRead(sk.CanonicalFile); err != nil {
			return "", fmt.Errorf("ReadSkill: cached source: %w", err)
		}
		header := fmt.Sprintf("# Skill: %s (source: %s; path: %s)\n\n", sk.Name, sk.Source, path)
		budget := min(50<<10, ctx.outputLimit())
		if len(header)+len(sk.Body) > budget {
			if budget < len(header)+200 {
				return "", fmt.Errorf("ReadSkill: output budget too small; read %q", path)
			}
			return header + truncateUTF8(sk.Body, budget-len(header)-150) + fmt.Sprintf("\n[incomplete; read %q to continue]", path), nil
		}
		return header + sk.Body, nil
	})
}
